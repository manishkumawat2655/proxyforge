package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig drops a config file into the test's own temp directory.
//
// t.TempDir() gives each test a fresh directory that is removed automatically
// when the test finishes, including on failure. No cleanup code, no collisions
// between parallel tests, no leftover files in the repo.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// TestLoadAppliesDefaultsForOmittedKeys is the test for the central design
// decision: decode ON TOP of a pre-populated defaults struct.
//
// The file below sets only `backends`. Everything else must come back at its
// default, which proves the decoder leaves absent keys untouched rather than
// zeroing them.
func TestLoadAppliesDefaultsForOmittedKeys(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: http://127.0.0.1:9001
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	def := Default()
	if cfg.Port != def.Port {
		t.Errorf("Port = %d, want default %d", cfg.Port, def.Port)
	}
	if cfg.Strategy != def.Strategy {
		t.Errorf("Strategy = %q, want default %q", cfg.Strategy, def.Strategy)
	}
	if cfg.HealthCheck.Interval != def.HealthCheck.Interval {
		t.Errorf("HealthCheck.Interval = %s, want default %s",
			cfg.HealthCheck.Interval, def.HealthCheck.Interval)
	}
	if cfg.HealthCheck.FailThreshold != def.HealthCheck.FailThreshold {
		t.Errorf("FailThreshold = %d, want default %d",
			cfg.HealthCheck.FailThreshold, def.HealthCheck.FailThreshold)
	}
	// Weight omitted in the file: backend.New normalizes 0 to 1 later, so the
	// config layer legitimately reports what the file said.
	if cfg.Backends[0].Weight != 0 {
		t.Errorf("Backends[0].Weight = %d, want 0 (normalized downstream)", cfg.Backends[0].Weight)
	}
}

// TestLoadOverridesDefaults proves explicit values actually win.
func TestLoadOverridesDefaults(t *testing.T) {
	path := writeConfig(t, `
port: 9999
admin_port: 9998
strategy: least_connections
backends:
  - url: http://127.0.0.1:9001
    weight: 5
  - url: http://127.0.0.1:9002
    weight: 2
health_check:
  interval: 250ms
  timeout: 100ms
  path: /healthz
  fail_threshold: 7
  pass_threshold: 4
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Port)
	}
	if cfg.Strategy != "least_connections" {
		t.Errorf("Strategy = %q, want least_connections", cfg.Strategy)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("len(Backends) = %d, want 2", len(cfg.Backends))
	}
	if cfg.Backends[0].Weight != 5 {
		t.Errorf("Backends[0].Weight = %d, want 5", cfg.Backends[0].Weight)
	}
	if cfg.HealthCheck.Interval != 250*time.Millisecond {
		t.Errorf("Interval = %s, want 250ms", cfg.HealthCheck.Interval)
	}
	if cfg.HealthCheck.FailThreshold != 7 {
		t.Errorf("FailThreshold = %d, want 7", cfg.HealthCheck.FailThreshold)
	}
}

// TestLoadRejectsUnknownKeys is the typo test.
//
// Without KnownFields(true), `strategey:` decodes silently and you run round
// robin in production while your config file claims otherwise. This is one of
// the most common and most infuriating config bugs in any system.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `
strategey: least_connections
backends:
  - url: http://127.0.0.1:9001
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a misspelled key; typos must fail loudly")
	}
	if !strings.Contains(err.Error(), "strategey") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// TestLoadRejectsBareDurationNumber documents yaml.v3's behaviour, which we are
// deliberately relying on: durations must be human-readable strings.
func TestLoadRejectsBareDurationNumber(t *testing.T) {
	path := writeConfig(t, `
backends:
  - url: http://127.0.0.1:9001
health_check:
  interval: 1500
`)

	if _, err := Load(path); err == nil {
		t.Error("Load accepted a bare number for a duration; 1500 is ambiguous and must be rejected")
	}
}

// TestLoadMissingFile proves the error stays inspectable through wrapping, so a
// caller can distinguish "no config file" from "bad config file".
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load succeeded on a missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) = false; %%w wrapping was lost. got: %v", err)
	}
}

