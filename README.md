# ProxyForge

A CLI reverse proxy and load balancer in Go, built with the standard library.

One third-party dependency (`gopkg.in/yaml.v3`, because the stdlib has no YAML).
No web framework, no CLI framework, no logging library, no assertion library.

```
proxyforge start --config config.yaml
proxyforge status
proxyforge add-backend --url http://127.0.0.1:9004 --weight 2
proxyforge remove-backend --url http://127.0.0.1:9004
```

---

## Architecture

```
                    ┌──────────────────────────────────────────────────┐
   client           │                   proxyforge                     │
     │              │                                                  │
     │  HTTP        │   ┌──────────────────────────────────────────┐   │
     └─────────────────▶│  logging.Middleware                      │   │
        :8080       │   │  • assign/propagate X-Request-Id         │   │
                    │   │  • start timer                           │   │
                    │   │  • wrap ResponseWriter to catch status   │   │
                    │   └────────────────────┬─────────────────────┘   │
                    │                        ▼                         │
                    │   ┌──────────────────────────────────────────┐   │
                    │   │  proxy.Server.ServeHTTP                  │   │
                    │   │                                          │   │
                    │   │   pool.Healthy() ──▶ []*Backend          │   │
                    │   │          │           (snapshot copy)     │   │
                    │   │          ▼                               │   │
                    │   │   strategy.Pick(candidates)              │   │
                    │   │          │                               │   │
                    │   │          ├─ nil ──▶ 503 no healthy       │   │
                    │   │          ▼                               │   │
                    │   │   b.Acquire() / defer b.Release()        │   │
                    │   │   b.Proxy.ServeHTTP  (ReverseProxy)      │   │
                    │   └────────────────────┬─────────────────────┘   │
                    │                        │                         │
                    │   ┌────────────────────┼─────────────────────┐   │
                    │   │  backend.Pool      │                     │   │
                    │   │  ┌──────────────┐  │  ┌──────────────┐   │   │
                    │   │  │ Backend      │  │  │ Backend      │   │   │
                    │   │  │ alive (mu)   │  │  │ alive (mu)   │   │   │
                    │   │  │ inFlight ⚛   │  │  │ inFlight ⚛   │   │   │
                    │   │  │ ReverseProxy │  │  │ ReverseProxy │   │   │
                    │   │  └──────▲───────┘  │  └──────▲───────┘   │   │
                    │   └─────────┼──────────┼─────────┼──────────┘   │
                    │             │          │         │              │
                    │   ┌─────────┴──────────┼─────────┴──────────┐   │
                    │   │  health.Checker  (goroutine + ticker)   │   │
                    │   │  probes /health concurrently, N fails   │   │
                    │   │  eject / M passes readmit               │   │
                    │   └────────────────────┼─────────────────────┘  │
                    │                        │                         │
                    │   ┌────────────────────┼─────────────────────┐   │
   proxyforge       │   │  admin.Server (127.0.0.1:9090)          │   │
   status  ─────────────▶  GET /status  POST/DELETE /backends     │   │
   add-backend      │   └──────────────────────────────────────────┘   │
   (separate proc)  └────────────────────────┬─────────────────────────┘
                                             │
                          ┌──────────────────┼──────────────────┐
                          ▼                  ▼                  ▼
                     backend :9001      backend :9002      backend :9003

                                                   ⚛ = atomic   mu = mutex-guarded
```

### Package layout

| Package | Responsibility |
|---|---|
| `cmd/proxyforge` | CLI dispatch, wiring, server lifecycle and shutdown |
| `cmd/dummybackend` | Test harness: a backend that names itself, with `?delay=` / `?fail=` |
| `internal/backend` | `Backend` (health state, in-flight counter, its own `ReverseProxy`) and the concurrent `Pool` |
| `internal/balancer` | `Strategy` interface and the four implementations |
| `internal/health` | Background prober driving the failure/recovery state machine |
| `internal/config` | YAML loading, defaults, validation |
| `internal/logging` | `slog` setup, request-ID middleware, status capture |
| `internal/admin` | Control-plane HTTP server and its client |
| `internal/proxy` | Request handler: select a backend, forward, release |

