# ProxyForge — Complete Project Summary

The whole project in one document: what it is, how it was built, every decision
and why, what was proven, and what was learned.

| Doc | Purpose |
|---|---|
| **SUMMARY.md** | ← you are here. The whole story, start to finish |
| [README.md](README.md) | What it is, architecture, usage |
| [SETUP.md](SETUP.md) | Install and run on a fresh machine |
| [TESTING.md](TESTING.md) | All 25 features with real captured output |
| [QA.md](QA.md) | 95 interview questions with answers |
| [LESSONS.md](LESSONS.md) | Objectives, what to take away, and how to consolidate it |
| [LEARNING_NOTES.md](LEARNING_NOTES.md) | Per-phase concepts, gotchas, interview questions |

---

## 1. What it is

ProxyForge is a **CLI reverse proxy and load balancer** written in Go. It sits
in front of N backend servers, distributes incoming HTTP requests across them
using a pluggable strategy, continuously checks which backends are alive, logs
every request with a trace ID and latency, and shuts down without dropping
in-flight work.

It was built as a **systems-level learning project** — the goal was to
understand `net/http`, goroutines, channels, `context`, mutexes and atomics by
using them for real, rather than to produce shippable infrastructure. Working
code was the side effect; understanding was the deliverable.

Built with the **Go standard library**. One third-party dependency
(`gopkg.in/yaml.v3`), because the stdlib has no YAML parser. No web framework,
no CLI framework, no logging library, no assertion library.

---

## 2. Final numbers

| Metric | Value |
|---|---|
| Production Go code | **2,399 LOC** across 17 files |
| Test code | **1,807 LOC** across 8 files |
| Test-to-code ratio | **0.75 : 1** |
| Test functions | **54** |
| Packages | **9** |
| Third-party dependencies | **1** (`gopkg.in/yaml.v3`) |
| Commits | **14**, one per build phase |
| Load balancing strategies | **4** |
| CLI commands | **4** (`start`, `status`, `add-backend`, `remove-backend`) |
| Go version floor | **1.22** |
| `gofmt` / `go vet` | clean |
| Full suite under `-race` | passing |

### Where the code lives

| Package | Src | Test | Responsibility |
|---|---:|---:|---|
| `internal/backend` | 404 | 203 | `Backend` (health state, in-flight counter, own `ReverseProxy`) + concurrent `Pool` |
| `cmd/proxyforge` | 461 | 193 | CLI dispatch, wiring, server lifecycle, shutdown |
| `internal/balancer` | 332 | 439 | `Strategy` interface + 4 implementations |
| `internal/admin` | 283 | 195 | Control-plane HTTP server and its client |
| `internal/config` | 253 | 304 | YAML load, defaults, validation |
| `internal/logging` | 222 | 237 | `slog` setup, request-ID middleware, status capture |
| `internal/health` | 197 | 236 | Background prober + failure/recovery state machine |
| `cmd/dummybackend` | 179 | 0 | Test harness backend |
| `internal/proxy` | 68 | 0 | Request handler: select → forward → release |

`internal/proxy` is small because it's *only* orchestration — the hard parts
live in the packages it composes. It's covered through their tests and the
end-to-end runs.

---

## 3. The journey — 10 phases

Each phase is one commit. The history reads as a build log.

| # | Commit | What it added | The lesson |
|---|---|---|---|
| 0 | `7a459e2` | Module, package skeleton, dummy backend harness | Directory layout *is* the build graph; `internal/` is compiler-enforced |
| 0b | `1aa4fcc` | Echo handler with `?delay=` / `?fail=` | Errors are values; `(value, error)` everywhere |
| 1 | `048a99a` | Minimal reverse proxy, one hardcoded backend | `httputil.ReverseProxy` internals; `Rewrite` vs `Director` |
| 2 | `cd046dd` | Backend pool, `Strategy` interface, round robin | **The data race.** `counter++` is three operations |
| 3 | `9e96228` | Weighted, least-connections, random | Smooth WRR; measuring vs assuming |
| 4 | `36d41ff` | Active health checking | Goroutine lifecycle, `select`, `context` cancellation |
| 5 | `9fbf9af` | YAML config + validation | Zero-value ambiguity; `errors.Join` |
| 6 | `ff94e47` | CLI subcommands + admin API | Crossing a process boundary |
| 7 | `7d11368` | Structured logging, request IDs, latency | Wrapping `ResponseWriter` breaks streaming |
| 8 | `c7464e3` | Graceful shutdown | Never pass the cancelled context to `Shutdown` |
| 9 | `5d7713d` | README, architecture, design decisions | — |
| + | `feec82f`, `bef23d0`, `1fc1016` | SETUP.md, `.cmd` wrappers, TESTING.md | Windows PATH and shell reality |

