// Package logging provides structured request logging with request IDs and
// latency.
//
// Everything here is stdlib: log/slog has been in Go since 1.21, so zap and
// logrus buy us nothing but dependencies.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// New builds a logger. format is "text" or "json"; level is one of
// debug/info/warn/error.
func New(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	// UnmarshalText accepts "debug", "INFO", "warn" etc. Reusing it beats
	// hand-writing a switch that then disagrees with slog's own parsing.
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	switch strings.ToLower(format) {
	case "json":
		// Machine-readable: one JSON object per line, which is what every log
		// aggregator wants.
		h = slog.NewJSONHandler(os.Stderr, opts)
	case "text", "":
		// key=value, readable in a terminal.
		h = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("log format %q: want \"text\" or \"json\"", format)
	}

	return slog.New(h), nil
}

// ctxKey is an unexported type used as a context key.
//
// It MUST be a custom type, not a string. Context keys are compared by
// interface equality (type AND value), so a bare string key like "requestID"
// collides with any other package that had the same idea -- silently, at
// runtime, with one package reading another's value. An unexported type cannot
// be constructed outside this package, so collision is impossible by
// construction.
type ctxKey struct{}

// reqInfo is the per-request state the middleware tracks.
//
// It is stored in the context as a POINTER, which is what lets a downstream
// handler attach the chosen backend after the middleware has already run.
// Contexts are immutable -- WithValue returns a new one -- so mutating a
// pointed-to struct is the standard way to pass information back UP the chain.
//
// This is safe without a lock because net/http handles each request on exactly
// one goroutine, so only one goroutine ever touches a given reqInfo.
type reqInfo struct {
	id      string
	backend string
}

// RequestID returns the request ID for this request, or "" outside a request.
func RequestID(ctx context.Context) string {
	if ri, ok := ctx.Value(ctxKey{}).(*reqInfo); ok {
		return ri.id
	}
	return ""
}

// SetBackend records which backend served this request, so the middleware can
// include it in the completion log line.
func SetBackend(ctx context.Context, name string) {
	if ri, ok := ctx.Value(ctxKey{}).(*reqInfo); ok {
		ri.backend = name
	}
}

// newRequestID returns a random 16-hex-character ID.
func newRequestID() string {
	var b [8]byte
	// crypto/rand.Read is documented never to fail on any supported platform
	// (it panics internally if the OS entropy source is broken), so ignoring
	// the error here is safe rather than lazy.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sanitizeRequestID makes a client-supplied ID safe to log.
//
// Accepting an inbound X-Request-Id is how a request gets correlated across
// services -- but logging attacker-controlled text verbatim is LOG INJECTION.
// A value containing a newline lets a client forge entire log entries, which
// can hide an attack from anyone reading the logs or poison a log-parsing
// alert. Cap the length and allow only unambiguous characters.
func sanitizeRequestID(s string) string {
	const maxLen = 64
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			// Drop anything else, newlines and control characters included.
		}
	}
	return sb.String()
}

// Middleware assigns a request ID, records status and bytes, times the request,
// and emits exactly one structured line when it completes.
//
// The signature func(http.Handler) http.Handler is Go's universal middleware
// shape -- take the next handler, return a wrapping one. Because http.Handler
// is a one-method interface, middleware composes by nesting with no framework
// involved.
//
// C++ contrast: this is the decorator pattern, except the "object" is a closure
// over next, and there is no base class anywhere in sight.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Honour an inbound request ID so a trace survives across service
			// hops; generate one otherwise.
			id := sanitizeRequestID(r.Header.Get("X-Request-Id"))
			if id == "" {
				id = newRequestID()
			}

			ri := &reqInfo{id: id}
			ctx := context.WithValue(r.Context(), ctxKey{}, ri)

			// Echo the ID back so a client can quote it in a bug report.
			w.Header().Set("X-Request-Id", id)

			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

			// r.WithContext returns a SHALLOW COPY of the request with the new
			// context. Requests are never mutated in place -- everything
			// downstream must receive the copy.
			next.ServeHTTP(rec, r.WithContext(ctx))

			logger.LogAttrs(ctx, slog.LevelInfo, "request",
				slog.String("request_id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("backend", ri.backend),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.bytes),
				// Milliseconds as a float: readable in a terminal and directly
				// usable for percentiles in an aggregator. Logging a
				// time.Duration renders as "1.234567ms", which is worse for
				// machines.
				slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

// responseRecorder wraps http.ResponseWriter to capture the status code and
// byte count.
//
// Why this exists at all: http.ResponseWriter is write-only. There is no
// StatusCode() to read back, so the ONLY way to know what status was sent is to
// intercept the call that sends it.
type responseRecorder struct {
	// Embedding promotes every ResponseWriter method, so we only override the
	// two we care about. C++ contrast: this is composition that reads like
	// inheritance -- but with no vtable and no base-class constructor.
	http.ResponseWriter

	status      int
	bytes       int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	// Guard against a double call. net/http logs a "superfluous
	// response.WriteHeader call" warning for the second one; without this
	// guard we would also record the wrong status, since the first write is
	// the one that actually goes out.
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	// The first Write implicitly sends 200 if WriteHeader was never called.
	// We have to mirror that or a body-only response would be logged with
	// whatever we initialized status to.
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer if it supports flushing.
//
// THIS IS THE TRAP that makes naive ResponseWriter wrappers break proxies.
//
// http.ResponseWriter is a small interface, but the concrete value net/http
// passes you ALSO implements http.Flusher, http.Hijacker and io.ReaderFrom.
// Code discovers those with a type assertion. The moment you wrap the writer in
// your own struct, that type assertion checks YOUR type -- and fails.
//
// For a reverse proxy the consequence is specific and nasty: httputil.
// ReverseProxy flushes streaming responses through http.Flusher. Lose it and
// Server-Sent Events, streaming JSON and long-polling all stop arriving
// incrementally -- they buffer until the response ends. Everything still
// "works" in a curl test against a small response, so this ships.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter to http.ResponseController.
//
// ResponseController (Go 1.20+) is the modern answer to the interface-loss
// problem above: it walks the Unwrap() chain to find a writer supporting the
// operation it needs -- Hijack for WebSocket upgrades, SetReadDeadline, Flush.
// Implementing this one method keeps every one of those working through our
// wrapper, including capabilities added to net/http in future versions.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
