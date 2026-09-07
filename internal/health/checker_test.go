package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// flakyServer is a test backend whose health can be toggled at runtime.
//
// httptest.Server starts a REAL HTTP server on a real loopback port, so this
// exercises the actual network path -- dial, request, response, status code --
// rather than a mock. That matters here: a mocked http.Client would not catch a
// missing body drain, a redirect-following bug, or a timeout that never fires.
type flakyServer struct {
	*httptest.Server
	healthy atomic.Bool
	hits    atomic.Int64
	// delay, when set, makes the handler sleep -- used to prove timeouts work.
	delay atomic.Int64 // nanoseconds
}

func newFlakyServer(t *testing.T) *flakyServer {
	t.Helper()
	fs := &flakyServer{}
	fs.healthy.Store(true)

	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits.Add(1)
		if d := fs.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		if fs.healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	// t.Cleanup runs at the end of THIS test, including on failure. Cleaner
	// than defer in a helper, where the defer would fire when the helper
	// returns rather than when the test finishes.
	t.Cleanup(fs.Close)
	return fs
}

func testConfig() Config {
	return Config{
		Interval:      20 * time.Millisecond,
		Timeout:       500 * time.Millisecond,
		Path:          "/health",
		FailThreshold: 2,
		PassThreshold: 2,
	}
}

// TestCheckerEjectsAndRecovers is the end-to-end state machine test against a
// real server.
//
// It drives CheckOnce directly rather than Run, so the test controls timing
// exactly instead of sleeping and hoping. Time-dependent tests that "sleep 100ms
// and check" are the main source of flaky CI.
func TestCheckerEjectsAndRecovers(t *testing.T) {
	fs := newFlakyServer(t)

	b, err := backend.New(fs.URL, 1)
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	pool := backend.NewPool(b)
	c := New(pool, testConfig())
	ctx := context.Background()

	// Healthy: repeated checks must not eject it.
	c.CheckOnce(ctx)
	c.CheckOnce(ctx)
	if !b.Alive() {
		t.Fatal("healthy backend was ejected")
	}

	// Break it. FailThreshold is 2, so one failure must NOT eject.
	fs.healthy.Store(false)
	c.CheckOnce(ctx)
	if !b.Alive() {
		t.Error("ejected after 1 failure, want ejection only at 2")
	}
	c.CheckOnce(ctx)
	if b.Alive() {
		t.Error("still alive after 2 consecutive failures, want ejected")
	}

	// It must stay out of Healthy() while dead -- this is what actually stops
	// traffic reaching it.
	if got := len(pool.Healthy()); got != 0 {
		t.Errorf("Healthy() = %d while backend is dead, want 0", got)
	}

	// Fix it. PassThreshold is 2, so one success must NOT readmit.
	fs.healthy.Store(true)
	c.CheckOnce(ctx)
	if b.Alive() {
		t.Error("readmitted after 1 success, want readmission only at 2")
	}
	c.CheckOnce(ctx)
	if !b.Alive() {
		t.Error("still dead after 2 consecutive successes, want recovered")
	}
	if got := len(pool.Healthy()); got != 1 {
		t.Errorf("Healthy() = %d after recovery, want 1", got)
	}
}

// TestCheckerTimeoutCountsAsFailure proves a HUNG backend is treated as dead.
//
// This is the failure mode that a naive checker misses entirely. A backend that
// accepts the TCP connection and then never responds is worse than one that
// refuses the connection: without a timeout, the probe blocks forever and the
// backend is never ejected, so traffic keeps flowing to a black hole.
func TestCheckerTimeoutCountsAsFailure(t *testing.T) {
	fs := newFlakyServer(t)

	cfg := testConfig()
	cfg.Timeout = 50 * time.Millisecond
	cfg.FailThreshold = 1

	b, _ := backend.New(fs.URL, 1)
	pool := backend.NewPool(b)
	c := New(pool, cfg)

	// Respond far slower than the timeout allows.
	fs.delay.Store(int64(300 * time.Millisecond))

	start := time.Now()
	c.CheckOnce(context.Background())
	elapsed := time.Since(start)

	if b.Alive() {
		t.Error("backend that timed out is still alive")
	}
	// The probe must abandon at the timeout, not wait out the full handler.
	if elapsed >= 300*time.Millisecond {
		t.Errorf("CheckOnce took %v; timeout of %v was not enforced", elapsed, cfg.Timeout)
	}
}

