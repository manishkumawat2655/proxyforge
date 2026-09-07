package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newTestSet builds a serverSet on OS-assigned ports with the given proxy
// handler.
//
// Port 0 means "let the kernel pick a free port", which we then read back from
// Listener.Addr(). Hardcoding ports makes tests flake the moment two run
// concurrently or a previous run left a socket in TIME_WAIT.
func newTestSet(t *testing.T, h http.Handler) (*serverSet, string) {
	t.Helper()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	set := &serverSet{
		proxy:   &http.Server{Handler: h, ReadHeaderTimeout: time.Second},
		proxyLn: proxyLn,
		admin:   &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second},
		adminLn: adminLn,
		// Discard log output so test runs stay readable.
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return set, "http://" + proxyLn.Addr().String()
}

// TestDrainWaitsForInFlightRequests is THE test for this phase.
//
// It fires a request that takes 400ms, waits until the handler has definitely
// started, then drains. The request must still complete with a full, correct
// body -- proving Shutdown waited rather than severing the connection.
//
// Without graceful shutdown the client would get a connection reset or a
// truncated body, which is exactly what a user experiences during a careless
// deploy.
func TestDrainWaitsForInFlightRequests(t *testing.T) {
	const workTime = 400 * time.Millisecond

	started := make(chan struct{})
	set, addr := newTestSet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(workTime)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("completed-normally"))
	}))
	set.start()

	type result struct {
		body string
		code int
		err  error
	}
	done := make(chan result, 1)

	go func() {
		resp, err := http.Get(addr + "/slow")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{body: string(b), code: resp.StatusCode, err: err}
	}()

	// Wait for the handler to actually begin. Sleeping a fixed amount and
	// hoping is the classic source of flaky concurrency tests; synchronizing on
	// a channel is exact.
	<-started

	drainStart := time.Now()
	if err := set.drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	drainTook := time.Since(drainStart)

	res := <-done
	if res.err != nil {
		t.Fatalf("in-flight request failed during drain: %v", res.err)
	}
	if res.code != http.StatusOK {
		t.Errorf("status = %d, want 200", res.code)
	}
	if res.body != "completed-normally" {
		t.Errorf("body = %q, want %q (a truncated body means the connection was severed)",
			res.body, "completed-normally")
	}
	// drain must have BLOCKED for roughly the handler's remaining work. If it
	// returned instantly, it did not wait for anything.
	if drainTook < workTime/2 {
		t.Errorf("drain returned after %v, but the handler needed %v; it did not wait",
			drainTook, workTime)
	}
}

// TestDrainStopsAcceptingNewConnections proves the listener closes immediately,
// even while old requests are still draining.
//
// That ordering is the whole point of a graceful shutdown: stop taking new work
// at once, finish existing work at leisure.
func TestDrainStopsAcceptingNewConnections(t *testing.T) {
	set, addr := newTestSet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	set.start()

	// Confirm it works before shutdown, so a failure after is meaningful.
	resp, err := http.Get(addr + "/")
	if err != nil {
		t.Fatalf("pre-shutdown request failed: %v", err)
	}
	_ = resp.Body.Close()

	if err := set.drain(2 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if _, err := http.Get(addr + "/"); err == nil {
		t.Error("server still accepted a connection after drain")
	}
}

// TestDrainTimesOutOnStuckHandler proves the timeout is real.
//
// A handler that never returns must not hang shutdown forever -- otherwise a
// single stuck request means the process never exits and an orchestrator
// eventually SIGKILLs it, severing every OTHER in-flight request too.
func TestDrainTimesOutOnStuckHandler(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	set, addr := newTestSet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // blocks until the test lets go
	}))
	set.start()

	go func() {
		resp, err := http.Get(addr + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	err := set.drain(200 * time.Millisecond)
	// Let the stuck handler go, so the test does not leak a goroutine.
	close(release)

	if err == nil {
		t.Fatal("drain returned nil for a handler that never finished; the timeout did nothing")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	// The message must tell an operator what actually happened.
	if !strings.Contains(err.Error(), "cut off") {
		t.Errorf("error %q should say in-flight requests were cut off", err)
	}
}

// TestStartReportsListenerFailure proves a dead server surfaces as an error on
// the channel rather than vanishing into a goroutine.
func TestStartReportsListenerFailure(t *testing.T) {
	set, _ := newTestSet(t, http.NotFoundHandler())
	errCh := set.start()

	// Closing the listener out from under Serve makes it fail with something
	// other than ErrServerClosed, which is the path we want to exercise.
	_ = set.proxyLn.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("received a nil error")
		}
	case <-time.After(2 * time.Second):
		t.Error("a failed server did not report on the error channel")
	}

	_ = set.drain(time.Second)
}

// TestCleanShutdownIsNotReportedAsAnError proves ErrServerClosed is filtered.
//
// Serve always returns a non-nil error, and ErrServerClosed is the one meaning
// "Shutdown was called, all is well". Treating it as a failure would make every
// clean shutdown exit non-zero -- which an orchestrator reads as a crash loop.
func TestCleanShutdownIsNotReportedAsAnError(t *testing.T) {
	set, _ := newTestSet(t, http.NotFoundHandler())
	errCh := set.start()

	if err := set.drain(2 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}

	select {
	case err := <-errCh:
		t.Errorf("clean shutdown reported an error: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Correct: nothing was sent.
	}
}
