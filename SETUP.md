# ProxyForge — Setup Guide

How to get ProxyForge running on a machine that has never seen it before:
install, transfer, configure, start, verify.

For what it *is* and how it works, see [README.md](README.md).
For the concepts behind it, see [LEARNING_NOTES.md](LEARNING_NOTES.md).

---

## 0. What you need

| | Required? | Why |
|---|---|---|
| **Go 1.22 or newer** | Yes | Builds everything. 1.22 specifically — we use the method-aware `ServeMux` and the fixed loop-variable semantics |
| **Internet, first build only** | Yes* | To download `gopkg.in/yaml.v3` once. See [offline install](#offline--air-gapped-install) to avoid it |
| **Git** | Recommended | Only to move the repo *with* its commit history |
| **A C compiler (gcc)** | Only for `go test -race` | The race detector needs cgo. Everything else works without it |

Ports used: **8080** (proxy), **9090** (admin API), **9001–9003** (test
backends). All bound to `127.0.0.1`, so no Windows firewall prompt and nothing
is reachable from outside the machine.

---

## 1. Install Go

### Windows

```powershell
winget install --id GoLang.Go -e
```

**Then close and reopen your terminal.** The installer edits the machine `PATH`,
but a terminal that's already running holds the copy it inherited at launch —
`go` will not resolve in it. This trips up almost everyone.

Verify:

```powershell
go version
# go version go1.27.0 windows/amd64
```

<details>
<summary>If <code>go</code> still isn't found after reopening</summary>

The binary lives at `C:\Program Files\Go\bin\go.exe`. Add it to PATH for the
current session:

```powershell
$env:Path += ";C:\Program Files\Go\bin"
```

To fix it permanently, reopen a terminal — or re-read the machine PATH in place:

```powershell
$env:Path = [System.Environment]::GetEnvironmentVariable("Path","Machine") + ";" +
            [System.Environment]::GetEnvironmentVariable("Path","User")
```
</details>

### macOS

```bash
brew install go
```

### Linux

```bash
# Debian/Ubuntu — check the version, distro packages are often old
sudo apt install golang-go && go version

# If it reports below 1.22, install from go.dev instead:
curl -LO https://go.dev/dl/go1.27.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.27.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc && source ~/.bashrc
```

---

## 2. Install a C compiler — only if you want `go test -race`

**Skip this if you only want to run the proxy.** Building and running need no C
compiler at all.

The Go race detector is built on cgo, so without gcc on `PATH`, `CGO_ENABLED`
auto-detects to `0` and you get:

```
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

`go build` and plain `go test` are entirely unaffected.

```powershell
# Windows — plain GCC, no LLVM bloat
winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e
# reopen the terminal afterwards
```

```bash
# macOS
xcode-select --install

# Debian/Ubuntu
sudo apt install build-essential
```

Verify with `gcc --version`, then `go env CGO_ENABLED` should report `1`.

---

## 3. Get the code onto the machine

There is **no GitHub remote** on this repo, so pick one of these.

### Option A — `git bundle` (recommended: keeps the commit history)

The git history is one commit per build phase and is genuinely part of the
project. A bundle is a single file containing the entire repo.

On the **old** machine:

```powershell
git bundle create proxyforge.bundle --all
```

That produces one ~110 KB file. Copy it over however you like — USB, email,
network share.

On the **new** machine:

```powershell
git clone proxyforge.bundle ProxyForge
cd ProxyForge
git log --oneline      # all 11 phase commits should be there
```

### Option B — copy the folder

Copy the whole `ProxyForge` directory. **Exclude `.logs\`** — it holds built
binaries and log files from local runs, and is gitignored for that reason.

```powershell
robocopy "D:\ProxyForge" "C:\dev\ProxyForge" /E /XD .logs
```

Works fine, but you lose the commit history unless you copy `.git\` too.

### Option C — push to a git host first

If you later put this on GitHub/GitLab:

```powershell
git remote add origin <your-repo-url>
git push -u origin master
```

Then it's an ordinary `git clone` on the new machine.

---

## 4. Verify the build

From the project root:

```powershell
go build ./...
```

The first run downloads `gopkg.in/yaml.v3` (needs internet, takes a few
seconds). Silence means success — Go tools say nothing when things go right.

Then run the tests:

```powershell
go test ./...              # works everywhere
go test -race ./...        # needs the C compiler from step 2
```

Expect `ok` for seven packages and `no test files` for two.

> Always use `-race` when you change anything concurrent. The round-robin
> counter test passes roughly 99% of the time with a *broken* non-atomic
> counter — without `-race`, a green run proves nothing.

---

## 5. Start the test backends

ProxyForge is a load balancer; it needs something to balance across. The repo
ships a dummy backend for exactly this.

### Windows

```powershell
.\scripts\run-backends.ps1
```

Starts three on ports 9001, 9002, 9003. Check one directly:

```powershell
curl.exe http://127.0.0.1:9001/health
# ok backend-9001 uptime=3s
```

Their logs go to `.logs\backend-<port>.err.log`. (Go's `log` writes to stderr,
so the interesting lines are in `.err.log`, not `.out.log`.)

### macOS / Linux

No script — start them by hand:

```bash
for p in 9001 9002 9003; do
  go run ./cmd/dummybackend -port $p &
done
```

Stop them later with `kill %1 %2 %3`, or `pkill -f dummybackend`.

### The dummy backend's useful tricks

```powershell
curl.exe "http://127.0.0.1:9001/?delay=2s"     # slow response — test least_connections
curl.exe "http://127.0.0.1:9001/?fail=503"     # forced error status
curl.exe -i http://127.0.0.1:9001/             # X-Backend-Name says who answered
```

---

## 6. Configure

Edit [config.yaml](config.yaml) in the project root. It ships working and fully
commented — for a first run you can leave it exactly as it is.

```yaml
port: 8080              # clients connect here
admin_port: 9090        # status / add-backend / remove-backend talk to this
strategy: round_robin   # round_robin | weighted_round_robin | least_connections | random

backends:
  - url: http://127.0.0.1:9001
    weight: 5           # only used by weighted_round_robin
  - url: http://127.0.0.1:9002
    weight: 1
  - url: http://127.0.0.1:9003
    weight: 1

health_check:
  interval: 5s          # how often to probe
  timeout: 2s           # must be LESS than interval
  path: /health
  fail_threshold: 3     # consecutive failures before removal from rotation
  pass_threshold: 2     # consecutive successes before it comes back

shutdown_timeout: 30s   # how long to let in-flight requests finish on Ctrl+C

log:
  level: info           # debug | info | warn | error
  format: text          # text (readable) | json (for log aggregators)
```

### Rules that will bite you

- **Backend URLs need the scheme.** `localhost:9001` is silently *not* what you
  mean — Go parses `localhost` as the scheme and ends up with no host. Write
  `http://localhost:9001`. Validation catches this at startup.
- **Durations are strings, not numbers.** `5s`, `500ms`, `1m30s`. A bare
  `interval: 5` is rejected — ambiguous units.
- **Unknown keys are rejected**, not ignored. `strategey:` fails at startup
  rather than silently leaving round robin in force.
- **`timeout` must be less than `interval`**, or probes overlap their own
  schedule.

Everything except `backends` is optional and falls back to the defaults above.

### Bad config fails loudly, all at once

```
proxyforge: invalid config config.yaml:
port: 0 is not in 1..65535
strategy: "round-robin" is not one of [least_connections random round_robin weighted_round_robin]
backends[0]: url "localhost:9001" must start with http:// or https://
health_check.path: "health" must start with /
```

Every problem in one run — fix them all, then start again.

---

## 7. Start it

```powershell
go run ./cmd/proxyforge start --config config.yaml
```

Or build a binary once and run that (faster to start, and what you'd deploy):

```powershell
go build -o proxyforge.exe ./cmd/proxyforge
.\proxyforge.exe start --config config.yaml
```

You should see:

```
level=INFO msg="backend registered" url=http://127.0.0.1:9001 weight=5
level=INFO msg="backend registered" url=http://127.0.0.1:9002 weight=1
level=INFO msg="backend registered" url=http://127.0.0.1:9003 weight=1
level=INFO msg="proxyforge listening" addr=127.0.0.1:8080 admin_addr=127.0.0.1:9090
  strategy=round_robin backends=3 health_path=/health health_interval=5s
```

It runs in the **foreground**. Leave this terminal alone and use a second one
for everything below.

---

## 8. Verify it works

In a **second** terminal:

```powershell
# Requests rotate across backends -- watch X-Backend-Name change
curl.exe -i http://127.0.0.1:8080/
curl.exe -i http://127.0.0.1:8080/
curl.exe -i http://127.0.0.1:8080/
```

```powershell
go run ./cmd/proxyforge status
```

```
strategy:   round_robin
proxy port: 8080
uptime:     20s
backends:   3

URL                    STATE  WEIGHT  IN-FLIGHT  FAILS  PASSES
http://127.0.0.1:9001  UP     5       0          0      2
http://127.0.0.1:9002  UP     1       0          0      2
http://127.0.0.1:9003  UP     1       0          0      2
```

### Prove health checking works

```powershell
# Kill one backend
Stop-Process -Id (Get-NetTCPConnection -LocalPort 9002 -State Listen).OwningProcess -Force

# Wait ~15s (3 failed checks at 5s intervals), then:
go run ./cmd/proxyforge status      # 9002 now shows DOWN

# Traffic keeps flowing with no errors at all:
1..6 | ForEach-Object { curl.exe -s -o NUL -w "%{http_code} " http://127.0.0.1:8080/ }
# 200 200 200 200 200 200
```

Restart it and it is readmitted automatically after 2 successful checks.

### Prove graceful shutdown works

```powershell
# In terminal 2 -- a request that takes 10 seconds:
curl.exe "http://127.0.0.1:8080/?delay=10s"

# Immediately press Ctrl+C in terminal 1 (the proxy)
```

The proxy logs `draining in-flight requests`, the curl **still completes with a
full response**, and only then does the process exit. Press Ctrl+C a second time
to give up and exit at once.

### Manage backends at runtime

```powershell
go run ./cmd/proxyforge add-backend --url http://127.0.0.1:9004 --weight 2
go run ./cmd/proxyforge remove-backend --url http://127.0.0.1:9002
```

These change the **running process only** — `config.yaml` is untouched, so the
change is gone after a restart. That's deliberate: rewriting your YAML would
destroy its comments and formatting.

---

## 9. Point it at your own backends

Replace the `backends` list in `config.yaml` with your real services:

```yaml
backends:
  - url: http://10.0.1.20:3000
  - url: http://10.0.1.21:3000
  - url: https://api-internal.example.com

health_check:
  path: /healthz          # whatever your service actually exposes
```

Two things to get right:

1. **The health path must return 2xx or 3xx.** A 404 counts as unhealthy and
   ejects the backend — deliberately, so a misconfigured path fails loudly
   instead of quietly keeping a broken server in rotation.
2. **To accept traffic from other machines**, change the proxy's bind address.
   Right now everything binds `127.0.0.1`. See the note below before you do.

> **Before exposing this beyond localhost:** the admin API on port 9090 can add
> and remove backends with **no authentication**. Binding it publicly would let
> anyone who can reach the box repoint your proxy at a server they control. The
> loopback bind *is* the security model. If you need external access, put the
> proxy port behind a firewall rule and leave the admin port on loopback.
> ProxyForge is a learning project — it has no TLS, no auth, and no rate
> limiting.

---

## 10. Stopping

| What | How |
|---|---|
| The proxy | `Ctrl+C` in its terminal — drains in-flight requests first |
| Test backends (Windows) | `.\scripts\stop-backends.ps1` |
| Test backends (Linux/macOS) | `pkill -f dummybackend` |

---

## Offline / air-gapped install

The only network need is downloading `gopkg.in/yaml.v3` on the first build. To
remove it, vendor the dependency on a machine that *does* have internet:

```powershell
go mod vendor
```

That creates a `vendor\` directory holding the dependency's source. Copy the
project (including `vendor\`) to the offline machine, and Go uses it
automatically — no network, no module cache needed.

`vendor\` is not committed here, to keep the repo small.

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `'go' is not recognized` | Your terminal was open before Go was installed and is holding a stale PATH. **New tabs in the same Windows Terminal window inherit that same stale copy** — you must close every window of it, not just the tab. Or sidestep PATH entirely with the `.cmd` wrappers above. |
| `'#' is not recognized` / `.ps1` does nothing | You're in `cmd.exe`, not PowerShell. `cmd` uses `REM` for comments and cannot run `.ps1` files at all. Use the `.cmd` wrappers, or switch to PowerShell (its prompt starts with `PS`). |
| `go: -race requires cgo` | No C compiler. Either install one (step 2) or drop `-race`. |
| `binding proxy port: ... Only one usage of each socket address` (Windows)<br>`... address already in use` (Linux/macOS) | Port 8080 or 9090 is already taken — often a ProxyForge you forgot to stop. Change `port`/`admin_port` in config, or find the culprit: `Get-NetTCPConnection -LocalPort 8080 -State Listen` |
| `proxyforge is not running` (exit code 2) | `status`/`add-backend` need a running `proxyforge start`. Start it first. |
| All requests return `503 no healthy backends` | Backends aren't running, or the health path is wrong. Test directly: `curl.exe http://127.0.0.1:9001/health` |
| Requests return `502 bad gateway` | A backend was selected but is unreachable. Normally means it died within the last health-check interval. |
| `field strategey not found in type config.Config` | A typo in `config.yaml`. Unknown keys are rejected on purpose — the message names the bad key. |
| `url "localhost:9001" must start with http://` | Backend URLs need a scheme. Use `http://localhost:9001`. |
| `cannot unmarshal !!int` into `time.Duration` | A duration was written as a bare number. Use `5s`, not `5`. |
| Config changes appear to do nothing | Config is read at startup only. Restart the proxy. |
| `add-backend` gone after a restart | Expected — runtime changes don't write to `config.yaml`. Edit the file to persist it. |
| Health checks fail but the backend works in a browser | The proxy probes `health_check.path`, not `/`. Confirm it: `curl.exe -i http://<backend>/health` |
| Windows firewall prompt on startup | Something is binding `0.0.0.0` instead of loopback. Stock config binds `127.0.0.1` and should never prompt. |

---

## The easy way: the `.cmd` wrappers

If `go` isn't on your PATH — or you just don't want to think about it — the repo
ships three wrappers that **locate Go themselves** and work identically in
`cmd.exe` and PowerShell.

```
.\backends.cmd start      start three test backends on 9001-9003
.\backends.cmd start 9004 start one extra on a specific port
.\backends.cmd stop       stop them all

.\pf.cmd start --config config.yaml
.\pf.cmd status
.\pf.cmd add-backend --url http://127.0.0.1:9004 --weight 2
.\pf.cmd remove-backend --url http://127.0.0.1:9004
.\pf.cmd build            force a rebuild after editing the source

.\test.cmd                run all tests
.\test.cmd race           run all tests with the race detector
```

They search for `go.exe` on PATH first, then `C:\Program Files\Go\bin`,
`C:\Go\bin` and `%LOCALAPPDATA%\Programs\Go\bin`. `pf.cmd` builds the binary on
first use and reuses it after that.

**Keep the `.\` prefix.** PowerShell requires it for scripts in the current
directory, and some hardened environments set
`NoDefaultCurrentDirectoryInExePath=1`, which makes `cmd` require it too.

Note `pf.cmd` does not rebuild on every call — Windows locks a running `.exe`,
so it would fail while `pf start` is up in another window. Run `.\pf.cmd build`
after you change the source.

---

## Quick reference

```powershell
# One-time, per machine
winget install --id GoLang.Go -e                          # then reopen terminal
winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e   # only for -race

# Per project
go build ./...                    # compile
go test ./...                     # test
go test -race ./...               # test with the race detector
go build -o proxyforge.exe ./cmd/proxyforge

# Running
.\scripts\run-backends.ps1                           # start 3 test backends
go run ./cmd/proxyforge start --config config.yaml   # start the proxy (foreground)
go run ./cmd/proxyforge status                       # inspect live state
go run ./cmd/proxyforge add-backend --url <url> --weight 1
go run ./cmd/proxyforge remove-backend --url <url>
.\scripts\stop-backends.ps1                          # stop test backends

# Ports:  8080 proxy | 9090 admin | 9001-9003 test backends  (all on 127.0.0.1)
# Exit codes:  0 ok | 1 error | 2 proxyforge is not running
```
