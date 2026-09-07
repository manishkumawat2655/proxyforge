// Package backend models the upstream servers ProxyForge forwards to, plus the
// pool that holds them.
//
// This package sits near the leaf of our import graph: it imports only
// `logging`, which itself imports nothing of ours. That is deliberate. `proxy`,
// `balancer` and `health` all need to talk about backends, so if `backend`
// imported any of them we would have an import cycle -- and Go has no forward
// declarations to break one with. The cycle is a hard compile error, so the
// package graph is forced to stay a DAG.
//
// That constraint is also why the shared http.Transport lives here rather than
// in `proxy`: each Backend owns its own ReverseProxy, so it needs the Transport,
// and it cannot reach into `proxy` to get it.
package backend

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manishkumawat24/proxyforge/internal/logging"
)

// Transport is the shared connection pool every proxied request travels over.
//
// One Transport for the whole process, always. A Transport IS the connection
// pool -- constructing one per request pools nothing and leaks idle
// connections. It is safe for concurrent use by design.
//
// http.DefaultTransport would "work", but its MaxIdleConnsPerHost is 2: fine
// for a general-purpose client touching many hosts occasionally, badly wrong for
// a proxy hammering a handful of hosts constantly. We would pay a fresh TCP
// three-way handshake on nearly every request.
var Transport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	MaxIdleConns:          200,
	MaxIdleConnsPerHost:   100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// Backend is one upstream server.
//
// Concurrency contract -- read this before touching any field:
//
//   - URL, Weight and Proxy are written once at construction and never mutated
//     afterwards, so any goroutine may read them without synchronization.
//   - inFlight is atomic; it changes on every single request.
//   - alive, failures and successes are guarded by mu and must only ever be
//     reached through the accessor methods below.
//
// Mixing those three disciplines in one struct is normal in Go. What is NOT
// normal is reaching past the accessors -- the fields are lowercase precisely so
// that nothing outside this package can.
type Backend struct {
	URL    *url.URL
	Weight int
	Proxy  *httputil.ReverseProxy

	// atomic.Int64 (Go 1.19+) rather than a bare int64 driven by
	// atomic.AddInt64. The wrapper type makes an accidental non-atomic read
	// impossible: there is no way to write b.inFlight and get a plain int64
	// back. The older style compiled fine and raced silently, which is exactly
	// the kind of bug you never find by reading code.
	//
	// C++ contrast: this is std::atomic<int64_t>. Same idea, same reason. Go
	// has no memory_order parameter -- every atomic op is sequentially
	// consistent. Less rope, less foot-shooting.
	inFlight atomic.Int64

	// C++ contrast: sync.RWMutex is std::shared_mutex, and Lock/Unlock are
	// lock()/unlock(). The big difference is that Go has no RAII and no
	// lock_guard -- you unlock with `defer`, which runs on function return
	// including during a panic. Forgetting the defer is the classic Go
	// deadlock.
	mu        sync.RWMutex
	alive     bool
	failures  int // CONSECUTIVE failed health checks
	successes int // CONSECUTIVE successful health checks
}

// New builds a Backend from a raw URL string.
//
// A weight of zero or less is normalized to 1 so that a config that omits
// weights still behaves sensibly under the weighted strategy.
func New(rawURL string, weight int) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing backend URL %q: %w", rawURL, err)
	}
	// url.Parse is far more permissive than it looks. "localhost:9001" parses
	// CLEANLY -- it reads "localhost" as the scheme and "9001" as an opaque
	// body, leaving Host empty and nothing dialable. Plain "9001" parses as a
	// bare path. Neither errors here; both explode much later inside the
	// Transport, with a message that does not point back to this input.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("backend URL %q must start with http:// or https://", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("backend URL %q has no host", rawURL)
	}
	if weight <= 0 {
		weight = 1
	}

	b := &Backend{
		URL:    u,
		Weight: weight,
		// New backends start alive. The health checker will knock them out
		// within one interval if they are not. Starting dead would mean a cold
		// start serves 503s until the first check completes.
		alive: true,
	}

	b.Proxy = &httputil.ReverseProxy{
		Transport: Transport,

		// Rewrite is the modern replacement for Director (Go 1.20+); setting
		// both panics at request time.
		//
		// Director only exposed the OUTBOUND request, which made it easy to
		// forward a client-supplied X-Forwarded-For -- a spoofing vector,
		// because anything downstream trusting that header would believe
		// whatever IP the client claimed. ProxyRequest exposes .In (the
		// untouched original) and .Out, and SetXForwarded REPLACES the client's
		// value rather than appending to it.
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL also sets Out.Host = "", so the outgoing Host header
			// defaults to the backend's own host. To preserve the client's
			// original Host (needed by some vhosted upstreams) you would add
			// pr.Out.Host = pr.In.Host here.
			pr.SetURL(u)
			pr.SetXForwarded()
		},

		// ErrorHandler fires only when NO response could be obtained at all:
		// connection refused, DNS failure, dial timeout, upstream closing the
		// socket before headers.
		//
		// A backend answering 500 does NOT come here. That is a *successful*
		// proxy operation carrying an unsuccessful response, and it streams
		// straight back to the client untouched.
		//
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// slog package-level functions write to the default logger, which
			// main sets once at startup via slog.SetDefault.
			//
			// The tradeoff, stated plainly: injecting a *slog.Logger into
			// backend.New would be more testable and more explicit, at the cost
			// of threading a logger through every constructor and every test.
			// For a leaf package whose only logging is this one error path, the
			// global is the better trade. If this package grew a second logging
			// concern, that calculus would flip.
			slog.Error("proxy error",
				"request_id", logging.RequestID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"backend", u.String(),
				"err", err,
			)
			// 502, not 500: we are a gateway and the upstream failed us. 500
			// would claim the fault was ours.
			http.Error(w, "502 bad gateway: upstream unreachable", http.StatusBadGateway)
		},
	}

	return b, nil
}

