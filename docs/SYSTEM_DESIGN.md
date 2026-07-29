# KAM — System Design

> KAM is a regional MeshCore analytics + room-server application. It connects
> to any KISS modem flashed with MeshCore firmware, decodes adverts and group
> texts, verifies Ed25519 signatures, persists to SQLite, optionally echoes
> mentions back to the mesh, and exposes operator metrics for network
> behaviour monitoring.

This document describes the runtime architecture: modules, goroutines, data
flow, and concurrency controls. Source of truth is the code; this is a map.

---

## 0. Project intent & scope

### 0.1 Purpose

KAM is built to be a **regional service** for communities that need a backup
messaging and broadcast channel for when traditional messaging infrastructure
(cellular, internet) becomes unavailable. Concretely it aims to:

- Share **emergency broadcasts** across a MeshCore mesh region.
- Relay **weather updates** and other public-safety advisories.
- Act as a **room server**: aggregate and serve group-text rooms over a
  region so participants without direct RF reach can still follow traffic.
- Provide **operator-facing analytics** so a human operator can watch network
  health, congestion, and traffic composition in real time.

### 0.2 Hardware target

- Any **KISS TNC modem flashed with MeshCore firmware**.
- Today only **USB-connected serial devices** are tested (e.g. Heltec-style
  boards exposed as `/dev/cu.usbserial-*` / `/dev/ttyUSB*`).
- The serial/KISS layer is hardware-agnostic by design; non-USB transports
  (TCP KISS, Bluetooth) are expected to work once a `serial.Port`-equivalent
  is provided, but are **not yet validated**.

### 0.3 Non-goals (today)

- KAM is **not** a general-purpose chat client UI. It is a server-side node.
- It is **not** a router/repeater firmware replacement; it is a consumer and
  optional producer of mesh traffic, not a relay hop.

### 0.4 Scope status

**The scope of the project is not final.** The technology choices below are
made to keep that optionality open: a regional, fault-tolerant service that
can grow new payload types, new persistence sinks, and new operator surfaces
without restructuring the core. Treat the module/goroutine maps in this
document as the *current* shape, not a contract.

### 0.5 Technology choices for robustness & fault tolerance

| Choice | Why it matters for a regional backup service |
|---|---|
| **Go** | Single static binary, trivial to deploy on a Pi / NUC / laptop at a regional site. Cheap concurrency for the multi-loop architecture. |
| **SQLite (WAL, NORMAL sync)** | Zero-ops embedded store — no separate DB process to keep alive during a regional outage. WAL keeps reads responsive while writers batch. Survives power loss with NORMAL sync + 5s busy timeout. |
| **Batched, non-blocking persistence queues** | A burst of RF traffic can never stall packet ingestion; the route loop stays responsive. Drops are counted, not silent. |
| **Hardware mutex + cap-1 waiter channels** | Protects constrained radios with no transaction IDs from queue flooding, so a flaky radio cannot wedge the node. |
| **Per-payload-type parsers in their own files** | New MeshCore payload types (e.g. emergency broadcast, weather) plug in without touching the router. |
| **Echo service decoupled from route loop** | Optional producer traffic cannot backpressure ingestion. |
| **Graceful, ordered shutdown** | Pending batches flush before the DB handle closes; the node can be restarted cleanly without losing ingested traffic. |

---

## 1. Module map

```
                        main.go  (flags, slog, signal handling)
                           |
                           v
                  internal/node  (Node lifecycle: New + Run)
                           |
   +-----------------------+-----------------------+----------+
   |                       |                       |          |
   v                       v                       v          v
internal/config    internal/kiss             internal/db   internal/echo
 (YAML load,        (Manager + TNC I/O +       (SQLite     (mention reply
  bootstrap,         router + parsers +        persistence service)
  dbpath)            registry + radio)          queues)
                           |
        +------------------+----------------+----------+
        |                  |                |          |
        v                  v                v          v
  kiss_reader.go    kiss_writer.go   kiss_router.go  cmd_radio.go
        |                  |                |          |
        v                  v                v          v
    serial.Read       serial.Write     parsers/       RadioClient
    -> rxChan         <- txChan        (advert,       (Get/Set/Apply
                                        group_text)    via registry)
                           |
                           v
                  internal/kiss/send  (Builder + Sender: outbound group text)
                           |
                           v
                  internal/models  (KamConfig, RadioConfig, ChannelConfig,
                                    EchoConfig, ChannelRecord)
```