// TestValidateReportsAllProblems is the errors.Join test.
//
// A config with four independent mistakes must report all four in one run. The
// alternative -- return on the first error -- means the user fixes one, re-runs,
// finds the next, four times over.
func TestValidateReportsAllProblems(t *testing.T) {
	cfg := Config{
		Port:      0,             // invalid
		AdminPort: 70000,         // invalid
		Strategy:  "round-robin", // hyphen typo, not a real name
		Backends:  nil,           // required
		HealthCheck: HealthCheck{
			Interval:      5 * time.Second,
			Timeout:       2 * time.Second,
			Path:          "/health",
			FailThreshold: 1,
			PassThreshold: 1,
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a config with four problems")
	}

	msg := err.Error()
	for _, want := range []string{"port", "admin_port", "strategy", "backends"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q; all problems should be reported at once.\ngot:\n%s",
				want, msg)
		}
	}
}

// TestValidateTable covers the individual rules, one case per rule.
func TestValidateTable(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Backends = []Backend{{URL: "http://127.0.0.1:9001", Weight: 1}}
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring expected in the message; "" means valid
	}{
		{
			name:   "valid baseline",
			mutate: func(*Config) {},
		},
		{
			name:    "port and admin_port collide",
			mutate:  func(c *Config) { c.AdminPort = c.Port },
			wantErr: "must differ",
		},
		{
			name:    "unknown strategy",
			mutate:  func(c *Config) { c.Strategy = "magic" },
			wantErr: "strategy",
		},
		{
			// url.Parse REJECTS this one outright: "127.0.0.1" is not a legal
			// scheme (schemes cannot start with a digit), so it is treated as a
			// path, and a first path segment may not contain a colon.
			name:    "host:port with a numeric host is unparseable",
			mutate:  func(c *Config) { c.Backends[0].URL = "127.0.0.1:9001" },
			wantErr: "unparseable",
		},
		{
			// This is the genuinely dangerous one. url.Parse ACCEPTS it
			// without error, reading "localhost" as the scheme and "9001" as
			// an opaque body -- so Host comes back empty and there is nothing
			// to dial. Only the explicit scheme check catches it.
			name:    "host:port with an alphabetic host parses but has no scheme",
			mutate:  func(c *Config) { c.Backends[0].URL = "localhost:9001" },
			wantErr: "http://",
		},
		{
			name:    "empty backend url",
			mutate:  func(c *Config) { c.Backends[0].URL = "" },
			wantErr: "url is required",
		},
		{
			name: "duplicate backend urls",
			mutate: func(c *Config) {
				c.Backends = append(c.Backends, Backend{URL: "http://127.0.0.1:9001", Weight: 1})
			},
			wantErr: "duplicates",
		},
		{
			name:    "negative weight",
			mutate:  func(c *Config) { c.Backends[0].Weight = -1 },
			wantErr: "negative",
		},
		{
			name:    "timeout not less than interval",
			mutate:  func(c *Config) { c.HealthCheck.Timeout = c.HealthCheck.Interval },
			wantErr: "must be less than",
		},
		{
			name:    "health path without leading slash",
			mutate:  func(c *Config) { c.HealthCheck.Path = "health" },
			wantErr: "must start with /",
		},
		{
			name:    "zero fail threshold",
			mutate:  func(c *Config) { c.HealthCheck.FailThreshold = 0 },
			wantErr: "fail_threshold",
		},
		{
			name:    "zero pass threshold",
			mutate:  func(c *Config) { c.HealthCheck.PassThreshold = 0 },
			wantErr: "pass_threshold",
		},
		{
			name:    "negative interval",
			mutate:  func(c *Config) { c.HealthCheck.Interval = -1 * time.Second },
			wantErr: "interval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)
			err := cfg.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadEmptyFile proves an empty file is not a parse crash: it means "all
// defaults", and then fails validation for the one thing that has no default.
func TestLoadEmptyFile(t *testing.T) {
	_, err := Load(writeConfig(t, ""))
	if err == nil {
		t.Fatal("empty config should fail validation (no backends)")
	}
	if !strings.Contains(err.Error(), "backends") {
		t.Errorf("empty config should complain about backends, got: %v", err)
	}
}

// TestRepoConfigYAMLIsValid loads the config.yaml checked into the repo root.
//
// Shipping a broken example config is a genuinely common own-goal: the docs say
// "just run this" and it fails on first contact. This test makes that
// impossible to merge.
func TestRepoConfigYAMLIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no config.yaml at repo root: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("the config.yaml shipped in this repo does not load: %v", err)
	}
}
