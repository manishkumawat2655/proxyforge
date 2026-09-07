# ProxyForge — Learning Notes

Running log, one entry per phase. Written for my future self, and for the version
of me sitting in an interview being asked "so what did you actually build?"

---

## Phase 0 — Scaffold + test harness

### What we built

- `go.mod` — module `github.com/manishkumawat24/proxyforge`, Go 1.22 floor
  (toolchain installed is 1.27.0; the `go` directive is a *minimum*, not a pin).
- The full package skeleton: `cmd/` for binaries, `internal/` for everything else.
- `cmd/dummybackend` — a standalone HTTP server that names itself in every
  response, with hooks for artificial latency (`?delay=`) and forced failure
  (`?fail=`).
- `scripts/run-backends.ps1` / `stop-backends.ps1` — spin up and tear down three
  backends on 9001–9003.

No proxy code yet. This phase exists so that every later phase has something
real to point at and something observable to prove it worked.

### Key concepts

**Modules and the import path.** `go.mod`'s module line is simultaneously the
dependency manifest root *and* the prefix for every internal import. Changing it
later rewrites every import line in the repo, so it is decided on day zero.

**Directory layout *is* the build graph.** No CMakeLists, no target definitions.
`go build ./cmd/dummybackend` builds that directory's `package main` into one
statically linked binary. Two `main` directories = two binaries.

**`internal/` is compiler-enforced.** Packages under `internal/` can only be
imported by code rooted at `internal/`'s parent directory. Not a convention —
an actual compile error. C++ has no equivalent; `detail::` namespaces and
unshipped headers are politeness, this is enforcement.

**Encapsulation is per-package, not per-type.** Capitalized identifier =
exported outside the package. Lowercase = package-private. Everything inside
`package backend` can see everything else in it. There is no `private:` and no
`friend`.

**No import cycles, ever.** Go has no forward declarations, so there is no way
to break a cycle the way `class Pool;` does in C++. An import cycle is a hard
compile error, which forces the package graph to be a DAG. If two packages want
each other, the abstraction boundary is in the wrong place.

**Errors are values, not exceptions.** `time.ParseDuration` returns
`(time.Duration, error)`. There is no `throw`, no `catch`, no stack unwinding,
and no `noexcept` to reason about. Ignoring an error is an explicit act you have
to write down (`_ = err`), which is the entire design intent.

**`flag` hands back pointers.** `flag.Int(...)` returns `*int` holding the
default; `flag.Parse()` is what populates it from `os.Args`.

### Gotchas hit

- **Unused imports and unused local variables are compile errors**, not
  warnings. Unused *function parameters* and struct fields are fine — which is
  why a handler with an empty body still builds.
- **`log` writes to stderr**, not stdout. Redirecting only stdout loses
  everything.
- **`http.ListenAndServe` uses a zero-value `http.Server` with no timeouts at
  all.** One client that opens a socket and never finishes its headers pins a
  goroutine indefinitely (Slowloris). Always construct `&http.Server{}`
  explicitly and set at least `ReadHeaderTimeout`.
- **`ServeMux` pattern matching:** trailing `/` means prefix match, no trailing
  `/` means exact. `"/"` is therefore the catch-all.
- **`WriteHeader` must precede any `Write`.** The first `Write` implicitly
  commits a 200 and the status can never be changed after that. This one-way
  door is exactly why Phase 7 has to wrap `ResponseWriter` to observe the status.
- **Binding to `:9001` vs `127.0.0.1:9001`** — the former listens on every
  interface and pops a Windows firewall prompt. A local harness should not be
  reachable off-box.
- Git does not track empty directories, so the `internal/*` folders won't appear
  in the first commit. They populate from Phase 1 onward.
- **Installing Go does not fix an already-open terminal.** The MSI edits the
  machine `Path` environment variable, but a running process holds the copy it
  inherited at launch. Restart the shell, or re-read it in-process with
  `[System.Environment]::GetEnvironmentVariable("Path","Machine")`.
- **git's `core.autocrlf` on Windows fights `gofmt`.** gofmt always writes LF; if
  git checks `.go` files out as CRLF, every format run marks the whole file
  dirty. `.gitattributes` with `* text=auto eol=lf` pins it.

### Interview questions this phase could generate

1. What does `internal/` mean in a Go project, and how is it enforced?
2. Why does Go make unused imports a compile error rather than a warning?
3. What's wrong with `http.ListenAndServe(addr, handler)` in production code?
4. Why can't you change an HTTP response's status code after writing the body?
5. Go has no forward declarations. What does that force about package structure,
   and how would you break a would-be circular dependency between two packages?
6. What's the difference between binding to `:8080` and `127.0.0.1:8080`?

---

## Phase 1 — Minimal reverse proxy

### What we built

- `internal/proxy` — a `Server` wrapping `httputil.ReverseProxy`, pointed at one
  hardcoded upstream, with a tuned `http.Transport` and a custom `ErrorHandler`.
- `cmd/proxyforge` — `-listen` and `-backend` flags, URL validation, and a
  `run() error` split out of `main()`.

Verified end to end: `curl :8080/some/path` reached backend-9001 with the path
intact; a backend `503` passed through as `503`; killing the backend produced a
`502` from our `ErrorHandler`.

### Key concepts

**Interfaces are satisfied implicitly.** `*proxy.Server` becomes an
`http.Handler` purely by having `ServeHTTP(http.ResponseWriter, *http.Request)`.
No inheritance, no `override`, no registration. The compiler verifies the match
at the *use* site, not the declaration site.

