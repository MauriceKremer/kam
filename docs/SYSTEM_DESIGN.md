# KAM — System Design

> KAM is a regional MeshCore analytics + room-server application. It connects
> to any KISS modem flashed with MeshCore firmware, decodes adverts and group
> texts, verifies Ed25519 signatures, persists to SQLite, runs
> operator-managed Starlark automations (ECHO is an example script), hosts
> MeshCore room servers that stock clients can log in to, and exposes operator
> metrics for network behaviour monitoring.

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
- Act as a **MeshCore room server** (advert type `ROOM`): clients log in,
  post, and receive pushed posts, so participants without direct RF reach can
  catch up. This is implemented (see [FeatureRoomServer.md](FeatureRoomServer.md)
  and §10). Mirroring group-text channels into a room is a later step.
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
| **SQLite (WAL, NORMAL sync)** | Zero-ops embedded store — no separate DB process to keep alive during a regional outage. WAL keeps reads responsive while writers batch. Survives power loss with NORMAL sync + 5s busy timeout. The room repository runs its own connection with FULL sync so an acknowledged post survives power loss. |
| **Batched, non-blocking persistence queues** | A burst of RF traffic can never stall packet ingestion; the route loop stays responsive. Drops are counted, not silent. |
| **Hardware mutex + cap-1 waiter channels** | Protects constrained radios with no transaction IDs from queue flooding, so a flaky radio cannot wedge the node. |
| **Per-payload-type parsers in their own files** | New MeshCore payload types (e.g. emergency broadcast, weather) plug in without touching the router. |
| **Automation runtime decoupled from route loop** | Script execution and outbound replies cannot backpressure ingestion; a runaway script is frozen by its timeout. |
| **Graceful, ordered shutdown** | Pending batches flush before the DB handle closes; the node can be restarted cleanly without losing ingested traffic. |
| **Commit-before-ACK for room posts** | A post is written (synchronous=FULL) before its ACK is queued, so a client is never told a post is safe when it is not. |
| **One transmit scheduler** | Group texts, automation replies and room packets share one radio. A single scheduler sends one frame at a time, waits for `TxDone`, orders by priority and keeps pushes and adverts inside an airtime budget, so room traffic cannot starve ACKs or exceed the duty cycle. |

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
internal/config    internal/kiss             internal/db   internal/automation
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
                  internal/models  (KamConfig, RadioConfig, ChannelConfig)
```

Room-server additions (plan phases 1-3):

```
internal/node --starts--> internal/kiss.TxScheduler  (every outbound packet)
              \--starts--> internal/room.Service      (one worker, all rooms)

routeLoop --packet tap (REQ, TXT_MSG, ACK, ANON_REQ, PATH, MULTIPART)--> room.Service.HandlePacket
room.Service --> internal/mesh   (packets, envelope crypto, identity, adverts, region codes)
room.Service --> db.RoomRepository (own SQLite connection, synchronous=FULL)
room.Service --> kiss.TxScheduler --> Manager.txChan --> writeLoop --> radio
send.Sender  --> kiss.TxScheduler (legacy TxChan adapter, queued as group text)
```

### Responsibilities

| Package | Role |
|---|---|
| `main` | Flag parse, slog setup, load `kam.yml`, build `Node`, block on SIGINT/SIGTERM. Also the short-lived `channel upsert` and `room show` commands. |
| `internal/node` | `Node` owns `*kiss.Manager`, `*kiss.RadioClient`, `*kiss.TxScheduler`, `*send.Sender`, the optional `*automation.Runtime` and the optional `*room.Service` with its repository. `New` bootstraps channels from YAML, opens serial, pushes+verifies radio cfg, creates the scheduler, then (best effort) starts the rooms and the automation runtime. `startMonitor` builds the unified local API handler from capability adapters and owns its HTTP server. `Run` blocks until ctx cancelled, then shuts down in the order of §6. |
| `internal/config` | `LoadKamConfig` (YAML + normalize/validate), `BootstrapChannels` (load `#public` + enabled channels from the config into parser topic registry), `UpsertRuntimeChannelAndApply` (write a runtime channel back into the YAML file + apply live), `ResolveDBPath`. |
| `internal/models` | Pure config + record structs. Validation lives here, including `MeshConfig`, `RoomConfig` and `KamConfig.ResolveRooms` (an invalid room is disabled, never fatal). |
| `internal/mesh` | MeshCore wire primitives with no transport dependency: `Packet` parse/encode, `EncryptThenMAC`/`MACThenDecrypt`, `Identity` (Ed25519 + X25519 via `math/big` and `crypto/ecdh`), signed advert payloads, region keys and transport codes, ACK hashes. |
| `internal/kiss` | KISS TNC protocol core: `Manager`, reader/writer/router loops, `RadioClient`, the time-bounded dedup cache, RxMeta hold/attach/flush, the packet tap, and the `TxScheduler` (priority queue, one frame in flight, `TxDone` gate, duty-cycle cap, airtime estimate). |
| `internal/kiss/parsers` | Per-payload-type parsers: `ParseAdvert` (Ed25519 verify), `ParseGroupText` (decrypt via channel secret), public-channel secret registry. |
| `internal/kiss/send` | `Builder` constructs outbound group-text frames; `Sender` queues them on the transmit scheduler. |
| `internal/room` | `Service`: logins and roles, posts, push sync, keep-alive, adverts, login rate limiting, identity files. One worker goroutine owns all room state. |
| `internal/api` | The local REST API's transport only: routes, JSON decode with unknown-field rejection, wire shapes, error class → status mapping, loopback Host guard, 8-slot concurrency limit, 5 s deadline, panic recovery, and the `GET /` index. Imports nothing from this repository except `internal/models`; capabilities reach it through consumer-defined seams (`Scripts`, `EventsSource`, `ChannelReader`, `RoomReader`, the status function), implemented by adapters in `internal/node`. A guard test enforces the import rule. |
| `internal/db` | SQLite handle + WAL. `Persistence` implements `kiss.Persistence`. `AdvertQueue` + `GroupTextQueue` = batched background writers. `AutomationRepository` stores Starlark scripts (`automation_scripts`) on its own single connection. `RoomRepository` stores `rooms`, `room_posts` and `room_clients` on its own single connection with `synchronous=FULL`. Channel config lives in YAML. |
| `internal/events` | The bounded metadata-only event ring, copied into pages under a short lock; the radio and room paths record through `models.MonitorSink`. Nothing here touches HTTP, SQLite or the modem. |
| `internal/automation` | `Runtime` loads, compiles, and runs operator-managed Starlark scripts for each group text on one bounded-queue worker; `reply(channel, text)` is the only host function. It applies `timeout_ms` defaults and exports its domain sentinels (`ErrScriptNotFound`, `ErrScriptConflict`, `ErrScriptNameTaken`) for the transport's adapters. ECHO is the example script `docs/examples/echo.star`. See [FeatureStarlark.md](FeatureStarlark.md). |