// String makes Backend printable with %s / %v. Implementing fmt.Stringer is the
// Go equivalent of overloading operator<< for std::ostream.
func (b *Backend) String() string { return b.URL.String() }

// Alive reports whether this backend is currently in rotation.
func (b *Backend) Alive() bool {
	// RLock, not Lock: many request goroutines read this constantly while only
	// the health checker writes it. An exclusive lock here would serialize
	// every request in the process against every other one.
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.alive
}

// SetAlive forces the alive flag and resets the consecutive-result counters.
// Used at startup and by admin operations; the health checker goes through
// RecordResult instead.
func (b *Backend) SetAlive(alive bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.alive = alive
	b.failures, b.successes = 0, 0
}

// Acquire records the start of an in-flight request.
//
// Callers must pair it with Release, and the idiom is:
//
//	b.Acquire()
//	defer b.Release()
//
// C++ contrast: you would wrap this in an RAII guard and let the destructor
// handle it. Go's `defer` is the closest equivalent -- it runs on function
// return AND while a panic unwinds, so the counter cannot leak on the error
// path. What it is NOT is scope-based: defer fires at FUNCTION exit, not at the
// end of the enclosing block. Deferring inside a loop accumulates.
func (b *Backend) Acquire() { b.inFlight.Add(1) }

// Release records the end of an in-flight request.
func (b *Backend) Release() { b.inFlight.Add(-1) }

// InFlight reports how many requests this backend is currently serving.
// Least Connections reads this; it is a point-in-time value that may already be
// stale by the time you act on it, which is fine for load balancing.
func (b *Backend) InFlight() int64 { return b.inFlight.Load() }

// RecordResult feeds one health-check outcome into this backend's state machine
// and reports whether the alive/dead state actually flipped.
//
// The state machine is deliberately hysteretic: it takes failThreshold
// CONSECUTIVE failures to eject a live backend, and passThreshold CONSECUTIVE
// successes to readmit a dead one. Any single opposite result resets the run.
// That is what stops a backend that fails every other check from flapping in
// and out of rotation on every tick.
//
// Returning "did it change" rather than "is it alive" lets the caller log only
// transitions instead of one line per backend per interval.
func (b *Backend) RecordResult(ok bool, failThreshold, passThreshold int) (changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ok {
		b.failures = 0 // a success breaks any failure run

		// Clamp at the threshold rather than counting forever. These counters
		// answer "how far through a transition are we", and once the
		// transition has happened the number carries no further information.
		// Uncapped, a backend healthy for a week reports successes in the
		// millions -- noise in `proxyforge status` instead of a signal.
		if b.successes < passThreshold {
			b.successes++
		}
		if !b.alive && b.successes >= passThreshold {
			b.alive = true
			return true
		}
		return false
	}

	b.successes = 0 // a failure breaks any success run
	if b.failures < failThreshold {
		b.failures++
	}
	if b.alive && b.failures >= failThreshold {
		b.alive = false
		return true
	}
	return false
}

// Stats is an immutable snapshot of a Backend, for the status endpoint.
//
// We hand out a copy rather than the Backend itself so callers cannot mutate
// live state, and so the whole snapshot is internally consistent -- taken under
// one lock acquisition rather than field by field.
type Stats struct {
	URL       string `json:"url"`
	Weight    int    `json:"weight"`
	Alive     bool   `json:"alive"`
	InFlight  int64  `json:"in_flight"`
	Failures  int    `json:"consecutive_failures"`
	Successes int    `json:"consecutive_successes"`
}

// Stats returns a consistent snapshot of this backend's state.
func (b *Backend) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return Stats{
		URL:       b.URL.String(),
		Weight:    b.Weight,
		Alive:     b.alive,
		InFlight:  b.inFlight.Load(),
		Failures:  b.failures,
		Successes: b.successes,
	}
}
