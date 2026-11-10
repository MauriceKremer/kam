# Feature: System details in `GET /v1/status`

**Status:** Implemented.
**Scope:** An opt-in `system` section on `GET /v1/status` — machine memory, CPU information, free disk space, and the SQLite database size — collected only when a request asks for it, with a configured minimum interval between collections.
**Related:** [APIdetails.md](APIdetails.md) (status contract), [FeatureUnifiedApi.md](FeatureUnifiedApi.md) §4 (transport rules and the dependency rule), [FeatureKamApi.md](FeatureKamApi.md) (status consumers, KAMmon), [README.md](../README.md) (the `monitor` block).

## 1. Summary

`GET /v1/status` today reports only KAM's own state: modem, radio, scheduler, queues, rooms, automation ([APIdetails.md](APIdetails.md) "GET /v1/status"). Everything it needs is already in memory, so it is cheap and answered on every poll.

This feature adds the machine around KAM as a separate, opt-in section:

- **memory** used / available on the machine,
- **CPU** information (model, core count, load averages — the numbers `top`'s summary line shows),
- **disk** used / available on the filesystem that holds the database,
- **database** size of `kam.db` including its WAL companion file.

Two rules keep it cheap, per the request that defines this feature:

1. **On request only.** The section is served only when the caller passes `?system=true` (or `?system=1`). The default pollers — KAMmon asks every one to two seconds ([FeatureKamApi.md](FeatureKamApi.md)) — keep receiving today's small response.
2. **At most one collection per `monitor.system_interval_sec` seconds.** Requests inside the window receive the cached snapshot with its `collected_at` timestamp; they are never rejected. `0` disables the throttle. There is no background sampler and nothing is collected when nobody asks.

Wire change is additive: `api_version` stays `1`, the status contract's "new fields are only ever added" holds.

## 2. Goals and non-goals

### Goals

- GL-1. `GET /v1/status?system=true` returns the `system` section (§4.2). Without the flag the response is byte-for-byte today's shape.
- GL-2. One collection per `monitor.system_interval_sec` seconds, cache-serve instead of reject (§4.3).
- GL-3. Collection with the Go standard library only, per OS build file, no new dependency (§5.2).
- GL-4. Documentation updated: APIdetails status section and README's `monitor` block; the `GET /` index row's purpose mentions the flag.

### Non-goals

- NG-1. Any history, time series or push: KAM keeps serving a snapshot; consumers poll ([FeatureKamApi.md](FeatureKamApi.md) data-store model).
- NG-2. Per-core or per-process CPU percentages and KAM's own memory use. Instantaneous `top`-style percentages need sampling state or an extra request; load averages are the portable summary signal (D5). KAM-process stats (heap, goroutines, RSS) are a possible later add-on, not part of this feature.
- NG-3. Windows parity at first delivery. Windows returns `null` fields (§4.4); revisit with a metrics library only if a Windows node actually wants the numbers (D3).
- NG-4. Authentication, quotas per client or audit. The listener is loopback-only with existing transport guards ([FeatureUnifiedApi.md](FeatureUnifiedApi.md) §4).
- NG-5. Other mounts, SMART data, temperatures, battery, network interfaces.

## 3. Sources and method

KAM-internal feature: the authority is KAM's own code and documentation, plus Go standard-library and platform references read locally on 2026-11-07 (macOS 27.2 arm64, Linux procfs man pages).

| Key | Source | Used for |
|---|---|---|
| API-DOC | [docs/APIdetails.md](APIdetails.md) | status contract, wire rules, field table |
| API | [internal/api/api.go](../internal/api/api.go) | `GET /v1/status` handler (line 236), `Status` wire shapes, transport policy |
| NODE | [internal/node/status.go](../internal/node/status.go), [internal/node/api.go](../internal/node/api.go) | status composition, listener start |
| MODELS | [internal/models/monitor.go](../internal/models/monitor.go) | `MonitorConfig`, defaults, YAML decoding, `Validate` |
| DBPATH | [internal/config/dbpath.go](../internal/config/dbpath.go) | DB path precedence: `-db` flag > `db_path` > `kam.db` |
| SR | Go `syscall` package docs (`pkg.go.dev/syscall`) | `Statfs` on Linux and macOS, `SysctlUint32`/`SysctlUint64`/`Sysctl` on macOS |
| PROCFS | `proc(5)` man page, man7.org | `/proc/meminfo` (`MemTotal`, `MemAvailable`), `/proc/loadavg` |
| DARWIN | `sysctl -a` read on the development machine, 2026-11-07 | `hw.memsize`, `hw.pagesize`, `vm.page_free_count`, `vm.page_speculative_count`, `vm.loadavg`, `machdep.cpu.brand_string`; `vm.stats.vm.*` no longer exported on the development host (macOS 27.2) — kept as a fallback for older releases |
| KAM-TOP | `top(1)` | which summary numbers users expect from "CPU like top": load averages and usage |
| README | [README.md](../README.md) | documented `monitor` block, "changes to this block require a restart" |

Each platform claim in §4.4 must be verified by the package's own tests on the target OS; where a value is approximate or unsupported the field becomes `null` rather than wrong.

## 4. Behaviour

### 4.1 Request

| Request | Result |
|---|---|
| `GET /v1/status` | today's response, without `system` |
| `GET /v1/status?system=true` | response with the `system` section (may be the cached snapshot) |
| `GET /v1/status?system=1` | same as `system=true` |
| `GET /v1/status?system=<anything else>` | `400 {"error":"system must be 1 or true"}` |

### 4.2 Response — the `system` section

```json
{
  "api_version": 1,
  "epoch": "...",
  "now": "2026-11-07T10:00:00.123Z",
  "...": "today's fields, unchanged",
  "system": {
    "collected_at": "2026-11-07T09:59:56.118Z",
    "memory": {
      "total_bytes": 17179869184,
      "available_bytes": 6442450944,
      "used_bytes": 10737418240,
      "used_percent": 62.5
    },
    "cpu": {
      "model": "Apple M1 Pro",
      "cores": 8,
      "load_1m": 0.52,
      "load_5m": 0.41,
      "load_15m": 0.33
    },
    "disk": {
      "path": "/home/pi",
      "total_bytes": 62500000000,
      "available_bytes": 25000000000,
      "used_bytes": 37500000000,
      "used_percent": 60.0
    },
    "database": {
      "path": "kam.db",
      "size_bytes": 1048576,
      "wal_size_bytes": 409664
    }
  }
}
```

| Field | Meaning |
|---|---|
| `collected_at` | When this snapshot was actually gathered; RFC 3339 UTC with milliseconds, same time style as `now`/`started_at`. Inside the throttle window it is older than `now` — that is the staleness signal. |
| `memory.*` | Machine RAM in bytes: `total`, `available` (the Linux `MemAvailable` semantics: probably-available, not just free), `used` = total − available, `used_percent` rounded to one decimal. |
| `cpu.model` | CPU brand string, when the OS reports one (`null` otherwise). |
| `cpu.cores` | `runtime.NumCPU()` — usable cores, all platforms. |
| `cpu.load_1m`, `load_5m`, `load_15m` | Load averages, when the OS provides them; `null` otherwise. No CPU percentage (D5). |
| `disk.*` | The filesystem holding the SQLite database (D7): `total`, `available` (free to non-root users), `used`, `used_percent`. `path` is the directory that was measured. |
| `database.*` | `path` as KAM resolved it (DBPATH), `size_bytes` from `os.Stat` of the DB file, `wal_size_bytes` of the `-wal` companion (`0` or `null` when there is none right now — WAL checkpoints make it shrink and grow). |

All four groups are objects even when a platform cannot fill them; unsupported values are `null`, never omitted fields and never an error (§4.5, SS-COL-4). The section only appears when requested (SS-API-3).

### 4.3 On-request collection and throttling

- Collection happens **only inside a request that asks for the section**. There is no background goroutine and no timer (SS-API-2).
- A tiny throttled cache wraps the collector: the first request inside an interval collects fresh; later requests within the window receive the cached snapshot unchanged.
- `monitor.system_interval_sec` (default `5`) sets the window; `0` collects on every request (SS-CFG-1).
- The cache holds exactly one snapshot; it is always the newest possible data at the throttled rate. A caller wanting fresher data polls slower, not faster.
- The throttle applies only to the `system` section. Today's unconditional status fields keep their current behaviour on every request (D4).

### 4.4 Fields by platform

| Section | Linux | macOS | Windows (first delivery) |
|---|---|---|---|
| `memory` | `/proc/meminfo`: `MemTotal`, `MemAvailable` | `hw.memsize` + `vm.page_free_count` + `vm.page_speculative_count` (fallback `vm.stats.vm.*` on older hosts); approximate: available ≈ free + speculative, matching `top`'s "unused" | `null` |
| `cpu.model` | `/proc/cpuinfo` `model name` | `machdep.cpu.brand_string` | `null` |
| `cpu.cores` | `runtime.NumCPU()` | `runtime.NumCPU()` | `runtime.NumCPU()` |
| `cpu.load_*` | `/proc/loadavg` | `vm.loadavg` sysctl (string, parsed) | `null` |
| `disk` | `syscall.Statfs` | `syscall.Statfs` | `null` |
| `database` | `os.Stat` of db and `-wal` files | same | same |

`database` is pure Go on every platform, so even a Windows node reports DB size from day one. The Darwin memory figure is approximate by OS design (kernel keeps precise accounting in Mach calls the standard library does not expose); it is marked approximate here, and if it proves misleading the field becomes `null` until D3's better source is chosen.

### 4.5 Error and degraded behaviour

- Any single collection error (unreadable `/proc` entry, stat failure, path gone) degrades that field or group to `null` and is logged at debug level; the rest of the section and the rest of the response are served normally (SS-COL-4).
- Collection cost is a handful of syscalls and two small file reads: microseconds to low milliseconds, far inside the 5-second request deadline ([FeatureUnifiedApi.md](FeatureUnifiedApi.md) §4). At the throttled rate the cost is noise.
- A missing or unreadable database file yields `null` sizes; `database.path` still shows the configured path. No new error surface is added to the API.

## 5. Design

### 5.1 Ownership and the dependency rule

[internal/api](../internal/api) stays transport-only. The plan keeps each piece where its invariant lives:

| Piece | Owner | Why |
|---|---|---|
| `system` wire shape (`SystemStatus` struct) | `internal/api` | all wire shapes live with the transport ([FeatureUnifiedApi.md](FeatureUnifiedApi.md) §4) |
| Collecting machine metrics | new `internal/systemstats` | capability, stdlib-only, build-tagged per OS |
| Throttle policy (interval, cache, clock) | `internal/systemstats` | it is about collection cost, not HTTP |
| The "only when requested" decision | `internal/api` handler | it is the one who reads the query flag |
| Translating snapshot → wire shape and wiring | `internal/node` | composition root, same as today's status closure |

Consequences:

- `internal/systemstats` must not import `internal/api` (capability packages never import the API layer); its snapshot is a plain struct, and node's closure copies it into `api.SystemStatus` — the same field-copy-adapter pattern the events seam uses ([FeatureUnifiedApi.md](FeatureUnifiedApi.md) §4).
- `internal/api` imports nothing new; the guard test ([FeatureUnifiedApi.md](FeatureUnifiedApi.md) UA-V-1) keeps passing.
- Seam change: `NewHandlerWithResources` gains a `system func() api.SystemStatus` parameter — the same "plain function" seam shape as the existing `status func() api.Status`. `nil` means the section is omitted even when requested (degenerate builds, tests).

### 5.2 `internal/systemstats`

```
internal/systemstats/
  systemstats.go   // Snapshot struct, Throttled wrapper, doc
  linux.go         // /proc/meminfo, /proc/loadavg, /proc/cpuinfo, Statfs
  darwin.go        // sysctl hw.memsize, vm.page_* counts (fallback vm.stats.vm.*), vm.loadavg, brand_string, Statfs
  other.go         // nulls everywhere (windows, plan9, ...)
```

- One `Collect() Snapshot` per OS file chosen by build tag; `Snapshot` holds plain values (`nil`-able for the unsupported ones).
- Parsers are separated from I/O with injected readers so `/proc` fixtures can be golden-tested on any host (the `/proc/meminfo` of a Raspberry Pi can be a test fixture, for example).
- `Throttled` wraps a collector with the interval, a mutex, a fake-clock seam and the cached snapshot. It is where the "one request every x seconds" invariant lives and is unit-testable without HTTP or a real OS.

### 5.3 Configuration

One new knob in the existing `monitor` block ([internal/models/monitor.go](../internal/models/monitor.go)):

```yaml
monitor:
  enabled: true
  listen: "127.0.0.1:8766"
  event_ring: 8192
  system_interval_sec: 5   # at most one system-details collection per 5 s; 0 = collect on every request
```

- Default `5` when the key is absent (the block's `UnmarshalYAML` already seeds defaults before decoding).
- `Validate` range: `0–86400`.
- The whole `monitor` block is startup-read, so changing the knob follows the existing rule: **restart required** (README).

### 5.4 Composition in node

- `node.New` already receives `dbPath` ([internal/node/node.go](../internal/node/node.go) line 98) but only stores `configPath`; store `dbPath` on the node the same way. `startAPI` takes it from there.
- In `startAPI`, build `systemSnapshot func() api.SystemStatus`: throttle (`monitor.system_interval_sec`) → `systemstats.Collect` → map into `api.SystemStatus`, with `database.path` from the resolved DB path (DBPATH) and `disk.path` from its directory. The status provider itself now lives in `internal/node/status.go`.
- Pass the closure as the new seam argument; `nil` only when the listener itself is built without node context (none today).

## 6. Requirements

| ID | Requirement | Priority | Source |
|---|---|---|---|
| SS-API-1 | `GET /v1/status?system=true` / `?system=1` serve the `system` section; any other value is `400` with the error text of §4.1; no flag leaves the response unchanged. | MUST | §4.1 |
| SS-API-2 | The system snapshot is collected only inside a request that asks for the section; no background sampler exists. | MUST | §4.3 |
| SS-API-3 | The `system` key is absent from responses that did not request it. | MUST | §4.2 |
| SS-COL-1 | Collection uses the Go standard library only, one OS file per build tag; unsupported fields are `null`. | MUST | §4.4, D3 |
| SS-COL-2 | Field semantics as §4.2: memory available = Linux `MemAvailable` semantics; load averages from the OS; disk from the DB's filesystem; DB size from the real files including `-wal`. | MUST | §4.2 |
| SS-COL-3 | Throttle: first request inside an interval collects fresh; others get the cached snapshot; interval from `monitor.system_interval_sec`; `0` disables. | MUST | §4.3 |
| SS-COL-4 | Collection errors degrade the affected fields to `null` and never fail the status request. | MUST | §4.5 |
| SS-CFG-1 | `monitor.system_interval_sec` defaults to `5`, validates `0–86400`, requires a restart to change. | MUST | §5.3 |
| SS-DOC-1 | APIdetails' status section documents the flag, the field table and the throttle; README's monitor block and KAM API rules gain the knob. The `GET /` index row mentions the flag in its purpose text. | MUST | §2 GL-4 |
| SS-TST-1 | Golden parser tests from `/proc` fixtures, host integration tests on the development OSes, `GOOS=windows` compile check, guard test stays green. | SHOULD | §9.4 |

## 7. Decisions

| D | Topic | Alternatives considered | Decision |
|---|---|---|---|
| D1 | How to opt in | (a) query flag on `/v1/status`; (b) new endpoint `GET /v1/system` | **(a) The flag** — the feature was requested as part of status, pollers keep one endpoint, no new route or index row. The existing index convention already carries query params in purpose/path text (`PUT /v1/modem/radio?preview=true`). |
| D2 | Inside the throttle window | (a) serve cached snapshot; (b) reject with `429` | **(a) Cache** — a fast poller must not be punished; no new error class; `collected_at` makes staleness visible. Rejection adds an error surface nothing needs (AGENTS philosophy: simplest thing that works). |
| D3 | Collection dependency | (a) stdlib-only, per-OS build files, Windows `null`; (b) `gopsutil` | **(a) stdlib-only** — six numbers, two well-known OS surfaces, no new dependency (AGENTS: no new dependency unless it clearly pays off). Switch trigger: when a real Windows (or BSD/other) node needs full parity, replace the per-OS files' internals with the metrics library behind the same `Collect` signature — the package boundary makes that a contained swap. |
| D4 | Throttle scope | (a) whole `/v1/status`; (b) `system` section only | **(b) section only** — today's status fields are in-memory counters; only collection is worth metering. |
| D5 | CPU signal | (a) load averages + cores (+ model), no percentages; (b) add usage `%` via sampled deltas in the cache; (c) per-core breakdown | **(a)** — percentages need two samples in time (extra state or a sleep inside the request) and per-OS code (Mach calls on macOS); load average is the portable top-summary signal and answers "is the machine overloaded". A later `usage_percent` (Linux `/proc/stat` delta across the cache window) is an additive field if wanted. |
| D6 | Database size source | (a) `os.Stat` of db + WAL files; (b) `PRAGMA page_count * page_size` | **(a)** — one stat call, includes the WAL naturally, never touches the database handle from a request path. |
| D7 | Disk scope | (a) filesystem of the DB; (b) all mounts; (c) configured filesystem | **(a)** — the only mount KAM can exhaust is the one it writes to (`kam.db`, WAL, room keys, logs). |
| D8 | Knob name and default | `system_interval_sec` / `stats_interval_sec` / `system_throttle_sec`; default 5 vs 2 | **`system_interval_sec: 5`** — reads as "collect at most every x seconds", matches the block's `_sec` suffixes; 5 s is fast enough for a monitor dashboard, slow enough that even a 1 s poller cannot spin the disk. |

## 8. Corrections

None to external sources. Two clarifications worth recording:

- APIdetails' "new fields are only ever added" still holds: `system` is added, but conditionally present (SS-API-3). The status field table must state the condition explicitly when this lands.
- README's "changes to this block require a restart" is inherited, unchanged, by the new knob (SS-CFG-1).

## 9. Impact

### 9.1 Files

| File | Change |
|---|---|
| `internal/models/monitor.go` | + `SystemIntervalSec` field, default, `Validate` range |
| `internal/models/monitor_test.go` (or config_test) | default / range tests |
| `internal/systemstats/` (new) | §5.2 package: `systemstats.go`, `linux.go`, `darwin.go`, `other.go`, `*_test.go` |
| `internal/api/api.go` | `SystemStatus` struct; `system` seam param on `NewHandlerWithResources`; flag handling in the `/v1/status` handler; index row purpose text |
| `internal/api/api_test.go` / guard test | flag on/off/invalid cases; section-absent default case; guard test unchanged (passes) |
| `internal/node/node.go` | store `dbPath` on `Node` |
| `internal/node/api.go` | build the throttled `systemSnapshot` closure and pass the new seam; `internal/node/status.go` assembles status |
| `internal/node/api_test.go` | `/v1/status?system=true` through the real listener |
| `docs/APIdetails.md` | status section: flag, field table, throttle note, example |
| `README.md` | monitor block + one sentence |

### 9.2 Compatibility

- Additive wire change; `api_version` stays `1`. Existing consumers (KAMmon poller, scripts doing `rest_get`, room tooling) are unaffected unless they send the flag.
- No CLI change, no migration, no new background goroutine, no shutdown-order impact.

### 9.3 Performance and security

- Cost without the flag: zero (one `request.URL.Query()` parse and a branch).
- Cost with the flag: a few syscalls and two tiny file reads, at most once per `system_interval_sec` regardless of request rate or concurrency (the 8-slot listener allows parallel requests; the mutex makes them share one snapshot).
- Security: the listener already serves loopback hosts only with the existing transport guards; machine memory/disk/load are host-local facts of the same class as today's counters. No secret is exposed: `database.path` is configuration the local user can already read.

### 9.4 Test plan (acceptance)

1. No flag → response has no `system` key (unmarshal + key-absence check).
2. `?system=true` on the development host → section filled; `database.size_bytes` > 0; `collected_at` within a few seconds of `now`.
3. `?system=yes` → `400` with §4.1's message.
4. Throttle unit test with a fake clock: two calls 1 s apart at a 5 s interval share `collected_at`; a call after the window collects fresh; interval `0` always collects.
5. Model tests: absent key → 5, `0` allowed, `86401` rejected.
6. Golden parser fixtures: `/proc/meminfo` (Raspberry Pi style), `/proc/loadavg`, `/proc/cpuinfo`; darwin parser smoke test on the dev machine.
7. `GOOS=windows go build ./...` passes (stub file active).
8. `internal/api` import guard test still passes; `go vet ./...` clean.

## 10. Open questions

- Is `5` seconds the right default window (D8), or should the throttle be tighter/looser?
- Windows parity now via a metrics library, or accepted as `null` until a Windows node wants it (D3)?
- Should KAM's own process stats (heap, goroutines, RSS) join the section in a later pass (NG-2 mentions the seam)?
