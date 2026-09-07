// Command proxyforge is a CLI reverse proxy and load balancer.
//
//	proxyforge start --config config.yaml
//	proxyforge status
//	proxyforge add-backend --url http://127.0.0.1:9004 --weight 2
//	proxyforge remove-backend --url http://127.0.0.1:9004
//
// start runs the proxy in the foreground. The other three are separate
// processes that talk to a running one over its loopback admin API -- see
// internal/admin for why that boundary exists and how it is crossed.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/manishkumawat24/proxyforge/internal/admin"
)

func main() {
	// main() does nothing but dispatch and exit-code mapping. Real work returns
	// an error, because os.Exit skips every deferred function -- which matters
	// enormously for the graceful shutdown in Phase 8.
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "proxyforge: %v\n", err)

		// Distinct exit codes so scripts can branch without parsing stderr.
		// 2 means "the proxy is not running", everything else is 1.
		if errors.Is(err, admin.ErrNotRunning) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// dispatch routes argv to a subcommand.
//
// This is the whole of our "CLI framework": a switch. Cobra would give nicer
// help output and shell completion for the price of three transitive
// dependencies, and it would hide exactly the mechanism worth understanding --
// that a subcommand is just a name plus its own flag.FlagSet.
func dispatch(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]

	switch cmd {
	case "start":
		return cmdStart(rest)
	case "status":
		return cmdStatus(rest)
	case "add-backend":
		return cmdAddBackend(rest)
	case "remove-backend":
		return cmdRemoveBackend(rest)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `proxyforge -- a CLI reverse proxy and load balancer

Usage:
  proxyforge start           [--config config.yaml]
  proxyforge status          [--admin-port 9090]
  proxyforge add-backend     --url <url> [--weight 1] [--admin-port 9090]
  proxyforge remove-backend  --url <url> [--admin-port 9090]

start runs the proxy in the foreground. The other commands are separate
processes that talk to a running proxy over its loopback admin API, so the
proxy must already be started.

Note that add-backend and remove-backend change the RUNNING process only.
They do not rewrite your config file, so the change is lost on restart.

Exit codes:
  0  success
  1  error
  2  proxyforge is not running
`)
}

// cliContext returns a context for admin calls, cancelled on Ctrl+C.
//
// Without this, Ctrl+C during a slow admin call would kill the process
// mid-request. With it, the in-flight HTTP call is cancelled cleanly and the
// command returns an error like any other failure.
//
// The caller must call the returned stop func. Returning it rather than
// discarding it inside is deliberate: `go vet`'s lostcancel check exists
// because a dropped cancel leaks the signal-handling goroutine and the
// registration with the runtime. It happens not to matter for a process that
// exits milliseconds later, but writing the leaky version teaches the wrong
// habit for the long-lived case.
func cliContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