> **Extensibility seam.** `Manager.SubscribeGroupText` is the integration point
> for any new consumer: analytics aggregation, room-server fan-out, emergency
> broadcast detection, operator dashboards. Each subscriber gets the decrypted
> message with attached RxMeta (SNR/RSSI) and path info; subscribers must not
> block the route loop (the automation runtime demonstrates the bounded-queue + worker pattern).
> `Manager.SubscribePackets` is the same seam for peer traffic (REQ, TXT_MSG,
> ACK, ANON_REQ, PATH, MULTIPART): it delivers the route type, transport codes,
> path and payload unchanged, and the room service uses it.

---

## 2. Goroutine map

KAM is a single process with several long-lived goroutines plus short-lived
request goroutines on the radio-control path. All long-lived loops exit on
`stopChan` close and are ordered at shutdown.

```
main goroutine
  |
  | node.New(...)
  |   |- config.BootstrapChannels  (sync: load channels from YAML, load registry)
  |   |- db.NewPersistence  -----> [advert writer goroutine]  (db.AdvertQueue)
  |   |                       \--> [group-text writer goroutine] (db.GroupTextQueue)
  |   |- kiss.NewManager (opens serial)
  |   |- manager.Start() -------> [readLoop]    (kiss_reader.go)
  |                        \----> [writeLoop]   (kiss_writer.go)
  |                        \----> [routeLoop]   (kiss_router.go)
  |   |- radio.ApplyRadioConfig  (sync, short-lived: registers waiter, blocks)
  |   |- kiss.NewTxScheduler + Start -> [tx scheduler goroutine]  (kiss/scheduler.go)
  |   |- send.NewSender(scheduler)
  |   |- rooms (best effort; a failure leaves radio ingestion running):
  |   |    db.OpenRoomRepository, room.LoadIdentities, room.NewService
  |   |    service.Start -> [room worker goroutine]   (internal/room)
  |   |    manager.SubscribePackets(service.HandlePacket)
  |   `- automation (best effort; a failure leaves radio ingestion running):
  |        db.OpenAutomationRepository
  |        automation.NewRuntime -> manager.SubscribeGroupText(runtime.Handle)
  |                              -> [automation worker goroutine]   (internal/automation)
  |   |- node.startMonitor -> [local API server goroutine on 127.0.0.1:8766]  (internal/api)
  |
  `- n.Run(ctx):  <-ctx.Done()  (SIGINT/SIGTERM)
       |- local API Shutdown (one server: monitoring and scripts)   (stop intake)
       |- unsubscribe (group text, packets)
       |- runtime.Stop()    (cancel active script, drop queued events, join worker)
       |- rooms.Stop()      (join worker, flush client sessions); close room repository
       |- scheduler.Stop()  (discard queued packets, join goroutine)
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
| 7 | automation `worker` | `automation.Runtime` | `NewRuntime`→`Stop` | recv bounded `queue`, runs scripts sequentially, `reply` sends via `send.Sender` |
| 7a | Local API server | `node.startMonitor` / `api.Listen` | node startup→HTTP shutdown | loopback HTTP on `monitor.listen` (default 8766), with at most 8 admitted requests; bounded event pages, script management, SQLite channel/post reads, and worker-owned room snapshots |
| 7b | tx scheduler | `kiss.TxScheduler` | `Start`→`Stop` | recv legacy `TxChan` frames, a timer, and `TxDone`/`TxAbandoned` modem events; sends one frame at a time to `Manager.txChan` |
| 7c | room worker | `room.Service` | `Start`→`Stop` | recv a bounded packet queue (64) and a 50 ms tick; owns every room's state; writes `RoomRepository`, submits to the scheduler |
| 8 | (planned) analytics/metrics aggregator | future package | subscribe via `SubscribeGroupText` | same tap pattern as the automation runtime |
| * | radio request | caller (e.g. `main`/`New`) | per `Get`/`Set` call | register waiter on `ResponseRegistry`, push `txChan`, block on resp |

> Goroutine 8 is **not yet implemented**; it is recorded here to make the
> intended growth direction explicit. Today the automation worker (7) is the
> only non-persistence consumer of the group-text tap, and the room worker (7c)
> is the only consumer of the packet tap.

The monitor does not add an aggregator goroutine. RX records briefly wait for
RxMeta using a bounded single pending record and timeout callback; TX records
stay with the in-flight writer frame. Room events are emitted by the room worker.
Room HTTP snapshots are copied on that worker rather than reading its maps from
request goroutines. Recording and HTTP page copies share a short ring lock; JSON
encoding, HTTP writes and existing persistence work never hold it. The node
shuts the monitor listener down before the underlying owners.

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

Every outbound data packet goes through `kiss.TxScheduler`. Producers:
- `send.Sender.Send` (group-text data frames, used by the automation worker's
  `reply`) through the scheduler's legacy `TxChan()` adapter, queued as
  `TxPriorityGroupText`;
- `room.Service` through `Submit`, with a priority, an optional delay (the
  reference's 200/300 ms reply delays) and a tag so a queued push can be
  cancelled.

The scheduler validates each packet with `mesh.Parse`, orders eligible packets
by priority (ACKs and direct replies, then flood replies and PATH, then group
texts, then pushes, then adverts), and hands `Manager.txChan` (buffered 10)
exactly one frame at a time. It then waits for the modem's `TxDone` (or the
writer's `TxAbandoned` event when the writer gave up after repeated `TxBusy` or a
gate timeout), with the manager's gate timeout plus 500 ms as a fallback.
Pushes and adverts are deferrable: when the sliding one-hour airtime estimate
(`kiss.LoRaAirtime`) would exceed `duty_cycle_percent` they wait; ACKs, replies
and group texts never do. A full queue (128) evicts the newest deferrable packet
to admit a non-deferrable one and rejects a deferrable one.

`writeLoop` is the sole serial writer and keeps its own `TxDone` gate; control
frames (`RadioClient` requests) use the manager's separate control queue, which
the writer always drains first (see [FeatureKissModem.md](FeatureKissModem.md)).

### 3.4 Rx dispatch + RxMeta hold

`routeLoop` hands each KISS data frame to `routePacket`, which parses it with
`mesh.Parse` (a reserved path hash size, a path over 64 bytes, a missing payload
or a payload over 184 bytes is dropped; so is an unsupported packet version),
slices transport/path bytes, drops duplicates, then dispatches. The dedup key is
the first 8 bytes of SHA-256(payload type | payload), held for 2 minutes in a
cache of at most 512 entries, so the same flood arriving over several routes
counts once while a retry with different attempt bits passes. Room packets KAM
transmits are marked seen, so a repeater's echo is not processed (group-text
echo handling is unchanged).

- `PAYLOAD_TYPE_ADVERT` → `parsers.ParseAdvert` (Ed25519 verify) →
  `Store.EnqueueAdvert` (non-blocking).
- `PAYLOAD_TYPE_REQ`, `TXT_MSG`, `ACK`, `ANON_REQ`, `PATH`, `MULTIPART` → the
  packet tap (`SubscribePackets`): a `kiss.RxPacket` with route type, transport
  codes, encoded `path_len`, path and payload. Handlers must not block; the room
  service enqueues to its bounded queue.
- `PAYLOAD_TYPE_GRP_TXT` → `parsers.ParseGroupText` (decrypt) →
  `holdGroupText`: stage the message, arm a
  `time.AfterFunc(rxMetaTimeout)` timer. The next `SUB_RX_META` (0xF9)
  frame calls `attachRxMeta(snr, rssi)`, attaches radio quality, and
  dispatches. If RxMeta never arrives, `flushPendingGroupText` dispatches
  without it. Dispatch = `Store.EnqueueGroupText` + tap subscribed handlers
  (automation runtime).

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

### 3.6 Automation runtime

`automation.Runtime` is fully decoupled from the route loop. `Handle` (called
inline by `dispatchGroupText`) clones the message and does a non-blocking
enqueue to a bounded queue (128); a full queue drops the event and logs it.
A single worker runs every enabled `READY` script sequentially in script-ID
order, each in a fresh Starlark thread bounded by the script's `timeout_ms`
(default 500 ms). A script that exceeds it is marked `FROZEN` (in memory
first, then persisted) until an operator recovers it. Compiled programs stay
resident; SQLite is touched only at startup, on API mutations, and on
`BROKEN`/`FROZEN` transitions. The only host function is `reply`, which goes
through `send.Sender` with a context-aware 500 ms budget.

Shutdown order matters: the runtime is stopped (active script cancelled,
queued events dropped, worker joined) **before** `manager.Stop()` so scripts
cannot enqueue TX frames into a write loop that has already exited.

### 3.7 Room service

`room.Service` follows the same decoupling. `HandlePacket` (called inline by the
packet tap) does a non-blocking enqueue to a bounded queue (64) and counts what
it drops. One worker goroutine owns every room's state, so no room state needs a
lock: it processes packets and runs a 50 ms tick for time-driven work.

Per room the worker keeps the client table (public key, shared secret, role,
replay timestamp, cursor, stored route, push state), the newest
`backlog_posts` posts (the only posts the push engine can ever send, so it never
queries SQLite), and the timers for adverts, session flush and retention.

- **Receive.** A direct packet that still has hops left is ignored (a forwarder's
  business) except an ACK. `ANON_REQ` is tried against every room whose first key
  byte matches `dest_hash`; a peer packet is tried against every client with a
  matching `src_hash` until a MAC verifies.
- **Posts.** The post is committed to SQLite (`synchronous=FULL`, together with
  the author's replay timestamp) and only then is its ACK queued. If the write
  fails there is no ACK and the client's timestamp is not advanced, so its retry
  is processed as new.
- **Push.** Round robin, one client per pass, at most one unacknowledged push per
  client, 1.2 s after a push and 150 ms when idle. A push that never left the
  scheduler queue by its ACK timeout (for example held back by the duty-cycle
  cap) is withdrawn without counting as a failure.
- **Persistence.** Client sessions, including nullable `last_heard_at`, are
   written in batches every 5 s and on stop; posts are not batched. Restoring a
   last-heard timestamp does not mark a client active for push delivery. Retention
   prunes by count and age every 10 minutes.
- **Monitoring.** Room counters and client/post summaries are copied through a
   request handled by the worker. The monitor reads retained posts and advert
   labels from SQLite outside the room-state ownership boundary.
- **Failure isolation.** A room that cannot start is logged and skipped; radio
  ingestion continues.

The worker is stopped after the packet subscription is removed and **before**
the scheduler and the manager, so its last flush and any queued ACKs still have
a write path.

---

## 4. End-to-end data flow

### 4.1 Inbound advert

```
radio ──bytes──> readLoop ──KISS frame──> rxChan ──> routeLoop
   |-> ParseAdvert (Ed25519 verify)
   |-> advert.HopCount = numHops; advert.HopStations = pathStations  (from path frame)
   |-> Store.EnqueueAdvert  ──> AdvertQueue.ch ──> writerLoop ──> SQLite adverts
                                                              (hop_count, hop_stations)
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
        automation.Handle ──clone──> bounded queue ──> worker ──> scripts ──> reply ──> send.Sender.Send
              ──> TxScheduler ──> txChan ──> writeLoop ──> radio
