package balancer

import (
	"sync"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// WeightedRoundRobin distributes requests in proportion to each backend's
// weight, using the "smooth weighted round-robin" algorithm that nginx uses.
//
// Why smooth, and not the obvious approach:
//
// The naive way to weight a rotation is to expand the list -- weights {a:5,
// b:1, c:1} becomes [a a a a a b c] and you round-robin that. It produces the
// right *totals*, but the ordering is terrible: five consecutive requests slam
// a, then b and c each get one. A burst of five requests all lands on one
// backend while two sit idle. It also allocates a slice proportional to the sum
// of weights, which for {1000, 1} is a 1001-element slice.
//
// Smooth WRR produces a, a, b, a, c, a, a for those same weights: identical
// totals (5/1/1 over each cycle of 7), but the heavy backend's turns are spread
// out instead of clumped. No allocation proportional to weight, either.
//
// The algorithm, per pick:
//  1. every backend's currentWeight += its configured weight
//  2. pick the backend with the highest currentWeight
//  3. that winner's currentWeight -= the total of all weights
//
// Step 3 is what creates the smoothing: winning pushes you deep negative, so
// you have to climb back before winning again, and how fast you climb is your
// weight. Over one full cycle the currentWeights return to where they started.
type WeightedRoundRobin struct {
	// A plain Mutex, not RWMutex. Every single Pick mutates state, so there are
	// no read-only callers to benefit -- and RWMutex is measurably slower than
	// Mutex when every acquisition is a write.
	mu sync.Mutex

	// Selection state, keyed by backend pointer. It has to be a map rather than
	// a slice parallel to candidates, because the candidate list changes shape
	// as backends fail and recover health checks; a positional slice would
	// silently attribute one backend's accumulated credit to another.
	current map[*backend.Backend]int
}

// NewWeightedRoundRobin builds a ready-to-use strategy.
//
// Unlike RoundRobin, the zero value is NOT usable here: a nil map panics on
// write (reads from a nil map are fine and return the zero value -- writes are
// not). Hence a real constructor.
func NewWeightedRoundRobin() *WeightedRoundRobin {
	return &WeightedRoundRobin{current: make(map[*backend.Backend]int)}
}

// Name identifies this strategy in config and status output.
func (w *WeightedRoundRobin) Name() string { return "weighted_round_robin" }

// Pick returns the next backend, weighted.
func (w *WeightedRoundRobin) Pick(candidates []*backend.Backend) *backend.Backend {
	if len(candidates) == 0 {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	total := 0
	var best *backend.Backend
	bestScore := 0

	// We iterate the candidates SLICE, never the map. Go deliberately
	// randomizes map iteration order, so ranging over w.current would make
	// tie-breaking non-deterministic and the tests flaky.
	//
	// C++ contrast: std::unordered_map has an arbitrary-but-stable order. Go
	// randomizes it on purpose, specifically to stop people writing code that
	// accidentally depends on the order.
	for _, b := range candidates {
		w.current[b] += b.Weight
		total += b.Weight

		if best == nil || w.current[b] > bestScore {
			best = b
			bestScore = w.current[b]
		}
	}

	w.current[best] -= total

	// Housekeeping: drop state for backends that are no longer candidates,
	// otherwise this map grows forever as backends are removed via the admin
	// API -- a slow leak keyed by pointers that will never be seen again.
	//
	// Guarded by the length check so the hot path (candidate set unchanged)
	// costs one integer comparison and allocates nothing.
	if len(w.current) > len(candidates) {
		present := make(map[*backend.Backend]struct{}, len(candidates))
		for _, b := range candidates {
			// struct{}{} is a zero-byte value -- this is Go's idiom for a set,
			// since there is no built-in set type. map[T]bool would cost a byte
			// per entry and invite the "is it absent or false?" confusion.
			present[b] = struct{}{}
		}
		for b := range w.current {
			if _, ok := present[b]; !ok {
				// Deleting from a map DURING range over that same map is
				// explicitly legal in Go (the spec allows it), unlike erasing
				// from a std::map mid-iteration without care.
				delete(w.current, b)
			}
		}
	}

	return best
}
