# ProxyForge — Objectives & Lessons

Why this project existed, whether it achieved that, and what to actually take
away from it.

If [SUMMARY.md](SUMMARY.md) is *what was built* and [QA.md](QA.md) is *how to
talk about it*, this is *what it was for*.

---

## Part 1 — Objectives

### The stated goal

> "My PRIMARY goal is to UNDERSTAND the technology, not to end up with a
> finished repo. Working code is the side effect; my learning is the
> deliverable."

That framing shaped every decision in the project. It's why the code carries
comments explaining *why* rather than *what*, why every design decision is
written down beside the alternative it beat, and why a mistake I made mid-build
is recorded rather than quietly fixed.

### The concrete objectives

| # | Objective | Why it was chosen |
|---|---|---|
| 1 | **See real concurrency, not toy concurrency** | Goroutines, mutexes and atomics are easy to *read about* and hard to *get right*. A proxy makes concurrency inherent — every request is a goroutine, the backend pool is shared mutable state |
| 2 | **Understand `net/http` from the inside** | Most backend work is HTTP. Knowing what `ReverseProxy` does — hop-by-hop headers, streaming, connection pooling — is the difference between using a framework and understanding one |
| 3 | **Stdlib first, justify every dependency** | Reaching for a library is how you skip the learning. Constraint forces engagement |
| 4 | **Build something with a real lifecycle** | Startup, steady state, failure, recovery, shutdown. Most tutorial projects only have the middle bit |
| 5 | **Learn Go by contrast with C++** | Mapping new concepts onto ones you already own is the fastest way to learn. It also exposes where the mental model *doesn't* transfer |
| 6 | **Produce something defensible in an interview** | Not a portfolio piece — a thing you can be interrogated about |

### Why a load balancer specifically

It's the smallest project that forces you to meet all of these at once:

- **A shared counter** → the data race
- **A backend pool mutated while being read** → mutexes, snapshots, lock ordering
- **Background health checking** → goroutine lifecycle, `context`, `select`
- **A CLI talking to a running process** → process boundaries, IPC design
- **In-flight requests during shutdown** → draining, signal handling
- **Multiple interchangeable algorithms** → interfaces that earn their existence

You couldn't hit those with a CRUD API. That's why this shape was chosen.

---

## Part 2 — Were the objectives met?

An honest scorecard.

| Objective | Met? | Evidence |
|---|---|---|
| Real concurrency | ✅ Fully | Data race found *and proven* with measured skew; lock ordering documented; goroutine lifecycle tested |
| `net/http` internals | ✅ Fully | Hop-by-hop headers, `Rewrite` vs `Director`, `Transport` pooling, the `Flusher` trap |
| Stdlib first | ✅ Fully | One dependency, and its justification is written down |
| Real lifecycle | ✅ Fully | Startup validation, health transitions, drain on shutdown — all verified against a live process |
| Go via C++ contrast | ✅ Fully | Contrasts throughout the code comments and notes |
| Interview-defensible | ⚠️ **Depends on you** | The material exists. Whether *you* can defend it depends on Part 6 below |