```

### 4.3 Outbound group text (script reply)

```
automation.worker ──> reply ──> send.Sender.Send(ctx, channel, body)
   |-> Builder.Build (encrypt with channel secret)
   |-> frame = [CMD_DATA][raw...]
   `-> TxScheduler ──> manager.txChan ──> writeLoop ──> serial.Write ──> radio
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

### 4.5 Room login, post and push

```
radio ──> readLoop ──> rxChan ──> routeLoop ──routePacket──> dedup ──> packet tap
   `-> room.HandlePacket ──> bounded queue ──> room worker

   ANON_REQ (login):
     rate limit ──> X25519 shared secret ──> MAC ──> authorize (password -> role)
     ──> client table (replay check, evict least recently active non-admin)
     ──> RESPONSE (direct) or PATH return carrying the response (flood)
     ──> TxScheduler (delay 300 ms)

   TXT_MSG (post):
     MAC ──> role check (read-write or admin) ──> RoomRepository.AppendPost (commit)
     ──> push cache ──> ACK ──> TxScheduler (delay 200 ms)

   tick (every 50 ms):
     next client in the round robin ──> oldest unsynced post, at least 6 s old
     ──> TXT_MSG (signed plain) ──> TxScheduler (push priority, cancellable tag)

   ACK, PATH extra or MULTIPART ACK:
     match a client's pending ACK ──> cursor = pushed post, failures = 0
```