### Responsibilities

| Package | Role |
|---|---|
| `main` | Flag parse, slog setup, load `kam.yml`, build `Node`, block on SIGINT/SIGTERM. |
| `internal/node` | `Node` owns `*kiss.Manager`, `*kiss.RadioClient`, `*send.Sender`, `*echo.Service`. `New` bootstraps DB, opens serial, pushes+verifies radio cfg. `Run` blocks until ctx cancelled, then stops echo before manager. |
| `internal/config` | `LoadKamConfig` (YAML + normalize/validate), `BootstrapChannels` (upsert seed channels into SQLite, load `#public` + enabled channels into parser topic registry), `ResolveDBPath`. |
| `internal/models` | Pure config + record structs. Validation lives here. |
| `internal/kiss` | KISS TNC protocol core: `Manager`, reader/writer/router loops, `ResponseRegistry`, `RadioClient`, dedup filter, RxMeta hold/attach/flush. |
| `internal/kiss/parsers` | Per-payload-type parsers: `ParseAdvert` (Ed25519 verify), `ParseGroupText` (decrypt via channel secret), public-channel secret registry. |
| `internal/kiss/send` | `Builder` constructs outbound group-text frames; `Sender` enqueues them onto `Manager.txChan`. |
| `internal/db` | SQLite handle + WAL. `Persistence` implements `kiss.Persistence`. `AdvertQueue` + `GroupTextQueue` = batched background writers. Channel/advert/group-text schemas. |
| `internal/echo` | `Service` subscribes to inbound group texts, replies to mentions via a dedicated worker. First example of a *consumer* of the group-text tap; the operator analytics/metrics surface is expected to subscribe the same way. |

> **Extensibility seam.** `Manager.SubscribeGroupText` is the integration point
> for any new consumer: analytics aggregation, room-server fan-out, emergency
> broadcast detection, operator dashboards. Each subscriber gets the decrypted
> message with attached RxMeta (SNR/RSSI) and path info; subscribers must not
> block the route loop (echo demonstrates the bounded-queue + worker pattern).

---

## 2. Goroutine map

KAM is a single process with several long-lived goroutines plus short-lived
request goroutines on the radio-control path. All long-lived loops exit on
`stopChan` close and are ordered at shutdown.

```
main goroutine
  |
  | node.New(...)
  |   |- config.BootstrapChannels  (sync: open DB, upsert seeds, load registry)
  |   |- db.NewPersistence  -----> [advert writer goroutine]  (db.AdvertQueue)
  |   |                       \--> [group-text writer goroutine] (db.GroupTextQueue)
  |   |- kiss.NewManager (opens serial)
  |   |- manager.Start() -------> [readLoop]    (kiss_reader.go)
  |                        \----> [writeLoop]   (kiss_writer.go)
  |                        \----> [routeLoop]   (kiss_router.go)
  |   |- radio.ApplyRadioConfig  (sync, short-lived: registers waiter, blocks)
  |   |- send.NewForManager
  |   `- if echo enabled:
  |        echo.New -> manager.SubscribeGroupText(echo.Handle)
  |                 -> [echo worker goroutine]   (internal/echo)
  |
  `- n.Run(ctx):  <-ctx.Done()  (SIGINT/SIGTERM)
       |- echo.Stop()       (close stopCh, wait doneCh)
       |- manager.Stop()
       |    |- close(stopChan)  -> readLoop, writeLoop, routeLoop exit
       |    |- Store.Close()
       |    |    |- AdvertQueue.Stop()    (drain + flush, wait done)
       |    |    `- GroupTextQueue.Stop() (drain + flush, wait done)
       |    `- stream.Close()
       `- return