The import graph is a DAG — Go makes cycles a hard compile error with no forward
declarations to escape through. `backend` imports only `logging`; everything else
depends inward on it.

---

## Quick start

Requires Go 1.22+. Setting this up on a fresh machine, or hitting a problem?
[SETUP.md](SETUP.md) covers installation, transferring the repo, configuration
and troubleshooting in full. [TESTING.md](TESTING.md) walks through all 25
features with the command for each and the real output it produces.

Not sure `go` is on your PATH? Use the wrappers — they find it themselves and
work identically in `cmd.exe` and PowerShell:

```
.\backends.cmd start                        start 3 test backends
.\pf.cmd start --config config.yaml         start the proxy
.\pf.cmd status                             inspect it (second terminal)
.\test.cmd                                  run the tests
.\backends.cmd stop                         tear down
```

Otherwise, with `go` on PATH:

```powershell
# 1. Start three dummy backends on 9001-9003
.\scripts\run-backends.ps1

# 2. Start the proxy
go run ./cmd/proxyforge start --config config.yaml

# 3. In another terminal, watch it rotate
curl.exe -i http://127.0.0.1:8080/          # look at X-Backend-Name
curl.exe http://127.0.0.1:8080/
curl.exe http://127.0.0.1:8080/

# 4. Inspect live state
go run ./cmd/proxyforge status

# 5. Tear down
.\scripts\stop-backends.ps1
```

On Linux/macOS, replace the scripts with three `go run ./cmd/dummybackend -port 900N &`.

### Things worth trying

```powershell
# Watch health checking eject and readmit a backend
Stop-Process -Id (Get-NetTCPConnection -LocalPort 9002 -State Listen).OwningProcess -Force
go run ./cmd/proxyforge status          # 9002 goes DOWN after 3 failed checks
                                        # traffic continues with zero 502s

# Watch weighting (set strategy: weighted_round_robin in config.yaml)
# with weights 5:1:1 you get 9001 9001 9002 9001 9003 9001 9001

# Watch least-connections adapt
curl.exe "http://127.0.0.1:8080/?delay=5s"   # occupies one backend
go run ./cmd/proxyforge status               # IN-FLIGHT shows 1

# Watch graceful shutdown
curl.exe "http://127.0.0.1:8080/?delay=10s"  # then Ctrl+C the proxy
                                             # the request still completes
```

---

## Configuration

See [config.yaml](config.yaml) for a commented example. Every key is optional
except `backends`.

| Key | Default | Notes |
|---|---|---|
| `port` | `8080` | Client-facing port |
| `admin_port` | `9090` | Control plane, bound to loopback only |
| `strategy` | `round_robin` | `round_robin`, `weighted_round_robin`, `least_connections`, `random` |
| `backends[].url` | — | Required. Must include `http://` or `https://` |
| `backends[].weight` | `1` | Only consulted by `weighted_round_robin` |
| `health_check.interval` | `5s` | Human-readable durations only; a bare number is rejected |
| `health_check.timeout` | `2s` | Must be **less than** `interval` |
| `health_check.path` | `/health` | Must start with `/` |
| `health_check.fail_threshold` | `3` | Consecutive failures to eject |
| `health_check.pass_threshold` | `2` | Consecutive successes to readmit |
| `shutdown_timeout` | `30s` | How long to drain before cutting requests off |
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `text` | `text` or `json` |

Unknown keys are **rejected**, not ignored — a typo like `strategey:` fails at
startup rather than silently leaving the default in force. Validation reports
every problem at once:

```
proxyforge: invalid config config.yaml:
port: 0 is not in 1..65535
strategy: "round-robin" is not one of [least_connections random round_robin weighted_round_robin]
backends[2]: url "http://127.0.0.1:9002" duplicates backends[1]
health_check.timeout (5s) must be less than health_check.interval (1s), ...
```