### How it grew

**Phase 0–1** established the skeleton and the simplest possible proxy: one
hardcoded upstream, forward and return. The dummy backend was built *first*,
deliberately — you cannot develop a load balancer without backends that tell you
which one answered, and that can be made slow or broken on demand.

**Phase 2** is where it became a real systems project. Introducing a pool of
backends meant a shared counter, and a shared counter meant a data race. This
was proven rather than asserted: the naive implementation produced
`a:2502 b:2489 c:2494 d:2515` instead of exactly 2500 each, and `go test -race`
flagged it instantly.

**Phase 3–4** added the remaining strategies and active health checking — the
first background goroutine with a real lifecycle, which forced `context`,
`select`, `time.Ticker` and `sync.WaitGroup` into the design.

**Phase 5–6** turned it from a program into a *tool*: file-based configuration
and a CLI whose subcommands run as separate processes, which required an actual
control plane to cross the process boundary.

**Phase 7–8** made it operable: you can see what it's doing, and you can stop it
without hurting anyone.

---

## 4. How a request flows

```
client ──▶ :8080
             │
             ├─ logging.Middleware
             │    assign or propagate X-Request-Id
             │    start timer, wrap ResponseWriter to capture status
             │
             ├─ proxy.Server.ServeHTTP
             │    pool.Healthy()          → snapshot copy of eligible backends
             │    strategy.Pick(...)      → one backend, or nil
             │       └─ nil ──────────────▶ 503 no healthy backends
             │    b.Acquire()             → in-flight++ (atomic)
             │    defer b.Release()       → in-flight-- even on panic
             │
             ├─ b.Proxy.ServeHTTP         → httputil.ReverseProxy
             │    strips hop-by-hop headers, sets X-Forwarded-*,
             │    streams body via io.Copy, propagates client disconnect
             │       └─ unreachable ─────▶ 502 bad gateway
             │
             └─ one structured log line: id, method, path, backend,
                status, bytes, latency_ms, remote
```

Meanwhile, independently:

- **`health.Checker`** probes every backend concurrently on a ticker, feeding
  results into each backend's state machine and flipping `alive` on transitions.
- **`admin.Server`** on `127.0.0.1:9090` answers `status` and mutates the pool
  when the CLI asks.

---

## 5. Features

### Load balancing
- **Round robin** — strict rotation via one `atomic.Uint64`
- **Weighted round robin** — nginx-style *smooth* WRR; weights `{5,1,1}` emit
  `a a b a c a a`, not `a a a a a b c`
- **Least connections** — routes by live in-flight count, with rotating
  tie-breaks so idle backends don't all funnel to index 0
- **Random** — `math/rand/v2`, zero shared state, zero contention
- Strategies are chosen by name in config and validated at startup

### Health checking
- Background goroutine probing all backends **concurrently** on an interval
- Hysteretic state machine: N consecutive failures to eject, M consecutive
  successes to readmit; any opposite result resets the run
- Per-probe timeout derived from the parent context
- Treats timeout, refused connection, DNS failure and non-2xx/3xx alike
- Dedicated `http.Transport` so probes can't evict live traffic's connections
- Logs **transitions only**, not every check

### CLI
- `start`, `status`, `add-backend`, `remove-backend`
- Stdlib `flag` with a per-subcommand `FlagSet`
- Aligned status table via `text/tabwriter`
- Exit codes `0` / `1` / `2` for scripting

