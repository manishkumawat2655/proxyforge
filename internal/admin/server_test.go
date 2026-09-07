package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manishkumawat24/proxyforge/internal/backend"
	"github.com/manishkumawat24/proxyforge/internal/balancer"
)

func newTestServer(t *testing.T, urls ...string) (*Server, *backend.Pool) {
	t.Helper()
	pool := backend.NewPool()
	for _, u := range urls {
		b, err := backend.New(u, 1)
		if err != nil {
			t.Fatalf("backend.New(%q): %v", u, err)
		}
		if err := pool.Add(b); err != nil {
			t.Fatalf("pool.Add: %v", err)
		}
	}
	return New(pool, &balancer.RoundRobin{}, 8080), pool
}

// do drives the handler directly with httptest.NewRecorder -- no real socket,
// no port to allocate, no cleanup. ResponseRecorder simply captures what the
// handler wrote.
func do(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// TestStatusReportsPool proves GET /status serializes the live pool, including
// backends that are DOWN -- an operator needs to see those most of all.
func TestStatusReportsPool(t *testing.T) {
	s, pool := newTestServer(t, "http://127.0.0.1:9001", "http://127.0.0.1:9002")
	pool.All()[1].SetAlive(false)

	w := do(t, s, http.MethodGet, "/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if got.Strategy != "round_robin" {
		t.Errorf("Strategy = %q, want round_robin", got.Strategy)
	}
	if got.ProxyPort != 8080 {
		t.Errorf("ProxyPort = %d, want 8080", got.ProxyPort)
	}
	if len(got.Backends) != 2 {
		t.Fatalf("len(Backends) = %d, want 2 (dead backends must still be listed)", len(got.Backends))
	}
	if !got.Backends[0].Alive || got.Backends[1].Alive {
		t.Errorf("alive flags = [%v %v], want [true false]",
			got.Backends[0].Alive, got.Backends[1].Alive)
	}
}

// TestAddBackend covers the success path and every rejection the CLI relies on
// being distinguishable by status code.
func TestAddBackend(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{
			name:     "valid backend is created",
			body:     `{"url":"http://127.0.0.1:9002","weight":3}`,
			wantCode: http.StatusCreated,
		},
		{
			// 409, not 400, so the CLI can tell "already there" from "bad
			// input" without string-matching the message.
			name:     "duplicate url conflicts",
			body:     `{"url":"http://127.0.0.1:9001","weight":1}`,
			wantCode: http.StatusConflict,
		},
		{
			name:     "url without a scheme is rejected",
			body:     `{"url":"localhost:9002"}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "malformed json is rejected",
			body:     `{"url":`,
			wantCode: http.StatusBadRequest,
		},
		{
			// DisallowUnknownFields: a typo'd field must not be silently
			// ignored, same reasoning as KnownFields in the config package.
			name:     "unknown field is rejected",
			body:     `{"url":"http://127.0.0.1:9003","wieght":5}`,
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t, "http://127.0.0.1:9001")
			w := do(t, s, http.MethodPost, "/backends", tt.body)
			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tt.wantCode, w.Body.String())
			}
		})
	}
}

// TestAddBackendActuallyReachesThePool proves the handler mutates live state,
// not just returns 201.
func TestAddBackendActuallyReachesThePool(t *testing.T) {
	s, pool := newTestServer(t, "http://127.0.0.1:9001")

	w := do(t, s, http.MethodPost, "/backends", `{"url":"http://127.0.0.1:9002","weight":4}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if got := pool.Len(); got != 2 {
		t.Fatalf("pool.Len() = %d, want 2", got)
	}
	if got := pool.All()[1].Weight; got != 4 {
		t.Errorf("weight = %d, want 4", got)
	}
}

// TestRemoveBackend covers removal and its error cases.
func TestRemoveBackend(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		wantCode int
		wantLen  int
	}{
		{
			name:     "existing backend is removed",
			target:   "/backends?url=http://127.0.0.1:9001",
			wantCode: http.StatusOK,
			wantLen:  0,
		},
		{
			name:     "unknown backend is 404",
			target:   "/backends?url=http://127.0.0.1:9999",
			wantCode: http.StatusNotFound,
			wantLen:  1,
		},
		{
			name:     "missing url parameter is 400",
			target:   "/backends",
			wantCode: http.StatusBadRequest,
			wantLen:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, pool := newTestServer(t, "http://127.0.0.1:9001")
			w := do(t, s, http.MethodDelete, tt.target, "")
			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if got := pool.Len(); got != tt.wantLen {
				t.Errorf("pool.Len() = %d, want %d", got, tt.wantLen)
			}
		})
	}
}

// TestMethodNotAllowed proves the Go 1.22 method-aware ServeMux is doing its
// job: wrong method gets an automatic 405 with a correct Allow header, with no
// hand-written switch on r.Method.
func TestMethodNotAllowed(t *testing.T) {
	s, _ := newTestServer(t, "http://127.0.0.1:9001")

	w := do(t, s, http.MethodPost, "/status", "{}")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /status = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow header = %q, want it to mention GET", allow)
	}
}

// TestOversizedBodyRejected proves the MaxBytesReader cap works.
//
// Without it, a client can stream gigabytes at this endpoint and the JSON
// decoder will faithfully try to buffer all of it -- a one-line
// memory-exhaustion DoS present on almost every naive JSON API.
func TestOversizedBodyRejected(t *testing.T) {
	s, _ := newTestServer(t, "http://127.0.0.1:9001")

	huge := `{"url":"http://127.0.0.1:9002","weight":1,"pad":"` + strings.Repeat("A", 64*1024) + `"}`
	w := do(t, s, http.MethodPost, "/backends", huge)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized body", w.Code)
	}
}
