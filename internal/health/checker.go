// Package health runs active health checks against the backend pool.
//
// "Active" means we probe the backends on a schedule, rather than waiting to
// learn they are down from a failed client request. The difference matters: a
// passive-only proxy discovers a dead backend by serving somebody a 502. Active
// checking means the dead backend is out of rotation before any real traffic
// hits it.
package health

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// Config controls probe scheduling and the failure/recovery thresholds.
type Config struct {
	// Interval between rounds of checks.
	Interval time.Duration
	// Timeout for one individual probe.
	Timeout time.Duration
	// Path appended to each backend's URL, e.g. "/health".
	Path string
	// FailThreshold is how many CONSECUTIVE failures eject a live backend.
	FailThreshold int
	// PassThreshold is how many CONSECUTIVE successes readmit a dead one.
	PassThreshold int
}

// Checker probes every backend in a pool on an interval.
type Checker struct {
	pool   *backend.Pool
	cfg    Config
	client *http.Client
}

// New builds a Checker.
func New(pool *backend.Pool, cfg Config) *Checker {
	return &Checker{
		pool: pool,
		cfg:  cfg,
		client: &http.Client{
			// A DEDICATED transport, deliberately not the shared
			// backend.Transport that real traffic uses.
			//
			// Isolation is the point: health probes should never evict the
			// idle connections that live requests depend on, and a burst of
			// probes should never contend with real traffic for the pool. The
			// tradeoff is that we are technically validating a slightly
			// different connection path than requests take -- acceptable,
			// since what we care about is "is the process answering", not
			// "is this specific socket good".
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   cfg.Timeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 2, // one probe per backend at a time
				IdleConnTimeout:     90 * time.Second,
			},

			// Belt and braces alongside the per-probe context deadline. The
			// context governs the whole operation including body reads; this
			// is a backstop in case a probe path ever forgets to set one.
			Timeout: cfg.Timeout,

			// Do NOT follow redirects. A backend answering 302 is not
			// answering the health check -- chasing the redirect could send us
			// to an entirely different host that IS healthy, and we would
			// happily keep a broken backend in rotation.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Run probes the pool until ctx is cancelled. It BLOCKS, so callers start it
// with `go checker.Run(ctx)`.
//
// Ownership rule this encodes: whoever starts a goroutine is responsible for
// knowing how it stops. Here the answer is "cancel the context", and Run is
// guaranteed to return promptly when you do.
//
// C++ contrast: there is no thread handle to join and no std::jthread with a
// stop_token. context.Context IS the stop token, passed explicitly as the first
// argument by convention -- and unlike a detached std::thread, a leaked
// goroutine keeps its whole captured object graph alive against the GC.
func (c *Checker) Run(ctx context.Context) {
	// Probe immediately rather than waiting a full interval. Otherwise a
	// backend that is already dead at startup stays in rotation -- serving
	// 502s to real clients -- for up to one Interval.
	c.CheckOnce(ctx)

	ticker := time.NewTicker(c.cfg.Interval)
	// Stopping the ticker is mandatory, not tidy-up. An unstopped Ticker holds
	// a runtime timer and its channel alive forever; in a long-lived process
	// that creates checkers dynamically this is a textbook goroutine/memory
	// leak. `defer` right after construction is the habit that prevents it.
	defer ticker.Stop()

	for {
		// select blocks until ONE of its cases is ready; if several are ready
		// it chooses uniformly at random.
		//
		// C++ contrast: this is the piece with no real equivalent. The nearest
		// thing is epoll/WaitForMultipleObjects over several handles, or a
		// condition variable with a predicate covering both "work available"
		// and "please stop".
		select {
		case <-ctx.Done():
			// ctx.Done() returns a channel closed on cancellation. A CLOSED
			// channel is always ready to receive, which is why this works for
			// any number of waiting goroutines at once -- no broadcast needed.
			return
		case <-ticker.C:
			c.CheckOnce(ctx)
		}
	}
}

// CheckOnce probes every backend once, concurrently, and waits for them all.
//
// Concurrently matters: with 20 backends and a 2-second timeout, probing
// serially would take up to 40 seconds and blow straight past a 5-second
// interval. The checker would fall permanently behind its own schedule.
func (c *Checker) CheckOnce(ctx context.Context) {
	var wg sync.WaitGroup

	// All() hands back a snapshot copy, so the admin API is free to mutate the
	// pool while this round is in flight.
	for _, b := range c.pool.All() {
		wg.Add(1) // before `go`, always -- inside the goroutine, Wait could
		// observe zero and return before anything had started.

		// Note we capture b directly rather than passing it as a parameter.
		//
		// This is safe ONLY because go.mod declares go 1.22. Before that, the
		// loop variable was a SINGLE variable reused across iterations, so
		// every goroutine here would race on it and most would observe the
		// final backend -- the single most notorious bug in Go. The fix was
		// `go func(b *backend.Backend){...}(b)`. Go 1.22 changed the spec so
		// each iteration gets a fresh variable, and the old workaround is now
		// unnecessary. The behaviour is selected by the go directive in
		// go.mod, NOT by your installed toolchain version.
		go func() {
			defer wg.Done()

			ok := c.probe(ctx, b)

			// RecordResult returns whether the alive/dead state actually
			// flipped, so we log transitions only. Logging every result would
			// emit len(backends) lines per interval forever, which buries the
			// one line that matters.
			if changed := b.RecordResult(ok, c.cfg.FailThreshold, c.cfg.PassThreshold); changed {
				if ok {
					slog.Info("backend recovered, back in rotation", "backend", b.String())
				} else {
					slog.Warn("backend removed from rotation", "backend", b.String(), "consecutive_failures", c.cfg.FailThreshold)
				}
			}
		}()
	}

	// Wait before returning so the caller knows a full round completed. Without
	// this, a slow round would overlap the next tick and probes would pile up.
	wg.Wait()
}

// probe performs one health request. It reports success, never an error --
// every failure mode (refused, timeout, DNS, 5xx) means the same thing to the
// state machine, and collapsing them here keeps the caller simple.
func (c *Checker) probe(ctx context.Context, b *backend.Backend) bool {
	// Derive a per-probe deadline from the parent context. This gives us BOTH
	// behaviours at once: cancelling the parent (shutdown) aborts an in-flight
	// probe immediately, and a hung backend cannot stall the round past
	// Timeout. Deriving rather than using context.Background() is what makes
	// shutdown fast.
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	// cancel MUST be called on every path or the parent context leaks a child
	// until IT is cancelled. `go vet` has a check for exactly this omission.
	defer cancel()

	// JoinPath (Go 1.19+) handles the slash arithmetic properly -- naive string
	// concatenation gives you "http://host//health" or "http://hosthealth"
	// depending on which side had the slash.
	u := b.URL.JoinPath(c.cfg.Path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout, context cancelled.
		return false
	}
	defer resp.Body.Close()

	// Draining the body before closing is REQUIRED to reuse the connection.
	// Closing an undrained body makes net/http discard the whole TCP
	// connection, so every probe would pay a fresh three-way handshake and the
	// idle pool would never be used. LimitReader caps it so a backend that
	// streams gigabytes from /health cannot exhaust our memory.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	// 2xx and 3xx count as healthy; 4xx and 5xx do not. A 404 on the health
	// path is a misconfiguration and SHOULD eject the backend loudly rather
	// than being quietly tolerated.
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}