```

### Goroutine inventory

| # | Goroutine | Owner | Lifecycle | Channel I/O |
|---|---|---|---|---|
| 1 | `main` | `main.go` | process lifetime | signal ctx |
| 2 | `readLoop` | `kiss.Manager` | `Start`→`Stop` | reads `serial`, sends `rxChan` |
| 3 | `writeLoop` | `kiss.Manager` | `Start`→`Stop` | recv `txChan`, writes `serial` |
| 4 | `routeLoop` | `kiss.Manager` | `Start`→`Stop` | recv `rxChan`, dispatches to registry/parsers/Store/handlers |
| 5 | advert `writerLoop` | `db.AdvertQueue` | `NewAdvertQueue`→`Stop` | recv `ch`, batch insert into SQLite |
| 6 | group-text `writerLoop` | `db.GroupTextQueue` | `NewGroupTextQueue`→`Stop` | recv `ch`, batch insert into SQLite |
| 7 | echo `worker` | `echo.Service` | `New`→`Stop` | recv `queue`, builds + sends reply |
| 8 | (planned) analytics/metrics aggregator | future package | subscribe via `SubscribeGroupText` | same tap pattern as echo |
| * | radio request | caller (e.g. `main`/`New`) | per `Get`/`Set` call | register waiter on `ResponseRegistry`, push `txChan`, block on resp |

> Goroutine 8 is **not yet implemented**; it is recorded here to make the
> intended growth direction explicit. Today echo (7) is the only non-persistence
> consumer of the group-text tap.

---

## 3. Concurrency model

### 3.1 Hardware mutex (`hwTxMutex`)

MeshCore radios are constrained: no transaction IDs and a small internal
queue. `Manager.hwTxMutex` makes only one KISS request in-flight at a time.
Any caller doing bidirectional hardware I/O (`RadioClient.Get/Set`,
`ApplyRadioConfig`) takes `LockDevice`/`UnlockDevice`. Because only one
goroutine holds the lock, the next response frame is unambiguously theirs.

### 3.2 Response registry (`internal/kiss/registry.go`)

`readLoop`→`routeLoop` runs continuously. When a `CMD_SETHARDWARE` response
arrives, `routeLoop` calls `registry.Dispatch(subCmd, payload)`, which routes
to the single registered waiter channel. Waiter channels are buffered
(`cap 1`) so the parser loop **never blocks** even if the caller already
abandoned the request on context timeout. `Register`/`Deregister` bracket
each request.

### 3.3 Tx path

Outbound frames go through `Manager.txChan` (buffered 10). Producers:
- `RadioClient.Set` (control frames)
- `send.Sender.Send` (group-text data frames), used by echo worker

`writeLoop` is the sole serial writer, so no extra serialization is needed
for the data path. Control-path serialization is the `hwTxMutex`.

### 3.4 Rx dispatch + RxMeta hold

`routeLoop` decodes the 1-byte header (`routeType` | `payloadType` |
`version`), slices transport/path bytes, dedups by SHA-256 of the app
payload (FIFO of 10), then dispatches:

- `PAYLOAD_TYPE_ADVERT` → `parsers.ParseAdvert` (Ed25519 verify) →
  `Store.EnqueueAdvert` (non-blocking).
- `PAYLOAD_TYPE_GRP_TXT` → `parsers.ParseGroupText` (decrypt) →
  `holdGroupText`: stage the message, arm a
  `time.AfterFunc(rxMetaTimeout)` timer. The next `SUB_RX_META` (0xF9)
  frame calls `attachRxMeta(snr, rssi)`, attaches radio quality, and
  dispatches. If RxMeta never arrives, `flushPendingGroupText` dispatches
  without it. Dispatch = `Store.EnqueueGroupText` + tap subscribed handlers
  (echo).

`pendingMu` guards the single pending message + timer. Only one group
text is held at a time; a new message flushes the previous one.

### 3.5 Persistence queues

`db.Persistence` implements `kiss.Persistence`:
`EnqueueAdvert` / `EnqueueGroupText` / `Close`. Both queues are bounded
channels with non-blocking enqueue (drop + counter on full). Each queue has
one writer goroutine that batches (advert: 50 / 1s; group-text analogous)
and inserts transactionally. `Stop` drains the channel then flushes the
final batch before returning.

```
routeLoop ──EnqueueAdvert──>  AdvertQueue.ch  ──>  advert writerLoop  ──>  SQLite (adverts)
routeLoop ──EnqueueGroupText─> GroupTextQueue.ch ──> gt writerLoop      ──>  SQLite (group_texts)
```

### 3.6 Echo service

`echo.Service` is fully decoupled from the route loop. `Handle` (called
inline by `dispatchGroupText`) only does cheap filters + non-blocking
enqueue to its own bounded queue. The worker goroutine does the
potentially-blocking `Build`+`Send` sequentially. This guarantees the
route loop never blocks on radio TX backpressure.

Shutdown order matters: `echo.Stop()` runs **before** `manager.Stop()` so
the worker cannot enqueue TX frames into a write loop that has already
exited.

---

## 4. End-to-end data flow

### 4.1 Inbound advert

```
radio ──bytes──> readLoop ──KISS frame──> rxChan ──> routeLoop
   |-> ParseAdvert (Ed25519 verify)
   |-> Store.EnqueueAdvert  ──> AdvertQueue.ch ──> writerLoop ──> SQLite adverts