### Configuration
- YAML: ports, strategy, backends with weights, health-check settings,
  shutdown timeout, log level and format
- Defaults applied by decoding *on top of* a pre-populated struct
- Unknown keys **rejected**, so typos fail loudly
- Every validation error reported in one pass

### Observability
- `log/slog`, text or JSON
- One line per request: request ID, method, path, **chosen backend**, status,
  bytes, latency in ms, remote address
- Inbound `X-Request-Id` honoured for cross-service tracing, and **sanitised**
  against log injection
- ID echoed back to the client in the response header

### Control plane
- Loopback-only HTTP admin API: `GET /status`, `POST /backends`,
  `DELETE /backends`
- Go 1.22 method-aware routing → automatic 405 with correct `Allow`
- Request bodies capped with `MaxBytesReader`; unknown JSON fields rejected
- 409 / 404 / 400 so callers distinguish cases by status code

### Lifecycle
- Listeners bound *before* announcing, so port conflicts fail immediately
- `signal.NotifyContext` for SIGINT/SIGTERM (and Windows Ctrl+C)
- `http.Server.Shutdown` drains in-flight requests up to a configurable timeout
- Ordered shutdown: admin API → proxy → health checker
- Second Ctrl+C exits immediately

---

## 6. Design decisions and rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| **Loopback HTTP admin API** for the control plane | Unix sockets / named pipes | Entirely different Windows and Unix code paths; needs a third-party package on Windows |
| | Config rewrite + signal reload | Windows has no usable signals beyond Ctrl+C, and `status` can't work this way at all |
| **`Strategy.Pick` takes a candidate slice**, not the `Pool` | Passing the Pool | Keeps health-filtering policy in one place; strategies become testable without a server |
| **Smooth WRR** | List expansion `[a a a a a b c]` | Same totals, but bursts five requests onto one backend and allocates proportional to total weight |
| **Runtime changes don't rewrite config** | Persisting `add-backend` to YAML | Would reformat the file and destroy every comment |
| **Removal doesn't cancel in-flight requests** | Severing them | Turns a routine operator action into client-visible errors |
| **`Pool.All()` copies the slice, shares the pointers** | Returning the internal slice | The slice can be reallocated by `Add`; each `Backend` locks itself |
| **Dedicated health-check `Transport`** | Sharing the proxy's | Probes must never evict idle connections live traffic depends on |
| **Stdlib `flag`** | Cobra | 3+ transitive deps for ergonomics, and it hides how subcommand dispatch works |
| **`log/slog`** | zap / logrus | Stdlib since Go 1.21 |
| **Plain `t.Errorf` tests** | testify | Table-driven testing is the idiom worth internalising |
| **`gopkg.in/yaml.v3`** | Hand-rolled parser | Teaches you about YAML, not about proxies |
| **`Transport` lives in `backend`, not `proxy`** | Keeping it in `proxy` | `Backend` owns its `ReverseProxy` and can't import `proxy` — the import DAG dictated the layout |

---

## 7. What was actually proven

Not asserted — measured, with output captured in [TESTING.md](TESTING.md).

| Claim | Evidence |
|---|---|
| Round robin rotates strictly | 6 requests → `9001 9002 9003 9001 9002 9003` |
| Weighted WRR spreads, doesn't clump | `9001 9001 9002 9001 9003 9001 9001` twice; totals `10:2:2` |
| Least connections measures load | 2 backends at `IN-FLIGHT 1` → next 4 requests all went to the idle third |
| Health checking ejects at the threshold | Ejected after exactly **3** consecutive failures |
| Outages stay invisible to clients | **6/6 requests returned 200** with a dead backend in the pool |
| Recovery is automatic | Readmitted after 2 successful probes, no restart |
| Graceful shutdown drains | 6s request completed **4.5s after the signal**, full 81-byte body, `200` |
| Proxy overhead is negligible | `latency_ms=900.948` for a 900ms backend delay → **0.948ms** |
| Config errors surface together | **7 problems reported in one run** |
| Typos fail loudly | `field strategey not found in type config.Config` |
| Log injection is neutralised | `ab cd;level=ERROR` logged as `abcdlevelERROR` |
| The race detector catches the naive counter | `WARNING: DATA RACE` with both stacks; distribution `2502/2489/2494/2515` vs exactly 2500 |
| 502 vs 503 are distinguished | `200 200 502 200` on a fresh kill; `503 no healthy backends` when all are down |
| Method-aware routing works | `POST /status` → `405` with `Allow: GET, HEAD` |