---

## Load balancing strategies

**`round_robin`** — strict rotation via one `atomic.Uint64`. The default.

**`weighted_round_robin`** — nginx's *smooth* WRR. Weights `{5,1,1}` produce
`a a b a c a a`, not `a a a a a b c`. Same totals, but the heavy backend's turns
are spread rather than clumped, so a burst of five requests doesn't all land on
one server while two idle. Also avoids allocating a slice proportional to the
sum of weights.

**`least_connections`** — sends each request to the backend with the fewest
in-flight requests. The only strategy that *measures* rather than assumes: a
backend in a GC pause accumulates in-flight requests and stops being chosen
until it drains. Ties rotate, so all-idle backends don't all funnel to index 0.

**`random`** — uniform pick via `math/rand/v2`. Zero shared state means zero
contention; round robin's atomic counter is one cache line every core fights
over, which becomes measurable at very high RPS. Distribution is only
statistically even.

---

## Design decisions

**A loopback HTTP admin API for the control plane.** `proxyforge status` is a
separate process from `proxyforge start` and shares none of its memory.
Alternatives considered: Unix domain sockets / named pipes (entirely different
Windows and Unix code paths, needs a third-party package on Windows), and
config-file rewrite plus signal reload (Windows has no usable signals beyond
Ctrl+C, and `status` cannot work that way at all). HTTP is pure stdlib and
identical everywhere. It binds `127.0.0.1` explicitly because it mutates the
backend pool with **no authentication** — loopback-only is the entire security
model.

**Runtime changes don't rewrite the config file.** `add-backend` affects the
running process only. Writing back would reformat the user's YAML and destroy
their comments — a worse surprise than "runtime changes are runtime-only". The
CLI says so after every call.

**`Strategy.Pick` takes a candidate slice, not the `Pool`.** The "which backends
are eligible" policy lives in `Pool.Healthy()`, so no strategy ever knows about
health checking. That's why all four are testable without starting a server.

**Removing a backend doesn't cancel in-flight requests.** They already hold a
`*Backend` and finish normally; removal only stops *future* selection. Yanking
live requests would turn an operator action into client-visible errors.

**`Pool.All()` returns a copy of the slice but shares the `*Backend` pointers.**
The pool protects the *slice* (which `Add` can reallocate); each `Backend` locks
itself.

**Health checks use a dedicated `Transport`.** Probes must never evict the idle
connections that live traffic depends on.

**Stdlib `flag` over Cobra.** Subcommand dispatch is a `switch` plus
`flag.NewFlagSet` — about 60 lines, no transitive dependencies, and it doesn't
hide the mechanism.

**`log/slog` over zap/logrus.** Stdlib since Go 1.21.

---

## Testing

```powershell
go test -race ./...
```

`-race` is not optional here. Concurrent code that passes without it proves
nothing — the round-robin counter test passes with a broken non-atomic counter
roughly 99% of the time.

What the tests actually prove:

- **Strategies** — exact rotation order (not just totals: totals alone can't
  distinguish smooth WRR from naive list expansion), even distribution across
  100 goroutines, tie rotation, no state leak when the candidate set shrinks.
- **Health state machine** — table-driven over failure/recovery sequences,
  including that a single opposite result resets the run (the anti-flapping
  property), and that timeouts, refused connections and 404s all eject.
- **Goroutine lifecycle** — cancelling the context makes `Run` return *and*
  stop probing.
- **Logging** — that the `ResponseWriter` wrapper still satisfies
  `http.Flusher` and cooperates with `http.ResponseController`. Without those,
  streaming responses silently buffer, and a curl test against a small response
  still passes.
- **Shutdown** — an in-flight request completes with a full body, new
  connections are refused immediately, a stuck handler hits the timeout, and
  `ErrServerClosed` isn't reported as a crash.

---