```

### 4.2 Inbound group text

```
radio ──bytes──> readLoop ──> rxChan ──> routeLoop
   |-> ParseGroupText (decrypt via channel secret)
   |-> holdGroupText  (arm RxMeta timer)
   |         |
   |         +-- next 0xF9 frame ──> attachRxMeta(snr,rssi) ──> dispatchGroupText
   |         `-- timeout          ──> flushPendingGroupText  ──> dispatchGroupText
   |
   dispatchGroupText:
   |-> Store.EnqueueGroupText ──> GroupTextQueue.ch ──> writerLoop ──> SQLite group_texts
   `-> subscribed handlers:
        echo.Handle ──filter──> echo.queue ──> echo.worker ──> send.Sender.Send
              ──> txChan ──> writeLoop ──> radio
```

### 4.3 Outbound group text (echo reply)

```
echo.worker ──> send.Sender.Send(ctx, channel, body)
   |-> Builder.Build (encrypt with channel secret)
   |-> frame = [CMD_DATA][raw...]
   `-> manager.txChan ──> writeLoop ──> serial.Write ──> radio
```

### 4.4 Radio configuration (startup)

```
main/node.New ──> RadioClient.ApplyRadioConfig(ctx, cfg)
   |-> Set(ctx, &cfg)   [LockDevice; register SUB_RESP_OK; txChan<- set frame]
   |                    writeLoop ──> serial ──> radio
   |                    radio ──> readLoop ──> rxChan ──> routeLoop ──> registry.Dispatch
   |                    <-respChan (OK)
   |-> Get(ctx)         [LockDevice; register SUB_RESP_RADIO; txChan<- get frame]
   |                    ... same round trip ...
   `-> returns verified RadioConfig (read back from hardware)