> C++ contrast: this is duck typing with compile-time checking. Closest analogue
> is a template constrained by a concept — the type never names the concept it
> satisfies. Unlike C++ virtual dispatch, there's no vtable pointer inside the
> struct; a Go interface value is a two-word (type, data) pair built at
> assignment.

**`Rewrite` vs `Director`.** `Director` (older) only exposed the outbound
request, which made it easy to forward a client-supplied `X-Forwarded-For` — a
spoofing vector, since anything downstream trusting that header believes
whatever IP the client claimed. `Rewrite` receives a `*ProxyRequest` with both
`.In` (untouched original) and `.Out`, and `SetXForwarded()` **replaces** the
client's value rather than appending. Setting both fields panics at request time.

**`SetURL` clears `Out.Host`**, so the outgoing `Host` header defaults to the
backend's own host. To preserve the client's original `Host` (needed for some
vhosted upstreams), you restore it explicitly with `pr.Out.Host = pr.In.Host`.

**What `ReverseProxy` does that you'd otherwise hand-roll:** strips hop-by-hop
headers (`Connection`, `Keep-Alive`, `Transfer-Encoding`, `Upgrade` — per RFC
7230 these are per-hop, not end-to-end), appends `X-Forwarded-For`, streams the
body with `io.Copy` instead of buffering it in memory, handles response
trailers, flushes for streaming responses, and propagates client disconnects to
the upstream via the request context.

**One shared `Transport`, always.** A `Transport` *is* the connection pool.
Creating one per request pools nothing and leaks idle connections.
`http.DefaultTransport`'s `MaxIdleConnsPerHost` is **2** — fine for a
general-purpose client hitting many hosts occasionally, badly wrong for a proxy
hammering a handful of hosts constantly.

### Gotchas hit

- **`url.Parse` is extremely permissive.** It accepts `"9001"` and
  `"127.0.0.1:9001"` without error, yielding a URL with an empty `Scheme` and
  often empty `Host`. The failure surfaces much later inside the Transport with
  an error that doesn't point back at the flag. Validate `Scheme` and `Host`
  explicitly at parse time.
- **A backend answering 500/503 does NOT trigger `ErrorHandler`.** That's a
  *successful* proxy operation carrying an unsuccessful response, and it passes
  straight through. `ErrorHandler` fires only when no response was obtained at
  all — connection refused, DNS failure, dial timeout, upstream closing before
  headers. Confirmed both branches by test.
- **`os.Exit` skips deferred functions**, which is why real work lives in
  `run() error` and only `main` calls `os.Exit`. This becomes critical in
  Phase 8.
- 502 is the right code for upstream failure, not 500 — we're a gateway, and
  the fault wasn't ours.

### Interview questions this phase could generate

1. What's the difference between `Director` and `Rewrite` in
   `httputil.ReverseProxy`, and what security bug motivated the change?
2. What are hop-by-hop headers, and why must a proxy strip them?
3. A backend returns 500. Does your proxy's `ErrorHandler` run? Why not?
4. Why is sharing one `http.Transport` across all requests mandatory rather
   than merely nice?
5. How does a Go interface value differ from a C++ object with a vtable?
6. Your proxy returns 502 to a client. Name four distinct causes.

---

## Phase 2 — Backend pool, Strategy interface, Round Robin

### What we built

- `internal/backend` — `Backend` (URL, weight, its own `ReverseProxy`, health
  state, in-flight counter) and `Pool` (concurrent-safe set with
  `Add`/`Remove`/`All`/`Healthy`/`Stats`).
- `internal/balancer` — the `Strategy` interface plus `RoundRobin`.
- `internal/proxy` rewired: snapshot healthy backends → `Pick` → `Acquire` →
  forward → `defer Release`.
- Table-driven tests including a 100-goroutine × 100-pick concurrency test.

Verified: 7 sequential requests produced 9001, 9002, 9003, 9001, 9002, 9003,
9001 exactly.

### Key concepts

**The import graph must be a DAG, and it dictated a design decision.** `proxy`,
`balancer` and `health` all need to name `Backend`, so `backend` must import
none of them. That forced the shared `http.Transport` to live in `backend`
rather than `proxy` — because each `Backend` owns its own `ReverseProxy` and
therefore needs the Transport, and it cannot reach into `proxy` to get it. In
C++ a forward declaration would have papered over this; Go has none, so the
cycle is a hard compile error and the layout has to be right.

**Three different concurrency disciplines in one struct.** `Backend` mixes
immutable-after-construction fields (`URL`, `Weight`, `Proxy` — no
synchronization needed), an atomic counter (`inFlight`), and mutex-guarded
fields (`alive`, `failures`, `successes`). That's normal. What makes it safe is
that the mutable fields are lowercase, so nothing outside the package can reach
past the accessors.

**Return a copy, not the slice.** `Pool.All()` copies under `RLock`. Returning
`p.backends` directly would hand the caller a slice whose backing array `Add`
can reallocate and `Remove` can shuffle underneath them. The `*Backend` pointers
inside are deliberately shared — each `Backend` locks itself; what the Pool
protects is the *slice*, not the backends.

**Lock ordering.** `Pool.Healthy()` calls `b.Alive()` (taking `b.mu`) while
holding `p.mu`. Nested locking is where deadlocks come from, and it's safe here
only because the order is strictly one-way: pool lock → backend lock, never the
reverse. Nothing in the package ever takes `p.mu` while holding a `b.mu`.