```
Client                                 KAM room (ADV_TYPE_ROOM, default scope)
  | --- ANON_REQ login(t, since, pw) -->|  rate limit; ECDH; replay check
  | <-- PATH(RESPONSE) / RESPONSE ------|  13-byte response; role in byte 7
  | --- PATH(route, [ACK]) ------------>|  stores the route; no reciprocal path
  |                                     |
  | --- TXT_MSG(plain) post ----------->|  commit to SQLite, then ACK
  | <-- ACK(sha256) --------------------|  hash over timestamp, flags, text, client key
  |                                     |
  |                 [round robin tick]  |  one unacknowledged push per client
  | <-- TXT_MSG(signed) post -----------|  expected ACK recorded before sending
  | --- ACK(sha256) ------------------->|  cursor advances; next push
  |                                     |
  | --- REQ(KEEP_ALIVE, since?) ------->|  direct only: ACK + unsynced count
```

---

## 5. Shared state and locks

| State | Guard | Notes |
|---|---|---|
| `hwTxMutex` | `sync.Mutex` | One in-flight hardware request. Held by `RadioClient.Get/Set`. |
| `ResponseRegistry.waiters` | `sync.Mutex` | Waiter channels are `cap 1` so dispatch never blocks. |
| `onGroupText` handlers | `onGroupTextMu sync.RWMutex` | Route loop reads (RLock); subscribe/unsubscribe writes. Unsubscribe nils the slot, doesn't shrink slice. |
| `pendingGroupText` + timer | `pendingMu sync.Mutex` | Single in-flight group text awaiting RxMeta. |
| `dedupCache` | `sync.Mutex` | Checked by `routeLoop`, marked by the scheduler (`MarkSeen`). Bounded: 2 minutes, 512 entries. |
| `onPacket` handlers | `onPacketMu sync.RWMutex` | Same pattern as `onGroupText`. |
| `TxScheduler` queue, airtime ledger, in-flight flag | `sync.Mutex` | Producers submit from any goroutine; one run goroutine sends. Modem events reach it through a buffered channel and never block the event loop. |
| Room state (client tables, post caches, timers, push state) | none (room worker only) | Counters are atomics, so `Stats()` is safe from any goroutine. |
| `RoomRepository` | single SQLite connection | Writes first in every transaction, so a concurrent commit by the persistence connection cannot fail it with `SQLITE_BUSY_SNAPSHOT`. |
| `AdvertQueue.ch` / `GroupTextQueue.ch` | channel | Bounded; enqueue non-blocking. |
| automation `queue` | channel | Bounded (128); enqueue non-blocking, drops are logged. |
| automation `scripts` (compiled programs) | `sync.RWMutex` + `mutationMu` | Worker snapshots under RLock; API mutations and freezes serialize on `mutationMu` and publish a new entry only after the SQLite write commits. |
| `stopChan` | `close()` | Signal-all for the three Manager loops. Each queue/service has its own stop signal. |

