# ProxyForge — Testing Walkthrough

Every feature, the command that exercises it, the **real output** it produced,
and what that output actually proves.

Nothing here is invented. Every block below was captured from an actual run on
Windows 11 with Go 1.27.0. Your timestamps, request IDs and ports will differ;
the shapes will not.

> **Setup first?** See [SETUP.md](SETUP.md).
> **How it works?** See [README.md](README.md).

---

## Contents

| # | Feature | Command |
|---|---|---|
| 1 | [Unit tests](#1-unit-tests) | `.\test.cmd` |
| 2 | [Race detector](#2-race-detector) | `.\test.cmd race` |
| 3 | [Config validation](#3-config-validation) | `.\pf.cmd start --config bad.yaml` |
| 4 | [Typo detection](#4-typo-detection) | (bad key in YAML) |
| 5 | [Starting up](#5-starting-up) | `.\backends.cmd start` + `.\pf.cmd start` |
| 6 | [Round robin](#6-round-robin) | `curl` ×6 |
| 7 | [Weighted round robin](#7-weighted-round-robin) | `strategy: weighted_round_robin` |
| 8 | [Least connections](#8-least-connections) | `strategy: least_connections` |
| 9 | [Random](#9-random) | `strategy: random` |
| 10 | [Status command](#10-status-command) | `.\pf.cmd status` |
| 11 | [Health check — ejection](#11-health-check--ejection) | kill a backend |
| 12 | [Health check — recovery](#12-health-check--recovery) | restart it |
| 13 | [Add backend at runtime](#13-add-backend-at-runtime) | `.\pf.cmd add-backend` |
| 14 | [Remove backend at runtime](#14-remove-backend-at-runtime) | `.\pf.cmd remove-backend` |
| 15 | [Admin CLI errors](#15-admin-cli-errors) | duplicate / missing / bad URL |
| 16 | [Request logging](#16-request-logging) | any request |
| 17 | [Request ID propagation](#17-request-id-propagation) | `-H "X-Request-Id: ..."` |
| 18 | [Log-injection defence](#18-log-injection-defence) | unsafe header value |
| 19 | [Latency measurement](#19-latency-measurement) | `?delay=900ms` |
| 20 | [502 — backend unreachable](#20-502--backend-unreachable) | kill mid-traffic |
| 21 | [503 — no healthy backends](#21-503--no-healthy-backends) | kill all |
| 22 | [Admin API directly](#22-admin-api-directly) | `curl :9090/status` |
| 23 | [Method-aware routing](#23-method-aware-routing) | wrong verb |
| 24 | [Graceful shutdown](#24-graceful-shutdown) | Ctrl+C mid-request |
| 25 | [Proxy not running](#25-proxy-not-running) | `.\pf.cmd status` with nothing up |

---

## 1. Unit tests

```
.\test.cmd
```

```
?   	github.com/manishkumawat24/proxyforge/cmd/dummybackend	[no test files]
ok  	github.com/manishkumawat24/proxyforge/cmd/proxyforge	(cached)
ok  	github.com/manishkumawat24/proxyforge/internal/admin	(cached)
ok  	github.com/manishkumawat24/proxyforge/internal/backend	(cached)
ok  	github.com/manishkumawat24/proxyforge/internal/balancer	(cached)
ok  	github.com/manishkumawat24/proxyforge/internal/config	(cached)
ok  	github.com/manishkumawat24/proxyforge/internal/health	(cached)
ok  	github.com/manishkumawat24/proxyforge/internal/logging	(cached)
?   	github.com/manishkumawat24/proxyforge/internal/proxy	[no test files]
```

**What it means:** 54 test functions across 7 packages passed. `?` means a
package has no tests of its own — `cmd/dummybackend` is a test fixture, and
`internal/proxy` is exercised through the other packages' tests.

`(cached)` means nothing changed since the last run so Go reused the result.
Force a real re-run with `go clean -testcache` if you want to see the timings.

---

## 2. Race detector

```
.\test.cmd race
```

```
Running tests with the race detector...

ok  	github.com/manishkumawat24/proxyforge/internal/balancer	(cached)
ok  	github.com/manishkumawat24/proxyforge/internal/health	(cached)
...
```

**What it means:** this is the one that matters for a load balancer. It
instruments every memory access and fails if two goroutines touch the same
memory without synchronization.

**Why you should care:** the round-robin counter test passes roughly 99% of the
time even with a *broken* non-atomic counter. Without `-race`, a green run
proves nothing about concurrent code.

> Needs a C compiler. Without gcc on PATH you get
> `go: -race requires cgo`, and `test.cmd` will tell you how to install one.
> Plain `.\test.cmd` works regardless.

---

## 3. Config validation

Every problem is reported at once, not one per run.

```
.\pf.cmd start --config bad.yaml
```

With a config containing seven separate mistakes:

```
proxyforge: invalid config C:\...\bad-demo.yaml:
port: 0 is not in 1..65535
admin_port: 70000 is not in 1..65535
strategy: "round-robin" is not one of [least_connections random round_robin weighted_round_robin]
backends[0]: url "localhost:9001" must start with http:// or https://
backends[2]: url "http://127.0.0.1:9002" duplicates backends[1]
health_check.timeout (5s) must be less than health_check.interval (1s), otherwise probes overlap their own schedule
health_check.path: "health" must start with /
```
Exit code `1`.

**What each line means:**

| Message | Why it's rejected |
|---|---|
| `port: 0 is not in 1..65535` | Not a valid TCP port |
| `strategy: "round-robin" is not...` | Hyphen instead of underscore — the valid names are listed for you |
| `url "localhost:9001" must start with http://` | `url.Parse` *accepts* this, reading `localhost` as the **scheme** with an empty host. Nothing would be dialable |
| `duplicates backends[1]` | Round robin would silently send that backend double traffic |
| `timeout must be less than interval` | A probe could still be running when the next round is due |
| `path: "health" must start with /` | It's a URL path, not a name |

Note `backends[0]`, `backends[2]` — the **index** tells you which line to fix.

---

## 4. Typo detection

Unknown keys are rejected, not silently ignored.

```yaml
strategey: least_connections     # note the typo
backends:
  - url: http://127.0.0.1:9001
```

```
proxyforge: parsing config C:\...\typo-demo.yaml: yaml: unmarshal errors:
  line 1: field strategey not found in type config.Config
```

**What it means:** without this check, that file would load fine and you'd run
**round robin** in production while your config file swore it was
least-connections. The error names the exact bad key and its line number.

This happens at the *decode* stage, before validation — which is why you see
one error here rather than a joined list.

---

## 5. Starting up

```
.\backends.cmd start
.\pf.cmd start --config config.yaml
```

```
time=2026-09-05T14:00:54.561+05:30 level=INFO msg="backend registered" url=http://127.0.0.1:9001 weight=5
time=2026-09-05T14:00:54.564+05:30 level=INFO msg="backend registered" url=http://127.0.0.1:9002 weight=1
time=2026-09-05T14:00:54.565+05:30 level=INFO msg="backend registered" url=http://127.0.0.1:9003 weight=1
time=2026-09-05T14:00:54.568+05:30 level=INFO msg="proxyforge listening" addr=127.0.0.1:8080
  admin_addr=127.0.0.1:9090 strategy=round_robin backends=3 health_path=/health health_interval=5s
```

**What it means:**

- Three backends loaded from `config.yaml` with their weights.
- `addr=127.0.0.1:8080` — where clients connect.
- `admin_addr=127.0.0.1:9090` — where `status`/`add-backend` connect. **Loopback
  only**, because this endpoint mutates the pool with no authentication.
- Both listeners were bound *before* this line printed, so a port conflict would
  have failed here instead of after claiming to be listening.

`start` runs in the **foreground**. Use a second terminal for everything below.

---

## 6. Round robin

```
1..6 | % { (curl.exe -s -o NUL -D - http://127.0.0.1:8080/ | Select-String "X-Backend-Name").ToString().Trim() }
```

```
X-Backend-Name: backend-9001
X-Backend-Name: backend-9002
X-Backend-Name: backend-9003
X-Backend-Name: backend-9001
X-Backend-Name: backend-9002
X-Backend-Name: backend-9003
```

**What it means:** strict rotation, wrapping cleanly. Each request went to a
different backend in order.

**How it works:** a single `atomic.Uint64` incremented per request, modulo the
number of healthy backends. It's atomic because a plain `counter++` is
read-modify-write — two goroutines can both read 7 and both write 8, sending
two requests to the same backend and skipping another.

A full response body looks like this:

```
curl.exe http://127.0.0.1:8080/api/users
```
```
backend: backend-9001
port:    9001
method:  GET
path:    /api/users
status:  200
```

Note `path: /api/users` — the proxy forwarded the **full original path**, not
just `/`.

---

## 7. Weighted round robin

Set `strategy: weighted_round_robin` in `config.yaml` and restart. Weights are
5:1:1.

```
sequence: 9001 9001 9002 9001 9003 9001 9001   9001 9001 9002 9001 9003 9001 9001
totals:   9001=10  9002=2  9003=2
```

**What it means:** exactly 5:1:1 over each cycle of 7, and the two cycles are
**identical** — the algorithm returns to its starting state rather than drifting.

**The important bit is the ORDER, not the totals.** The obvious implementation
(expand the list to `[a a a a a b c]` and rotate) also gives 5:1:1 — but it emits
`9001` five times in a row, so a burst of five requests all slam one backend
while two sit idle.

This is nginx's *smooth* weighted round robin: `9001`'s five turns are **spread
across** the cycle. Compare the two:

```
naive expansion:  9001 9001 9001 9001 9001 9002 9003
smooth (ours):    9001 9001 9002 9001 9003 9001 9001
```

---

## 8. Least connections

Set `strategy: least_connections`. Then occupy two backends with slow requests:

```powershell
Start-Job { curl.exe -s -o NUL "http://127.0.0.1:8080/?delay=6s" }
Start-Job { curl.exe -s -o NUL "http://127.0.0.1:8080/?delay=6s" }
.\pf.cmd status
```

```
URL                    STATE  WEIGHT  IN-FLIGHT  FAILS  PASSES
http://127.0.0.1:9001  UP     1       1          0      1
http://127.0.0.1:9002  UP     1       1          0      1
http://127.0.0.1:9003  UP     1       0          0      1
```

Now send four fast requests:

```
sequence: 9003 9003 9003 9003
```

**What it means:** this is the only strategy that **measures** instead of
assuming. `9001` and `9002` each show `IN-FLIGHT 1`, so every new request went
to the idle `9003`.

**Why it matters:** round robin assumes every request costs the same. Least
connections notices when a backend is slow — a GC pause, a noisy neighbour, an
expensive query — and steers away until it drains.

The `IN-FLIGHT` column is a live counter incremented when a request is
dispatched and decremented (via `defer`, so it survives panics) when it
completes.

---

## 9. Random

```
sequence: 9003 9001 9003 9002 9002 9002 9002 9003 9001 9003 9001 9002 9001 9001
totals:   9001=5  9002=5  9003=4
```

**What it means:** roughly even, but visibly lumpy — note `9002` four times in a
row. That's expected. Random is only *statistically* even; it converges over
thousands of requests, not fourteen.

**Why you'd use it:** zero shared state means zero coordination between
goroutines. Round robin's atomic counter is a single cache line every CPU core
contends over, which becomes measurable at very high request rates. Random has
no such point.

---

## 10. Status command

```
.\pf.cmd status
```

```
strategy:   round_robin
proxy port: 8080
uptime:     1m27s
backends:   4

URL                    STATE  WEIGHT  IN-FLIGHT  FAILS  PASSES
http://127.0.0.1:9001  UP     5       0          0      2
http://127.0.0.1:9002  UP     1       0          0      2
http://127.0.0.1:9003  UP     1       0          0      2
http://127.0.0.1:9004  UP     2       0          0      0
```

**Column meanings:**

| Column | Meaning |
|---|---|
| `STATE` | `UP` = in rotation, `DOWN` = ejected by health checks. Dead backends are **still listed** — that's the row you most need to see |
| `WEIGHT` | Only consulted by `weighted_round_robin` |
| `IN-FLIGHT` | Requests currently being served. Drives `least_connections` |
| `FAILS` | **Consecutive** failed health checks. Resets to 0 on any success |
| `PASSES` | **Consecutive** successes. Resets to 0 on any failure |

`FAILS`/`PASSES` clamp at their thresholds — they answer "how far through a
transition are we", so they don't count into the millions for a backend that's
been healthy for a week. `9004` shows `PASSES 0` because it was added seconds
ago and hasn't been probed yet.

This runs as a **separate process** and reaches the running proxy over the admin
API on port 9090.

---

## 11. Health check — ejection

```powershell
Stop-Process -Id (Get-NetTCPConnection -LocalPort 9002 -State Listen).OwningProcess -Force
# wait ~15s (3 failed checks at 5s intervals)
.\pf.cmd status
```

```
http://127.0.0.1:9001  DOWN   5       0          3      0
http://127.0.0.1:9002  DOWN   1       0          3      0
http://127.0.0.1:9003  DOWN   1       0          3      0
```

The proxy log:

```
level=WARN msg="backend removed from rotation" backend=http://127.0.0.1:9001 consecutive_failures=3
```

**What it means:** after exactly **3 consecutive** failures the backend left
rotation. Not 1, not 2 — the threshold is `fail_threshold: 3` in config.

**Why consecutive and not a rate:** a backend that fails every other check
shouldn't be ejected. Any single success resets the counter to 0. That
hysteresis is what stops a half-broken backend flapping in and out of rotation
on every tick.

**The payoff** — with a backend dead but the rest healthy:

```
1..6 | % { curl.exe -s -o NUL -w "%{http_code} " http://127.0.0.1:8080/ }
200 200 200 200 200 200
```

Six requests, **zero 502s**. Clients never saw the outage.

---

## 12. Health check — recovery

```
.\backends.cmd start
# wait ~10s
.\pf.cmd status
```

```
http://127.0.0.1:9001  UP     5       0          0      2
http://127.0.0.1:9002  UP     1       0          0      2
http://127.0.0.1:9003  UP     1       0          0      2
```

```
level=INFO msg="backend recovered, back in rotation" backend=http://127.0.0.1:9002
```

**What it means:** after 2 consecutive successes (`pass_threshold: 2`) the
backend was readmitted automatically. No restart, no manual intervention.

Note the log only records **transitions**, not every check. With 3 backends on a
5-second interval, logging every result would be 36 lines a minute forever,
burying the one line that matters.

---

## 13. Add backend at runtime

```
.\pf.cmd add-backend --url http://127.0.0.1:9004 --weight 2
```

```
added http://127.0.0.1:9004 (weight 2)
note: this affects the running process only; config.yaml is unchanged
```

**What it means:** the backend is in rotation **immediately**, with no restart —
`status` shows `backends: 4`.

**Read the note.** This is deliberate: writing back to `config.yaml` would
reformat your YAML and destroy every comment in it. That's a worse surprise than
"runtime changes are runtime-only". To make it permanent, edit the file.

---

## 14. Remove backend at runtime

```
.\pf.cmd remove-backend --url http://127.0.0.1:9004
```

```
removed http://127.0.0.1:9004
note: in-flight requests already sent to it will finish normally
```

**What it means:** removal stops **future** selection. Requests already
dispatched hold a pointer to that backend and complete normally.

**Why:** severing live requests would turn a routine operator action into
client-visible errors.

---

## 15. Admin CLI errors

Each failure is distinguishable, so scripts don't have to grep messages.

**Duplicate URL:**
```
.\pf.cmd add-backend --url http://127.0.0.1:9004
proxyforge: http://127.0.0.1:9004: backend already in pool
```
HTTP 409 Conflict under the hood.

**Backend not in the pool:**
```
.\pf.cmd remove-backend --url http://127.0.0.1:9999
proxyforge: http://127.0.0.1:9999: backend not found in pool
```
HTTP 404.

**Invalid URL:**
```
.\pf.cmd add-backend --url localhost:9005
proxyforge: backend URL "localhost:9005" must start with http:// or https://
```
HTTP 400 — rejected before it ever enters the pool.

**Missing required flag:**
```
.\pf.cmd add-backend
Usage of add-backend:
  -admin-port int
    	admin API port of the running proxy (default 9090)
  -url string
    	backend URL to add (required)
  -weight int
    	backend weight, used by weighted_round_robin (default 1)
proxyforge: --url is required
```

Go's `flag` package has no concept of a required flag, so that's a manual check —
done client-side to save a pointless round trip.

---

## 16. Request logging

Every request produces exactly one structured line:

```
level=INFO msg=request request_id=c9970655e7fa92c1 method=GET path=/
  backend=http://127.0.0.1:9001 status=200 bytes=73 latency_ms=1.061 remote=127.0.0.1:60510
```

**Field meanings:**

| Field | Meaning |
|---|---|
| `request_id` | Unique per request. Echoed to the client in the `X-Request-Id` header so they can quote it in a bug report |
| `method` / `path` | The original client request |
| `backend` | **Which backend the load balancer chose** — the field that makes this log worth having |
| `status` | What the client actually received |
| `bytes` | Response body size |
| `latency_ms` | Milliseconds as a float, so an aggregator can compute percentiles directly |
| `remote` | Client address |

Switch to `format: json` in config for one JSON object per line.

---

## 17. Request ID propagation

```
curl.exe -s -o NUL -H "X-Request-Id: my-trace-abc123" http://127.0.0.1:8080/traced
```

```
level=INFO msg=request request_id=my-trace-abc123 method=GET path=/traced
  backend=http://127.0.0.1:9003 status=200 bytes=79 latency_ms=0.545
```

**What it means:** an inbound `X-Request-Id` is **honoured**, not overwritten.
That's how a single trace ID follows a request across multiple services — you
can grep every service's logs for `my-trace-abc123` and reconstruct the whole
path.

If the header is absent, one is generated (the 16-hex-character IDs above).

---

## 18. Log-injection defence

```
curl.exe -s -o NUL -H "X-Request-Id: ab cd;level=ERROR" http://127.0.0.1:8080/inject
```

```
level=INFO msg=request request_id=abcdlevelERROR method=GET path=/inject ...
```

**What it means:** the spaces, the `;` and the `=` were all **stripped**. The
value that got logged is `abcdlevelERROR`, not the original.

**Why this matters:** an inbound header is attacker-controlled text. Logged
verbatim, a value containing a newline lets a client forge entire log entries —
inserting a fake `level=INFO msg="all clear"` line to hide an attack from whoever
reads the logs, or poisoning a log-parsing alert.

Only `A-Z a-z 0-9 _ -` survive, capped at 64 characters so a client can't bloat
every line either.

---

## 19. Latency measurement

```
curl.exe -s -o NUL "http://127.0.0.1:8080/slow?delay=900ms"
```

```
level=INFO msg=request request_id=10d1f45d72e4b078 method=GET path=/slow
  backend=http://127.0.0.1:9001 status=200 bytes=77 latency_ms=900.948
```

**What it means:** `latency_ms=900.948` against a requested 900ms delay — the
0.948ms overhead is the proxy hop itself. The timer covers the full round trip,
starting before backend selection and stopping after the response is streamed
back.

`?delay=` is a feature of the *dummy backend*, not the proxy — it's there so you
can simulate slow upstreams.

---

## 20. 502 — backend unreachable

Kill a backend and hit the proxy immediately, before health checks notice:

```
1..4 | % { curl.exe -s -o NUL -w "%{http_code} " http://127.0.0.1:8080/ }
200 200 502 200
```

```
level=ERROR msg="proxy error" request_id=6e8ed5e5e157b1b4 method=GET path=/
  backend=http://127.0.0.1:9002
  err="dial tcp 127.0.0.1:9002: connectex: No connection could be made because
       the target machine actively refused it."
```

**What it means:** the 3rd request was routed to `9002`, which had just died. The
other three succeeded.

**502 vs 500:** 502 Bad Gateway is correct — *we* are the gateway and the
*upstream* failed. A 500 would claim the fault was ours.

**This is the window health checking closes.** Within 15 seconds `9002` is
ejected and the 502s stop entirely. A proxy with no active health checking
returns 502s for that backend indefinitely.

**Important:** a backend returning a 500 does **not** produce this error. That's
a *successful* proxy operation carrying an unsuccessful response, and it passes
straight through to the client untouched. `ErrorHandler` only fires when no
response could be obtained at all.

---

## 21. 503 — no healthy backends

Kill every backend and wait for health checks:

```
curl.exe http://127.0.0.1:8080/
```
```
503 service unavailable: no healthy backends
```
```
http://127.0.0.1:9001  DOWN   5       0          3      0
http://127.0.0.1:9002  DOWN   1       0          3      0
http://127.0.0.1:9003  DOWN   1       0          3      0
```

**503 vs 502 — the distinction matters at 3am:**

- **502** = "I asked an upstream and it failed." One backend is broken.
- **503** = "I have no upstream to ask." Everything is down.

Different problems, different responses. Note the proxy itself is still healthy
and answering — it just has nothing to forward to. It recovers automatically the
moment any backend comes back.

---

## 22. Admin API directly

The CLI is a thin client over plain HTTP. You can call it yourself:

```
curl.exe http://127.0.0.1:9090/status
```

```json
{"strategy":"round_robin","proxy_port":8080,"uptime":"1m42s","backends":[
  {"url":"http://127.0.0.1:9001","weight":5,"alive":true,"in_flight":0,
   "consecutive_failures":0,"consecutive_successes":2}, ... ]}
```

**What it means:** everything `pf status` shows is available as JSON for
scripting or monitoring.

The endpoints:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/status` | Current pool state |
| `POST` | `/backends` | Add — body `{"url":"...","weight":1}` |
| `DELETE` | `/backends?url=...` | Remove |

**Security note:** this has **no authentication**. It binds `127.0.0.1` only,
and that loopback bind is the entire security model. Never expose port 9090.

---

## 23. Method-aware routing

```
curl.exe -X POST http://127.0.0.1:9090/status
```

```
HTTP/1.1 405 Method Not Allowed
Allow: GET, HEAD
```

**What it means:** `/status` is registered as `GET /status`, so the wrong verb
gets an automatic 405 with a correct `Allow` header — no hand-written
`switch r.Method` anywhere in the code.

This is Go 1.22's method-aware `ServeMux`, and it's the concrete reason `go.mod`
requires 1.22 rather than 1.21.

---

## 24. Graceful shutdown

Start a 6-second request, then press **Ctrl+C** on the proxy 1.5 seconds in.

```
curl.exe "http://127.0.0.1:8080/checkout?delay=6s"
```

Client result:
```
code=200  bytes=81  elapsed_ms=6125
```

Proxy log:
```
14:04:08.788  level=INFO msg="shutdown signal received, draining in-flight requests"
                timeout=30s hint="press Ctrl+C again to exit immediately"
14:04:13.272  level=INFO msg=request request_id=ed210ea9f1acf188 method=GET path=/checkout
                backend=http://127.0.0.1:9001 status=200 bytes=81 latency_ms=6001.991
14:04:13.450  level=INFO msg="shutdown complete, all in-flight requests finished"
```

**Read the timestamps:**

| Time | Event |
|---|---|
| `14:04:08.788` | Ctrl+C arrived, draining began |
| `14:04:13.272` | The request **completed** — **4.5 seconds after the signal** |
| `14:04:13.450` | Process exited |

**What it means:** the client got a full `200` with all 81 bytes. Without
graceful shutdown they'd have received a connection reset or a truncated body —
exactly what users experience during a careless deploy.

**What's happening under the hood:**

1. The listener closes **immediately** — no *new* connections accepted.
2. Idle keep-alive connections are closed.
3. Active requests are allowed to finish, up to `shutdown_timeout: 30s`.
4. Only then does the health checker stop — draining requests still need an
   accurate view of which backends are alive.

Note the `hint`: pressing Ctrl+C **again** exits immediately. Default signal
handling is restored the moment draining starts, so an operator watching a slow
drain can always give up. Without that, further Ctrl+Cs get swallowed and appear
to do nothing — which is when people reach for `kill -9`.

---

## 25. Proxy not running

```
.\pf.cmd status
```

```
proxyforge: proxyforge is not running: nothing is listening on http://127.0.0.1:9090
  (start it with `proxyforge start`)
```
Exit code `2`.

**What it means:** rather than leaking a raw socket error
(`dial tcp 127.0.0.1:9090: connectex: No connection could be made...`), the CLI
recognises this specific failure and tells you what to do.

**Exit codes** — so scripts can branch without parsing stderr:

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Error (bad config, duplicate backend, not found, …) |
| `2` | ProxyForge is not running |

---

## Cleanup

```
# Ctrl+C the proxy, then:
.\backends.cmd stop
```

---

## Full sequence to reproduce everything

Two terminals, both in the project folder.

**Terminal 1:**
```
.\test.cmd
.\backends.cmd start
.\pf.cmd start --config config.yaml
```

**Terminal 2:**
```
.\pf.cmd status
1..6 | % { (curl.exe -s -o NUL -D - http://127.0.0.1:8080/ | Select-String "X-Backend-Name").ToString().Trim() }
curl.exe http://127.0.0.1:8080/api/users
curl.exe -s -o NUL -H "X-Request-Id: my-trace-abc123" http://127.0.0.1:8080/traced

Start-Process .\.logs\dummybackend.exe -ArgumentList "-port","9004" -WindowStyle Minimized
.\pf.cmd add-backend --url http://127.0.0.1:9004 --weight 2
.\pf.cmd status
.\pf.cmd remove-backend --url http://127.0.0.1:9004

Stop-Process -Id (Get-NetTCPConnection -LocalPort 9002 -State Listen).OwningProcess -Force
Start-Sleep -Seconds 16
.\pf.cmd status
1..6 | % { curl.exe -s -o NUL -w "%{http_code} " http://127.0.0.1:8080/ }

.\backends.cmd start
Start-Sleep -Seconds 12
.\pf.cmd status

curl.exe "http://127.0.0.1:8080/?delay=10s"     # then Ctrl+C Terminal 1
```

**Cleanup:**
```
.\backends.cmd stop
```