**Small interfaces.** `Strategy` has two methods and knows nothing about health,
the Pool, or HTTP. The "filter to healthy candidates" policy lives in
`Pool.Healthy()` so no strategy ever has to think about it — which is why the
strategies are testable without starting a server.

**503 vs 502.** No healthy backends at all → 503 ("I have no upstream to ask").
Asked an upstream and it failed → 502. Different operational problem.

### Gotchas hit — the big one

**`r.n++` is a data race, and it's the whole point of this phase.** Read,
add, write — three steps, not one. Two goroutines both read 7, both write 8;
one request is duplicated and one backend skipped.

Proved it rather than assuming it. Naive counter, 100 goroutines × 100 picks
across 4 backends:

```
distribution: map[a:2502 b:2489 c:2494 d:2515]   (want 2500 each)
```

Under `go test -race`: `WARNING: DATA RACE — Read at 0x... / Previous write at
0x...` with both stacks. With `atomic.Uint64` the counts are *exactly* 2500.

The insidious part is that skew is only ~1%. It looks almost right. It passes
casual testing, survives code review, and ships. **Concurrent code that "worked
when I ran it" is worthless evidence** — only `-race` is evidence.

Other gotchas:

- `atomic.Int64` (the wrapper type) over a bare `int64` + `atomic.AddInt64`: the
  wrapper makes an accidental non-atomic read *impossible to write*. The old
  style compiled fine and raced silently.
- `wg.Add(1)` must be before `go`, never inside the goroutine — otherwise
  `Wait()` can observe a zero counter and return before anything started.
- `t.Helper()` in test helpers, or every failure reports at the helper's line
  instead of the caller's and tells you nothing.
- Go defines both signed and unsigned integer overflow as wrapping. In C++
  *signed* overflow is UB — one less hole to fall into here.
- `defer` fires at **function** exit, not block exit. Deferring in a loop
  accumulates.
- Removing a backend from the pool does not cancel in-flight requests already
  dispatched to it — they hold a `*Backend` and finish normally. That's correct
  behaviour; yanking them would turn an operator action into client errors.

### Interview questions this phase could generate

1. Why is `counter++` unsafe across goroutines when it's a single line of Go?
   What does the CPU actually do?
2. `go test` passes, `go test -race` fails. What does that tell you, and which
   result do you trust?
3. Why does `Pool.All()` return a copy of the slice but share the `*Backend`
   pointers? Isn't that half-safe?
4. You hold the pool lock and call a method that takes a backend lock. When is
   that safe and when does it deadlock?
5. Why is `Strategy.Pick` handed a candidate slice instead of the `Pool` itself?
6. `atomic.Int64` vs `sync.Mutex` vs `sync.RWMutex` — pick one for a
   read-heavy counter and defend it.
7. When should a proxy return 503 rather than 502?

---

## Phase 3 — Weighted, Least Connections, Random

### What we built

Three more `Strategy` implementations, no changes to the interface — which is
the point of having had an interface. Plus `url|weight` syntax on `-backends`.

Verified live at 5:1:1 across 14 requests:
`9001 9001 9002 9001 9003 9001 9001` twice over → totals 10:2:2.

### Key concepts

**Smooth weighted round-robin (the nginx algorithm).** Per pick: every
backend's `currentWeight += weight`; pick the max; the winner does
`currentWeight -= totalWeight`. Winning pushes you deep negative so you must
climb back, and your weight is your climb rate. After `sum(weights)` picks the
state returns exactly to its start.

Why not the obvious approach: expanding `{a:5, b:1, c:1}` into `[a a a a a b c]`
gives the same *totals* but emits five consecutive `a`s — a burst of five
requests slams one backend while two idle. It also allocates a slice
proportional to the sum of weights (`{1000, 1}` → 1001 elements). Smooth gives
`a a b a c a a`.

**A test that only counts picks cannot tell these apart.** Both produce 5/1/1.
Asserting the exact *order* is what actually pins the algorithm — that's why
`TestWeightedRoundRobinSequence` checks sequence, not totals.

**Least Connections adapts; round robin assumes.** Round robin assumes every
request costs the same. Least connections measures: a backend in a GC pause
accumulates in-flight requests and stops being chosen until it drains.

**The scan is not a snapshot, deliberately.** Each `InFlight()` is an atomic
load, but backend 0's count can change while we read backend 3's. Locking the
pool for a consistent view would serialize every request in the process against
every other — far more expensive than an occasional imperfect choice. Load
balancing is a heuristic; approximately right without contention beats exactly
right under a global lock.

**Tie-breaking matters more than it looks.** At low traffic every backend sits
at 0 in-flight, so a naive scan returns index 0 forever and one backend takes
everything. A rotating start offset makes all-tied picks round-robin instead.

**Value receivers vs pointer receivers.** `Random` uses value receivers because
it has no state. The method-set rule this exposes: with a value receiver both
`Random` and `*Random` satisfy `Strategy`, because the method set of `*T`
includes methods declared on `T`. The reverse is false — with pointer receivers,
`var s Strategy = Random{}` would not compile. That asymmetry causes most early
"does not implement" errors.

**`math/rand/v2`, not `math/rand`.** v2's top-level functions use a per-P
generator with no global mutex (v1 was safe but via one global lock — which
would reintroduce exactly the contention Random exists to avoid), and it's
auto-seeded, killing the classic "forgot `rand.Seed`, same sequence every run"
bug. Use `rand.IntN(n)`, never `rand.Int() % n` — modulo bias, same trap as
`rand() % n` in C.

