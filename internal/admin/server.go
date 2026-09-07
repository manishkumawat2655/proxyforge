// Package admin is ProxyForge's control plane.
//
// The problem it solves: `proxyforge status` runs as a SEPARATE PROCESS from
// `proxyforge start`. A new process shares none of the running proxy's memory,
// so it cannot read the pool directly. Something has to carry the question
// across a process boundary.
//
// We use a small HTTP API on a second port, bound to loopback. The alternatives
// and why they lost:
//
//   - Named pipes / Unix domain sockets: no open TCP port, but the Windows and
//     Unix code paths are entirely different, and on Windows it needs a
//     third-party package. Rejected on portability.
//   - Rewrite the YAML file and signal the running process to reload: Windows
//     has essentially no usable signals beyond Ctrl+C, and `status` cannot work
//     this way at all -- there is nothing to read.
//
// HTTP wins because it is pure stdlib, identical on every platform, and reuses
// everything we already built in Phase 1.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/manishkumawat24/proxyforge/internal/backend"
	"github.com/manishkumawat24/proxyforge/internal/balancer"
)

// Status is the payload returned by GET /status.
//
// The `json:"..."` tags work exactly like the yaml ones in internal/config --
// struct tags read by a reflection-based encoder. Same rule applies: only
// EXPORTED fields are visible to encoding/json, so a lowercase field here would
// silently never appear in the output.
type Status struct {
	Strategy  string          `json:"strategy"`
	ProxyPort int             `json:"proxy_port"`
	Uptime    string          `json:"uptime"`
	Backends  []backend.Stats `json:"backends"`
}

// AddRequest is the body of POST /backends.
type AddRequest struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

// Server exposes the control plane over HTTP.
type Server struct {
	pool      *backend.Pool
	strategy  balancer.Strategy
	proxyPort int
	started   time.Time
}

// New builds the admin server.
func New(pool *backend.Pool, strategy balancer.Strategy, proxyPort int) *Server {
	return &Server{
		pool:      pool,
		strategy:  strategy,
		proxyPort: proxyPort,
		started:   time.Now(),
	}
}

// Handler returns the admin routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Go 1.22+ ServeMux patterns understand METHODS, not just paths. Before
	// 1.22 you registered "/backends" and hand-wrote a switch on r.Method,
	// remembering to return 405 for anything unexpected. Now the mux does it:
	// a DELETE to a POST-only route gets an automatic 405 Method Not Allowed
	// with a correct Allow header.
	//
	// This is exactly why go.mod says 1.22 rather than 1.21.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /backends", s.handleAddBackend)
	mux.HandleFunc("DELETE /backends", s.handleRemoveBackend)

	return mux
}

// handleStatus reports the live state of the pool.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Status{
		Strategy:  s.strategy.Name(),
		ProxyPort: s.proxyPort,
		Uptime:    time.Since(s.started).Round(time.Second).String(),
		// Stats() snapshots each backend under its own lock, so this is a
		// consistent view per backend even while requests are in flight.
		Backends: s.pool.Stats(),
	})
}

// handleAddBackend adds a backend to the live pool.
//
// Deliberate limitation, documented rather than hidden: this changes the
// RUNNING process only. It does not rewrite config.yaml, so the backend is gone
// after a restart. Writing back to the config file would mean reformatting the
// user's YAML and destroying their comments, which is a worse surprise than
// "runtime changes are runtime-only".
func (s *Server) handleAddBackend(w http.ResponseWriter, r *http.Request) {
	var req AddRequest

	// http.MaxBytesReader caps how much we will read. Without it, a client can
	// stream gigabytes at this endpoint and the decoder will faithfully try to
	// buffer all of it -- a trivial memory-exhaustion DoS on any JSON endpoint.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields() // same reasoning as KnownFields in config

	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	b, err := backend.New(req.URL, req.Weight)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.pool.Add(b); err != nil {
		// 409 Conflict is the correct code for "this already exists", and it
		// lets the CLI distinguish a duplicate from a malformed URL without
		// string-matching the message.
		if errors.Is(err, backend.ErrDuplicate) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"added":  b.URL.String(),
		"weight": fmt.Sprint(b.Weight),
	})
}

// handleRemoveBackend removes a backend from the live pool.
func (s *Server) handleRemoveBackend(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		writeError(w, http.StatusBadRequest, "missing required query parameter: url")
		return
	}

	if err := s.pool.Remove(rawURL); err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Worth being explicit about what did NOT happen: requests already
	// dispatched to this backend are still running and will finish normally.
	// Removal stops future selection, it does not sever live connections.
	writeJSON(w, http.StatusOK, map[string]string{"removed": rawURL})
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	// Content-Type must be set BEFORE WriteHeader -- header mutations after the
	// status line is committed are silently dropped.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// If encoding fails here we cannot do anything useful about it: the status
	// line is already on the wire, so we cannot switch to a 500. Logging is the
	// only honest option, and for these tiny payloads it will not happen.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError returns a JSON error body so the CLI can parse it uniformly.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
