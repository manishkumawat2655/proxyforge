// Package config loads and validates ProxyForge's YAML configuration.
//
// Two ideas drive the design here:
//
//  1. Defaults are applied by pre-populating the struct BEFORE decoding, not by
//     patching zero values afterwards. See Load for why that distinction
//     matters more than it looks.
//  2. Validation reports EVERY problem at once, not the first one. Fixing a
//     config file one error per run is a miserable experience.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/manishkumawat24/proxyforge/internal/balancer"
	"github.com/manishkumawat24/proxyforge/internal/logging"
)

// Config is the whole configuration file.
//
// The `yaml:"..."` strings are STRUCT TAGS: arbitrary metadata attached to a
// field, readable at runtime via reflection. The yaml package uses them to map
// snake_case document keys onto Go's CamelCase exported fields.
//
// C++ contrast: there is no equivalent before C++26 reflection. You would hand-
// write a from_yaml() per struct, or generate it, or lean on a macro. Go trades
// some compile-time safety (a tag typo is not a compile error) for not writing
// that code at all.
//
// Note every field is exported (capitalized). Reflection-based decoders cannot
// see unexported fields, so a lowercase field here would silently stay at its
// zero value forever -- a genuinely nasty bug to track down.
type Config struct {
	Port            int           `yaml:"port"`
	AdminPort       int           `yaml:"admin_port"`
	Strategy        string        `yaml:"strategy"`
	Backends        []Backend     `yaml:"backends"`
	HealthCheck     HealthCheck   `yaml:"health_check"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	Log             Log           `yaml:"log"`
}

// Log controls structured logging output.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text or json
}

// Backend is one upstream entry in the config file.
type Backend struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// HealthCheck holds the probe settings.
type HealthCheck struct {
	// yaml.v3 decodes time.Duration from human strings like "5s" or "1m30s"
	// natively, and REJECTS a bare number. That is the behaviour we want: a
	// plain `interval: 1500` is ambiguous (milliseconds? seconds?) and Go's
	// own answer -- nanoseconds -- would surprise everyone.
	Interval      time.Duration `yaml:"interval"`
	Timeout       time.Duration `yaml:"timeout"`
	Path          string        `yaml:"path"`
	FailThreshold int           `yaml:"fail_threshold"`
	PassThreshold int           `yaml:"pass_threshold"`
}

// Default returns the configuration used when the file omits a setting.
func Default() Config {
	return Config{
		Port:            8080,
		AdminPort:       9090,
		Strategy:        "round_robin",
		ShutdownTimeout: 30 * time.Second,
		HealthCheck: HealthCheck{
			Interval:      5 * time.Second,
			Timeout:       2 * time.Second,
			Path:          "/health",
			FailThreshold: 3,
			PassThreshold: 2,
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads, decodes and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// %w preserves the underlying *fs.PathError, so a caller can still
		// use errors.Is(err, os.ErrNotExist) to tell "missing file" from
		// "malformed file".
		return nil, fmt.Errorf("reading config: %w", err)
	}

	// THE KEY MOVE: start from the defaults and decode ON TOP of them.
	//
	// The decoder only assigns fields whose keys are actually present in the
	// document, so anything omitted keeps its default. This sidesteps Go's
	// zero-value ambiguity entirely -- with an empty Config we could never
	// distinguish "port: 0" (explicitly set, invalid) from "port omitted"
	// (use 8080), because both leave the field as 0.
	//
	// The usual alternative -- decode into a zero struct, then patch every
	// field that came out zero -- makes it IMPOSSIBLE to configure a legitimate
	// zero value. Whether that bites depends on the field, which is exactly the
	// kind of subtlety you do not want in config handling.
	cfg := Default()

	dec := yaml.NewDecoder(bytes.NewReader(data))

	// Reject keys we do not recognize instead of ignoring them silently.
	//
	// Without this, `strategey: least_connections` (typo) decodes without
	// complaint and you run round robin in production while your config file
	// swears otherwise. Silent acceptance of unknown fields is one of the most
	// common config bugs there is.
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil {
		// An entirely empty file is not a decode failure -- Decode returns
		// io.EOF. Treat it as "use every default", then let Validate object to
		// the missing backends with a useful message.
		if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing config %s: %w", path, err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", path, err)
	}

	return &cfg, nil
}

// Validate checks the whole config and reports EVERY problem, not just the
// first.
//
// errors.Join (Go 1.20+) bundles multiple errors into one value whose Error()
// prints them newline-separated, and which errors.Is/As can still match against
// any member. Returning on the first problem would mean a user with four
// mistakes runs the binary four times to find them all.
//
// C++ contrast: an exception carries one failure and unwinds immediately. This
// is closer to accumulating into a std::vector<std::string> of diagnostics --
// except the result is still a first-class error you can inspect.
func (c *Config) Validate() error {
	var errs []error

	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("port: %d is not in 1..65535", c.Port))
	}
	if c.AdminPort < 1 || c.AdminPort > 65535 {
		errs = append(errs, fmt.Errorf("admin_port: %d is not in 1..65535", c.AdminPort))
	}
	if c.Port == c.AdminPort {
		errs = append(errs, fmt.Errorf("port and admin_port are both %d; they must differ", c.Port))
	}

	// Ask the balancer package which names are legal rather than duplicating
	// the list. One source of truth means adding a strategy cannot leave
	// validation out of date.
	if !slices.Contains(balancer.Names(), c.Strategy) {
		errs = append(errs, fmt.Errorf("strategy: %q is not one of %v", c.Strategy, balancer.Names()))
	}

	if c.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("shutdown_timeout: must be positive, got %s", c.ShutdownTimeout))
	}

	errs = append(errs, c.validateBackends()...)
	errs = append(errs, c.validateHealthCheck()...)

	// Delegate to the logging package rather than duplicating its accepted
	// values here -- one source of truth, checked at startup instead of
	// failing later.
	if _, err := logging.New(c.Log.Level, c.Log.Format); err != nil {
		errs = append(errs, fmt.Errorf("log: %w", err))
	}

	// Join returns nil if every element is nil, so this is the "all good" path
	// too -- no special-casing an empty slice.
	return errors.Join(errs...)
}

// validateBackends checks the backend list.
func (c *Config) validateBackends() []error {
	var errs []error

	if len(c.Backends) == 0 {
		return append(errs, errors.New("backends: at least one backend is required"))
	}

	seen := make(map[string]int, len(c.Backends))
	for i, b := range c.Backends {
		// Index the entry in every message. "backends[2]: ..." tells the user
		// exactly which line to fix; "invalid url" does not.
		where := fmt.Sprintf("backends[%d]", i)

		if strings.TrimSpace(b.URL) == "" {
			errs = append(errs, fmt.Errorf("%s: url is required", where))
			continue
		}

		u, err := url.Parse(b.URL)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: url %q is unparseable: %v", where, b.URL, err))
			continue
		case u.Scheme != "http" && u.Scheme != "https":
			// This branch is doing real work, not box-ticking. url.Parse
			// ACCEPTS "localhost:9001" without error -- it reads "localhost"
			// as the scheme and "9001" as an opaque body, leaving Host empty.
			// Nothing would be dialable, and the failure would otherwise
			// surface much later inside the Transport with an error that does
			// not point back at the config file.
			//
			// ("127.0.0.1:9001" is different: a scheme cannot start with a
			// digit, so url.Parse rejects it outright in the case above.)
			errs = append(errs, fmt.Errorf("%s: url %q must start with http:// or https://", where, b.URL))
		case u.Host == "":
			errs = append(errs, fmt.Errorf("%s: url %q has no host", where, b.URL))
		}

		if b.Weight < 0 {
			errs = append(errs, fmt.Errorf("%s: weight must not be negative, got %d", where, b.Weight))
		}

		// A duplicate URL is not harmless: round robin would send that backend
		// double the traffic while the operator sees two innocent-looking lines.
		if prev, dup := seen[b.URL]; dup {
			errs = append(errs, fmt.Errorf("%s: url %q duplicates backends[%d]", where, b.URL, prev))
		} else {
			seen[b.URL] = i
		}
	}

	return errs
}

// validateHealthCheck checks the probe settings.
func (c *Config) validateHealthCheck() []error {
	var errs []error
	hc := c.HealthCheck

	if hc.Interval <= 0 {
		errs = append(errs, fmt.Errorf("health_check.interval: must be positive, got %s", hc.Interval))
	}
	if hc.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("health_check.timeout: must be positive, got %s", hc.Timeout))
	}
	// A timeout longer than the interval means a probe can still be running
	// when the next round is due. The ticker drops the tick rather than piling
	// up, so it degrades rather than breaks -- but it is almost always a
	// mistake, and silently checking half as often as configured is worse than
	// a startup error.
	if hc.Interval > 0 && hc.Timeout > 0 && hc.Timeout >= hc.Interval {
		errs = append(errs, fmt.Errorf(
			"health_check.timeout (%s) must be less than health_check.interval (%s), "+
				"otherwise probes overlap their own schedule", hc.Timeout, hc.Interval))
	}
	if !strings.HasPrefix(hc.Path, "/") {
		errs = append(errs, fmt.Errorf("health_check.path: %q must start with /", hc.Path))
	}
	if hc.FailThreshold < 1 {
		errs = append(errs, fmt.Errorf("health_check.fail_threshold: must be at least 1, got %d", hc.FailThreshold))
	}
	if hc.PassThreshold < 1 {
		errs = append(errs, fmt.Errorf("health_check.pass_threshold: must be at least 1, got %d", hc.PassThreshold))
	}

	return errs
}