### Gotchas hit

- **Pointer-keyed map = slow memory leak.** `WeightedRoundRobin` keys state by
  `*Backend`. Backends removed via the admin API would leave entries behind
  forever. Pruning is guarded by a length check so the hot path costs one
  integer comparison and zero allocations. There's a test asserting the map
  shrinks.
- **Never range over the state map to pick.** Go randomizes map iteration order
  *on purpose* (unlike `std::unordered_map`'s arbitrary-but-stable order), so
  tie-breaking would be non-deterministic and tests flaky. Range the candidate
  slice instead.
- **`delete` during `range` over the same map is explicitly legal in Go** —
  unlike erasing from a `std::map` mid-iteration.
- **A nil map panics on write but not on read.** That's why `RoundRobin`'s zero
  value works and `WeightedRoundRobin` needs a constructor.
- **`t.Fatalf` is not safe from a non-test goroutine** — it calls
  `runtime.Goexit` on the wrong goroutine and the test hangs. Use `t.Errorf` +
  `return` inside goroutines.
- **Don't assert an even split for Random.** It's only statistically even; an
  exact assertion flakes a few times a year, which is worse than no test. Assert
  the true weaker property (every backend chosen, within a ±30% band).
- An unguarded map write in `WeightedRoundRobin` wouldn't be a lost update —
  it's a hard runtime throw ("concurrent map writes") that kills the process and
  cannot be `recover`ed. Caught by the concurrency test under `-race`.
- `strings.SplitN(raw, "|", 2)` not `Split` — a URL may legally contain `|` in
  its query string, and only the first separator should count.

### Interview questions this phase could generate

1. Implement weighted round robin. Now explain why list-expansion is wrong even
   though it gives the right totals.
2. Your least-connections scan reads counters that change mid-scan. Is that a
   bug? Defend your answer.
3. All backends are idle. Why does a naive least-connections implementation
   send everything to one of them?
4. When does a value receiver make a type satisfy an interface that a pointer
   receiver wouldn't — and vice versa?
5. Why is `rand.Int() % n` biased, and when does it matter?
6. You key a cache by pointer. What's the failure mode nobody notices for six
   months?
7. Which strategy would you pick for backends with wildly varying request
   costs, and which for maximum throughput at very high RPS?

---

## Phase 4 — Active health checking

### What we built

`internal/health` — a `Checker` that probes every backend concurrently on a
ticker, feeds results into `Backend.RecordResult`, and logs only transitions.
Plus a full table-driven test of the state machine and `httptest`-backed tests
for timeout, refused connection, 404, cancellation and concurrency.

Verified live: killed 9002 → ejected after 2 failed checks, traffic continued on
9001/9003 with **zero 502s**; restarted it → readmitted after 2 passes. Compare
Phase 2, where killing a backend produced a 502 on every third request.

### Key concepts

**Active vs passive checking.** Passive means learning a backend is down by
serving somebody a 502. Active means probing on a schedule so it's out of
rotation before real traffic reaches it. That difference is the entire phase.

**`select` is the construct with no C++ equivalent.** It blocks until one of
several channel operations is ready, choosing uniformly at random among ready
cases. Nearest analogues are `epoll`/`WaitForMultipleObjects` over several
handles, or a condvar with a predicate covering both "work available" and
"please stop".

**`ctx.Done()` returns a channel that is *closed* on cancellation.** A closed
channel is permanently ready to receive, which is why one cancellation wakes any
number of waiting goroutines with no broadcast mechanism. Closing a channel is
Go's idiomatic "this happened" signal, and unlike a condition variable there's
no lost-wakeup race if you close before anyone waits.

**Whoever starts a goroutine owns knowing how it stops.** Here the answer is
"cancel the context", and `TestRunStopsOnContextCancel` makes that a guarantee
rather than an intention — it asserts `Run` returns *and* that probing actually
ceases.

> C++ contrast: there's no thread handle to join and no `std::jthread`
> `stop_token`. `context.Context` *is* the stop token, passed explicitly as the
> first argument by convention. And unlike a detached `std::thread`, a leaked
> goroutine keeps its entire captured object graph alive against the GC.

**Derive the probe context from the parent.** `context.WithTimeout(ctx, ...)`
gives both behaviours at once: shutdown aborts in-flight probes immediately, and
a hung backend can't stall the round. Using `context.Background()` there would
make shutdown wait out every probe.

**Hysteresis prevents flapping.** N consecutive failures to eject, M consecutive
successes to readmit, and any opposite result resets the run. Without the reset,
a backend failing every other check would accumulate failures forever and
eventually be ejected despite being half healthy.

### Gotchas hit

- **An unstopped `time.Ticker` leaks** its runtime timer and channel forever.
  `defer ticker.Stop()` immediately after construction.
- **The loop-variable capture bug — and why it's gone.** Before Go 1.22, `for _,
  b := range` reused a *single* variable, so every goroutine capturing `b` raced
  on it and most saw the final element. The old fix was passing it as a
  parameter. Go 1.22 gives each iteration a fresh variable. Critically, this is
  selected by **the `go` directive in go.mod, not your installed toolchain** —
  ours says 1.22, so direct capture is safe here.
- **You must drain a response body before closing it.** Closing an undrained
  body makes net/http discard the whole TCP connection, so every probe pays a
  fresh handshake and the idle pool is never used. `io.Copy(io.Discard,
  io.LimitReader(body, 4096))` — the `LimitReader` stops a backend that streams
  gigabytes from `/health` exhausting our memory.
- **Failing to call a `context` `cancel` leaks the child** until the parent is
  cancelled. `go vet` has a dedicated check for this.
- **Don't follow redirects on a health check.** A 302 could send the probe to a
  *different* host that is healthy, keeping a broken backend in rotation.
  `CheckRedirect: return http.ErrUseLastResponse`.
- **A hung backend is worse than a refused one.** It accepts the TCP connection
  and never answers; without a timeout the probe blocks forever and the backend
  is never ejected, so traffic keeps flowing into a black hole. Explicitly
  tested.
- **404 on the health path must eject.** If it counted as healthy, a
  misconfigured path would keep a broken backend serving traffic and you'd learn
  from users.
- Probe backends **concurrently**: 20 backends × 2s timeout served serially is
  40s per round, permanently behind a 5s interval. Tested by asserting wall-clock
  time ≈ one probe, not the sum.
- Tests drive `CheckOnce` directly rather than `Run` + `sleep`. "Sleep and hope"
  is the main source of flaky CI.
- Health checks use a **separate** `http.Transport` from real traffic, so probes
  can't evict the idle connections live requests depend on.
- `t.Cleanup` over `defer` in a test helper — `defer` fires when the *helper*
  returns, not when the test finishes.

### Interview questions this phase could generate

1. Walk through `select` with a `ctx.Done()` case. Why does closing a channel
   wake every waiter?
2. A backend accepts connections but never responds. What does your health
   checker do, and what would a naive one do?
3. Why do you need *consecutive* failure counts rather than a failure rate?
4. What breaks if you don't drain an HTTP response body before closing it?
5. Explain the pre-Go-1.22 loop variable capture bug. How do you know which
   semantics your code gets?
6. Your checker probes 50 backends with a 3s timeout on a 5s interval. What
   goes wrong if probes are serial?
7. Why derive the probe's context from the parent instead of using
   `context.Background()`?

---

## Phase 5 — YAML config and validation

### What we built

`internal/config` — struct-tagged types, `Default()`, `Load()`, and a `Validate()`
that reports every problem at once. Plus `config.yaml` at the repo root and a
test that *loads the shipped config file* so a broken example can't be merged.

Our one third-party dependency: `gopkg.in/yaml.v3`. Go's stdlib has JSON, XML
and CSV but no YAML, and hand-rolling a YAML parser teaches you about YAML, not
about proxies.

### Key concepts

**Struct tags and reflection.** `yaml:"admin_port"` is arbitrary metadata on a
field, read at runtime via reflection to map snake_case document keys onto
CamelCase Go fields. C++ has no equivalent before C++26 reflection — you'd
hand-write or generate a `from_yaml()` per struct. Go trades some compile-time
safety (a tag typo isn't a compile error) for not writing that code.

**Reflection-based decoders cannot see unexported fields.** A lowercase field
would silently stay at its zero value forever. Every config field must be
exported.

**Decode on top of defaults — the central decision.** The decoder only assigns
keys actually present in the document, so:

```go
cfg := Default()      // pre-populate
dec.Decode(&cfg)      // present keys overwrite; absent keys keep defaults
```

This sidesteps Go's zero-value ambiguity completely. With an empty struct you
could never distinguish `port: 0` (explicitly set, invalid) from `port` omitted
(use 8080) — both leave the field `0`. The common alternative, "decode into
zero, then patch every field that came out zero", makes it *impossible* to
configure a legitimate zero value.

**`KnownFields(true)` — reject unknown keys.** Without it, `strategey:` decodes
silently and you run round robin in production while the config file claims
otherwise. Verified: it now fails at startup naming the offending key.

**`errors.Join` (Go 1.20+)** bundles many errors into one value whose `Error()`
prints them newline-separated, and which `errors.Is`/`As` still match against
any member. It returns `nil` when everything is nil, so it doubles as the
success path. Verified output — 7 problems reported in one run:

```
port: 0 is not in 1..65535
admin_port: 70000 is not in 1..65535
strategy: "round-robin" is not one of [least_connections random round_robin weighted_round_robin]
backends[0]: url "localhost:9001" must start with http:// or https://
backends[2]: url "http://127.0.0.1:9002" duplicates backends[1]
health_check.timeout (5s) must be less than health_check.interval (1s), ...
health_check.path: "health" must start with /
```

> C++ contrast: an exception carries one failure and unwinds immediately. This
> is closer to accumulating a `vector<string>` of diagnostics — except the
> result is still a first-class `error` you can inspect programmatically.

**Validate once at the edge, then trust.** Everything downstream of `Load` can
assume the config is sane, instead of re-checking defensively everywhere.

### Gotchas hit

- **`url.Parse` is permissive in a specific, surprising way — and I had the
  details wrong until a test corrected me.** `url.Parse("127.0.0.1:9001")`
  *errors* ("first path segment in URL cannot contain colon"), because a scheme
  can't start with a digit. But **`url.Parse("localhost:9001")` succeeds** —
  reading `localhost` as the *scheme* and `9001` as an opaque body, leaving
  `Host` empty and nothing dialable. Only the explicit scheme check catches
  that one. Both cases now have their own test.
- **yaml.v3 decodes `time.Duration` from `"5s"` natively and rejects a bare
  `1500`.** That's the behaviour we want: a bare number is ambiguous, and Go's
  own reading (nanoseconds) would surprise everyone. Verified experimentally
  before relying on it.
- **An empty file returns `io.EOF` from `Decode`, not a parse error.** Treat it
  as "all defaults" and let validation object to what has no default.
- **Index every message: `backends[2]: ...`** tells the user which line to fix;
  "invalid url" doesn't.
- **Duplicate backend URLs are not harmless** — round robin sends that backend
  double traffic while the operator sees two innocent lines.
- **`%w` wrapping preserves `*fs.PathError`**, so callers can still use
  `errors.Is(err, os.ErrNotExist)` to tell "missing config" from "bad config".
  There's a test for exactly that.
- Validation asks `balancer.Names()` which strategies exist rather than
  duplicating the list — one source of truth, so adding a strategy can't leave
  validation stale.
- `t.TempDir()` gives each test a fresh auto-removed directory: no cleanup code,
  no collisions, nothing left in the repo.

### Interview questions this phase could generate

1. Your config struct has `Port int`. The file says `port: 0`. How do you tell
   that apart from the key being absent?
2. Why must config struct fields be exported?
3. What happens when a config file contains a key your struct doesn't have —
   and what *should* happen?
4. What does `errors.Join` give you that returning the first error doesn't?
5. `url.Parse("localhost:9001")` returns no error. What did it actually parse,
   and why is that dangerous?
6. Why validate at load time rather than where each value is used?

---

## Phase 6 — CLI subcommands and the admin API

### What we built

- `internal/admin` — an HTTP control plane (`GET /status`, `POST /backends`,
  `DELETE /backends`) plus a `Client` for the CLI side.
- `cmd/proxyforge` — `start`, `status`, `add-backend`, `remove-backend` via
  stdlib `flag.NewFlagSet`, with a `tabwriter` status table and meaningful exit
  codes.

Verified the whole round trip: status with nothing running exits 2 with a human
message; add/remove mutate the live pool; duplicate add → conflict; removing an
unknown URL → not found.

### Key concepts

**The problem this phase actually solves is a process boundary.** `proxyforge
status` is a *different process* from `proxyforge start` and shares none of its
memory. Something has to carry the question across. We chose a loopback HTTP API
on a second port because it's pure stdlib and identical on every platform.
Rejected: Unix sockets/named pipes (completely different Windows and Unix code
paths, needs a third-party package on Windows), and config-rewrite + signal
reload (Windows has no usable signals beyond Ctrl+C, and `status` can't work
that way at all).

**Go 1.22's method-aware `ServeMux`.** `mux.HandleFunc("GET /status", ...)`
means a wrong method gets an automatic 405 with a correct `Allow` header. Before
1.22 you registered the path and hand-wrote a `switch r.Method`, remembering the
405 yourself. This is the concrete reason go.mod says 1.22.

**`flag.NewFlagSet` per subcommand.** The package-level `flag.String`/`flag.Parse`
write into one global set, which cannot express per-subcommand flags. Also
`flag.ContinueOnError` rather than the default `ExitOnError` — otherwise the flag
package calls `os.Exit` out from under you, skipping deferred cleanup and making
the behaviour untestable.

**`errors.As` vs `errors.Is`.** `Is` compares against a specific *value* (a
sentinel). `As` walks the chain looking for a specific *type* and assigns it to
your target so you can read its fields. We use `As` for `*net.OpError` because
it's a struct with detail, not a singleton — that's what turns "dial tcp
127.0.0.1:9090: connectex: No connection could be made..." into "proxyforge is
not running".

**Distinct exit codes** (0 / 1 / 2) so scripts can branch without parsing stderr.

### Gotchas hit

- **Bind the admin API to `127.0.0.1`, never `:port`.** It can add and remove
  backends with *no authentication*. On all interfaces, anyone who can reach the
  box could repoint your proxy at a server they control. Loopback-only is the
  entire security model, so it can't be left to a default.
- **`http.MaxBytesReader` on every JSON endpoint.** Without it a client streams
  gigabytes and the decoder faithfully buffers all of it — a one-line
  memory-exhaustion DoS present on most naive JSON APIs. There's a test.
- **`DisallowUnknownFields`** on the JSON decoder, same reasoning as
  `KnownFields` in config: `{"wieght": 5}` must fail, not silently mean weight 1.
- **`tabwriter` buffers until `Flush`** — the column widths aren't known until
  then. Forget it and you get *no output at all*, not misaligned output.
- **The `flag` package has no required flags.** Required-ness is always a manual
  check.
- **Status must list DOWN backends.** Hiding them is exactly backwards — that's
  the row the operator most needs to see.
- **The consecutive-success counter needed clamping.** Caught by reading real
  status output: `PASSES` climbed 1, 2, ... 6 and would reach the millions for a
  backend healthy for a week. The counter answers "how far through a transition
  are we", so past the threshold it's noise. Now clamped in both directions.
- **`add-backend`/`remove-backend` change the running process only** and don't
  rewrite `config.yaml`. Writing back would reformat the user's YAML and destroy
  their comments — a worse surprise than "runtime changes are runtime-only". The
  CLI says so explicitly after each call.
- 409 Conflict for a duplicate, 404 for an unknown URL — so the CLI distinguishes
  cases by status code rather than string-matching messages.
- `httptest.NewRecorder` drives handlers with no socket, no port, no cleanup.

### Interview questions this phase could generate

1. Your CLI's `status` command needs data that lives in another process. Walk
   through three ways to get it and defend your choice.
2. When do you use `errors.Is` and when `errors.As`?
3. What's wrong with a JSON endpoint that calls `json.NewDecoder(r.Body)`
   directly?
4. Why bind an admin endpoint to loopback specifically?
5. Why `flag.ContinueOnError` instead of the default?
6. You remove a backend from the pool while it's serving 50 requests. What
   happens to them, and what *should* happen?

---

## Phase 7 — Structured logging with request IDs and latency

### What we built

`internal/logging` — `log/slog` setup (text or JSON), a middleware assigning
request IDs and timing requests, and a `responseRecorder` that captures the
status code. All `log.Printf` calls across the project converted to `slog`.
Added a `log:` section to config.

Real output:

```
level=INFO msg=request request_id=cb3ec73a8aeefddd method=GET path=/api/thing
  backend=http://127.0.0.1:9002 status=200 bytes=82 latency_ms=121.963
level=INFO msg=request request_id=trace-from-upstream method=GET path=/traced
  backend=http://127.0.0.1:9003 status=200 bytes=79 latency_ms=1.391
```

`log/slog` is stdlib since Go 1.21, so zap and logrus buy nothing but
dependencies.

### Key concepts

**`func(http.Handler) http.Handler` is Go's universal middleware shape** — take
the next handler, return one that wraps it. Because `http.Handler` is a
one-method interface, middleware composes by nesting with no framework. It's the
decorator pattern where the "object" is a closure over `next` and there's no
base class anywhere.

**Context keys must be a custom unexported type.** Keys compare by interface
equality (type *and* value), so a bare string `"requestID"` collides with any
other package that had the same idea — silently, at runtime, one package reading
another's value. `type ctxKey struct{}` can't be constructed outside its package,
so collision is impossible by construction.

**Contexts are immutable, so passing data back *up* needs a pointer.** The
middleware runs before the proxy and has already handed control downward. Storing
a `*reqInfo` in the context lets `SetBackend` mutate what the middleware will
later log. Safe without a lock because net/http handles each request on exactly
one goroutine.

**`r.WithContext(ctx)` returns a shallow copy.** Requests are never mutated in
place; everything downstream must get the copy.

**Why wrap `ResponseWriter` at all:** it's write-only. There's no `StatusCode()`
to read back, so intercepting `WriteHeader` is the only way to know what was
sent.

### Gotchas hit — the big one

**Wrapping `http.ResponseWriter` silently breaks streaming.** The concrete value
net/http hands your handler also implements `http.Flusher`, `http.Hijacker` and
`io.ReaderFrom`. Code finds those with a type assertion. The moment you wrap it
in your own struct, `w.(http.Flusher)` checks *your* type and fails.

For a reverse proxy the consequence is specific: `httputil.ReverseProxy` flushes
streaming responses through `http.Flusher`. Lose it and Server-Sent Events,
streaming JSON and long-polling stop arriving incrementally — they buffer until
the response ends. **It still passes a curl test against a small response**,
which is exactly why this bug ships. Fixed by implementing both `Flush()` and
`Unwrap() http.ResponseWriter` (the latter is what `http.ResponseController`
walks, covering Hijack/deadlines/future capabilities too). Two dedicated tests.

Other gotchas:

- **Log injection.** An inbound `X-Request-Id` is attacker-controlled text.
  Logged verbatim, a newline lets a client forge entire log entries — hiding an
  attack from whoever reads the logs, or poisoning a log-parsing alert.
  Sanitized to `[A-Za-z0-9_-]`, capped at 64 chars, with a table test that
  includes a forged-entry payload.
- **Mirror the implicit 200.** The first `Write` sends 200 if `WriteHeader` was
  never called; the recorder must do the same or body-only responses log
  whatever it was initialized to.
- **Guard double `WriteHeader`.** The first call is the one that goes on the
  wire (net/http warns about "superfluous response.WriteHeader call"), so record
  the first, not the last.
- **JSON numbers decode as `float64`** — asserting `m["status"] == 418` fails;
  it must be `float64(418)`.
- Log latency as a **float in milliseconds**, not a `time.Duration` — the latter
  renders as `"1.234567ms"`, which is worse for aggregators computing
  percentiles.
- `slog.SetDefault` once at startup lets leaf packages log without threading a
  logger through every constructor. Explicit injection is more testable; for a
  handful of call sites this was the better trade, and the tradeoff is written
  down at the call site rather than left implicit.

### Interview questions this phase could generate

1. You wrap `http.ResponseWriter` to log status codes. What breaks, and why
   won't your tests catch it?
2. Why must a context key be an unexported custom type rather than a string?
3. Contexts are immutable. How does a downstream handler pass information back
   up to middleware that already ran?
4. A client sends `X-Request-Id: foo\nlevel=ERROR msg="all clear"`. What's the
   attack?
5. What is `http.ResponseController` for, and which method must your wrapper
   implement to cooperate with it?
6. Where does the implicit 200 status come from, and when does it bite?

---

## Phase 8 — Graceful shutdown

### What we built

`signal.NotifyContext` for SIGINT/SIGTERM, a `serverSet` owning both listeners
with `start()`/`drain()`, a `shutdown_timeout` config setting, and a `WaitGroup`
so shutdown genuinely joins the health checker.

Verified two ways. Unit tests for the drain logic (in-flight request completes,
new connections refused immediately, stuck handler hits the timeout,
`ErrServerClosed` filtered). And a real Ctrl+C against the real binary, via
`GenerateConsoleCtrlEvent`:

```
12:47:59.030  shutdown signal received, draining in-flight requests
12:48:01.624  request ... status=200 bytes=77 latency_ms=4002   ← 2.6s AFTER the signal
12:48:01.634  shutdown complete, all in-flight requests finished
```

The client got a complete 77-byte body. Without draining it would have been a
connection reset or a truncated response.

### Key concepts

**`signal.NotifyContext` (Go 1.16+)** collapses the old dance — make a
`chan os.Signal`, `signal.Notify`, spawn a goroutine to translate a receive into
a cancel — into one line.

**`Shutdown` vs `Close`.** `Shutdown` closes the listeners immediately (no new
connections), closes idle keep-alive connections, then *waits* for active
requests. `Close` severs everything at once — clients mid-response get a
truncated body and a reset. `Shutdown` does not wait for hijacked connections
(WebSockets) or never-ending long-polls; that's what the timeout is for.

**Call `stop()` as soon as draining starts.** That restores default signal
handling so a *second* Ctrl+C kills instantly. Without it, an operator watching
a slow drain has no way to give up — every further Ctrl+C is swallowed and
appears to do nothing, which is exactly when people reach for `kill -9`.

**Bind listeners before announcing.** `ListenAndServe` binds *inside* the
goroutine, so "address already in use" surfaces after you've already logged
"listening on :8080". Creating the `net.Listener` up front returns that error
before anything claims to be running — and lets tests use port 0.

**Shutdown ordering is a design decision.** Admin API first (don't accept
control-plane changes against a pool that's going away), then the proxy, and
only *then* cancel the health checker — draining requests still need an accurate
view of which backends are alive.

### Gotchas hit

- **THE graceful-shutdown bug: never pass the cancelled context to
  `Shutdown`.** The signal already cancelled `ctx`. Passing it means `Shutdown`
  returns instantly with `context.Canceled`, drains *nothing*, and severs every
  in-flight request — while your logs cheerfully report a graceful shutdown.
  The deadline must derive from `context.Background()`.
- **Buffer the error channel to the number of senders.** With an unbuffered
  channel, the second server to fail blocks forever sending to a channel nobody
  reads any more, leaking that goroutine for the life of the process.
- **`Serve` always returns non-nil; filter `ErrServerClosed`.** Otherwise every
  clean shutdown exits non-zero and an orchestrator reads it as a crash loop.
- **Windows has no POSIX signals.** `syscall.SIGTERM` is *defined* and compiles,
  but nothing ever delivers it. What arrives is `os.Interrupt` from Ctrl+C.
  Listing both keeps one code path correct on both platforms. Go's runtime maps
  `CTRL_C_EVENT`/`CTRL_BREAK_EVENT` → SIGINT and
  `CTRL_CLOSE_EVENT`/`LOGOFF`/`SHUTDOWN` → SIGTERM. Note Windows force-kills
  ~5s after `CTRL_CLOSE_EVENT`, so a 30s drain can't complete on a console close.
- **A `WaitGroup`, not an assumption.** Returning from `cmdStart` while a probe
  is still in flight would make "shutdown complete" a lie.
- **`os.Exit` skips every `defer`** — which is why all real work lives in
  functions returning `error` and only `main` exits. This is where that pays off.
- Synchronize tests on a channel, never `time.Sleep`-and-hope. The drain test
  closes a `started` channel from inside the handler so it's exact.

### Interview questions this phase could generate

1. Walk through graceful shutdown. What's the difference between `Shutdown` and
   `Close`?
2. Your shutdown code passes the signal-cancelled context to `Shutdown`. What
   happens, and why do the logs still look fine?
3. Why should a second Ctrl+C behave differently from the first?
4. Why buffer an error channel to exactly the number of goroutines?
5. `Serve` returned an error. How do you tell a crash from a clean shutdown?
6. What does `os.Exit` do to your deferred functions, and how do you structure
   a program around that?
7. Your service has a 30s drain timeout and one stuck request. What should
   happen, and what happens if you get it wrong?

---

## Phase 9 — README and wrap-up

[README.md](README.md) — architecture diagram, package responsibilities,
quickstart, config reference, strategy comparison, and the design decisions with
their rejected alternatives.

### Final state

- 9 phases, one commit each.
- 54 test functions, all passing under `-race`.
- `gofmt` clean, `go vet` clean.
- One third-party dependency: `gopkg.in/yaml.v3`.

### The five things most worth remembering

1. **`counter++` is three operations.** The naive round robin skewed
   distribution by ~1% — looking almost right is what let it survive review.
   `go test -race` found it in milliseconds. Concurrent code that passes without
   `-race` is untested, not correct.

2. **Wrapping `http.ResponseWriter` silently breaks streaming.** The concrete
   value implements `Flusher`/`Hijacker` too, and a type assertion against your
   wrapper fails. SSE and streaming JSON buffer instead of streaming, while a
   curl test against a small response still passes. `Flush()` + `Unwrap()`.

3. **Never pass the signal-cancelled context to `Shutdown`.** It returns
   instantly, drains nothing, severs every live request — and the logs still say
   "graceful shutdown".

4. **The import graph must be a DAG, and it will change your design.** Go has no
   forward declarations. Wanting `backend` to own its `ReverseProxy` is what
   moved `Transport` out of `proxy`. A cycle is the compiler telling you the
   boundary is in the wrong place.

5. **Config: decode on top of defaults, and reject unknown keys.** Otherwise you
   cannot distinguish `port: 0` from an omitted `port`, and `strategey:` silently
   runs the wrong strategy in production.

### What I'd add next

TLS termination · retries with a circuit breaker · rate limiting · auth on the
admin API · a `/metrics` endpoint · config hot-reload · sticky sessions ·
per-backend connection caps.