---

## 6. Shutdown ordering

`Node.Run` blocks on `<-ctx.Done()` (SIGINT/SIGTERM), then:

1. Shut down the local API (one HTTP server; it serves monitoring and script
   management), then unsubscribe the group-text and packet handlers (stop
   intake), then `automation.Runtime.Stop()` (cancel the active script, drop
   queued events, join the worker, wait for in-flight mutations) and close the
   automation repository, all **before** the TX path goes away.
2. `room.Service.Stop()` joins the worker after a final flush of client
   sessions; then close the room repository.
3. `TxScheduler.Stop()` discards queued packets and joins its goroutine.
4. `manager.Stop()`:
   1. `close(stopChan)` — `readLoop`, `writeLoop`, `routeLoop` exit.
   2. `Store.Close()`:
      - `AdvertQueue.Stop()` — drain + final flush, wait `done`.
      - `GroupTextQueue.Stop()` — drain + final flush, wait `done`.
      - `db.Close()`.
   3. `stream.Close()`.

Ordering invariant: automation and rooms before the scheduler, the scheduler
before the manager, persistence flush before DB close, DB close before serial
close.

---

## 7. SQLite

One file (`kam.db`) holds `adverts`, `group_texts`, the automation table
`automation_scripts` (script source, timeout, status, last error, revision),
and the room tables `rooms` (id and public key), `room_posts` (room, server
timestamp, author key, text) and `room_clients` (role, cursor, replay
timestamp, stored route), with a `room_schema_version` table for migrations.
Channel configuration is **not** persisted here — it lives in `kam.yml` (see
`internal/config`). Opened with
WAL mode, 5s busy timeout, NORMAL synchronous mode; the room repository sets
FULL on its own connection so a post survives power loss before it is
acknowledged. Schemas and tuning
constants live in `internal/db` (see README "Advert Persistence" and
"Group Text Persistence" for full DDL and metrics).