```

---

## 5. Shared state and locks

| State | Guard | Notes |
|---|---|---|
| `hwTxMutex` | `sync.Mutex` | One in-flight hardware request. Held by `RadioClient.Get/Set`. |
| `ResponseRegistry.waiters` | `sync.Mutex` | Waiter channels are `cap 1` so dispatch never blocks. |
| `onGroupText` handlers | `onGroupTextMu sync.RWMutex` | Route loop reads (RLock); subscribe/unsubscribe writes. Unsubscribe nils the slot, doesn't shrink slice. |
| `pendingGroupText` + timer | `pendingMu sync.Mutex` | Single in-flight group text awaiting RxMeta. |
| `seenHashes` dedup FIFO | none (route loop only) | Accessed only from `routeLoop`. |
| `AdvertQueue.ch` / `GroupTextQueue.ch` | channel | Bounded; enqueue non-blocking. |
| echo `queue` | channel | Bounded; enqueue non-blocking. |
| `stopChan` | `close()` | Signal-all for the three Manager loops. Each queue/service has its own stop signal. |

---

## 6. Shutdown ordering

`Node.Run` blocks on `<-ctx.Done()` (SIGINT/SIGTERM), then:

1. `echo.Stop()` — close `stopCh`, wait `doneCh`. Drains echo queue
   **before** the TX path goes away.
2. `manager.Stop()`:
   1. `close(stopChan)` — `readLoop`, `writeLoop`, `routeLoop` exit.
   2. `Store.Close()`:
      - `AdvertQueue.Stop()` — drain + final flush, wait `done`.
      - `GroupTextQueue.Stop()` — drain + final flush, wait `done`.
      - `db.Close()`.
   3. `stream.Close()`.

Ordering invariant: echo before manager, persistence flush before DB close,
DB close before serial close.

---

## 7. SQLite

One file (`kam.db`) holds `channels`, `adverts`, `group_texts`. Opened with
WAL mode, 5s busy timeout, NORMAL synchronous mode. Schemas and tuning
constants live in `internal/db` (see README "Advert Persistence" and
"Group Text Persistence" for full DDL and metrics).

---

## 8. Operator metrics & analytics

A core goal of KAM is to give a regional operator visibility into the mesh so
they can understand actual network behaviour — not just decode traffic, but
*characterise* it. The signals already captured in the ingestion path:

| Signal | Source | Already persisted? |
|---|---|---|
| Payload type (advert / group text / …) | `routeLoop` header decode | advert + group-text rows; other types logged only |
| Route type (flood / direct / transport) | header bits | not persisted |
| Hop count + per-hop path hashes | path frame | group-text rows (`HopCount`, `HopStations`) |
| Radio RX quality (SNR, RSSI) | `SUB_RX_META` (0xF9) | group-text rows (`SNR`, `RSSI`, `HasRxMeta`) |
| Clock drift | advert/group-text parser | both row types (`time_drift_seconds`) |
| Sender / channel / body | group-text parser | group-text rows |
| Node identity, node type, nickname, location | advert parser | advert rows |
| Duplicate suppression | `seenHashes` FIFO | counter only (drop logged) |
| Persistence queue depth / drops / flushes | `AdvertQueue.Metrics()`, `GroupTextQueue` counters | in-memory counters, not yet exported |

### 8.1 Network-congestion monitoring (intended)

Congestion is inferred from the combination of:
- **Dedup drop rate** — same advert/text seen N times ⇒ flooding/saturation.
- **Persistence queue depth + drops** — ingestion outrunning SQLite ⇒ burst.
- **RxMeta SNR/RSSI distribution per channel/hop** — link-quality regressions.
- **Message-type mix over time** — a spike in `GRP_TXT` vs `ADVERT` ratio, or
  a new payload type appearing, indicates a behavioural shift.
- **Clock drift across senders** — nodes drifting apart may miss windows.

None of these rollups are exposed yet. The data is captured at ingestion; the
missing piece is an aggregator goroutine (goroutine 8 in §2) that taps the
same `SubscribeGroupText` stream plus the queue `Metrics()` counters and
periodically snapshots them to an operator-facing surface.

### 8.2 Room-server role (intended)

As a room server, KAM is expected to:
- Persist every decrypted group-text room durably (already done —
  `group_texts` table).
- Expose room history + live tail to operators / bridged clients (API surface
  not yet built).
- Optionally **re-broadcast** emergency/weather advisories onto relevant
  channels via `send.Sender` (same path echo uses). Trigger policy and
  authorisation are open design questions.

### 8.3 Broadcast / emergency & weather (intended)

A regional backup service must originate traffic, not only consume it. The
outbound primitive already exists (`send.Sender.Send(ctx, channel, body)` →
`txChan` → `writeLoop` → radio). What is missing is the policy layer:
- Who is allowed to issue an emergency broadcast?
- Which channels carry weather vs emergency vs ops?
- Repeat/relay rules for reach across the region.
- Throttling so the originating node does not flood the mesh.

This is explicitly out of scope for the current code but is the reason the
outbound path was built generically.

---

## 9. Roadmap & open scope

The project scope is not final. The following are **directions the
architecture is shaped to allow**, not commitments:

1. **Operator metrics API / dashboard** — export queue metrics + traffic
   rollups. Aggregator goroutine taps `SubscribeGroupText` + queue
   `Metrics()`.
2. **Room-server HTTP/WebSocket surface** — serve `group_texts` history and
   live tails from SQLite.
3. **New payload-type parsers** — `internal/kiss/parsers/<type>.go` per type.
   The router is a pure dispatcher; emergency/weather payloads plug in here.
4. **Originating broadcasts** — emergency/weather/ops via `send.Sender`, with
   policy + throttle layer.
5. **Non-USB transports** — TCP KISS / Bluetooth behind the `serial.Port`
   interface used by `readLoop`/`writeLoop`.
6. **Separate SQLite files per table** — if SQLite write contention becomes
   the bottleneck under heavy regional load, the store path in `main.go`
   can split adverts/group-texts into separate DB files (the queues already
   own their own handles conceptually).
7. **Retention / compaction** — regional nodes will accumulate; a retention
   policy over `adverts` and `group_texts` is not yet defined.
8. **Multi-radio / multi-channel ingestion** — one process, several
   `*kiss.Manager` instances. `Node` currently holds one; the struct shape
   does not forbid more.