// Package balancer holds the pluggable request-distribution strategies.
//
// Every strategy is a pure-ish function of the candidate list it is handed. It
// knows nothing about health checking, nothing about the Pool, and nothing about
// HTTP. That separation is what makes these testable without spinning up a
// single server.
package balancer

import (
	"fmt"
	"sort"

	"github.com/manishkumawat24/proxyforge/internal/backend"
)

// Strategy chooses which backend serves the next request.
//
// C++ contrast: this is an abstract base class with two pure virtuals -- except
// no implementation ever names it. A type satisfies Strategy simply by having
// both methods; there is no ": public Strategy", no override keyword, and no
// vtable pointer embedded in the struct. The compiler checks the match where a
// concrete type is *assigned* to a Strategy variable.
//
// Note the interface is small. That is idiomatic Go: "the bigger the interface,
// the weaker the abstraction". A two-method interface is trivial to implement,
// trivial to fake in a test, and hard to get wrong.
type Strategy interface {
	// Name is the identifier used in config files and status output.
	Name() string

	// Pick selects one backend from candidates.
	//
	// Contract:
	//   - candidates contains only backends eligible to serve (the caller has
	//     already filtered out unhealthy ones). A Strategy must never consult
	//     Alive() itself.
	//   - Returns nil if and only if candidates is empty.
	//   - MUST be safe to call concurrently from many goroutines. Every
	//     implementation here carries mutable selection state, so this is the
	//     single most important line in this file.
	Pick(candidates []*backend.Backend) *backend.Backend
}

// New returns the Strategy registered under name.
//
// Kept in one place so config validation and the CLI agree on exactly which
// names are legal, and so adding a strategy means touching one function.
func New(name string) (Strategy, error) {
	switch name {
	case "round_robin", "":
		return &RoundRobin{}, nil
	case "weighted_round_robin":
		return NewWeightedRoundRobin(), nil
	case "least_connections":
		return &LeastConnections{}, nil
	case "random":
		return &Random{}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q (valid: %v)", name, Names())
	}
}

// Names lists every valid strategy name, sorted, for error messages and help
// text.
func Names() []string {
	names := []string{"round_robin", "weighted_round_robin", "least_connections", "random"}
	sort.Strings(names)
	return names
}