**Backups.** The database now contains operator script source. Include it in
operational backups, and copy it consistently: take an online
`sqlite3 kam.db ".backup kam-backup.db"`, or copy `kam.db` together with its
`-wal` and `-shm` files while the node is stopped. Restoring a backup restores
scripts and their enabled state.

**Room identity keys are not in the database.** Each room's Ed25519 seed is a
hex file at `<database directory>/rooms/<id>.key`. The public key
is the room's address: losing the file means clients no longer recognise the
room, so back the `rooms` directory up with the database. Passwords are in
`kam.yml` in plain text, like channel keys (decision D11 in
[FeatureRoomServer.md](FeatureRoomServer.md)).

---

## 8. Operator metrics & analytics

A core goal of KAM is to give a regional operator visibility into the mesh so
they can understand actual network behaviour — not just decode traffic, but
*characterise* it. The signals already captured in the ingestion path:

| Signal | Source | Already persisted? |
|---|---|---|
| Payload type (advert / group text / …) | `routeLoop` header decode | advert + group-text rows; other types logged only |
| Route type (flood / direct / transport) | header bits | not persisted |
| Hop count + per-hop path hashes | path frame | advert rows (`hop_count`, `hop_stations`) + group-text rows (`HopCount`, `HopStations`) |
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

### 8.2 Room-server role

As a room server, KAM now:
- Answers logins, stores posts durably (acknowledged posts survive a restart or
  power loss), and pushes them to logged-in clients (§3.7, §4.5).
- Keeps persisting every decrypted group-text channel in `group_texts` (a
  separate feature; channels are not mirrored into rooms).

