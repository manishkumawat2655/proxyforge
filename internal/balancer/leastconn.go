package balancer

import (
	"sync/atomic"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// LeastConnections sends each request to whichever backend is currently serving
// the fewest in-flight requests.
//
// This is the strategy that actually adapts to reality. Round robin assumes
// every request costs the same; least connections measures instead. If one
// backend is slow -- GC pause, noisy neighbour, a genuinely expensive query --
// its in-flight count climbs and it stops being chosen until it drains.
//
// The zero value is usable.
type LeastConnections struct {
	// Rotating start offset, used only for tie-breaking. When every backend has
	// the same load (the common case at low traffic -- everything is 0), a
	// naive scan always returns index 0 and all traffic piles onto one backend
	// until it is slow enough to lose. Rotating the scan's starting point makes
	// ties round-robin instead.
	rr atomic.Uint64
}

// Name identifies this strategy in config and status output.
func (l *LeastConnections) Name() string { return "least_connections" }

// Pick returns the least-loaded backend, breaking ties in rotation.
func (l *LeastConnections) Pick(candidates []*backend.Backend) *backend.Backend {
	n := uint64(len(candidates))
	if n == 0 {
		return nil
	}

	start := l.rr.Add(1) - 1

	var best *backend.Backend
	var bestLoad int64

	for k := uint64(0); k < n; k++ {
		b := candidates[(start+k)%n]

		// Each InFlight() is an atomic load, but the SCAN as a whole is not a
		// snapshot -- backend 0's count can change while we are reading backend
		// 3's. So the "least loaded" answer is already slightly stale by the
		// time we return it.
		//
		// That is fine, and worth understanding rather than fixing. Locking the
		// entire pool to get a consistent view would serialize every request in
		// the process against every other one, which costs far more than the
		// occasional imperfect choice. Load balancing is a heuristic; being
		// approximately right without contention beats being exactly right
		// under a global lock.
		load := b.InFlight()

		// Strictly less-than, so an equal load never displaces the incumbent.
		// Combined with the rotating start, that is what makes ties rotate.
		if best == nil || load < bestLoad {
			best = b
			bestLoad = load
		}
	}

	return best
}
