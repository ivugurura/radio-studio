# Stress-testing the Studio `/listen` route

Goal: find the **maximum number of concurrent listeners** one `radio-studio`
process serves on `/studios/<id>/listen` before it degrades, for a given box and
network path. The number is hardware- and NIC-specific — always record it next
to the machine spec and whether the test went direct or through Nginx.

Tools in this repo:

| Tool | What it is | When |
|---|---|---|
| `cmd/loadtest` | stdlib Go streaming load generator, real-time body pacing | **reference** — capacity, eviction, underrun |
| `deploy/loadtest.k6.js` | k6 script, same ramp shape | nicer percentiles / dashboards; no real-time pacing |
| `ENABLE_PPROF=1` on the server | localhost `:6060` with `/debug/pprof/*` + `/debug/vars` | goroutine + memstats during a ramp |

---

## How `/listen` behaves (why the test is shaped this way)

- One goroutine, one ~8 s buffered channel (~128 KB at 128 kbps), one socket per
  listener. A single `distribute()` goroutine per studio fans every ~4 KB chunk
  (~4/s) out to all listeners.
- **Slow-listener eviction** (`internal/stream/studio.go`) only fires after 50
  *consecutive* undelivered chunks — i.e. a listener that reads essentially
  nothing for ~13 s. A merely-slow listener is not evicted; it just falls
  behind. So the leading health signal is **per-connection throughput**
  (`perconn_min_kbps`), and server-side drops (`close_ended`) are the confirming
  signal once the box is genuinely overloaded.
- No `WriteTimeout`, no listener cap, no auth on `/listen`. The server will keep
  accepting connections until it runs out of CPU, RAM, or file descriptors.
- The audio source is always present: with no backend playlist and no
  `DEFAULT_TRACK_FILE`, AutoDJ loops an embedded silent MP3 paced at the
  configured bitrate. So `/listen` emits a steady ~16 KB/s stream with zero
  extra setup.

## Failure signals (any one = capacity exceeded)

1. New connections fail (`connect_errs > 0`) or never get a first byte.
2. `perconn_min_kbps` falls below ~90 % of the target rate (audio underrun), or
   `close_ended` / `close_reset` climb (server dropping established listeners).
3. Server CPU pinned near 100 % across all cores for a sustained window.
4. RSS near the box limit / OOM risk, or `too many open files` in the logs.
5. Egress plateaus below `listeners x 16 KB/s` while the count still rises.

`cmd/loadtest` prints `result: first failure at intended=N ...` when it sees 1
or 2. The **last clean ramp step** is the reported capacity.

---

## Pre-flight

**File descriptors.** Each listener costs 1 FD on the server (2 behind Nginx).

- Load box: `ulimit -n 1048576` before running (`cmd/loadtest` also best-effort
  raises its own limit and prints the result).
- Server (staging): add to the systemd unit and reload:

  ```ini
  # /etc/systemd/system/radio-studio.service  [Service]
  LimitNOFILE=262144
  ```

  ```sh
  sudo systemctl daemon-reload && sudo systemctl restart radio-studio
  cat /proc/$(pgrep -f radio-studio)/limits | grep 'Max open files'
  ```

**Confirm audio is flowing** (should sit at ~16 KB/s):

```sh
curl -N http://127.0.0.1:7081/studios/reformation-rw/listen | pv -br > /dev/null
```

**Egress + cost.** Sustained egress ≈ `128 kbps x listeners`. 5 000 listeners ≈
80 MB/s ≈ ~288 GB/hour. Check the provider's bandwidth cap and egress billing
before any long soak.

**Isolation.** Use a non-production studio id, or a maintenance window. Run the
load generator from a **separate** box (ideally same region) — never on the
server under test.

---

## Phase 0 — local baseline (validate the tooling)

```sh
# terminal 1 — server, no backend needed
LISTEN_ADDR=7080 ENABLE_PPROF=1 go run cmd/server/main.go

# terminal 2 — 10 listeners for 30 s
go run cmd/loadtest/main.go \
  -url http://127.0.0.1:7080/studios/reformation-rw/listen \
  -start 10 -step 0 -max 10 -hold 30s -out dry.csv

# terminal 3 — cross-checks
curl -s http://127.0.0.1:7080/studios/reformation-rw/status        # listeners_count: 10
curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head -1
curl -s http://127.0.0.1:6060/debug/vars | jq .memstats.Alloc
```

Expect: `connected=10`, `perconn_*_kbps ~= 15.8`, `dropped=0`, and
`result: NO failure signal`. Then ramp the laptop as far as it goes
(`-start 500 -step 500 -step-interval 20s -max 20000 -hold 30s`) just to see the
shape of the curve — the laptop number is not the answer.

Sanity-check the drop detector by killing the server mid-run: `cmd/loadtest`
should report `close_reset` for every connection and a `first failure` line.

---

## Phase A — staging, direct port (the app's own ceiling)

From a load box in the same region, hitting `:7081` directly (bypasses Nginx,
removes WAN noise — isolates CPU / RAM / goroutines / FDs):