That last row is the honest one. A repo can be complete without the
understanding having transferred. See [Part 6](#part-6--how-to-actually-convert-this-into-learning).

---

## Part 3 — The lessons that transfer anywhere

These are language-independent. They'd be just as true in C++, Java or Rust.

### 3.1 — Bugs that look almost right are the dangerous ones

The naive round-robin counter distributed **2502 / 2489 / 2494 / 2515** requests
instead of exactly 2500 each. A 1% skew reads as noise. It passes casual
testing, survives code review, and ships.

Compare that to a bug that crashes. A crash is a *gift* — it tells you
immediately, loudly, with a stack trace.

> **The lesson:** rank bugs by how quietly they fail, not by how bad the outcome
> is. The quiet ones need tooling, not attention, because attention doesn't
> scale and you will get tired.

### 3.2 — "It worked when I ran it" is not evidence

For anything concurrent, a passing run tells you almost nothing. Whether a race
manifests depends on scheduling, core count, cache timing and load.

`go test -race` found the counter bug in milliseconds. Without it, that test
passed roughly 99% of the time — **with a broken implementation**.

> **The lesson:** for concurrency, "I tested it" means "I ran it under a race
> detector". The claim you can make is only as strong as the instrument you
> used.

### 3.3 — Prove your claims, don't assert them

Three times in this project a written claim turned out to be wrong:

- I wrote that `url.Parse("127.0.0.1:9001")` parses successfully. It doesn't —
  a test corrected me. (`localhost:9001` is the one that does.)
- I wrote that curl would send a header containing a newline. It refuses.
- I claimed the race detector needs no setup. On Windows it silently disables
  itself without a C compiler.

A fourth happened while writing *this very document*: I predicted that making
`Pool.All()` return the internal slice would fail the test suite. It doesn't —
see break 4 in [Part 6](#step-2--break-things-deliberately). I only found out
because I ran all six breaks instead of reasoning about them.

> **The lesson:** the gap between "I'm confident this is how it works" and "I
> watched it work" is where most wrong documentation lives. Run it.

That applies hardest to the claims you feel *most* sure about — those are the
ones you don't think to check.

### 3.4 — Make the failure mode visible in the design

Compare two error messages for the same mistake:

```
invalid url
```
```
backends[2]: url "http://127.0.0.1:9002" duplicates backends[1]
```

The second names the field, the index, the value, and the conflict. Someone can
fix it without reading your code.

Same principle in `errors.Join` reporting seven config problems in one run
rather than one per run, and in `ErrNotRunning` turning
`dial tcp 127.0.0.1:9090: connectex: ...` into
`proxyforge is not running (start it with proxyforge start)`.

> **The lesson:** error messages are a user interface. The person reading them
> is usually you, at 3am, with less context than you have now.

### 3.5 — Silent acceptance of bad input is a bug

`strategey: least_connections` in a config file. Without `KnownFields(true)`, it
loads fine and you run **round robin** in production while the file swears
otherwise.

The same principle appears three more times: rejecting unknown JSON fields,
rejecting a malformed `?delay=` instead of ignoring it, and rejecting a 404 on
the health path instead of tolerating it.

> **The lesson:** when input doesn't make sense, fail. Guessing what the user
> meant produces systems that lie to you.

### 3.6 — Distinguish failure modes; don't collapse them

502 vs 503. Duplicate (409) vs not-found (404) vs malformed (400). Exit code 1
vs 2. Timeout vs connection-refused in the probe log.

Each distinction costs a few lines and buys a diagnosis. Collapsing them throws
away the most useful signal you have precisely when you need it.

### 3.7 — Say what you deliberately didn't do

[SUMMARY.md §10](SUMMARY.md) lists what ProxyForge doesn't do: no TLS, no
retries, no auth, no metrics. That section makes the project *more* credible,
not less.

> **The lesson:** stating scope reads as judgement. Leaving it implicit reads as
> an oversight. In an interview, "I deliberately didn't build X because Y" is a
> strong answer; being surprised by the question is not.

### 3.8 — Small, honest increments beat one big commit

Ten phases, one commit each, each with tests passing before the commit. The git
history reads as a build log — you can see exactly when the pool arrived, when
health checking arrived, and what each cost.

> **The lesson:** commit history is documentation you get for free if you're
> disciplined, and lose forever if you're not.

---

## Part 4 — The Go-specific lessons

### 4.1 — The compiler is a design tool, not an obstacle

Go has **no forward declarations**, so an import cycle is a hard error with no
escape hatch. When `backend` needed the shared `Transport` but couldn't import
`proxy`, the compiler wasn't in the way — it was telling me the boundary was
wrong. Moving `Transport` produced a better layout.

Same with unused imports and variables being errors rather than warnings. It
feels hostile for about a week, then you never think about it again.

### 4.2 — `defer` is not RAII, and the difference matters

It's the closest thing Go has, and it does run during a panic unwind. But:

- It's **function**-scoped, not block-scoped. `defer` in a loop accumulates.
- Arguments are evaluated **immediately**, not at execution.
- **`os.Exit` skips it entirely** — which is why every entry point here returns
  an `error` and only `main` exits.

### 4.3 — Interfaces are discovered, not declared

`*proxy.Server` becomes an `http.Handler` purely by having `ServeHTTP`. No
inheritance, no registration, no vtable pointer in the struct. The compiler
checks at the point of *use*.

The practical consequence: **keep interfaces small**. `Strategy` has two methods
and knows nothing about health, pools or HTTP — which is exactly why all four
implementations are testable without starting a server.

### 4.4 — Concurrency primitives are a menu, not a default

| | Use when |
|---|---|
| `atomic` | One word — a counter, a flag |
| `Mutex` | Multiple fields that must change together |
| `RWMutex` | Reads vastly outnumber writes *and* the critical section is long enough to pay for the bookkeeping |
| Channels | Ownership transfer, signalling, pipelines |

The mistake to avoid is picking one reflexively. `RWMutex` is *slower* than
`Mutex` when every acquisition is a write — which is why `WeightedRoundRobin`
uses a plain `Mutex`.

And "share memory by communicating" is guidance, not law. A read-heavy shared
structure is exactly where a mutex wins.

### 4.5 — The standard library is unusually complete

`log/slog` replaced zap. `flag` replaced Cobra. `t.Errorf` replaced testify.
`httptest` replaced a mocking framework. `errors.Join` replaced a
multierror package. `math/rand/v2` fixed the seeding and bias traps.

> **The lesson:** check the stdlib before adding a dependency. In Go the answer
> is "it's already there" more often than in most ecosystems.

### 4.6 — Zero values are a design surface

`&RoundRobin{}` works because its zero value is meaningful.
`WeightedRoundRobin` needs a constructor because a nil map panics on write.

Making the zero value useful is a Go design habit worth copying — but it also
creates the config trap where `port: 0` and an omitted `port` are
indistinguishable. Both sides of that coin are worth knowing.

### 4.7 — Language semantics can be version-gated

The Go 1.22 loop-variable change is selected by **the `go` directive in
`go.mod`**, not by your installed toolchain. A module saying `go 1.21` built
with Go 1.27 still gets the old capture behaviour.

That's how Go shipped a breaking change without breaking anyone — and it means
"which Go am I running" is the wrong question. The right one is "what does
go.mod say".

---

## Part 5 — The systems and networking lessons

### 5.1 — A proxy is mostly about what it *removes*

Hop-by-hop headers (`Connection`, `Keep-Alive`, `Transfer-Encoding`, `Upgrade`)
apply to a single transport connection, not end-to-end. Forwarding them corrupts
the next hop.

And a client-supplied `X-Forwarded-For` must be **replaced**, not appended —
otherwise anything downstream trusting it believes whatever IP the client
claimed, breaking allowlists, rate limits and audit logs.

### 5.2 — Timeouts are not optional; they're the design

`http.ListenAndServe` uses a zero-value `Server` with **no timeouts at all** —
one slow client holds a goroutine indefinitely (Slowloris).

A health check with no timeout never ejects a *hung* backend, so traffic flows
into a black hole forever. A hung backend is worse than a refused one, because
refusal fails fast and loudly.

> **The lesson:** every network operation needs a deadline. The default is
> almost always "wait forever", and "forever" is a bug.

### 5.3 — Detection latency is a design parameter

`fail_threshold × interval` is how long a dead backend keeps receiving traffic.
At 3 × 5s that's a 15-second window of 502s.

Make it shorter and you flap on transient blips. Make it longer and outages last
longer. There is no correct answer, only a trade-off you should make
deliberately — and hysteresis (consecutive counts, reset on any opposite result)
is what lets you pick aggressive thresholds without flapping.

### 5.4 — Graceful shutdown is a correctness property

Stop accepting new connections **immediately**; let existing work **finish**.
Get it backwards — or pass the already-cancelled context to `Shutdown` — and
every in-flight request is severed while your logs report success.

And one stuck request must not block shutdown forever, because an orchestrator
will eventually SIGKILL you and cut off *everything else* too.

### 5.5 — Approximately right without contention beats exactly right under a lock

Least-connections reads counters that change mid-scan, so the answer is stale by
the time it returns. Making it exact would require locking the whole pool —
serialising every request in the process.

> **The lesson:** in a hot path, know which inaccuracies are acceptable. Load
> balancing is a heuristic; a throughput ceiling is not a heuristic.

### 5.6 — Security is often one line, in the right place

The admin API mutates the backend pool with **no authentication**. Bound to
`0.0.0.0`, anyone who can reach the box could repoint the proxy at a server they
control — full traffic interception with one curl.

`127.0.0.1` is the entire defence. That's why it's explicit in the code with a
comment saying so, rather than left to a default.

Same category: `MaxBytesReader` on a JSON endpoint, and sanitising a
client-controlled header before logging it.

---

## Part 6 — How to actually convert this into learning

Worth being direct about this, because it's the objective that isn't automatic.

The repo is complete. That doesn't mean the understanding has transferred —
reading code produces *recognition*, which feels like understanding and isn't.
The test is whether you can reproduce it cold.

Here is what I'd actually do, in order.

### Step 1 — Read the spine, in this order

```
internal/backend/backend.go      the concurrency contract: what's atomic,
                                 what's mutex-guarded, what's immutable
internal/backend/pool.go         why All() copies the slice but shares pointers
internal/balancer/roundrobin.go  read the comment on the `n` field twice
internal/proxy/proxy.go          only 68 lines — the whole request path
internal/health/checker.go       select, context, ticker, WaitGroup
```

Don't read the rest yet. Those five files are 90% of the ideas.

### Step 2 — Break things deliberately

This is where the learning actually happens. For each one, **predict what
happens before you run it**, then run it.

The outcomes below were measured, not guessed — I ran all six against a copy of
the repo.

| # | Break this | What actually happens |
|---|---|---|
| 1 | Change `atomic.Uint64` to a plain `uint64` in `roundrobin.go` | `go test` **passes**. `go test -race` → `WARNING: DATA RACE` |
| 2 | Delete the `Flush()` method from `responseRecorder` | **FAILS**: *"wrapped ResponseWriter no longer satisfies http.Flusher; streaming responses would buffer"* |
| 3 | In `drain()`, derive the deadline from an already-cancelled context instead of `context.Background()` | **FAILS**: *"drain timed out after 5s, in-flight requests were cut off: context canceled"* |
| 4 | Make `Pool.All()` return `p.backends` directly instead of a copy | **Everything passes.** See below |
| 5 | Remove `defer ticker.Stop()` in `checker.go` | **Everything passes** |
| 6 | Remove the body drain in `probe()` | **Everything passes** |

**Breaks 1, 4, 5 and 6 are the important ones — four out of six fail silently.**

**Break 1** is the cleanest demonstration in the whole project: a genuinely
broken concurrent implementation that a normal test run declares healthy. Only
the race detector disagrees.

**Break 4** is subtler and worth working through properly. `TestPoolAllReturnsCopy`
still passes, because a slice header is a *value*: the caller's copy of
`(pointer, len, cap)` still says `len == 1` even after `Add` appends. The test
checks the length, and the length is fine.

The bug is real anyway — `Remove` shuffles elements **in place** in the backing
array, so a goroutine iterating an old header sees mutated data. I proved it by
writing a test that iterates `All()` while `Remove` runs concurrently:

```
broken All()  →  WARNING: DATA RACE
correct All() →  ok
```

> **The lesson from break 4:** `-race` only finds races on code paths that
> actually execute. It is only as good as your test coverage. Both halves of
> that sentence matter.

**Breaks 5 and 6** never fail any test, ever. A leaked ticker and an undrained
body are real defects — a runtime timer that lives forever, and a TCP connection
discarded on every probe — but nothing observable in a test suite changes. If
you can explain *why each is a bug* without a failing test to point at, you've
understood something tests cannot teach you.

### Step 3 — Rebuild one piece from scratch

Delete `internal/balancer/weighted.go` and write it again from the algorithm
description alone:

> per pick: every backend's `current += weight`; pick the max; the winner does
> `current -= totalWeight`

The tests are already there and they assert the exact sequence, so you'll know
immediately whether you got it right. This is the single highest-value hour in
the whole list.

### Step 4 — Answer the questions cold

[QA.md](QA.md) has 95 questions with answers. **Cover the answers.** Say yours
out loud, then compare.

Start with the three flagged at the end of that file — Q2.1, Q7.1, Q9.2. If you
can tell those three stories fluently, you can hold a conversation about
concurrency.

### Step 5 — Extend it

Pick one and build it yourself:

- **Retries with a circuit breaker** — teaches idempotency and state machines
- **`/metrics` endpoint** — teaches instrumentation without a library
- **Sticky sessions via consistent hashing** — teaches why naive `hash % N` is
  wrong, and slots into the existing `Strategy` interface
- **Config hot-reload** — teaches swapping live state safely

Extending someone else's design is how you find out whether you understood it.

---

## Part 7 — Self-assessment

You've internalised this project if you can do these **without looking**:

**Concurrency**
- [ ] Explain why `counter++` is three operations and what specifically goes wrong
- [ ] Say why a 1% distribution skew is more dangerous than a crash
- [ ] Choose between `atomic`, `Mutex` and `RWMutex` and defend the choice
- [ ] Explain why `Pool.All()` copies the slice but shares the pointers
- [ ] State the lock ordering rule and describe the deadlock it prevents
- [ ] Explain why closing a channel wakes every waiter
- [ ] Say who is responsible for stopping a goroutine, and how

**HTTP and proxying**
- [ ] Name four distinct causes of a 502 — and one thing that *isn't*
- [ ] Explain hop-by-hop headers and why a proxy strips them
- [ ] Say why `X-Forwarded-For` must be replaced rather than appended
- [ ] Explain why one shared `Transport` is mandatory
- [ ] Say when 503 is correct instead of 502

**The three traps**
- [ ] Explain why wrapping `ResponseWriter` breaks streaming, and why tests miss it
- [ ] Explain what happens if you pass the cancelled context to `Shutdown`
- [ ] Explain the pre-1.22 loop variable bug and how you know which semantics you get

**Design**
- [ ] Justify the loopback HTTP admin API over two alternatives
- [ ] Explain why smooth WRR beats list expansion despite identical totals
- [ ] Explain why consecutive failure counts, not a failure rate
- [ ] Say why `Strategy.Pick` takes a slice rather than the Pool

**Honesty**
- [ ] State three things this project deliberately doesn't do, and why
- [ ] Give a real answer to "what would you do differently"

If you can tick most of those, the objective was met. If you can't tick the
three traps, go back to Step 2 — those are the ones that separate having read
about concurrency from having understood it.

---

## In one line

The point of ProxyForge was never the proxy. It was to meet — in a setting small
enough to hold in your head — the specific class of bug that doesn't announce
itself, and to learn the habits that catch it anyway.
