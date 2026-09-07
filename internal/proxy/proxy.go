// Package proxy is ProxyForge's front end: it accepts client requests, selects
// a healthy backend, forwards the request, and streams the response back.
package proxy

import (
	"net/http"

	"github.com/manishkumawat24/proxyforge/internal/backend"
	"github.com/manishkumawat24/proxyforge/internal/balancer"
	"github.com/manishkumawat24/proxyforge/internal/logging"
)

// Server routes each inbound request to a backend chosen by its Strategy.
//
// It satisfies http.Handler implicitly -- there is no ": public Handler" and no
// override keyword. Having ServeHTTP with the right signature IS the
// implementation; the compiler verifies it where a *Server is used as a Handler.
type Server struct {
	pool     *backend.Pool
	strategy balancer.Strategy
}

// New builds a Server over the given pool and strategy.
func New(pool *backend.Pool, strategy balancer.Strategy) *Server {
	return &Server{pool: pool, strategy: strategy}
}

// ServeHTTP makes *Server an http.Handler.
//
// net/http runs this on a fresh goroutine per connection (reused across
// keep-alive requests on that connection), so it WILL execute concurrently with
// itself -- often hundreds of times over. Everything it touches must be safe for
// that: Pool.Healthy takes a read lock and returns a copy, Strategy.Pick is
// documented as concurrency-safe, and Acquire/Release are atomic.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Snapshot the eligible backends. This is a copy, so the health checker is
	// free to mutate the pool while we are still working with our list. The
	// worst case is that we dispatch to a backend that went unhealthy
	// microseconds ago -- unavoidable in any distributed system, and handled by
	// the 502 path rather than by trying to hold a lock across the whole
	// request.
	candidates := s.pool.Healthy()

	b := s.strategy.Pick(candidates)
	if b == nil {
		// Record the miss so the log line for this request says why it failed
		// rather than showing an empty backend field.
		logging.SetBackend(r.Context(), "none")
		// Every backend is down (or the pool is empty). 503 is correct here,
		// not 502: 502 says "I asked an upstream and it failed", while 503 says
		// "I have no upstream to ask". Different operational problem, and the
		// distinction matters at 3am.
		http.Error(w, "503 service unavailable: no healthy backends", http.StatusServiceUnavailable)
		return
	}

	// Tell the logging middleware which backend won, so the completion line
	// names it. The middleware ran BEFORE us and has already returned control
	// downward, so this travels back up via a pointer held in the context.
	logging.SetBackend(r.Context(), b.URL.String())

	// Track this request as in-flight for the whole duration. Least Connections
	// reads this counter, so it must be accurate even when the handler exits
	// early or panics -- hence defer, which runs on return AND during a panic
	// unwind.
	b.Acquire()
	defer b.Release()

	// ServeHTTP blocks until the response has been fully streamed back to the
	// client. That is exactly why the in-flight counter is meaningful: it
	// counts requests genuinely in progress, not merely dispatched.
	b.Proxy.ServeHTTP(w, r)
}

// Pool exposes the backend pool, for the admin API and status output.
func (s *Server) Pool() *backend.Pool { return s.pool }

// Strategy exposes the active strategy, for status output.
func (s *Server) Strategy() balancer.Strategy { return s.strategy }