---

## 8. The hard problems

The six things that were genuinely difficult, and what each taught.

### 1. `counter++` is a data race
Round robin's counter is read-modify-write — three operations, not one. Two
goroutines can both read 7 and both write 8, so one request is duplicated and
one backend skipped.

The insidious part: the skew was only **~1%** (`2502/2489/2494/2515` instead of
2500 each). It *looks* almost right. It passes casual testing, survives code
review, and ships. Fixed with `atomic.Uint64`.

> **Concurrent code that passes without `-race` is untested, not correct.**

### 2. Wrapping `ResponseWriter` silently breaks streaming
To log status codes you must wrap `http.ResponseWriter`, because it's
write-only. But the concrete value net/http passes also implements
`http.Flusher`, `http.Hijacker` and `io.ReaderFrom` — and code finds those with
a *type assertion*, which fails against your wrapper.

`httputil.ReverseProxy` uses `http.Flusher` to flush streaming responses. Lose
it and Server-Sent Events, streaming JSON and long-polling all buffer until the
response ends — **while still passing a curl test against a small response**.
Fixed with `Flush()` plus `Unwrap()`, each with its own test.

### 3. Never pass the cancelled context to `Shutdown`
The signal already cancelled `ctx`. Passing that same context to
`http.Server.Shutdown` makes it return instantly with `context.Canceled`, drain
nothing, and sever every in-flight request — **while the logs cheerfully report
a graceful shutdown**. The deadline must derive from `context.Background()`.

### 4. The import graph is a DAG, and it changes your design
Go has no forward declarations, so an import cycle is a hard compile error with
no escape hatch. Wanting each `Backend` to own its `ReverseProxy` meant
`backend` needed the shared `Transport` — and it couldn't import `proxy` to get
it. The `Transport` moved. The compiler was telling us the boundary was in the
wrong place.

### 5. Zero-value ambiguity in configuration
`port: 0` and an omitted `port` both leave the field at `0`. The usual fix —
decode into a zero struct, then patch anything that came out zero — makes it
*impossible* to configure a legitimate zero value. The right move is to
pre-populate defaults and decode **on top of** them, because the decoder only
touches keys actually present.

### 6. Everything about Windows
`syscall.SIGTERM` compiles but is never delivered. `go test -race` needs a C
toolchain or `CGO_ENABLED` silently drops to 0. `gofmt` writes LF while git's
`autocrlf` writes CRLF, producing infinite phantom diffs. New terminal *tabs*
inherit the stale PATH of the window that spawned them. `cmd.exe` can't run a
`.ps1` at all.

