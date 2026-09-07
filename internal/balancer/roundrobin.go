package balancer

import (
	"sync/atomic"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// RoundRobin hands out backends in strict rotation: 0, 1, 2, 0, 1, 2, ...
//
// The zero value is ready to use -- no constructor needed. That is a Go design
// habit worth copying: make the zero value meaningful so callers can write
// &RoundRobin{} and be correct.
type RoundRobin struct {
	// THE central concurrency problem of this whole project lives in this one
	// field, so it is worth being precise about it.
	//
	// The obvious implementation is:
	//
	//     i := r.n
	//     r.n++
	//     return candidates[i%len(candidates)]
	//
	// That compiles, passes every single-threaded test you write, and is
	// broken. `r.n++` is not one operation -- it is read, add, write. Two
	// goroutines can both read 7, both compute 8, and both write 8. Result:
	// two requests go to the SAME backend and one backend is skipped entirely.
	// Under real load your "round robin" quietly degenerates into something
	// lumpy, and the counter drifts below the true request count.
	//
	// Worse, it is a data race in the formal sense, which in Go means the
	// program's behaviour is undefined -- not merely "an off-by-one sometimes".
	// `go test -race` flags it in milliseconds.
	//
	// atomic.Uint64 makes the read-modify-write a single indivisible
	// instruction (LOCK XADD on x86).
	//
	// C++ contrast: identical reasoning to std::atomic<uint64_t> with
	// fetch_add. The difference is that Go's atomics have no memory_order
	// parameter -- every operation is sequentially consistent. You give up
	// relaxed-ordering performance and gain never having to reason about it.
	n atomic.Uint64
}

// Name identifies this strategy in config and status output.
func (r *RoundRobin) Name() string { return "round_robin" }

// Pick returns the next backend in rotation.
func (r *RoundRobin) Pick(candidates []*backend.Backend) *backend.Backend {
	if len(candidates) == 0 {
		return nil
	}

	// Add returns the value AFTER adding, so subtract 1 to make the very first
	// pick land on index 0. Purely cosmetic, but it makes tests readable.
	//
	// Overflow is safe and intentional here: Go defines unsigned integer
	// overflow as wrapping. At one billion requests per second this wraps
	// after roughly 584 years, and when it does the modulo simply continues.
	//
	// C++ contrast: unsigned overflow wraps in C++ too, but SIGNED overflow is
	// undefined behaviour. In Go both are defined to wrap, so there is no
	// UB-shaped hole to fall into.
	i := r.n.Add(1) - 1

	// Note we take len(candidates) fresh on every call rather than caching it.
	// The candidate list changes as backends fail health checks, and a cached
	// length would index out of bounds the moment the pool shrank.
	return candidates[i%uint64(len(candidates))]
}
