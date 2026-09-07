# ProxyForge — Interview Questions & Answers

Every question this project can generate, with answers you can actually say out
loud.

Answers are written in two layers: a **short answer** you'd give first, then the
detail for when they push. Interviewers almost always push.

---

## Contents

| Part | Topic | Q's |
|---|---|---|
| [0](#part-0--about-the-project) | About the project | 6 |
| [1](#part-1--go-language-fundamentals) | Go language fundamentals | 9 |
| [2](#part-2--concurrency) | **Concurrency** — the big one | 12 |
| [3](#part-3--nethttp-and-proxying) | net/http and proxying | 9 |
| [4](#part-4--load-balancing) | Load balancing | 8 |
| [5](#part-5--health-checking) | Health checking | 7 |
| [6](#part-6--configuration) | Configuration | 6 |
| [7](#part-7--logging-and-observability) | Logging and observability | 7 |
| [8](#part-8--cli-and-control-plane) | CLI and control plane | 6 |
| [9](#part-9--graceful-shutdown) | Graceful shutdown | 7 |
| [10](#part-10--testing) | Testing | 6 |
| [11](#part-11--design-and-trade-offs) | Design and trade-offs | 6 |
| [12](#part-12--curveballs) | Curveballs | 6 |
| | **Total** | **95** |

---

# Part 0 — About the project

### Q0.1 — Tell me about this project.

**Short answer:** ProxyForge is a reverse proxy and load balancer I wrote in Go
using the standard library. It sits in front of N backend servers, distributes
HTTP requests across them using one of four pluggable strategies, health-checks
them in the background, logs every request with a trace ID and latency, and
drains in-flight requests on shutdown. About 2,400 lines of production code and
1,800 of tests, one third-party dependency.

**If they want more:** I built it in ten phases, one commit each, specifically to
learn concurrent systems programming — `net/http`, goroutines, `context`,
mutexes and atomics. The most interesting part was Phase 2, where introducing a
pool of backends meant a shared counter, and the naive `counter++` turned out to
be a data race that skewed distribution by about 1% — just wrong enough that it
would survive code review.

### Q0.2 — Why did you build it?

I wanted to understand how a load balancer actually works rather than treat
nginx as a black box, and I wanted a project where concurrency was *inherent*
rather than bolted on. A proxy is ideal for that: every request is a goroutine,
the backend pool is shared mutable state, and health checking runs
independently in the background. You can't build one without meeting the real
problems.

### Q0.3 — What was the hardest part?

**Short answer:** Realising that concurrency bugs mostly don't announce
themselves.

The round-robin counter is the example I'd give. The naive version distributed
2502/2489/2494/2515 requests across four backends instead of exactly 2500 each.
That's a 1% skew — it looks almost right, so it passes casual testing and ships.
Only `go test -race` catches it, immediately. That changed how I think about
testing concurrent code: a green run without `-race` isn't evidence.

A close second was discovering that wrapping `http.ResponseWriter` to log status
codes silently breaks streaming responses, because the wrapper no longer
satisfies `http.Flusher`. That one still passes a curl test.

### Q0.4 — What would you do differently?

Three things.

1. **I'd inject the logger rather than using `slog.SetDefault`.** It was the
   right call for the number of call sites I had, but it makes those packages
   harder to test in isolation, and that cost grows.
2. **I'd add retries and a circuit breaker earlier.** Right now one failed
   request is one failed request; a proxy that can retry an idempotent request
   against a different backend is meaningfully more useful.
3. **I'd design the admin API with auth from the start**, even a shared token.
   Loopback-only is a real security model, but it's also a decision that's hard
   to revisit once people rely on the endpoint.

### Q0.5 — What are its limitations?

No TLS termination, no retries or circuit breaking, no rate limiting, no auth on
the admin API, no metrics endpoint, no config hot-reload, no sticky sessions. It
reads config once at startup.

I'd call it correct but not production-hardened. Being explicit about that is
part of the design — I'd rather have a small thing I fully understand than a
large thing I half-understand.

### Q0.6 — How did you verify it works?

Two ways. 54 table-driven tests under the race detector, and end-to-end runs
against real processes where I measured actual behaviour:

- Killed a backend → 6/6 requests still returned 200, zero client-visible 502s
- Sent a real Ctrl+C during a 6-second request → it completed 4.5 seconds
  *after* the signal, with a full body
- Measured proxy overhead at 0.948ms against a 900ms backend delay

I wrote the outputs down rather than describing them from memory, because
"it should work" and "I watched it work" are different claims.

---

# Part 1 — Go language fundamentals

### Q1.1 — What does `internal/` mean, and how is it enforced?

**Short answer:** A package under `internal/` can only be imported by code
rooted at `internal/`'s parent directory. It's a **compile error** otherwise,
enforced by the toolchain — not a convention.

So if someone runs `go get github.com/me/proxyforge`, they physically cannot
import `internal/balancer`. It lets you have a public module with a genuinely
private implementation.

> **C++ contrast:** the closest analogue is a `detail::` namespace or simply not
> shipping the header. Both are politeness. This is enforcement.

### Q1.2 — Why are unused imports a compile error rather than a warning?

Because a warning you can ignore is a warning everyone ignores. Unused imports
are a code-rot signal — they slow builds, imply dependencies that don't exist,
and accumulate. The Go team decided the friction of fixing them immediately is
smaller than the cost of letting them pile up. There's deliberately no flag to
disable it.

Note it applies to unused **imports** and unused **local variables** only.
Unused function parameters and struct fields are fine — which is why a handler
with an empty body still compiles.

### Q1.3 — How does a Go interface value differ from a C++ object with a vtable?

**Short answer:** In C++ the vtable pointer lives *inside* the object. In Go the
interface value is a separate two-word pair — `(type descriptor, data pointer)`
— built at the moment of assignment.

Consequences:

- A Go type doesn't declare which interfaces it satisfies. `*Server` becomes an
  `http.Handler` purely by having `ServeHTTP` with the right signature.
- The concrete type carries no per-interface overhead. The same struct can
  satisfy any number of interfaces with zero cost to the struct itself.
- The compiler checks the match at the point of **use**, not declaration.
- A nil interface and an interface holding a nil pointer are **different
  things** — the classic `err != nil` surprise.

### Q1.4 — Explain value vs pointer receivers and the method-set rule.

**Short answer:** With a **value** receiver, both `T` and `*T` satisfy the
interface. With a **pointer** receiver, only `*T` does.

```go
type Random struct{}
func (Random) Pick(...) ...          // value receiver

var s Strategy = Random{}    // ✅ compiles
var s Strategy = &Random{}   // ✅ also compiles
```

```go
func (r *RoundRobin) Pick(...) ...   // pointer receiver

var s Strategy = &RoundRobin{}  // ✅
var s Strategy = RoundRobin{}   // ❌ "does not implement"
```

The reason: the method set of `*T` includes methods declared on `T` (you can
always dereference a pointer), but the method set of `T` does *not* include
methods on `*T` (you can't take the address of an arbitrary value).

This asymmetry causes most early "does not implement" errors. In this project
`Random` uses value receivers because it has no state; everything else uses
pointer receivers because they carry mutable selection state.

### Q1.5 — Go has no forward declarations. What does that force?

**Short answer:** The package import graph must be a **DAG**. An import cycle is
a hard compile error with no escape hatch.

In C++ you break a cycle with `class Pool;` and an include guard. Go has nothing
equivalent, so if two packages want to import each other, that's the compiler
telling you the abstraction boundary is in the wrong place.

**It changed this project's design.** I wanted each `Backend` to own its
`ReverseProxy`, which meant `backend` needed the shared `http.Transport` — but
`backend` can't import `proxy`, because `proxy` imports `backend`. So the
`Transport` moved into `backend`. The compiler forced a better layout.

### Q1.6 — How does encapsulation work in Go?

Per **package**, not per type. A capitalized identifier is exported outside its
package; lowercase is package-private. Everything inside `package backend` can
see everything else in it.

There is no `private:`, no `protected:`, no `friend`. That's coarser than C++,
but it means "one package = one thing you can reason about as a whole", and it's
why package boundaries in Go carry real design weight.

### Q1.7 — Errors are values. What does that actually change?

No exceptions, no stack unwinding, no `noexcept` to reason about. `time.ParseDuration`
returns `(time.Duration, error)`, and ignoring the error is something you have
to explicitly write (`_ = err`).

The tooling that matters:

- **`fmt.Errorf("...: %w", err)`** wraps, preserving the original.
- **`errors.Is(err, target)`** compares against a specific **value** (a
  sentinel), walking the wrap chain.
- **`errors.As(err, &target)`** finds a specific **type** and assigns it, so you
  can read its fields.
- **`errors.Join(errs...)`** bundles many errors into one, and returns nil if
  they're all nil.

### Q1.8 — When do you use `errors.Is` vs `errors.As`?

**`Is` for values, `As` for types.**

```go
errors.Is(err, backend.ErrDuplicate)   // is it this specific sentinel?

var opErr *net.OpError
errors.As(err, &opErr)                 // is there a *net.OpError in the chain,
                                       // and give it to me so I can inspect it
```

In this project I use `Is` for sentinels like `ErrNotFound`, and `As` for
`*net.OpError` — because that's a struct with detail I want, not a singleton to
compare against. That's what turns
`dial tcp 127.0.0.1:9090: connectex: No connection could be made...` into
`proxyforge is not running`.

### Q1.9 — What's the difference between a nil map read and a nil map write?

**Reads from a nil map are fine** and return the zero value. **Writes panic.**

That's why `RoundRobin`'s zero value is usable (`&RoundRobin{}` works) but
`WeightedRoundRobin` needs a constructor — it holds a `map[*Backend]int` that
must be initialised before the first write.

Related: a missing key returns the zero value rather than throwing, so "absent"
and "present but zero" are indistinguishable unless you use the two-value form
`v, ok := m[k]`.

---

# Part 2 — Concurrency

*This is the section that gets drilled hardest. Every answer here should come
with the reason, not just the rule.*

### Q2.1 — Why is `counter++` unsafe across goroutines when it's one line of Go?

**Short answer:** Because it's not one operation. It's read, add, write — three
steps with no atomicity between them.

Two goroutines can both read 7, both compute 8, and both write 8. One increment
is lost. In round robin that means two requests go to the same backend and one
backend is skipped entirely.

**The part that makes it dangerous:** the damage is *subtle*. I measured the
naive version distributing 2502/2489/2494/2515 across four backends instead of
exactly 2500 each. A 1% skew looks like noise. It passes casual testing,
survives code review, and ships.

Formally it's a data race, which in Go means the program's behaviour is
**undefined** — not merely "occasionally off by one".

Fix: `atomic.Uint64`, which makes the read-modify-write a single indivisible
instruction (`LOCK XADD` on x86).

### Q2.2 — `go test` passes but `go test -race` fails. Which do you trust?

**`-race`, always.**

A data race is a property of the *program*, not of a particular run. Without
instrumentation, whether you observe it depends on scheduling, core count, cache
timing and load — so a passing run tells you nothing. The race detector
instruments every memory access and reports the actual unsynchronised
read/write pair with both stack traces.

The rule I'd state: **concurrent code that passes without `-race` is untested,
not correct.**

Caveat worth mentioning: `-race` only finds races on code paths that actually
execute, so it's only as good as your test coverage. It also slows execution
~10x and increases memory use, so it's a test-time tool, not a production one.

### Q2.3 — `atomic` vs `Mutex` vs `RWMutex` — how do you choose?

| Use | When |
|---|---|
| **`sync/atomic`** | A single word of memory — a counter, a flag, a pointer swap. Cheapest, no blocking |
| **`sync.Mutex`** | Multiple fields that must change together, or any critical section wider than one word |
| **`sync.RWMutex`** | Same as Mutex, but reads massively outnumber writes and the critical section is long enough that the extra bookkeeping pays for itself |

In this project:

- Round-robin counter and in-flight counters → **atomic** (single word, hot path)
- Backend `alive`/`failures`/`successes` → **RWMutex** (every request reads
  `Alive()`, only the health checker writes)
- `WeightedRoundRobin`'s state map → **plain Mutex**, because *every* call
  mutates it, so there are no read-only callers to benefit and RWMutex would
  just be slower

**Don't reach for RWMutex reflexively.** It's measurably slower than Mutex when
every acquisition is a write, and its read path is more expensive than a plain
lock for very short critical sections.

### Q2.4 — Why does `Pool.All()` return a copy of the slice but share the pointers?

**Short answer:** Different things need protecting.

The **slice** is mutable shared state — `Add` can reallocate the backing array
and `Remove` shuffles elements. If I returned `p.backends` directly, a caller
ranging over it while the admin API added a backend would be reading a
reallocated array. That's a textbook race.

The **`*Backend` pointers** are deliberately shared, because each `Backend` does
its own internal locking. Copying the Backends themselves would give every
caller a stale snapshot of health and in-flight state, which is precisely what
we don't want.

So: copy the container, share the contents.

### Q2.5 — You hold the pool lock and call a method that takes a backend lock. When is that safe?

**Short answer:** When the lock ordering is strictly one-directional and you can
prove nothing ever takes them in the opposite order.

`Pool.Healthy()` holds `p.mu` and calls `b.Alive()`, which takes `b.mu`. That's
safe here **only** because pool-lock-then-backend-lock is the only order that
ever occurs — nothing in the package takes `p.mu` while holding a `b.mu`.

If both orders existed, you'd have a classic AB-BA deadlock: goroutine 1 holds
`p.mu` waiting on `b.mu`, goroutine 2 holds `b.mu` waiting on `p.mu`.

The general rule: **establish a global lock ordering and document it.** If you
can't state the ordering, you don't have one.

### Q2.6 — Walk through `select` with a `ctx.Done()` case.

```go
for {
    select {
    case <-ctx.Done():
        return
    case <-ticker.C:
        c.CheckOnce(ctx)
    }
}
```

`select` blocks until one of its cases is ready. If several are ready it picks
one **uniformly at random** — which prevents starvation but means you can't rely
on priority.

This is the construct with no real C++ equivalent. The nearest things are
`epoll`/`WaitForMultipleObjects` over several handles, or a condition variable
with a predicate covering both "work available" and "please stop".

### Q2.7 — Why does closing a channel wake every waiter?

**Short answer:** A closed channel is *permanently* ready to receive. Receives on
it return immediately with the zero value.

That's why `context` cancellation works for any number of goroutines with no
broadcast mechanism — `ctx.Done()` returns a channel that gets closed, and
everyone selecting on it wakes at once.

It's also why closing is the idiomatic "this happened" signal: unlike a
condition variable, there's no lost-wakeup race if you close before anyone
starts waiting. A goroutine that arrives late still sees a closed channel.

Rules: only the sender should close, closing twice panics, and sending on a
closed channel panics.

### Q2.8 — Who is responsible for stopping a goroutine?

**Whoever started it.** If you write `go f()`, you must be able to answer "how
does this stop?"

In this project the answer is always "cancel the context". `Checker.Run(ctx)`
returns promptly when `ctx` is cancelled, and there's a test asserting both that
it returns *and* that it genuinely stops probing.

> **C++ contrast:** there's no thread handle to join, and no `std::jthread`
> `stop_token`. `context.Context` *is* the stop token, passed explicitly as the
> first parameter by convention. And unlike a detached `std::thread`, a leaked
> goroutine keeps its entire captured object graph alive against the GC — so a
> goroutine leak is also a memory leak.

### Q2.9 — Explain the pre-Go-1.22 loop variable capture bug.

**Before 1.22**, `for _, b := range items` reused a **single** variable across
all iterations. So:

```go
for _, b := range backends {
    go func() { probe(b) }()      // every goroutine captures the SAME b
}
```

Most goroutines would observe the final element, and it was also a data race.
The standard fix was passing it as a parameter: `go func(b *Backend){...}(b)`.

**Go 1.22 changed the spec** so each iteration gets a fresh variable, making
direct capture safe.

**The critical detail:** which semantics you get is selected by **the `go`
directive in `go.mod`**, not by your installed toolchain. A module saying
`go 1.21` compiled with Go 1.27 still gets the old behaviour. That's how Go ships
a breaking language change without breaking anyone.

### Q2.10 — Why must `wg.Add(1)` be before the `go` statement?

Because if it's inside the goroutine, `wg.Wait()` can run before the goroutine
is scheduled, observe a counter of zero, and return immediately — while the work
hasn't started.

```go
wg.Add(1)              // ✅ before
go func() {
    defer wg.Done()
    ...
}()
```

### Q2.11 — Why buffer an error channel to the number of senders?

**Short answer:** So a sender can never block forever on a channel nobody is
reading any more.

```go
errCh := make(chan error, 2)   // one slot per server goroutine
```

The consumer does `select { case err := <-errCh: ... }` and then moves on. If
the *second* server also fails afterwards, it sends to a channel with no
receiver. Unbuffered, that goroutine blocks forever — a leak for the life of the
process. With capacity 2, both sends complete regardless.

The rule: **size a result channel to the number of possible senders**, or make
sure someone drains it.

### Q2.12 — What does `defer` guarantee, and where does it bite?

**Guarantees:** it runs when the **function** returns — including on an early
return and while a panic unwinds. That's what makes `defer b.Release()` safe:
the in-flight counter can't leak on an error path.

**Where it bites:**

- It's **function**-scoped, not block-scoped. `defer` inside a loop accumulates
  until the function exits — a real leak in a long loop.
- Arguments are evaluated **immediately**, not at execution time.
- `os.Exit` skips deferred functions entirely. That's why all real work in this
  project lives in functions returning `error`, and only `main` calls `os.Exit`.

> **C++ contrast:** it's the closest thing to RAII, but it's manual and
> function-scoped rather than automatic and scope-scoped.

---

# Part 3 — net/http and proxying

### Q3.1 — What's wrong with `http.ListenAndServe(addr, handler)` in production?

**It uses a zero-value `http.Server`, which has no timeouts at all.**

A client that opens a connection and never finishes sending its headers occupies
a goroutine and a file descriptor indefinitely. That's the Slowloris attack, and
it needs almost no resources from the attacker.

Minimum viable server:

```go
srv := &http.Server{
    Addr:              addr,
    Handler:           h,
    ReadHeaderTimeout: 10 * time.Second,   // cheapest Slowloris defence
}
```

Also consider `ReadTimeout`, `WriteTimeout`, `IdleTimeout` and `MaxHeaderBytes`.

### Q3.2 — What's the difference between `Director` and `Rewrite`?

`Rewrite` (Go 1.20+) replaces `Director`. Setting both **panics** at request time.

`Director` only handed you the **outbound** request. Anything that wanted the
original inbound request had to capture it in a closure, and it was easy to
accidentally forward a client-supplied `X-Forwarded-For`.

**That's a spoofing vector** — anything downstream trusting that header would
believe whatever IP the client claimed, which breaks IP allowlists, rate
limiting and audit logs.

`Rewrite` receives a `*ProxyRequest` exposing both `.In` (untouched original) and
`.Out`, and `SetXForwarded()` **replaces** the client's value rather than
appending to it.

```go
Rewrite: func(pr *httputil.ProxyRequest) {
    pr.SetURL(target)
    pr.SetXForwarded()
}
```

### Q3.3 — What are hop-by-hop headers, and why must a proxy strip them?

Headers that apply to a **single transport-level connection**, not end-to-end.
Per RFC 7230: `Connection`, `Keep-Alive`, `Proxy-Authenticate`,
`Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`.

Forwarding them corrupts the next hop's connection semantics — you'd be telling
the backend about a keep-alive or transfer-encoding negotiated with a completely
different socket. `Connection` can also name *additional* headers to treat as
hop-by-hop, which must be honoured.

`httputil.ReverseProxy` handles this for you. It's one of the main reasons not to
hand-roll a proxy with `http.Client`.

### Q3.4 — What else does `ReverseProxy` do that you'd otherwise write yourself?

- Strips hop-by-hop headers (including those named in `Connection`)
- Appends `X-Forwarded-For` and can set `X-Forwarded-Host`/`Proto`
- **Streams** the body with `io.Copy` rather than buffering it in memory
- Handles response trailers
- Flushes for streaming responses (SSE, long-polling) via `http.Flusher`
- Supports connection upgrade (WebSockets) via `Hijack`
- Propagates client disconnects to the upstream through the request context
- Provides `ErrorHandler` and `ModifyResponse` hooks

### Q3.5 — A backend returns 500. Does your `ErrorHandler` run?

**No.** That's a *successful* proxy operation carrying an *unsuccessful*
response. It streams straight back to the client untouched.

`ErrorHandler` fires only when **no response could be obtained at all**:
connection refused, DNS failure, dial timeout, TLS handshake failure, or the
upstream closing the socket before sending headers.

This trips people up constantly. If you want to react to upstream 5xx — for
passive health checking, say — you need `ModifyResponse`, not `ErrorHandler`.

### Q3.6 — Why is sharing one `http.Transport` mandatory?

**Because the `Transport` *is* the connection pool.** Creating one per request
pools nothing, so every request pays a fresh TCP handshake (plus TLS if
applicable), and idle connections accumulate until their timeouts fire — an fd
leak under load.

One `Transport` per process, shared across every goroutine. It's designed for
concurrent use.

Also: **`http.DefaultTransport`'s `MaxIdleConnsPerHost` is 2.** That's tuned for
a general-purpose client touching many hosts occasionally — badly wrong for a
proxy hammering a handful of hosts constantly. This project raises it to 100.

### Q3.7 — Your proxy returns 502. Name four distinct causes.

1. **Connection refused** — nothing listening on that port
2. **Dial timeout** — the host is unreachable or dropping packets
3. **DNS resolution failure** — the backend hostname doesn't resolve
4. **Upstream closed the connection before sending headers** — it crashed
   mid-request, or a proxy in between reset it

Also: TLS handshake failure, and a malformed/unparseable response.

**Not** a cause: the backend returning 500. That passes through as 500.

### Q3.8 — When should a proxy return 503 instead of 502?

- **502 Bad Gateway** — "I asked an upstream and it failed." One backend is
  broken; others may be fine.
- **503 Service Unavailable** — "I have no upstream to ask." Every backend is
  ejected, or the pool is empty.

Different operational problems, and the distinction matters at 3am. 502 says
*investigate that backend*; 503 says *everything is down*. Collapsing them into
one code throws away the most useful signal you have.

This project returns 503 when `strategy.Pick` returns nil, and 502 from
`ErrorHandler`.

### Q3.9 — Why can't you change the status code after writing the body?

Because HTTP puts the status line **first** on the wire. The first `Write`
implicitly sends `200 OK` if `WriteHeader` wasn't called, and once those bytes
are gone you can't recall them.

Practical consequences:

- Set all headers **before** `WriteHeader`. Mutations afterwards are silently
  discarded — no error, no panic.
- Calling `WriteHeader` twice: the first wins, and net/http logs
  `superfluous response.WriteHeader call`.
- This is exactly why capturing the status for logging requires **wrapping**
  `ResponseWriter` — there's no way to read it back.

---

# Part 4 — Load balancing

### Q4.1 — Implement weighted round robin. Why is list expansion wrong?

**Smooth WRR (nginx's algorithm)**, per pick:

1. every backend: `current += weight`
2. pick the backend with the highest `current`
3. the winner: `current -= totalWeight`

Winning pushes you deep negative, so you must climb back — and your weight is
your climb rate. After `sum(weights)` picks the state returns exactly to where it
started.

**Why list expansion is wrong.** Expanding `{a:5, b:1, c:1}` to `[a a a a a b c]`
gives the same *totals* but the wrong *order*:

```
naive:   a a a a a b c      ← five consecutive requests slam one backend
smooth:  a a b a c a a      ← a's turns are spread across the cycle
```

A burst of five requests all lands on `a` while `b` and `c` sit idle. It also
allocates a slice proportional to the **sum of weights** — `{1000, 1}` needs a
1001-element slice.

**The testing lesson:** a test that only counts picks passes for *both*
algorithms. You have to assert the exact **order** to pin the behaviour.

### Q4.2 — Least connections reads counters that change mid-scan. Is that a bug?

**No — it's a deliberate trade-off, and I'd defend it.**

Each `InFlight()` is an atomic load, but the scan as a whole isn't a snapshot:
backend 0's count can change while I'm reading backend 3's. So the answer is
already slightly stale by the time I return it.

Making it exact would mean locking the entire pool for the duration of the scan
— which serialises every request in the process against every other one. That
costs far more than the occasional imperfect choice.

**Load balancing is a heuristic.** Being approximately right without contention
beats being exactly right under a global lock. The consequence of a stale read
is one request going to a slightly-less-idle backend; the consequence of a
global lock is a throughput ceiling.

### Q4.3 — All backends are idle. Why does naive least-connections fail?

Because every backend reports 0 in-flight, so a naive `if load < best` scan
always returns **index 0**. All traffic piles onto one backend until it's slow
enough to lose — which is the exact opposite of load balancing, and it's worst
at low traffic when everything is idle.

**Fix:** rotate the scan's starting offset with an atomic counter, and use
strict `<` so an equal load never displaces the incumbent. Ties then round-robin
naturally. I have a test asserting that 300 all-tied picks split exactly
100/100/100.

### Q4.4 — Why is `rand.Int() % n` biased?

Because the generator's range usually isn't an exact multiple of `n`. If the
range is `[0, 2^63)` and `n` doesn't divide it evenly, the low residues occur
once more often than the high ones.

For small `n` the bias is astronomically small — the real reason to use
`rand.IntN(n)` is that it's **correct by construction** and you never have to
work out whether it matters. It becomes genuinely significant when `n` is a
large fraction of the range, and it matters *always* in cryptographic contexts.

Same trap as `rand() % n` in C.

### Q4.5 — Why `math/rand/v2` rather than `math/rand`?

1. **No global lock.** v2's top-level functions use a per-P generator. The v1
   top-level functions were goroutine-safe but via a single global mutex —
   which would have reintroduced exactly the contention that makes Random
   attractive in the first place.
2. **Auto-seeded.** The classic v1 bug was forgetting
   `rand.Seed(time.Now().UnixNano())` and getting the identical "random"
   sequence on every process start.
3. Better API — `IntN` instead of `Intn`, no modulo bias trap.

### Q4.6 — You key a map by pointer. What's the failure mode?

**A slow memory leak nobody notices for six months.**

`WeightedRoundRobin` keys its selection state by `*Backend`. Every backend
removed via the admin API — or temporarily ejected by health checks — leaves an
entry behind for a pointer that will never be seen again. The map grows
monotonically.

Fix: prune entries not present in the current candidate set, guarded by a length
check so the hot path (candidate set unchanged) costs one integer comparison and
zero allocations. There's a test asserting the map shrinks.

The general lesson: **any pointer-keyed cache needs an eviction story.**

### Q4.7 — Why does `Strategy.Pick` take a candidate slice instead of the Pool?

Three reasons:

1. **The health-filtering policy lives in one place** (`Pool.Healthy()`). No
   strategy ever has to know that health checking exists.
2. **Testability** — I can test all four strategies with a plain slice of
   backends and no server, no pool, no HTTP.
3. **Small interfaces.** `Strategy` has two methods and no knowledge of the
   Pool's locking, which means implementing a new strategy is trivial and
   impossible to get subtly wrong.

"The bigger the interface, the weaker the abstraction."

### Q4.8 — Which strategy for which workload?

| Workload | Strategy | Why |
|---|---|---|
| Uniform request cost, homogeneous backends | **Round robin** | Simplest, perfectly even, predictable |
| Heterogeneous hardware (one box is 5× bigger) | **Weighted RR** | Send traffic proportional to capacity |
| **Wildly varying request cost** | **Least connections** | The only one that *measures*. Adapts to GC pauses, slow queries, noisy neighbours |
| Very high RPS, many cores | **Random** | Zero shared state, so no cache-line contention on a shared counter |

For the "varying request cost" case specifically: round robin assumes every
request costs the same, so one slow request behind a backend still gets it the
next request anyway. Least connections notices the backlog and steers away.

---

# Part 5 — Health checking

### Q5.1 — Active vs passive health checking?

- **Passive** — infer health from real traffic failing. Free, but you learn a
  backend is down by *serving somebody a 502*.
- **Active** — probe on a schedule, independent of traffic. Costs a little
  bandwidth, but the dead backend is out of rotation before real traffic reaches
  it.

I measured the difference: with active checking, killing a backend produced
**6/6 requests returning 200**. Without it, roughly one request in three hit the
dead backend and got a 502.

Production systems usually do both — active for fast detection, passive to catch
what probes miss (a backend that answers `/health` but fails real requests).

### Q5.2 — A backend accepts connections but never responds. What happens?

**With a timeout:** the probe is abandoned at the deadline and counts as a
failure. After the threshold the backend is ejected. I have a test for exactly
this.

**Without a timeout:** the probe blocks *forever*. The backend is never marked
unhealthy, so traffic keeps flowing into a black hole — and the checker itself
is now stuck.

**This is worse than a refused connection**, because refusal fails fast and
loudly. A hung backend fails silently and slowly, and it's the case naive
implementations miss.

```go
ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
defer cancel()
```

### Q5.3 — Why *consecutive* failures rather than a failure rate?

**To create hysteresis and prevent flapping.**

A backend that fails every other check is degraded but usable. With a rate-based
rule it would eventually cross the threshold and be ejected. With consecutive
counting, any single success resets the counter to zero — so it stays in
rotation.

Same in the other direction: a dead backend needs M consecutive successes to be
readmitted, and any failure resets that run. Without it, a flapping backend
enters and leaves rotation every tick, which is worse than either state — you
get connection churn, cache invalidation, and unreadable logs.

I clamp both counters at their thresholds, so `status` shows "how far through a
transition are we" rather than counting into the millions for a healthy backend.

### Q5.4 — Why probe backends concurrently?

**Arithmetic.** 20 backends × a 2-second timeout, probed serially, is up to 40
seconds per round. On a 5-second interval the checker falls permanently behind
its own schedule and effectively stops working.

Concurrently, a round takes as long as the *slowest single probe*. I have a test
asserting the wall-clock time for 3 backends at 150ms each is close to 150ms,
not 450ms.

The `sync.WaitGroup` matters too — waiting for the round to finish before
returning stops rounds overlapping and probes piling up.

### Q5.5 — What breaks if you don't drain a response body before closing it?

**Connection reuse.** If you `Close()` an undrained body, net/http can't return
the connection to the idle pool — it has to discard the whole TCP connection,
because there are unread bytes on the wire.

So every probe pays a fresh three-way handshake and the idle pool is never used.
Under load that's a lot of wasted connections and ephemeral ports.

```go
defer resp.Body.Close()
_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
```

**The `LimitReader` is not optional** — without it, a backend that streams
gigabytes from `/health` exhausts your memory.

### Q5.6 — Why not follow redirects on a health check?

Because a 302 is **not** the backend answering the health check.

If you follow it, you might land on an entirely different host that *is*
healthy — and happily keep a broken backend in rotation. The redirect target
could even be a load balancer that routes you to a different instance.

```go
CheckRedirect: func(*http.Request, []*http.Request) error {
    return http.ErrUseLastResponse
}
```

Relatedly: a **404** on the health path must count as unhealthy. If 404 were
tolerated, a misconfigured path would keep a broken backend serving traffic and
you'd find out from users.

### Q5.7 — Why derive the probe's context from the parent?

**To get two behaviours from one line:**

```go
ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)   // parent ctx, not Background
```

1. Cancelling the parent (shutdown) aborts in-flight probes **immediately**
2. A hung backend can't stall the round past `Timeout`

Using `context.Background()` there would break the first: shutdown would have to
wait out every probe's full timeout before the checker could stop.

And `cancel` must be called on every path, or the parent leaks a child context
until *it* is cancelled. `go vet` has a dedicated check for that omission.

---

# Part 6 — Configuration

### Q6.1 — `Port int` and the file says `port: 0`. How do you tell that from absent?

**Short answer:** You can't, from the decoded value alone — both leave the field
at `0`. So you don't try. You **decode on top of pre-populated defaults**:

```go
cfg := Default()          // Port = 8080
dec.Decode(&cfg)          // only assigns keys actually present in the document
```

Keys present in the file overwrite; keys absent keep their default. The ambiguity
never arises.

**Why the common alternative is worse:** "decode into a zero struct, then patch
anything that came out zero" makes it *impossible* to configure a legitimate
zero value. Whether that bites depends on the field, which is exactly the kind of
subtlety you don't want in config handling.

Other options if you truly need to distinguish: pointer fields (`*int`, nil =
absent), or a custom `UnmarshalYAML` that records what it saw.

### Q6.2 — Why must config struct fields be exported?

Because reflection-based decoders **cannot set unexported fields** —
`reflect.Value.CanSet()` returns false for them, and the decoder skips them
silently.

A lowercase field would stay at its zero value forever, with no error and no
warning. It's a genuinely nasty bug to track down because everything *looks*
right.

### Q6.3 — What should happen when a config file has a key your struct doesn't?

**It should fail loudly.** `dec.KnownFields(true)` for YAML,
`dec.DisallowUnknownFields()` for JSON.

Without it, `strategey: least_connections` decodes without complaint, and you run
**round robin** in production while your config file swears it's
least-connections. That's one of the most infuriating classes of config bug
there is, because the file is right there telling you the wrong thing.

With it:

```
line 1: field strategey not found in type config.Config
```

Names the key, names the line. Fails at startup, not in production.

### Q6.4 — What does `errors.Join` give you over returning the first error?

**Every problem in one run instead of one problem per run.**

A user with seven config mistakes shouldn't have to start the binary seven
times. My validation collects into a `[]error` and joins at the end:

```
port: 0 is not in 1..65535
admin_port: 70000 is not in 1..65535
strategy: "round-robin" is not one of [least_connections random round_robin weighted_round_robin]
backends[0]: url "localhost:9001" must start with http:// or https://
backends[2]: url "http://127.0.0.1:9002" duplicates backends[1]
health_check.timeout (5s) must be less than health_check.interval (1s)
health_check.path: "health" must start with /
```

`errors.Join` (Go 1.20+) returns nil when every element is nil, so it doubles as
the success path — no special-casing an empty slice. And `errors.Is`/`As` still
match against any member.

Note the **indices**: `backends[2]` tells you which line to fix. "invalid url"
doesn't.

### Q6.5 — `url.Parse("localhost:9001")` returns no error. What did it parse?

**Scheme = `"localhost"`, Opaque = `"9001"`, Host = `""`.**

It's syntactically a valid URL — `scheme:opaque` — so `url.Parse` is happy.
There's just nothing dialable, and the failure surfaces much later inside the
`Transport` with an error that doesn't point back at your config.

Interesting contrast: `url.Parse("127.0.0.1:9001")` **does** error, because a
scheme can't start with a digit, so it's treated as a path and "first path
segment in URL cannot contain colon".

I actually had this backwards in a comment until a test corrected me. Both cases
now have their own test.

**Lesson:** always validate `Scheme` and `Host` explicitly after parsing.

### Q6.6 — Why validate at load time rather than at point of use?

**Fail fast, and trust thereafter.**

Validating once at the edge means everything downstream can assume the config is
sane, instead of defensively re-checking in fifteen places. It also means a bad
config kills the process at startup — when someone is watching — rather than at
2am on the first request that happens to touch that field.

It's the same principle as parsing over validating: convert untrusted input into
a trusted value once, at the boundary.

---

# Part 7 — Logging and observability

### Q7.1 — You wrap `ResponseWriter` to log status codes. What breaks?

**Streaming. Silently.**

`http.ResponseWriter` is a small interface, but the concrete value net/http hands
you **also** implements `http.Flusher`, `http.Hijacker` and `io.ReaderFrom`.
Code discovers those with a type assertion:

```go
if f, ok := w.(http.Flusher); ok { f.Flush() }
```

The moment you wrap it in your own struct, that assertion checks **your** type
and fails.

For a reverse proxy the consequence is specific: `httputil.ReverseProxy` flushes
streaming responses through `http.Flusher`. Lose it and Server-Sent Events,
streaming JSON and long-polling all stop arriving incrementally — they buffer
until the response ends. Lose `Hijacker` and WebSocket upgrades break outright.

**Why your tests won't catch it:** everything still works perfectly against a
small response. `curl` on a 73-byte body looks identical either way.

**Fix — implement both:**

```go
func (r *rec) Flush() {
    if f, ok := r.ResponseWriter.(http.Flusher); ok { f.Flush() }
}
func (r *rec) Unwrap() http.ResponseWriter { return r.ResponseWriter }
```

### Q7.2 — What is `http.ResponseController` for?

It's the Go 1.20+ answer to exactly the problem above. Instead of type-asserting
against a wrapper that may not implement what you need, it **walks the
`Unwrap()` chain** to find a writer that supports the operation:

```go
rc := http.NewResponseController(w)
rc.Flush()
rc.Hijack()
rc.SetReadDeadline(t)
```

So implementing the single method `Unwrap() http.ResponseWriter` on your wrapper
keeps Flush, Hijack, deadlines — **and any capability net/http adds in future** —
working through it. That's why I implement both `Flush()` (for direct
assertions in older code) and `Unwrap()` (for everything modern).

### Q7.3 — Why must a context key be an unexported custom type?

Because context keys are compared by **interface equality — type *and* value**.

A bare string key like `"requestID"` collides with any other package that had the
same idea. Silently, at runtime, with one package reading another's value. There
is no warning and no way to detect it.

```go
type ctxKey struct{}                      // unexported, zero-size
ctx = context.WithValue(ctx, ctxKey{}, v)
```

An unexported type **cannot be constructed outside its package**, so collision is
impossible by construction rather than by convention.

### Q7.4 — Contexts are immutable. How do you pass data back up?

**Store a pointer, mutate what it points to.**

`context.WithValue` returns a *new* context, so a downstream handler can't add a
value that middleware which already ran will see. But it *can* mutate a struct
the context points at:

```go
type reqInfo struct { id, backend string }

ctx := context.WithValue(r.Context(), ctxKey{}, &reqInfo{id: id})
// ... deeper in the stack:
logging.SetBackend(r.Context(), b.URL.String())
```

That's how the middleware's log line knows which backend the proxy chose, even
though the middleware ran first.

**It's safe without a lock** because net/http handles each request on exactly one
goroutine, so only one goroutine ever touches a given `reqInfo`. I'd note that
explicitly in a review — the safety comes from the concurrency model, not from
luck.

### Q7.5 — A client sends `X-Request-Id: foo\nlevel=ERROR msg="all clear"`. What's the attack?

**Log injection.** The header is attacker-controlled text. Logged verbatim, the
newline lets the client **forge entire log entries**.

They can insert a fake `level=INFO msg="all clear"` line to hide an attack from
whoever reads the logs, or poison a log-parsing alert, or corrupt a SIEM's
ingestion. In JSON-formatted logs they may be able to inject fields that override
real ones downstream.

**Fix:** allowlist safe characters and cap the length.

```go
// keep only [A-Za-z0-9_-], max 64 chars
```

I tested it: sending `ab cd;level=ERROR` logs as `abcdlevelERROR`.

**Why accept the header at all?** Because honouring an inbound request ID is how
a trace survives across service hops — you can grep every service for one ID and
reconstruct the path. The value is real; it just has to be sanitised.

### Q7.6 — Where does the implicit 200 come from, and when does it bite?

If a handler calls `Write` without ever calling `WriteHeader`, net/http sends
`200 OK` automatically before the body.

It bites when you're **wrapping** `ResponseWriter` to record the status: a
body-only response never calls your `WriteHeader`, so you'd log whatever you
initialised the field to. The wrapper has to mirror the behaviour:

```go
func (r *rec) Write(b []byte) (int, error) {
    if !r.wroteHeader { r.WriteHeader(http.StatusOK) }
    ...
}
```

You also need a guard against a **double** `WriteHeader` — the first call is the
one that goes on the wire, so record the first, not the last.

### Q7.7 — What makes a good structured log line for a proxy?

The one field that makes it worth having is **which backend was chosen**. Without
it you can't correlate a slow request to a slow instance.

```
request_id=cb3ec73a method=GET path=/api/thing backend=http://127.0.0.1:9002
status=200 bytes=82 latency_ms=121.963 remote=127.0.0.1:56322
```

Details that matter:

- **One line per request**, at completion — not one at start and one at end
- **Latency as a float in milliseconds**, not a `time.Duration`. A Duration
  renders as `"1.234567ms"`, which an aggregator can't easily turn into
  percentiles
- **Request ID echoed to the client** in a response header, so they can quote it
  in a bug report
- **JSON format available** for aggregators; text for humans

---

# Part 8 — CLI and control plane

### Q8.1 — Your `status` command needs data from another process. Walk through the options.

This is the real design problem of the CLI. `proxyforge status` is a **separate
process** and shares no memory with `proxyforge start`.

| Option | Pros | Cons |
|---|---|---|
| **Loopback HTTP API** ✅ | Pure stdlib, identical on every OS, reuses the HTTP machinery already there, trivially scriptable with curl | Opens a port that must be bound carefully |
| Unix socket / named pipe | No TCP port at all, filesystem permissions | Completely different Windows and Unix code paths; needs a third-party package on Windows |
| Config rewrite + signal reload | No listener | Windows has no usable signals beyond Ctrl+C, and `status` can't work this way at all — there's nothing to read |
| Shared memory / mmap file | Fast | Manual synchronisation, no schema, awful to debug |

I chose the HTTP API. The deciding factor was that I develop on Windows, where
the socket option needs a dependency and the signal option doesn't work at all.

### Q8.2 — Why bind an admin endpoint to loopback specifically?

**Because the endpoint mutates the backend pool with no authentication.**

Bound to `0.0.0.0`, anyone who can reach the box could `POST /backends` and
repoint the proxy at a server they control — a complete traffic interception with
one curl command.

`127.0.0.1` means the kernel refuses connections that don't originate on the
machine. **That bind is the entire security model**, which is why it's explicit
in the code rather than left to a default.

If it ever needed to be remote, the minimum would be a shared token plus TLS, and
I'd still put it behind a firewall rule.

### Q8.3 — What's wrong with `json.NewDecoder(r.Body).Decode(&v)`?

**Unbounded read.** A client can stream gigabytes at that endpoint and the
decoder will faithfully try to buffer all of it — a one-line
memory-exhaustion DoS present on most naive JSON APIs.

```go
dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
dec.DisallowUnknownFields()
```

`MaxBytesReader` caps it and returns a clean error past the limit.
`DisallowUnknownFields` catches typos like `{"wieght": 5}` instead of silently
defaulting the weight to 1.

### Q8.4 — Why `flag.ContinueOnError` instead of the default?

The default is `flag.ExitOnError`, which calls **`os.Exit` out from under you**
on a bad flag.

That skips every deferred function — so cleanup doesn't run — and it makes the
behaviour untestable, because a test that hits it kills the test binary.

`ContinueOnError` returns the error so I can handle it, clean up, and map it to
my own exit code.

Related: **the `flag` package has no concept of a required flag.** Required-ness
is always a manual check.

### Q8.5 — Why stdlib `flag` instead of Cobra?

For a four-command CLI, subcommand dispatch is a `switch` plus one
`flag.NewFlagSet` per command — about 60 lines total, zero dependencies.

Cobra brings three-plus transitive dependencies to buy nicer help text and shell
completion. For a learning project it also **hides the mechanism** I was trying
to understand: that a subcommand is nothing but a name plus its own flag set.

I'd reach for Cobra on a CLI with nested commands, many flags, and real
completion requirements. Not for four verbs.

### Q8.6 — You remove a backend while it's serving 50 requests. What happens?

**Those 50 finish normally.** They were already dispatched and hold a `*Backend`
pointer; removal only takes it out of the pool so **future** selections skip it.

That's the behaviour I want. Severing live requests would turn a routine operator
action into 50 client-visible errors — which teaches operators to avoid the tool.

The CLI says so explicitly after every removal, because it's the kind of
behaviour people assume goes the other way.

The same reasoning applies to health-check ejection: an ejected backend keeps
serving what it already has.

---

# Part 9 — Graceful shutdown

### Q9.1 — Walk through graceful shutdown. `Shutdown` vs `Close`?

**`Shutdown(ctx)`:**
1. Closes the listeners immediately — no **new** connections accepted
2. Closes idle keep-alive connections
3. **Waits** for active requests to complete
4. Returns when they're all done, or when `ctx` expires

**`Close()`:** severs everything at once. Clients mid-response get a truncated
body and a connection reset.

The full sequence in this project:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
<-ctx.Done()
stop()                                  // second Ctrl+C now kills instantly
set.drain(cfg.ShutdownTimeout)          // admin API first, then proxy
cancelChecker(); wg.Wait()              // only now stop health checking
```

**Ordering matters:** admin API first (don't accept pool changes during
shutdown), then the proxy, then the health checker — because draining requests
still need an accurate view of which backends are alive.

`Shutdown` does **not** wait for hijacked connections (WebSockets) or
never-ending long-polls. That's what the timeout is for.

### Q9.2 — You pass the signal-cancelled context to `Shutdown`. What happens?

**This is the classic bug in this area.**

The signal already cancelled that context. So `Shutdown` returns **immediately**
with `context.Canceled`, drains nothing, and severs every in-flight request —
**while your logs cheerfully report a graceful shutdown.** You'd have no idea it
was broken.

The deadline must be derived from a *fresh* context:

```go
ctx, cancel := context.WithTimeout(context.Background(), timeout)   // NOT the cancelled one
defer cancel()
srv.Shutdown(ctx)
```

### Q9.3 — Why should a second Ctrl+C behave differently?

Because an operator watching a 30-second drain needs a way to give up.

If your handler stays installed, every further Ctrl+C is swallowed and appears to
do nothing — which is exactly when people reach for `kill -9`, and a SIGKILL cuts
off *every* remaining request rather than just impatiently ending the wait.

Calling `stop()` as draining begins restores default signal handling, so the
second Ctrl+C terminates immediately. I put that in the log line as a hint:

```
msg="shutdown signal received, draining in-flight requests"
  timeout=30s hint="press Ctrl+C again to exit immediately"
```

### Q9.4 — `Serve` returned an error. Crash or clean shutdown?

**`Serve` always returns a non-nil error** — there's no success return. The one
meaning "Shutdown was called and this is fine" is `http.ErrServerClosed`:

```go
if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
    errCh <- err
}
```

Get this wrong and **every clean shutdown exits non-zero**, which an
orchestrator like Kubernetes reads as a crash — and you get a restart loop from
a service that's shutting down perfectly.

### Q9.5 — 30-second drain timeout, one stuck request. What should happen?

The stuck request should be **abandoned at the deadline** and the process should
exit.

If instead you waited forever, one stuck request means the process never exits.
An orchestrator eventually SIGKILLs it — which severs **every other** in-flight
request too. So refusing to give up on one request costs you all of them.

My `drain` returns a wrapped `context.DeadlineExceeded` with a message saying
in-flight requests were cut off, so it's visible in the exit path rather than
silent. There's a test for it.

### Q9.6 — What does `os.Exit` do to deferred functions?

**Skips them entirely.** No defers run, no cleanup, no flush.

That's why every entry point in this project is structured as:

```go
func main() {
    if err := dispatch(os.Args[1:]); err != nil {
        fmt.Fprintf(os.Stderr, "proxyforge: %v\n", err)
        os.Exit(1)
    }
}
```

Real work returns an `error`; only `main` calls `os.Exit`, after all defers in
the call stack have unwound. It's a small discipline that becomes essential the
moment you have anything to clean up.

Same applies to `log.Fatal`, which calls `os.Exit(1)` internally — worth knowing
before you scatter it through a codebase.

### Q9.7 — Why bind listeners before logging "listening"?

Because `ListenAndServe` binds **inside** the goroutine. So "address already in
use" surfaces *after* you've already printed that you're listening on :8080 —
and it surfaces on an error channel rather than as a return value from startup.

Creating the `net.Listener` up front means a port conflict fails immediately,
before anything claims to be running:

```
proxyforge: binding proxy port: listen tcp 127.0.0.1:8080: bind: Only one usage
of each socket address ... is normally permitted.
```

Bonus: it lets tests bind port **0**, let the OS pick a free port, and read it
back from `Listener.Addr()` — so tests never collide.

---

# Part 10 — Testing

### Q10.1 — What is table-driven testing and why is it the Go idiom?

Test cases are **data** in a slice of anonymous structs, exercised by one loop
body:

```go
tests := []struct{
    name    string
    weights []int
    want    []int
}{
    {name: "5-1-1 spreads the heavy backend", weights: []int{5,1,1}, want: []int{0,0,1,0,2,0,0}},
    {name: "equal weights degenerate to round robin", weights: []int{1,1,1}, want: []int{0,1,2,0,1,2}},
}

for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) { ... })
}
```

Adding a case is adding a **line**, not copy-pasting a function. `t.Run` creates
a named subtest so failures identify the exact case and you can re-run just it
with `-run 'TestX/equal_weights'`.

> This is what you'd reach for `TEST_P`/`INSTANTIATE_TEST_SUITE_P` in GoogleTest
> to get. Go needs no framework — a slice literal and a range loop do it.

### Q10.2 — Why assert the exact order in the weighted round robin test?

**Because totals can't distinguish the two algorithms.**

Naive list expansion and smooth WRR both produce 5:1:1. A test that counts picks
passes for both — including the one that bursts five consecutive requests onto
one backend.

Asserting the sequence `[0,0,1,0,2,0,0]` is what actually pins the behaviour I
care about. It's a good general lesson: **test the property you actually want,
not the one that's easy to measure.**

I also have a test running 100 full cycles and asserting the totals stay exact,
because a subtly wrong implementation (subtracting the wrong total) looks fine
for one cycle and then drifts.

### Q10.3 — Why not assert an even split for the Random strategy?

**Because it would be flaky by construction.** Random is only *statistically*
even; an exact-count assertion would fail a few times a year for no reason,
which is worse than having no test.

I assert the weaker, actually-true properties: every backend gets chosen at
least once over 10,000 trials, and each lands within ±30% of expected. Tight
enough to catch a broken distribution, loose enough never to flake.

**A test that fails randomly gets disabled, and then you have nothing.**

### Q10.4 — How do you test time-dependent behaviour without flakiness?

**Synchronise on channels, never `time.Sleep` and hope.**

The drain test needs the handler to have *actually started* before shutdown
begins:

```go
started := make(chan struct{})
handler := func(w, r) {
    close(started)          // signal exactly when we begin
    time.Sleep(workTime)
    ...
}
<-started                   // exact, not a guess
set.drain(5*time.Second)
```

Sleeping a fixed amount is the single biggest source of flaky CI — it passes on
your laptop and fails on a loaded build box.

Similarly, the health checker tests drive `CheckOnce` **directly** rather than
starting `Run` and waiting for a tick. The test controls the timing entirely.

### Q10.5 — `httptest.Server` vs a mocked `http.Client`?

I used **`httptest.Server`** for the health checker — a real HTTP server on a
real loopback port.

A mocked `http.Client` would not have caught: a missing body drain, a
redirect-following bug, a timeout that never fires, or a connection genuinely
being refused. Those are exactly the failure modes the health checker exists to
handle.

`httptest.NewRecorder` is the right tool for the other case — testing a handler
in isolation with no socket, no port, no cleanup.

Rule of thumb: mock at the boundary of *your* system, not inside the standard
library.

### Q10.6 — What are the testing gotchas you hit?

- **`t.Fatalf` is not safe from a non-test goroutine.** It calls
  `runtime.Goexit` on the wrong goroutine and the test hangs. Use `t.Errorf` +
  `return` inside goroutines.
- **`t.Helper()`** in helpers, or every failure reports at the helper's line
  instead of the caller's and tells you nothing.
- **`t.Cleanup` over `defer`** in a helper — `defer` fires when the *helper*
  returns, not when the test finishes.
- **`t.TempDir()`** gives each test a fresh auto-removed directory: no cleanup
  code, no collisions between parallel tests.
- **JSON numbers decode as `float64`** — asserting `m["status"] == 418` fails;
  it must be `float64(418)`.
- **`-race` needs cgo.** With no C compiler on PATH, `CGO_ENABLED` drops to 0 and
  the race detector refuses to run.

---

# Part 11 — Design and trade-offs

### Q11.1 — Why one third-party dependency and not zero, or twenty?

**Zero wasn't achievable** without writing a YAML parser, which teaches you about
YAML rather than about proxies. The requirement was YAML config, so
`gopkg.in/yaml.v3` earns its place.

**Twenty** would have meant Cobra for the CLI, zap for logging, testify for
assertions — and every one of those hides a mechanism I was trying to learn. The
stdlib alternatives (`flag`, `log/slog`, `t.Errorf`) exist and are good.

The general rule I'd apply on a real team is different: dependencies are fine,
but each one should be **justified against what the stdlib already does**, and
you should know what happens if it's abandoned.

### Q11.2 — How would you make this production-ready?

Roughly in order:

1. **TLS termination** — the most obvious gap
2. **Retries with a circuit breaker** — retry idempotent requests against a
   different backend; trip the breaker so a struggling backend isn't hammered
3. **`/metrics` endpoint** — request counts, latency histograms, ejection
   counters. Logs alone don't give you percentiles
4. **Auth on the admin API** — a shared token at minimum
5. **Rate limiting** — token bucket per client IP
6. **Config hot-reload** — rebuild the pool without dropping traffic
7. **Connection caps per backend** — protect a weak upstream from a thundering
   herd

### Q11.3 — How would you scale this to 100,000 requests/second?

Several angles, and I'd measure before doing any of them:

- **Strategy choice matters at that rate.** Round robin's atomic counter is one
  cache line every core contends over. `Random` has zero shared state and would
  likely win.
- **`GOMAXPROCS` and connection pool sizing** — `MaxIdleConnsPerHost` needs to be
  well above peak concurrency per backend or you thrash connections.
- **Reduce per-request allocations** — the log line's `Attr` slice, the
  `Pool.Healthy()` snapshot copy. That snapshot allocates on every request; at
  100k rps I'd cache it and invalidate on pool change.
- **Horizontal scaling** — one process won't be the right answer; you'd run N
  behind a L4 balancer, which changes least-connections into a per-instance view.

The honest answer is I'd profile first. `pprof` would tell me whether the
bottleneck is allocation, lock contention, or just syscalls.

### Q11.4 — What's the difference between this and nginx or HAProxy?

Scale of ambition, mostly. They have TLS, HTTP/2 and /3, caching, rewriting,
ACLs, connection pooling tuned over decades, and event loops in C.

What's genuinely the same: the core loop of *select a backend, forward, observe
the result, take failing backends out of rotation*. Smooth weighted round robin
is literally nginx's algorithm.

What Go buys is that the concurrency is expressible — goroutine-per-request with
blocking code, instead of a callback-driven event loop. What it costs is
throughput per core and GC pauses.

I'd never suggest running this instead of nginx. I'd suggest it's a good way to
understand what nginx is doing.

### Q11.5 — Why goroutine-per-request rather than an event loop?

Because Go's runtime already does the event loop for you.

A goroutine costs ~2KB of stack initially and grows on demand. When one blocks on
I/O, the runtime parks it and hands the OS thread to another — so a blocking
`time.Sleep(2*time.Second)` in a handler costs a goroutine, not a thread. You can
have hundreds of thousands.

> **C++ contrast:** thread-per-connection with real OS threads caps out in the
> low thousands, which is why C++ servers reach for epoll and callbacks. Go gives
> you the *programming model* of thread-per-connection with the *scaling* of an
> event loop.

The trade-off is you don't control scheduling, and GC pauses are real (though
sub-millisecond in modern Go).

### Q11.6 — What's the single most important thing you learned?

**That concurrency bugs mostly don't announce themselves.**

The round-robin race skewed distribution by 1%. The `ResponseWriter` wrapper
broke streaming but passed every curl test. Passing the cancelled context to
`Shutdown` would have severed every request while logging a successful graceful
shutdown.

None of those three produce an error message. All three would pass code review.
What catches them is tooling that doesn't rely on judgement — `go test -race`,
`go vet` — plus tests that assert the property you actually care about rather
than the one that's easy to check.

---

# Part 12 — Curveballs

### Q12.1 — What happens if a client disconnects mid-request?

The request's `context` is cancelled, and `ReverseProxy` propagates that to the
upstream — so the backend's own `r.Context()` is cancelled and it can stop work.

In this project the in-flight counter still decrements correctly, because
`defer b.Release()` runs on any return path.

What I'd add for a real system: check `r.Context().Err()` before expensive work,
and be aware that a cancelled context on the *client* side doesn't roll back
anything the backend already committed.

### Q12.2 — Two `add-backend` commands run at exactly the same moment with the same URL. What happens?

One succeeds, one gets a 409 Conflict.

`Pool.Add` takes the write lock, scans for a duplicate URL, and appends — all
under the same lock acquisition. So the check and the insert are atomic with
respect to each other. Whichever goroutine gets the lock second sees the first
one's entry.

If the check and the insert were separate lock acquisitions, that would be a
classic TOCTOU race and both could be admitted.

### Q12.3 — A backend is healthy for `/health` but returns 500 for every real request. What does your system do?

**Nothing — and that's a real limitation.** Active health checking only knows
what the probe tells it, and the probe says the backend is fine.

This is the argument for **passive** health checking alongside active: track the
error rate of real traffic per backend and eject on that too. I'd implement it
with `ModifyResponse` (not `ErrorHandler`, since a 500 is a successful proxy
operation) feeding a sliding-window error rate into the same state machine.

The deeper lesson is that a health endpoint should exercise the same
dependencies real requests use, or it's just checking that the process is
running.

### Q12.4 — Your proxy is returning 502s intermittently. How do you debug it?

1. **Check `proxyforge status`** — is one backend flapping? `FAILS` and
   `PASSES` show where it is in the state machine.
2. **Grep the logs by backend** — `backend=http://...:9002 status=502`. If it's
   one backend, it's that instance. If it's spread evenly, it's the network or
   something shared.
3. **Read the `proxy error` lines** — they carry the underlying error, which
   distinguishes connection-refused from timeout from DNS.
4. **Correlate the request ID** with the backend's own logs.
5. **Check whether it correlates with health-check transitions** — an
   intermittent 502 right before an ejection means the health interval is too
   long relative to how fast the backend fails.

The reason each of those is possible is that the log line carries the chosen
backend and a request ID. Without those two fields you're guessing.

### Q12.5 — How would you add sticky sessions?

Hash a stable client attribute to a backend, so the same client keeps landing on
the same instance.

I'd implement it as another `Strategy` — the interface already supports it,
which is the payoff of having one. The naive version is `hash(clientIP) % len(candidates)`,
but that **remaps almost every client whenever the pool size changes**, which
defeats the purpose during a deploy.

The right answer is **consistent hashing** with virtual nodes: adding or removing
one backend remaps only ~1/N of clients instead of nearly all of them.

Caveat worth raising: stickiness fights load balancing. A backend with unlucky
hash assignments gets more traffic, and you lose the ability to drain a backend
smoothly.

### Q12.6 — Why does this need a mutex at all — can't you just use channels?

You could, and the classic Go advice is "share memory by communicating".

The channel version would make the pool owned by a single goroutine, with all
reads and writes going through a request channel. That's genuinely clean and
removes locks entirely.

**Why I didn't:** the pool is read on *every single request* and written rarely.
Funnelling every read through one goroutine makes that goroutine a serialisation
point for the whole process — strictly worse than an `RWMutex` that lets all
readers proceed in parallel.

The Go proverb people forget is the second half of it: **"Don't communicate by
sharing memory; share memory by communicating"** is guidance, not a law, and the
sync package exists for exactly the cases where a mutex is the better fit. A
read-heavy shared data structure is the textbook example.

---

## How to prepare with this

1. **Read [SUMMARY.md](SUMMARY.md) §8** — the six hard problems. Those generate
   the best questions and the best answers.
2. **Be able to draw the request flow** from [README.md](README.md) on a
   whiteboard.
3. **Know your numbers**: 2,400 LOC, 54 tests, 4 strategies, 1 dependency, ~1ms
   overhead, zero 502s during a backend outage.
4. **Practise Q2.1, Q7.1 and Q9.2 out loud.** The race, the `Flusher` trap and
   the cancelled-context bug are the three stories that show you understand
   concurrency rather than just having used it.
5. **Have an honest answer ready for "what would you do differently"** (Q0.4).
   Saying "nothing" reads as not having reflected.
