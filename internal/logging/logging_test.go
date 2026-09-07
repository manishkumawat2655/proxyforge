package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonLogger returns a logger writing JSON into a buffer, so tests can assert
// on structured fields rather than scraping formatted text.
func jsonLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// decodeLine parses the single log line the middleware emitted.
func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("middleware emitted no log line")
	}
	if strings.Count(line, "\n") > 0 {
		t.Fatalf("expected exactly one log line, got:\n%s", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\n%s", err, line)
	}
	return m
}

// TestMiddlewareLogsRequestFields proves one structured line per request with
// the fields we actually need to debug production.
func TestMiddlewareLogsRequestFields(t *testing.T) {
	logger, buf := jsonLogger()

	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetBackend(r.Context(), "http://127.0.0.1:9001")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/thing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	m := decodeLine(t, buf)

	if m["method"] != http.MethodPost {
		t.Errorf("method = %v, want POST", m["method"])
	}
	if m["path"] != "/api/thing" {
		t.Errorf("path = %v, want /api/thing", m["path"])
	}
	// JSON numbers decode as float64 -- a classic surprise when asserting on
	// decoded JSON in any language, Go included.
	if m["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418", m["status"])
	}
	if m["bytes"] != float64(5) {
		t.Errorf("bytes = %v, want 5", m["bytes"])
	}
	if m["backend"] != "http://127.0.0.1:9001" {
		t.Errorf("backend = %v, want the backend set by the handler", m["backend"])
	}
	if _, ok := m["latency_ms"]; !ok {
		t.Error("latency_ms is missing")
	}
	if id, _ := m["request_id"].(string); id == "" {
		t.Error("request_id is empty")
	}
}

// TestRequestIDIsEchoedAndReachesTheHandler proves the ID is both visible to
// the client (so they can quote it in a bug report) and readable downstream.
func TestRequestIDIsEchoedAndReachesTheHandler(t *testing.T) {
	logger, buf := jsonLogger()

	var seen string
	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	header := rec.Header().Get("X-Request-Id")
	if header == "" {
		t.Fatal("X-Request-Id response header is not set")
	}
	if seen != header {
		t.Errorf("handler saw %q but the response header says %q", seen, header)
	}
	if logged := decodeLine(t, buf)["request_id"]; logged != header {
		t.Errorf("logged request_id %v does not match the header %q", logged, header)
	}
}

// TestInboundRequestIDIsHonoured proves a trace survives across service hops.
func TestInboundRequestIDIsHonoured(t *testing.T) {
	logger, _ := jsonLogger()

	var seen string
	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "upstream-trace-123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "upstream-trace-123" {
		t.Errorf("request ID = %q, want the inbound one preserved", seen)
	}
}

// TestRequestIDSanitization is the log-injection test.
//
// A client-supplied ID is attacker-controlled text. Logged verbatim, a newline
// lets them forge entire log entries -- hiding an attack from anyone reading
// the logs, or poisoning a log-parsing alert. This is a real vulnerability
// class, not a theoretical one.
func TestRequestIDSanitization(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "newline injection is stripped",
			in:   "abc\nlevel=ERROR msg=\"fake entry\"",
			want: "abclevelERRORmsgfakeentry",
		},
		{name: "carriage return is stripped", in: "abc\r\ndef", want: "abcdef"},
		{name: "quotes and spaces are stripped", in: `a" b'c`, want: "abc"},
		{name: "safe characters survive", in: "trace-id_123", want: "trace-id_123"},
		{name: "empty stays empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeRequestID(tt.in); got != tt.want {
				t.Errorf("sanitizeRequestID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	// Length cap, so a client cannot bloat every log line.
	long := strings.Repeat("a", 500)
	if got := sanitizeRequestID(long); len(got) != 64 {
		t.Errorf("length = %d, want it capped at 64", len(got))
	}
}

// TestImplicit200IsRecorded covers the case where a handler writes a body
// without ever calling WriteHeader. net/http sends 200 implicitly, and the
// recorder must mirror that or the log reports whatever it was initialized to.
func TestImplicit200IsRecorded(t *testing.T) {
	logger, buf := jsonLogger()

	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("no explicit WriteHeader"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := decodeLine(t, buf)["status"]; got != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", got)
	}
}

// TestDoubleWriteHeaderKeepsTheFirstStatus proves we record what actually went
// on the wire. The first WriteHeader wins; net/http ignores the second (and
// logs a "superfluous response.WriteHeader call" warning).
func TestDoubleWriteHeaderKeepsTheFirstStatus(t *testing.T) {
	logger, buf := jsonLogger()

	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.WriteHeader(http.StatusInternalServerError) // ignored by net/http
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := decodeLine(t, buf)["status"]; got != float64(http.StatusNotFound) {
		t.Errorf("status = %v, want 404 (the first call is the one that is sent)", got)
	}
}

// TestRecorderPreservesFlusher is THE test for the trap that breaks streaming
// proxies.
//
// The concrete value net/http hands a handler also implements http.Flusher.
// Wrapping it in a plain struct makes `w.(http.Flusher)` fail against OUR type,
// and httputil.ReverseProxy uses exactly that assertion to flush streaming
// responses. Lose it and Server-Sent Events, streaming JSON and long-polling
// all silently buffer until the response ends.
//
// It still passes a curl test against a small response, which is precisely why
// this bug ships.
func TestRecorderPreservesFlusher(t *testing.T) {
	logger, _ := jsonLogger()

	var flusherOK, flushed bool
	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		flusherOK = ok
		if ok {
			_, _ = w.Write([]byte("chunk"))
			f.Flush()
			flushed = true
		}
	}))

	// httptest.NewRecorder implements http.Flusher, so this genuinely exercises
	// the pass-through.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !flusherOK {
		t.Fatal("wrapped ResponseWriter no longer satisfies http.Flusher; " +
			"streaming responses through this proxy would buffer instead of streaming")
	}
	if !flushed {
		t.Error("Flush did not run")
	}
}

// TestRecorderUnwrapsForResponseController proves http.ResponseController can
// reach the underlying writer through our wrapper.
//
// ResponseController (Go 1.20+) walks the Unwrap() chain to find a writer
// supporting the operation it needs -- Flush, Hijack for WebSocket upgrades,
// SetReadDeadline. Implementing that one method keeps all of them working,
// including capabilities net/http gains in future versions.
func TestRecorderUnwrapsForResponseController(t *testing.T) {
	logger, _ := jsonLogger()

	var ctrlErr error
	h := Middleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
		ctrlErr = http.NewResponseController(w).Flush()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if ctrlErr != nil {
		t.Errorf("ResponseController.Flush through the wrapper failed: %v; "+
			"Unwrap() is missing or wrong", ctrlErr)
	}
}

// TestNewRejectsBadConfig proves logging config errors surface at startup
// rather than silently defaulting.
func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New("verbose", "text"); err == nil {
		t.Error(`New("verbose", ...) succeeded; "verbose" is not a slog level`)
	}
	if _, err := New("info", "xml"); err == nil {
		t.Error(`New(..., "xml") succeeded; only text and json are supported`)
	}
	if _, err := New("info", "json"); err != nil {
		t.Errorf(`New("info","json") failed: %v`, err)
	}
	// Empty format means "use text", matching the config default.
	if _, err := New("warn", ""); err != nil {
		t.Errorf(`New("warn","") failed: %v`, err)
	}
}