```sh
ulimit -n 1048576
go run ./cmd/loadtest/main.go \
  -url http://STAGING_HOST:7081/studios/reformation-rw/listen \
  -start 200 -step 200 -step-interval 45s -max 20000 -hold 60s \
  -out phaseA.csv
```

Watch the monitors below. Stop at the first failure signal; the last clean step
is the app ceiling. One VPS load box generates roughly 10–20 k streaming
connections — past that, run `cmd/loadtest` on 2–3 boxes with staggered
`-start` values and sum the reported `connected` counts.

## Phase B — staging, via Nginx (the real edge)

Deploy `deploy/nginx/listen.conf` into the fronting `server {}` block, raise
`worker_connections` / `worker_rlimit_nofile` (notes in that file),
`nginx -t && systemctl reload nginx`, then run the **same** command against the
public hostname:

```sh
go run ./cmd/loadtest \
  -url https://stream.example.org/studios/reformation-rw/listen \
  -start 200 -step 200 -step-interval 45s -max 20000 -hold 60s \
  -out phaseB.csv
```

Compare B to A: if B tops out lower, Nginx (workers / FDs / CPU) is the limit —
tune it and rerun. If A and B match, the Go process is the limit.

## Phase C — soak

Hold ~80 % of the lower of A/B for 20–30 min:

```sh
go run ./cmd/loadtest -url <same> \
  -start <0.8*capacity> -step 0 -max <0.8*capacity> -hold 30m -out soak.csv
```

Pass = flat RSS, flat FD count, zero `close_*`, steady `perconn_mean_kbps`.
Listeners are removed on disconnect, so a rising FD/RSS line here means a leak.

---

## Server-side monitors (run during every phase)

```sh
# listener count the server actually holds — compare to the generator's "intended"
watch -n5 'curl -s http://127.0.0.1:7081/studios/reformation-rw/status'
curl -s http://127.0.0.1:7081/studios/reformation-rw/snapshot | jq '{active,bytes_total}'

# connects + evictions live
sudo journalctl -u radio-studio -f | grep --line-buffered -E 'new listener|dropped slow listener|too many open files'

# CPU / RSS
top -p "$(pgrep -f radio-studio)"          # or: pidstat -p $(pgrep -f radio-studio) 5

# file descriptors / established sockets
watch -n5 'ls /proc/$(pgrep -f radio-studio)/fd | wc -l'
ss -tn state established | wc -l

# goroutines (~= live listeners + ~8) and heap, needs ENABLE_PPROF=1
watch -n5 "curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head -1"
curl -s http://127.0.0.1:6060/debug/vars | jq '.memstats | {Alloc, Sys, NumGC}'

# NIC throughput vs listeners x 16 KB/s
nload   # or: iftop -B
```

A widening gap between the generator's `intended` and the server's
`listeners_count` = the server is dropping listeners → capacity exceeded.

---

## Recording the result

| Date | Box (vCPU / RAM / NIC) | Path | Drain | Max healthy listeners | First failure signal | Egress at peak |
|---|---|---|---|---|---|---|
|  |  | direct :7081 | realtime |  |  |  |
|  |  | via nginx | realtime |  |  |  |

Once staging gives a stable number, add a one-line **Expected concurrency** note
to `deploy/command.md` so ops has a reference.

---

## `cmd/loadtest` flags

| Flag | Default | Meaning |
|---|---|---|
| `-url` | — | target `/listen` URL (required) |
| `-start` | 100 | listeners opened immediately |
| `-step` / `-step-interval` | 100 / 30s | listeners added each interval (`-step 0` = no ramp) |
| `-max` | 5000 | peak concurrent listeners |
| `-hold` | 60s | time held at `-max` before teardown |
| `-rate` | 16000 | per-conn drain rate, bytes/s (128 kbps) |
| `-drain` | realtime | `realtime` = pace to `-rate`; `fast` = read flat out (pure conn/throughput capacity) |
| `-spawn-delay` | 2ms | gap between dials while spawning |
| `-out` | — | CSV of progress rows |
| `-insecure` | false | skip TLS verification |

CSV columns: `elapsed_s, intended, connected, connect_errs, early_closed,
close_ended, close_reset, close_other, agg_mbps, ttfb_p50_ms, ttfb_p95_ms,
ttfb_p99_ms, perconn_min_kbps, perconn_mean_kbps`.

## k6 alternative

```sh
# install: https://grafana.com/docs/k6/latest/set-up/install-k6/
k6 run \
  -e URL=http://STAGING_HOST:7081/studios/reformation-rw/listen \
  -e MAX=5000 -e START=200 -e STEP=200 -e STEP_S=45 -e HOLD_S=45 \
  deploy/loadtest.k6.js
```

k6 drains as fast as it can (no real-time pacing), so it measures connection and
throughput capacity, not player-accurate underrun. Thresholds fail the run if
`http_req_failed > 1%` or any short read (`listen_underruns`) occurs. Treat
`cmd/loadtest -drain=realtime` as authoritative for the capacity number.
