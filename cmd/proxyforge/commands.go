package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/manishkumawat24/proxyforge/internal/admin"
	"github.com/manishkumawat24/proxyforge/internal/backend"
	"github.com/manishkumawat24/proxyforge/internal/balancer"
	"github.com/manishkumawat24/proxyforge/internal/config"
	"github.com/manishkumawat24/proxyforge/internal/health"
	"github.com/manishkumawat24/proxyforge/internal/logging"
	"github.com/manishkumawat24/proxyforge/internal/proxy"
)

// Shared across the three client subcommands, so the flag name and its help
// text cannot drift apart between them.
const (
	adminPortFlag  = "admin-port"
	adminPortUsage = "admin API port of the running proxy"
)

// newFlagSet builds a per-subcommand flag set.
//
// The package-level flag.String/flag.Parse we used through Phase 5 write into
// one global FlagSet, which cannot express "different flags per subcommand".
// flag.NewFlagSet gives each command its own namespace.
//
// ContinueOnError (rather than the default ExitOnError) means a bad flag
// returns an error we can handle, instead of the flag package calling os.Exit
// out from under us -- which would skip our deferred cleanup and make the
// behaviour untestable.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// cmdStart runs the proxy in the foreground.
func cmdStart(args []string) error {
	fs := newFlagSet("start")
	configPath := fs.String("config", "config.yaml", "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := logging.New(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	// Set the process-wide default so leaf packages (backend's ErrorHandler,
	// the health checker) can log without every constructor taking a logger.
	// Explicit injection would be more testable; for the handful of call sites
	// we have, this is the better trade -- and it is set exactly once, here,
	// before anything starts serving.
	slog.SetDefault(logger)

	// Config is validated by Load, so everything below can trust it.
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		return err
	}

	pool, err := poolFromConfig(cfg)
	if err != nil {
		return err
	}

	// signal.NotifyContext gives us a context cancelled on SIGINT/SIGTERM.
	//
	// Before Go 1.16 this was a manual dance: make a chan os.Signal, call
	// signal.Notify, spawn a goroutine to receive and translate it into a
	// cancel. NotifyContext collapses all of that into one line.
	//
	// WINDOWS NOTE: syscall.SIGTERM is defined on Windows and this compiles,
	// but nothing ever delivers it -- Windows has no POSIX signals. What DOES
	// arrive is os.Interrupt, from Ctrl+C in a console. Listing both keeps one
	// code path that behaves correctly on Windows and Linux alike.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The health checker gets a context we can cancel independently, so that a
	// server failure (not just a signal) also stops it.
	checkerCtx, cancelChecker := context.WithCancel(ctx)
	defer cancelChecker()

	checker := health.New(pool, health.Config{
		Interval:      cfg.HealthCheck.Interval,
		Timeout:       cfg.HealthCheck.Timeout,
		Path:          cfg.HealthCheck.Path,
		FailThreshold: cfg.HealthCheck.FailThreshold,
		PassThreshold: cfg.HealthCheck.PassThreshold,
	})

	// A WaitGroup so shutdown can actually JOIN the checker rather than assume
	// it stopped. Without this we would return from cmdStart while a probe was
	// still in flight, and "shutdown complete" would be a lie.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		checker.Run(checkerCtx)
	}()

	// Bind both listeners BEFORE announcing anything. ListenAndServe would bind
	// inside a goroutine, so "address already in use" would surface after we had
	// already logged that we were listening.
	//
	// The admin API binds 127.0.0.1 explicitly, never ":port". It can add and
	// remove backends with no authentication whatsoever, so loopback-only is the
	// entire security model -- not a detail to leave to a default.
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	adminAddr := fmt.Sprintf("127.0.0.1:%d", cfg.AdminPort)

	proxyLn, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		return fmt.Errorf("binding proxy port: %w", err)
	}
	adminLn, err := net.Listen("tcp", adminAddr)
	if err != nil {
		_ = proxyLn.Close()
		return fmt.Errorf("binding admin port: %w", err)
	}

	set := &serverSet{
		proxy: &http.Server{
			// Middleware wraps the proxy: the logging layer runs first, assigns
			// a request ID and starts the clock, then hands off. Reading
			// outward-in is the trick to following any middleware chain.
			Handler:           logging.Middleware(logger)(proxy.New(pool, strategy)),
			ReadHeaderTimeout: 10 * time.Second,
		},
		proxyLn: proxyLn,
		admin: &http.Server{
			Handler:           admin.New(pool, strategy, cfg.Port).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		},
		adminLn: adminLn,
		logger:  logger,
	}

	errCh := set.start()

	logger.Info("proxyforge listening",
		"addr", proxyAddr,
		"admin_addr", adminAddr,
		"strategy", strategy.Name(),
		"backends", pool.Len(),
		"health_path", cfg.HealthCheck.Path,
		"health_interval", cfg.HealthCheck.Interval.String(),
	)

	// Block until either a server dies or a shutdown signal arrives.
	select {
	case err := <-errCh:
		cancelChecker()
		wg.Wait()
		return err
	case <-ctx.Done():
	}

	// Restore default signal handling immediately.
	//
	// This is what makes a SECOND Ctrl+C kill the process instantly. Without
	// it, an operator watching a slow drain has no way to give up -- every
	// further Ctrl+C is swallowed by our handler and appears to do nothing,
	// which is precisely when people reach for `kill -9`.
	stop()

	logger.Info("shutdown signal received, draining in-flight requests",
		"timeout", cfg.ShutdownTimeout.String(),
		"hint", "press Ctrl+C again to exit immediately")

	drainErr := set.drain(cfg.ShutdownTimeout)

	// Only now stop the health checker. Draining requests still need an
	// accurate view of which backends are alive, so killing the checker first
	// would be actively harmful.
	cancelChecker()
	wg.Wait()

	if drainErr != nil {
		return drainErr
	}
	logger.Info("shutdown complete, all in-flight requests finished")
	return nil
}

