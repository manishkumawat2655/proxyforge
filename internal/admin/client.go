package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrNotRunning means nothing is listening on the admin port.
//
// This gets its own sentinel because it is far and away the most common
// failure, and it deserves a human answer ("start the proxy first") rather than
// "dial tcp 127.0.0.1:9090: connectex: No connection could be made...".
var ErrNotRunning = errors.New("proxyforge is not running")

// Client talks to a running proxyforge's admin API.
//
// This is the other half of the process boundary: `proxyforge status` builds one
// of these and asks the running `proxyforge start` process what it knows.
type Client struct {
	base string
	http *http.Client
}

// NewClient builds a client for the admin API on the given port.
func NewClient(port int) *Client {
	return &Client{
		base: fmt.Sprintf("http://127.0.0.1:%d", port),
		http: &http.Client{
			// A CLI command must not hang. If the proxy is wedged, we want a
			// prompt error, not a terminal that sits there forever.
			Timeout: 5 * time.Second,
		},
	}
}

// Status fetches the running proxy's state.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	if err := c.do(ctx, http.MethodGet, "/status", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddBackend registers a new backend with the running proxy.
func (c *Client) AddBackend(ctx context.Context, rawURL string, weight int) error {
	return c.do(ctx, http.MethodPost, "/backends", nil, AddRequest{URL: rawURL, Weight: weight}, nil)
}

// RemoveBackend removes a backend from the running proxy.
func (c *Client) RemoveBackend(ctx context.Context, rawURL string) error {
	// url.Values handles escaping, so a backend URL containing & or ? cannot
	// break out of the query parameter. Hand-concatenating query strings is how
	// injection bugs are born.
	q := url.Values{"url": []string{rawURL}}
	return c.do(ctx, http.MethodDelete, "/backends", q, nil, nil)
}

// do performs one admin request, decoding either a success body or an error
// body. Passing nil for body or out skips encoding/decoding respectively.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	endpoint := c.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Translate "connection refused" into something a human can act on.
		//
		// errors.As walks the wrapping chain looking for a specific error TYPE
		// and, when found, assigns it to the target so you can inspect its
		// fields. errors.Is compares against a specific VALUE instead. The
		// distinction matters: net.OpError is a struct with detail we want,
		// not a singleton to compare against.
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return fmt.Errorf("%w: nothing is listening on %s (start it with `proxyforge start`)",
				ErrNotRunning, c.base)
		}
		return fmt.Errorf("calling admin API: %w", err)
	}
	defer resp.Body.Close()

	// Cap the read: this is a trusted local service, but a bounded read costs
	// nothing and means a misbehaving server cannot exhaust our memory.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The server sends {"error": "..."}; surface that rather than a bare
		// status code.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("admin API returned %s", resp.Status)
	}

	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}