Still open: local REST API endpoints for clients, roles, posts and
forced adverts; a `ServerStats`-compatible status reply (which would let
clients and other KAM nodes read this node's metrics); history and live-tail
surfaces for operators; and re-broadcasting emergency/weather advisories via
`send.Sender` (trigger policy and authorisation are open design questions).

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
2. **Room administration and history surfaces** — the room server is
   implemented; local REST API endpoints for clients, roles, posts and
   forced adverts (plan phase 4) and a `group_texts` history/live-tail surface
   are not.
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

---

## 10. MeshCore room-server protocol (reference)

> **Status.** KAM implements this behaviour for plan phases 1-3 (login, roles,
> posts, push sync, keep-alive, adverts, session persistence); see
> [FeatureRoomServer.md](FeatureRoomServer.md) for requirements, decisions and
> every deviation. Statements below that were checked against the pinned
> firmware source and found wrong are marked *(corrected)*.

MeshCore has **no single room-server spec doc**. The behaviour is defined by
three sources that must be read together:

1. `docs/payloads.md` — advert flag `0x03 is room server`, and the **Room
   server login** payload under *Anonymous request*.
2. `docs/companion_protocol.md` — client side: `CMD_SEND_LOGIN` (0x1A) and
   `PUSH_CODE_LOGIN_SUCCESS`/`FAIL`.
3. Reference firmware `examples/simple_room_server/MyMesh.cpp` — the actual
   server state machine (the authoritative source; this section tracks the
   `main`-branch version).

A room server is a MeshCore node advertising `ADV_TYPE_ROOM`. It is a **BBS**:
persists posts in a cyclic queue and pushes new posts to logged-in clients.
The reference room server defaults to `disable_fwd = 1`, so it does not forward
mesh packets unless configured to *(corrected)*; KAM never forwards. Traffic is **direct**
(ECDH-derived shared secret, `PAYLOAD_TYPE_ANON_REQ`/`REQ`/`RESPONSE`/
`TXT_MSG`) — *not* group text. KAM's group-text path does not capture it.

### 10.1 Constants (main-branch)

| Constant | Value | Meaning |
|---|---|---|
| `MAX_UNSYNCED_POSTS` | 32 | cyclic post queue size |
| `MAX_POST_TEXT_LEN` | 151 | buffer size; `StrHelper::strncpy` keeps at most **150 bytes** *(corrected)* |
| `MAX_CLIENTS` | 20 | client table size; the least recently active non-admin is evicted when full *(corrected: omitted before)* |
| `POST_SYNC_DELAY_SECS` | 6 | settle before a post is pushable |
| `PUSH_NOTIFY_DELAY_MILLIS` | 2000 | post-add → first push window |
| `SYNC_PUSH_INTERVAL` | 1200 | round-robin push cadence |
| `PUSH_ACK_TIMEOUT_FLOOD` | 12000 ms | ACK wait when pushing via flood |
| `PUSH_TIMEOUT_BASE` / `FACTOR` | 4000 / 2000 | direct ACK wait = base + factor×(hops+1) |
| `flood_max` / `flood_max_unscoped` / `flood_max_advert` | 64 / 64 / 8 | forward caps |
| `flood_advert_interval` | 47h | flood advert cadence |
| `LAZY_CONTACTS_WRITE_DELAY` | 5000 ms | ACL flash-write debounce |

### 10.2 Permission model

| Role | Granted by | Perm constant | Capabilities |
|---|---|---|---|
| Admin | `_prefs.password` | `PERM_ACL_ADMIN` | full CLI, post, telemetry, ACL mgmt |
| Read-write | `_prefs.guest_password` ("room password") | `PERM_ACL_READ_WRITE` | post + read |
| Read-only | no password, `_prefs.allow_read_only` | `PERM_ACL_GUEST` | read only, no ACK on posts |

Note the counter-intuitive naming: the **guest password** grants
**read-write**, and a passwordless login is the true **guest/read-only**.

The reference blocks posting only for role `GUEST` (0); `READ_ONLY` (1) can
post *(corrected)*. KAM enforces the intent instead: only read-write and admin
clients post (decision D3).

### 10.3 Login flow

Client → server: `PAYLOAD_TYPE_ANON_REQ`, ciphertext plaintext = Room server
login:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | sender timestamp (replay nonce) |
| 4 | 4 | `sync_since` (client's last-synced watermark) |
| 8 | N | password (null-terminated) |

Server (`onAnonDataRecv`):
1. **Blank-password fast path**: `password==""` and sender already in ACL →
   accept as re-key/re-handshake (no password check).
2. Else match `password`→admin, `guest_password`→read-write, else (if
   `allow_read_only`)→guest. Mismatch → **no response** (client times out).
3. Replay guard: `sender_timestamp <= client->last_timestamp` → reject.
4. `acl.putClient`; store ECDH `shared_secret`; set `sync_since`, reset
   `pending_ack`/`push_failures`; mark the client dirty. The reference persists
   **admin entries only** *(corrected)*; KAM persists every client session in
   batches every 5 s (decision D4).
5. If request was flood, `out_path_len = OUT_PATH_UNKNOWN` (force rediscovery).
6. Reply `PAYLOAD_TYPE_RESPONSE`:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | server timestamp (clock sync) |
| 4 | 1 | `RESP_SERVER_LOGIN_OK` (0) |
| 5 | 1 | legacy keep-alive interval |
| 6 | 1 | admin? 1 : (permissions==0 ? 2 : 0) |
| 7 | 1 | permissions bitmask |
| 8 | 4 | random blob (packet uniqueness) |
| 12 | 1 | `FIRMWARE_VER_LEVEL` |

If the login arrived via **flood**, the reply is wrapped in a **Returned
Path** (`PAYLOAD_TYPE_PATH`) so the client learns the route to the server and
can switch to `sendDirect`. Flood replies are **region-scoped**
(`sendFloodReply` reuses the request's region transport codes when known).

### 10.4 Post submission (client → server)

Client → server: `PAYLOAD_TYPE_TXT_MSG` (encrypted with shared secret),
plaintext = plain-text-message: `timestamp(4) | (txt_type<<2)|attempt(1) | msg`.

Server `onPeerDataRecv` decodes `txt_type`:
- `TXT_TYPE_PLAIN` (0) → `addPost()` into cyclic queue (read-write and admin
  by intent; the reference rejects only role `GUEST`, see §10.2). Schedules
  `next_push`.
- `TXT_TYPE_CLI_DATA` (1) → admin-only CLI command; reply sent back as
  `TXT_MSG`. Non-admin → no ACK, no reply.
- `TXT_TYPE_SIGNED_PLAIN` (2) → server-side only (used for pushes, §10.5).

Replay rule for `TXT_MSG`: `sender_timestamp >= last_timestamp` (equal =
retry → ACK but do **not** re-add the post). `REQ` drops
`sender_timestamp < last_timestamp`, the same boundary; it simply has no retry
case *(corrected: it is not stricter)*.

Server ACKs with 4-byte truncated SHA256 of `{timestamp, text, sender
pubkey}`. Direct if `out_path` known (optionally a 2nd `createMultiAck`
+300ms when `multi_acks` enabled), else region-scoped flood.

### 10.5 Push sync (server → client)

Server **actively pushes**; clients do not poll. `loop()` round-robins over
ACL clients (`next_client_idx`). For each client with `pending_ack==0`,
`last_activity!=0`, `push_failures<3`:

Scan cyclic posts for one where `post_timestamp > client.sync_since` AND
`author != client` (don't echo own posts) AND `now >= post_timestamp +
POST_SYNC_DELAY_SECS`. `pushPostToClient` builds `PAYLOAD_TYPE_TXT_MSG`:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | original `post_timestamp` |
| 4 | 1 | `(TXT_TYPE_SIGNED_PLAIN<<2)|attempt` |
| 5 | 4 | author pubkey prefix (first 4 bytes) |
| 9 | N | post text |

`pending_ack = SHA256(payload ‖ client.pubkey)` (4-byte truncated). Send:
`sendDirect` via `out_path` if known (ACK wait `4000+2000×(hash_count+1)` ms,
`hash_count = out_path_len & 63`), else `sendFloodScoped(default_scope)`
(ACK wait 12000 ms). Round-robin advances; next pass in 1200 ms after a push,
else 150 ms (skip to next client fast).

### 10.6 ACK → sync advancement

Client ACKs with the 4-byte hash. `processAck` matches against `pending_ack`:
1. `pending_ack = 0` (allow next push).
2. `push_failures = 0`.
3. **`sync_since = push_post_timestamp`** — post marked delivered.

ACK timeout before next iteration → `push_failures++`, `pending_ack=0`.
After 3 failures the client starves until it shows authenticated activity
(login, `TXT_MSG` or `REQ`). The reference does **not** reset `out_path` at that
point; only a flood login clears it *(corrected)*. KAM does reset it (RS-PUSH-6)
so the next exchange rediscovers the route. When the client table is full the
least recently active non-admin is evicted.

### 10.7 Keep-alive + status/telemetry

`REQ_TYPE_KEEP_ALIVE` (0x02, direct only): refreshes `last_activity`,
optionally force-sets `sync_since`, clears `pending_ack`, and the ACK appends
an unsynced-count byte so the client learns its backlog.

Other `REQ` types handled by `handleRequest`:
- `GET_STATUS` (0x01) → `ServerStats` struct: battery, TX queue, noise floor,
  last RSSI/SNR, RX/TX packet counts, flood/direct sent+recv, dup counts,
  err flags, `n_posted`, `n_post_push`, uptime, airtime. **This is exactly
  the operator-metrics payload KAM wants to aggregate (§8).**
- `GET_TELEMETRY_DATA` (0x03) → CayenneLPP (guests get base-only mask).
- `GET_ACCESS_LIST` (0x05, admin only) → admin entries (6-byte pubkey prefix
  + perms).

### 10.8 Region / flood scoping

Newer firmware adds a region map (`region_map`, `TransportKey`,
`default_scope`). `ROUTE_TYPE_TRANSPORT_FLOOD` packets carry 2 transport
codes; `filterRecvFloodPacket` resolves the packet's region and
`sendFloodReply`/`sendFloodScoped` reuse the request's scope for replies so
floods stay within a region. `allowPacketForward` enforces `flood_max`,
`flood_max_unscoped`, and `flood_max_advert` (adverts capped at 8 hops to
limit propagation). KAM's router already slices the transport-codes block;
room-server replies must preserve scoping.

### 10.9 Compact sequence

```
Client                                 Server (ADV_TYPE_ROOM, region-scoped)
  | --- ANON_REQ login(t, since, pw) -->|  ECDH shared secret; replay check
  | <-- RESPONSE(t,OK,perm,fw_ver) ----|  + Returned Path if login was flood
  |                                    |
  | --- TXT_MSG(plain) post ----------->|  addPost() -> cyclic queue; sched push
  | <-- ACK(sha256) -------------------|  hash = SHA256(ts+text+pubkey)
  |                                    |
  |                  [loop round-robin]|  pick unsynced post for this client
  | <-- TXT_MSG(signed) post ----------|  pending_ack = SHA256(payload|pubkey)
  | --- ACK(sha256) ------------------>|  sync_since = post_timestamp; next push
  |                                    |
  | --- REQ(KEEP_ALIVE, since?) ------->|  ACK + unsynced_count (direct only)
  | --- REQ(GET_STATUS) -------------->|  ServerStats  -> operator metrics (§8)
```

### 10.10 KAM implementation

How each piece of the reference behaviour maps onto KAM (plan phases 1-3):

1. **ANON_REQ handling + ECDH session** — the router parses every packet with
   `mesh.Parse` and delivers `ANON_REQ`, `REQ`, `TXT_MSG`, `ACK`, `PATH` and
   `MULTIPART` to the packet tap; `internal/room` keeps the client table (the ACL
   analogue). The shared secret is X25519 over the Ed25519 keys, byte-compatible
   with `ed25519_key_exchange` and checked against the MeshCore test key pair.
2. **Crypto stays in KAM** — the modem's `EncryptData`/`DecryptData` take a
   caller-supplied key, so offloading them gains nothing *(corrected: this was
   suggested earlier)*, and the modem cannot be given a chosen identity. Identity,
   signing and key exchange run in `internal/mesh` (decision D1).
3. **Post store + push state machine** — `room_posts` in SQLite plus an in-memory
   cache of the newest posts; per-client `sync_since`, pending ACK and failure
   count; the round-robin push engine runs on the room worker (§3.7).
4. **Path learning** — `PATH` stores the client's route; `PATH` extras, plain
   `ACK` and `MULTIPART` ACKs all advance the cursor.
5. **Region scoping** — replies reuse the request's region when it matches a
   configured one; adverts and pushes use `mesh.default_scope`. Region keys are
   `SHA-256("#" + name)[:16]`.
6. **Operator metrics** — not done: `GET_STATUS` is unsupported (no reply),
   so KAM does not yet report `ServerStats`. A KAM client that polls other room
   servers for status remains a future item.