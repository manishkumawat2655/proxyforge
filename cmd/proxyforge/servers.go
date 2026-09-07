package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// serverSet owns the two listeners proxyforge runs: the proxy itself and the
// admin API.
//
// It takes already-created net.Listeners rather than calling ListenAndServe,
// for two reasons:
//
//  1. Failing fast. ListenAndServe binds inside the goroutine, so "port already
//     in use" surfaces asynchronously -- after we have already logged
//     "listening on :8080". Binding up front means that error is returned
//     before anything claims to be running.
//  2. Testability. A test can listen on port 0, let the OS pick a free port,
//     and read it back from Listener.Addr(). Hardcoded ports make tests flaky
//     the moment two run at once.
type serverSet struct {
	proxy   *http.Server
	proxyLn net.Listener
	admin   *http.Server
	adminLn net.Listener
	logger  *slog.Logger
}

// start begins serving on both listeners and returns a channel that receives
// the first fatal error, if any.
//
// The channel is BUFFERED with capacity 2 -- one slot per goroutine. With an
// unbuffered channel, the second goroutine to fail would block forever trying
// to send to a channel nobody is receiving from any more, and that goroutine
// would leak for the life of the process. Sizing a result channel to the number
// of possible senders is the standard way to avoid that.
func (s *serverSet) start() <-chan error {
	errCh := make(chan error, 2)

	go func() {
		// Serve always returns a non-nil error. ErrServerClosed is the one that
		// means "Shutdown was called and this is fine" -- treating it as a
		// failure would make every clean shutdown look like a crash.
		if err := s.proxy.Serve(s.proxyLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()

	go func() {
		if err := s.admin.Serve(s.adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin server: %w", err)
		}
	}()

	return errCh
}

// drain stops both servers, letting in-flight requests finish.
//
// http.Server.Shutdown closes the listeners immediately (so no NEW connections
// are accepted), closes idle keep-alive connections, and then waits for active
// requests to complete. Contrast http.Server.Close, which severs everything at
// once -- clients mid-response get a truncated body and a connection reset.
//
// Shutdown does NOT wait for hijacked connections (WebSockets) or for
// long-polling requests that never end on their own; those are the caller's
// problem, which is what the timeout is for.
func (s *serverSet) drain(timeout time.Duration) error {
	// CRITICAL: derive the shutdown deadline from context.Background(), NOT
	// from the context that was just cancelled by the signal.
	//
	// This is the single most common graceful-shutdown bug. Passing the
	// already-cancelled ctx to Shutdown means Shutdown returns instantly with
	// context.Canceled, drains nothing, and every in-flight request is severed
	// -- while your logs cheerfully report a graceful shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Admin first. Once we are shutting down we do not want the control plane
	// accepting backend changes against a pool that is going away, and the
	// admin API has no long-running requests to drain.
	if err := s.admin.Shutdown(ctx); err != nil {
		// Not fatal: losing the admin API during shutdown changes nothing for
		// clients. Log it and carry on to what actually matters.
		s.logger.Error("admin server did not shut down cleanly", "err", err)
	}

	// Now the proxy, which is where real requests live.
	if err := s.proxy.Shutdown(ctx); err != nil {
		// Shutdown returns the context's error if the deadline passes first,
		// meaning some requests were still running and have now been abandoned.
		return fmt.Errorf("drain timed out after %s, in-flight requests were cut off: %w", timeout, err)
	}

	return nil
}
