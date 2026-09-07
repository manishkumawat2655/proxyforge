// Command dummybackend runs one throwaway HTTP server that identifies itself in
// every response. Run several on different ports to give ProxyForge something
// real to balance across, then watch the responses change as it rotates.
//
// Usage:
//
//	go run ./cmd/dummybackend -port 9001
//	go run ./cmd/dummybackend -port 9002 -name beta
//
// This is a throwaway test harness, not production code -- but the HTTP habits
// in it (explicit server, real timeouts) are the ones worth copying.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// server holds this fake backend's identity. The handlers are methods on it so
// they can reach that identity without package-level globals -- globals and
// goroutines are a bad pair, and every http.Handler runs on its own goroutine.
//
// C++ contrast: this is your class. There is no `class` keyword and no
// public/private specifiers. Lowercase field names are visible only inside this
// package; capitalized ones would be visible to importers. Access control in Go
// is per-package, not per-type.
type server struct {
	name    string
	port    int
	started time.Time
}

func main() {
	// flag.Int does NOT return an int -- it returns a *int that still holds the
	// default value. flag.Parse() is what actually fills it in from os.Args.
	// C++ contrast: no argv loop, no getopt, no third-party arg parser needed.
	port := flag.Int("port", 9001, "TCP port to listen on")
	name := flag.String("name", "", "human-readable name for this backend (default: backend-<port>)")
	flag.Parse()

	if *name == "" {
		// Sprintf, not string concatenation with +. Idiomatic and allocation-friendly.
		*name = fmt.Sprintf("backend-%d", *port)
	}

	s := &server{name: *name, port: *port, started: time.Now()}

	// ServeMux is Go's built-in HTTP router -- it is itself an http.Handler,
	// which is how handlers compose in Go (we lean on that hard in Phase 7).
	//
	// Pattern rule you must internalize: a pattern ending in "/" is a PREFIX
	// match, anything else is an EXACT match. So "/health" matches only
	// /health, while "/" is the catch-all for everything else.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.handleEcho)

	// Bind to 127.0.0.1 rather than :port. ":9001" listens on every interface
	// including your LAN address; on Windows that also triggers a firewall
	// prompt. A local test harness has no business being reachable off-box.
	addr := fmt.Sprintf("127.0.0.1:%d", *port)

	// We construct http.Server explicitly instead of calling the one-line
	// http.ListenAndServe(addr, mux), because that helper uses a zero-value
	// Server -- meaning NO timeouts at all. A client that opens a connection
	// and never finishes sending its headers would occupy a goroutine forever.
	// ReadHeaderTimeout is the cheapest defense against that (Slowloris).
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// log writes to STDERR by default, not stdout. Worth knowing before you go
	// hunting for missing output in a redirected log file.
	log.Printf("%s listening on http://%s", s.name, addr)

	// ListenAndServe blocks until the server stops, and ALWAYS returns a
	// non-nil error -- there is no success return. http.ErrServerClosed is the
	// one that means "clean shutdown"; we start caring about that in Phase 8.
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("%s stopped: %v", s.name, err)
		os.Exit(1)
	}
}

// handleHealth is the endpoint ProxyForge's health checker will poll in Phase 4.
// For now it is unconditionally healthy; we add a way to force it to fail once
// we have a checker worth testing.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Order matters: WriteHeader must come before any Write. The first Write
	// implicitly sends a 200, so once a single byte is out you can no longer
	// change the status code. That one-way door is why Phase 7 has to wrap
	// ResponseWriter to find out what status was actually sent.
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "ok %s uptime=%s\n", s.name, time.Since(s.started).Round(time.Second))
}

// handleEcho answers every non-/health request with a plain-text body saying
// which backend served it. This is the whole point of the harness: when
// ProxyForge starts rotating in Phase 2, you will literally see the name change
// between curls.
//
// Expected behaviour:
//
//   - Set the response header "X-Backend-Name" to s.name. Later phases assert
//     on this header rather than parsing the body, so it must always be set --
//     including on the error paths below.
//
//   - Write a body identifying, at minimum: this backend's name and port, and
//     the request's method and path. One "key: value" per line is fine.
//
//   - Support ?delay=<duration> (e.g. ?delay=250ms, ?delay=2s): wait that long
//     before responding. You will need this to exercise Least Connections in
//     Phase 3 and health-check timeouts in Phase 4. A malformed duration should
//     produce 400, not be silently ignored -- silent ignores make for miserable
//     debugging at 1am.
//
//   - Support ?fail=<code> (e.g. ?fail=503): respond with that status code
//     instead of 200. A non-numeric or nonsensical code is also a 400.
//
// Hints on the Go surface you need, not on the approach:
//
//   - r.URL.Query() gives you a url.Values, which is a map. A missing key
//     returns the zero value ("") -- Go maps never throw on a missing key, so
//     "was it absent?" and "was it empty?" look identical unless you check.
//   - time.ParseDuration and strconv.Atoi both return (value, error). Go has no
//     exceptions: the error IS the second return value and ignoring it is a
//     deliberate act.
//   - http.Error(w, msg, code) writes a status and a plain-text body in one go.
func (s *server) handleEcho(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Identify ourselves first, so the header is present even on the error
	// paths below. Setting a header only mutates a map -- nothing is sent to
	// the client until the first WriteHeader/Write.
	w.Header().Set("X-Backend-Name", s.name)

	// url.Values is a map[string][]string, and a Go map returns the ZERO VALUE
	// for a missing key instead of throwing. So "?delay absent" and "?delay="
	// are indistinguishable here -- both yield "". We deliberately treat both
	// as "not requested" rather than as an error.
	if raw := q.Get("delay"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			// Reject loudly. Silently ignoring a malformed knob is how you end
			// up at 1am wondering why your latency test "isn't working".
			http.Error(w, fmt.Sprintf("bad delay %q: %v", raw, err), http.StatusBadRequest)
			return
		}
		if d < 0 || d > 30*time.Second {
			http.Error(w, fmt.Sprintf("delay %s out of range (0s..30s)", d), http.StatusBadRequest)
			return
		}
		// time.Sleep parks THIS goroutine only -- it does not block an OS
		// thread. The Go scheduler hands the underlying thread to another
		// goroutine, so 1000 concurrent slow requests cost ~1000 small
		// goroutine stacks, not 1000 OS threads.
		//
		// C++ contrast: std::this_thread::sleep_for in a thread-per-connection
		// server really does idle a kernel thread, which is why that model caps
		// out in the low thousands of connections.
		time.Sleep(d)
	}

	status := http.StatusOK
	if raw := q.Get("fail"); raw != "" {
		code, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, fmt.Sprintf("bad fail %q: %v", raw, err), http.StatusBadRequest)
			return
		}
		if code < 100 || code > 599 {
			http.Error(w, fmt.Sprintf("fail %d is not a valid HTTP status code", code), http.StatusBadRequest)
			return
		}
		status = code
	}

	// Content-Type has to be set BEFORE WriteHeader. Header mutations made
	// after the status line is committed are silently discarded -- no panic,
	// no error, they just vanish.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)

	fmt.Fprintf(w, "backend: %s\n", s.name)
	fmt.Fprintf(w, "port:    %d\n", s.port)
	fmt.Fprintf(w, "method:  %s\n", r.Method)
	fmt.Fprintf(w, "path:    %s\n", r.URL.Path)
	fmt.Fprintf(w, "status:  %d\n", status)
}
