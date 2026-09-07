package balancer

import (
	"math/rand/v2"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// Random picks a uniformly random backend.
//
// Worth taking seriously despite being three lines: with no shared counter at
// all, it has zero coordination between goroutines and therefore zero
// contention. Round robin's atomic counter is a single cache line that every
// CPU core fights over -- at very high request rates that cache-line ping-pong
// becomes measurable. Random has no such point.
//
// The tradeoff is that even distribution is only statistical. Over 10 requests
// across 3 backends you will see visible lumpiness; over 100,000 it converges.
//
// Note the VALUE receiver on these methods, unlike the other strategies. Random
// carries no state, so there is nothing to mutate and nothing to share.
//
// The method-set rule this exposes: a value receiver means BOTH Random and
// *Random satisfy Strategy, because the method set of *T includes every method
// declared with receiver T. The reverse is not true -- if these had pointer
// receivers, a plain Random value would NOT satisfy Strategy, and `var s
// Strategy = Random{}` would fail to compile. That asymmetry is behind most
// "does not implement" errors people hit early in Go.
type Random struct{}

// Name identifies this strategy in config and status output.
func (Random) Name() string { return "random" }

// Pick returns a uniformly random backend.
func (Random) Pick(candidates []*backend.Backend) *backend.Backend {
	if len(candidates) == 0 {
		return nil
	}

	// math/rand/v2 (Go 1.22+), not math/rand. Two reasons:
	//
	//  1. The top-level v2 functions are safe for concurrent use and use a
	//     per-P generator, so there is no global mutex to contend on. The v1
	//     top-level functions were also safe, but via a single global lock --
	//     which would have reintroduced exactly the contention this strategy
	//     exists to avoid.
	//  2. v2 is auto-seeded. The classic v1 bug was forgetting
	//     rand.Seed(time.Now().UnixNano()) and getting the identical "random"
	//     sequence on every process start.
	//
	// IntN, not Int()%n: modulo on a full-range integer introduces modulo bias
	// when n does not evenly divide the generator's range. IntN handles it.
	// Same trap as rand() % n in C.
	return candidates[rand.IntN(len(candidates))]
}
