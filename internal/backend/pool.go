package backend

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors. Callers compare with errors.Is rather than string matching.
//
// C++ contrast: these are your exception *types*, except they are ordinary
// values you return. errors.Is walks a wrapping chain (built with %w in
// fmt.Errorf), so an error wrapped three layers deep still matches -- the rough
// equivalent of catching a base class.
var (
	ErrDuplicate = errors.New("backend already in pool")
	ErrNotFound  = errors.New("backend not found in pool")
)

// Pool is the set of backends the proxy balances across.
//
// It is safe for concurrent use. Request goroutines read it constantly, the
// health checker reads it every interval, and the admin API mutates it -- all
// at the same time.
type Pool struct {
	mu       sync.RWMutex
	backends []*Backend
}

// NewPool builds a pool from zero or more backends.
//
// The `...*Backend` is a variadic parameter, Go's answer to a parameter pack.
// Unlike C++ variadics it is not a template: inside the function bs is simply a
// []*Backend, resolved at runtime, with no per-instantiation code generated.
func NewPool(bs ...*Backend) *Pool {
	p := &Pool{backends: make([]*Backend, 0, len(bs))}
	p.backends = append(p.backends, bs...)
	return p
}

// Add puts a backend in the pool, rejecting duplicates by URL.
func (p *Pool) Add(b *Backend) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, existing := range p.backends {
		if existing.URL.String() == b.URL.String() {
			// %w wraps the sentinel so errors.Is(err, ErrDuplicate) still
			// matches while the message stays specific about which URL.
			return fmt.Errorf("%s: %w", b.URL, ErrDuplicate)
		}
	}
	p.backends = append(p.backends, b)
	return nil
}

// Remove takes a backend out of the pool by URL.
//
// In-flight requests already dispatched to it are NOT cancelled -- they were
// handed a *Backend pointer and will finish against it normally. Removal only
// stops FUTURE requests from selecting it. That is the behaviour you want:
// yanking live requests would turn an operator action into client-visible
// errors.
func (p *Pool) Remove(rawURL string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, b := range p.backends {
		if b.URL.String() == rawURL {
			// Delete from a slice while preserving order. append with a
			// spread re-slices in place -- there is no std::vector::erase and
			// no iterator invalidation to reason about, because every other
			// goroutine reads a COPY (see All below), never this slice.
			p.backends = append(p.backends[:i], p.backends[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%s: %w", rawURL, ErrNotFound)
}

// All returns a snapshot copy of every backend in the pool.
//
// Returning a COPY of the slice is the entire point of this method. If we
// returned p.backends directly, the caller would hold a slice whose backing
// array Add can reallocate and Remove can shuffle underneath them -- a textbook
// data race that `go test -race` catches and that production serves as a torn
// read or a stale entry.
//
// Note what is NOT copied: the *Backend pointers themselves are shared, which is
// intended. Each Backend does its own internal locking, so concurrent access to
// the objects is fine. What we are protecting here is the SLICE, not the
// backends.
func (p *Pool) All() []*Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]*Backend, len(p.backends))
	copy(out, p.backends)
	return out
}

// Healthy returns a snapshot of only the backends currently in rotation.
//
// This is where the "which backends are eligible" policy lives, so that no
// Strategy has to know anything about health. A Strategy just picks from
// whatever candidate list it is handed.
func (p *Pool) Healthy() []*Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]*Backend, 0, len(p.backends))
	for _, b := range p.backends {
		// Careful: this calls b.Alive(), which takes b.mu while we hold p.mu.
		// Nested locking is where deadlocks are born. It is safe here ONLY
		// because the ordering is strictly one-way -- pool lock then backend
		// lock, never the reverse. Nothing in package backend ever takes p.mu
		// while holding a b.mu.
		if b.Alive() {
			out = append(out, b)
		}
	}
	return out
}

// Len reports how many backends are in the pool, healthy or not.
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.backends)
}

// Stats returns a snapshot of every backend's state, for the status endpoint.
func (p *Pool) Stats() []Stats {
	// Deliberately built from All() rather than under p.mu, so we hold the pool
	// lock for as little time as possible. The result is "eventually
	// consistent" -- each Backend's snapshot is internally consistent, but two
	// backends may be sampled microseconds apart. For a status display that is
	// entirely fine, and it keeps a slow JSON encode off the hot path's lock.
	bs := p.All()
	out := make([]Stats, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Stats())
	}
	return out
}