Each of these cost real time and is now documented in
[SETUP.md](SETUP.md#troubleshooting).

---

## 9. Go concepts used, and where

| Concept | Where it appears |
|---|---|
| Implicit interface satisfaction | `Strategy`, `http.Handler` — no `implements`, no vtable pointer |
| Small interfaces | `Strategy` has 2 methods and knows nothing about health or HTTP |
| Value vs pointer receivers | `Random` uses value receivers; the method-set rule is why both `Random` and `*Random` satisfy `Strategy` |
| Goroutines | One per connection (net/http), one per health probe, one per server |
| Channels | `errCh` for server failures, `done` for lifecycle signalling |
| `select` | The checker's `ctx.Done()` / `ticker.C` loop |
| `context` | Cancellation, per-probe deadlines, request-scoped values |
| `sync.Mutex` / `RWMutex` | Pool slice, backend health state |
| `sync/atomic` | Round-robin counter, in-flight counters |
| `sync.WaitGroup` | Joining probe goroutines and the checker on shutdown |
| `defer` | Unlocking, `Release()`, `cancel()`, body close — runs on panic too |
| Struct tags + reflection | YAML and JSON decoding |
| Error wrapping (`%w`) | `errors.Is` / `errors.As` through wrapped chains |
| `errors.Join` | Reporting all config problems at once |
| Sentinel errors | `ErrDuplicate`, `ErrNotFound`, `ErrNotRunning` |
| Middleware | `func(http.Handler) http.Handler` — decorator via closure |
| Table-driven tests | Every strategy and state-machine test |
| `httptest` | Real servers on real loopback ports, not mocks |
| Race detection | `go test -race` across the whole suite |
| `net/http` internals | Hop-by-hop headers, `X-Forwarded-*`, connection pooling, `Flusher`, `ResponseController` |

---

## 10. What it deliberately does not do

Being explicit about scope is part of the design.

- **No TLS termination** — plain HTTP only
- **No retries or circuit breaking** — a failed request fails
- **No rate limiting**
- **No auth on the admin API** — the loopback bind *is* the security model
- **No metrics endpoint** — logs only, no Prometheus
- **No config hot-reload** — config is read once at startup
- **No sticky sessions**
- **No per-backend connection caps**

> **Do not expose this to the internet.** The admin API mutates the backend pool
> with no authentication whatsoever.

---

## 11. Where to find things

```
ProxyForge/
├── SUMMARY.md              ← this file
├── README.md               architecture diagram, usage, design decisions
├── SETUP.md                install, transfer, configure, troubleshoot
├── TESTING.md              25 features with real captured output
├── LEARNING_NOTES.md       per-phase concepts, gotchas, interview questions
│
├── pf.cmd                  run proxyforge (finds Go itself, cmd + PowerShell)
├── backends.cmd            start/stop test backends
├── test.cmd                run the test suite
├── config.yaml             commented example config
│
├── cmd/
│   ├── proxyforge/         CLI, wiring, server lifecycle, shutdown
│   └── dummybackend/       test harness with ?delay= and ?fail=
├── internal/
│   ├── backend/            Backend + Pool  (the concurrency core)
│   ├── balancer/           Strategy interface + 4 implementations
│   ├── health/             background prober + state machine
│   ├── config/             YAML, defaults, validation
│   ├── logging/            slog, request IDs, status capture
│   ├── admin/              control-plane server + client
│   └── proxy/              select → forward → release
└── scripts/                PowerShell equivalents of the .cmd wrappers
```

**If you're reading the code for the first time**, go in this order:
`internal/backend/backend.go` → `internal/balancer/roundrobin.go` →
`internal/proxy/proxy.go` → `internal/health/checker.go`. That's the spine.

---

## 12. What would come next

Roughly in order of value:

1. **TLS termination** — the most obvious gap for anything real
2. **Retries with a circuit breaker** — currently one failure is one failed request
3. **`/metrics` endpoint** — Prometheus counters for requests, latencies, ejections
4. **Config hot-reload** — watch the file, rebuild the pool without dropping traffic
5. **Auth on the admin API** — a shared token at minimum, if it ever leaves loopback
6. **Rate limiting** — token bucket per client IP
7. **Sticky sessions** — consistent hashing on a cookie or client IP
8. **Per-backend connection caps** — protect a weak upstream from a thundering herd

---

## 13. In one paragraph

ProxyForge is a 2,400-line reverse proxy and load balancer written in Go with
the standard library and a single third-party dependency. It distributes HTTP
traffic across a pool of backends using one of four pluggable strategies,
actively health-checks them on background goroutines with a hysteretic
failure/recovery state machine, exposes runtime control through a loopback HTTP
API driven by a CLI, logs every request with a trace ID and latency, and drains
in-flight work on shutdown. It is covered by 54 table-driven tests running clean
under Go's race detector, and every claim about its behaviour was verified
against a live process rather than assumed. It was built in ten phases, one
commit each, as a way to learn concurrent systems programming in Go — and the
most valuable thing in the repo may be the record of what went wrong along the
way.