// cmdStatus prints the running proxy's state.
func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	adminPort := fs.Int(adminPortFlag, 9090, adminPortUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := cliContext()
	defer stop()

	st, err := admin.NewClient(*adminPort).Status(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("strategy:   %s\n", st.Strategy)
	fmt.Printf("proxy port: %d\n", st.ProxyPort)
	fmt.Printf("uptime:     %s\n", st.Uptime)
	fmt.Printf("backends:   %d\n\n", len(st.Backends))

	// tabwriter aligns columns without anyone counting spaces. It buffers
	// everything until Flush, which is when the column widths are finally
	// known -- so forgetting Flush produces no output at all.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "URL\tSTATE\tWEIGHT\tIN-FLIGHT\tFAILS\tPASSES")
	for _, b := range st.Backends {
		state := "UP"
		if !b.Alive {
			state = "DOWN"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\n",
			b.URL, state, b.Weight, b.InFlight, b.Failures, b.Successes)
	}
	return tw.Flush()
}

// cmdAddBackend registers a backend with the running proxy.
func cmdAddBackend(args []string) error {
	fs := newFlagSet("add-backend")
	rawURL := fs.String("url", "", "backend URL to add (required)")
	weight := fs.Int("weight", 1, "backend weight, used by weighted_round_robin")
	adminPort := fs.Int(adminPortFlag, 9090, adminPortUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The flag package has no notion of a required flag, so required-ness is
	// always a manual check. Doing it here rather than letting an empty URL
	// travel to the server means a better message and one less round trip.
	if *rawURL == "" {
		fs.Usage()
		return fmt.Errorf("--url is required")
	}

	ctx, stop := cliContext()
	defer stop()

	if err := admin.NewClient(*adminPort).AddBackend(ctx, *rawURL, *weight); err != nil {
		return err
	}

	fmt.Printf("added %s (weight %d)\n", *rawURL, *weight)
	fmt.Println("note: this affects the running process only; config.yaml is unchanged")
	return nil
}

// cmdRemoveBackend removes a backend from the running proxy.
func cmdRemoveBackend(args []string) error {
	fs := newFlagSet("remove-backend")
	rawURL := fs.String("url", "", "backend URL to remove (required)")
	adminPort := fs.Int(adminPortFlag, 9090, adminPortUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *rawURL == "" {
		fs.Usage()
		return fmt.Errorf("--url is required")
	}

	ctx, stop := cliContext()
	defer stop()

	if err := admin.NewClient(*adminPort).RemoveBackend(ctx, *rawURL); err != nil {
		return err
	}

	fmt.Printf("removed %s\n", *rawURL)
	fmt.Println("note: in-flight requests already sent to it will finish normally")
	return nil
}

// poolFromConfig builds a live Pool from validated config.
func poolFromConfig(cfg *config.Config) (*backend.Pool, error) {
	pool := backend.NewPool()
	for _, bc := range cfg.Backends {
		// Weight 0 in the file means "unspecified"; backend.New normalizes it
		// to 1. Keeping that in one place lets the config layer stay honest
		// about what the file actually said.
		b, err := backend.New(bc.URL, bc.Weight)
		if err != nil {
			return nil, err
		}
		if err := pool.Add(b); err != nil {
			return nil, err
		}
		slog.Info("backend registered", "url", b.String(), "weight", b.Weight)
	}
	return pool, nil
}