// TestCheckerDeadBackendIsEjected proves a refused connection is handled -- the
// case where the process is simply gone.
func TestCheckerDeadBackendIsEjected(t *testing.T) {
	fs := newFlakyServer(t)
	b, _ := backend.New(fs.URL, 1)
	pool := backend.NewPool(b)

	cfg := testConfig()
	cfg.FailThreshold = 1
	c := New(pool, cfg)

	// Kill the server outright, so the port refuses connections.
	fs.Close()

	c.CheckOnce(context.Background())
	if b.Alive() {
		t.Error("backend with a refused connection is still alive")
	}
}

// TestCheckerNon2xxIsUnhealthy proves a 404 on the health path ejects the
// backend rather than being quietly tolerated.
//
// A misconfigured health path is a real outage waiting to happen: if 404
// counted as healthy, you would keep a broken backend in rotation and only find
// out from users.
func TestCheckerNon2xxIsUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	b, _ := backend.New(srv.URL, 1)
	pool := backend.NewPool(b)
	cfg := testConfig()
	cfg.FailThreshold = 1
	c := New(pool, cfg)

	c.CheckOnce(context.Background())
	if b.Alive() {
		t.Error("backend returning 404 on the health path is still alive")
	}
}

// TestRunStopsOnContextCancel proves the goroutine lifecycle contract: cancel
// the context and Run returns promptly.
//
// A leaked checker goroutine would keep its whole pool alive against the GC and
// keep probing backends after shutdown had supposedly finished. This test is
// what makes "cancel the context" a guarantee rather than an intention.
func TestRunStopsOnContextCancel(t *testing.T) {
	fs := newFlakyServer(t)
	b, _ := backend.New(fs.URL, 1)
	pool := backend.NewPool(b)
	c := New(pool, testConfig())

	ctx, cancel := context.WithCancel(context.Background())

	// A channel closed when Run returns. Closing a channel is the idiomatic
	// "this happened" broadcast -- every receiver wakes, and unlike a
	// condition variable there is no lost-wakeup race if you close before
	// anyone starts waiting.
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// Let at least one interval elapse so we know it is genuinely looping.
	time.Sleep(60 * time.Millisecond)
	if fs.hits.Load() == 0 {
		t.Fatal("Run never probed the backend")
	}

	cancel()

	select {
	case <-done:
		// Correct: Run observed cancellation and returned.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation; goroutine leaked")
	}

	// And it must genuinely stop probing, not merely return.
	after := fs.hits.Load()
	time.Sleep(100 * time.Millisecond)
	if got := fs.hits.Load(); got != after {
		t.Errorf("probes continued after cancellation: %d -> %d", after, got)
	}
}

// TestCheckOnceProbesAllBackendsConcurrently proves the round is parallel.
//
// Serial probing does not scale: 20 backends behind a 2s timeout would take 40s
// per round and the checker would fall permanently behind a 5s interval. We
// assert the wall-clock time is close to ONE probe, not the sum of three.
func TestCheckOnceProbesAllBackendsConcurrently(t *testing.T) {
	const delay = 150 * time.Millisecond

	bs := make([]*backend.Backend, 0, 3)
	for i := 0; i < 3; i++ {
		fs := newFlakyServer(t)
		fs.delay.Store(int64(delay))
		b, _ := backend.New(fs.URL, 1)
		bs = append(bs, b)
	}
	pool := backend.NewPool(bs...)

	cfg := testConfig()
	cfg.Timeout = 2 * time.Second
	c := New(pool, cfg)

	start := time.Now()
	c.CheckOnce(context.Background())
	elapsed := time.Since(start)

	// Serial would be ~450ms. Allow generous headroom for scheduling so this
	// does not flake on a loaded CI box, while still failing loudly if the
	// probes are actually sequential.
	if elapsed > 350*time.Millisecond {
		t.Errorf("CheckOnce took %v for 3 backends at %v each; probes appear serial",
			elapsed, delay)
	}
}
