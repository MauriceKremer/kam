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
                                    EchoConfig)
```

### Responsibilities

| Package | Role |
|---|---|
| `main` | Flag parse, slog setup, load `kam.yml`, build `Node`, block on SIGINT/SIGTERM. |
| `internal/node` | `Node` owns `*kiss.Manager`, `*kiss.RadioClient`, `*send.Sender`, `*echo.Service`. `New` bootstraps channels from YAML, opens serial, pushes+verifies radio cfg. `Run` blocks until ctx cancelled, then stops echo before manager. |
| `internal/config` | `LoadKamConfig` (YAML + normalize/validate), `BootstrapChannels` (load `#public` + enabled channels from the config into parser topic registry), `UpsertRuntimeChannelAndApply` (write a runtime channel back into the YAML file + apply live), `ResolveDBPath`. |
| `internal/models` | Pure config + record structs. Validation lives here. |
| `internal/kiss` | KISS TNC protocol core: `Manager`, reader/writer/router loops, `ResponseRegistry`, `RadioClient`, dedup filter, RxMeta hold/attach/flush. |
| `internal/kiss/parsers` | Per-payload-type parsers: `ParseAdvert` (Ed25519 verify), `ParseGroupText` (decrypt via channel secret), public-channel secret registry. |
| `internal/kiss/send` | `Builder` constructs outbound group-text frames; `Sender` enqueues them onto `Manager.txChan`. |
| `internal/db` | SQLite handle + WAL. `Persistence` implements `kiss.Persistence`. `AdvertQueue` + `GroupTextQueue` = batched background writers. Advert/group-text schemas only (channel config lives in YAML). |
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
  |   |- config.BootstrapChannels  (sync: load channels from YAML, load registry)
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

One file (`kam.db`) holds `adverts` and `group_texts` only. Channel
configuration is **not** persisted here — it lives in `kam.yml` (see
`internal/config`). Opened with
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

---

## 10. MeshCore room-server protocol (reference)

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
persists posts in a cyclic queue, pushes new posts to logged-in clients, and
also selectively forwards mesh packets. Traffic is **direct**
(ECDH-derived shared secret, `PAYLOAD_TYPE_ANON_REQ`/`REQ`/`RESPONSE`/
`TXT_MSG`) — *not* group text. KAM's group-text path does not capture it.

### 10.1 Constants (main-branch)

| Constant | Value | Meaning |
|---|---|---|
| `MAX_UNSYNCED_POSTS` | 32 | cyclic post queue size |
| `MAX_POST_TEXT_LEN` | 151 | 160 − 9-byte overhead |
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
   `pending_ack`/`push_failures`; mark ACL dirty (lazy 5s write).
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
- `TXT_TYPE_PLAIN` (0) → `addPost()` into cyclic queue (admin/read-write
  only; read-only → no ACK, rejected). Schedules `next_push`.
- `TXT_TYPE_CLI_DATA` (1) → admin-only CLI command; reply sent back as
  `TXT_MSG`. Non-admin → no ACK, no reply.
- `TXT_TYPE_SIGNED_PLAIN` (2) → server-side only (used for pushes, §10.5).

Replay rule for `TXT_MSG`: `sender_timestamp >= last_timestamp` (equal =
retry → ACK but do **not** re-add the post). `REQ` uses the stricter
`sender_timestamp < last_timestamp` → reject.

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
After 3 failures the client starves; `out_path` reset to `OUT_PATH_UNKNOWN`
on next flood contact triggers path rediscovery. Inactive clients evicted
(LRU) when the ACL table fills.

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

### 10.10 Implications for KAM

Acting as a room server in KAM means implementing, on the Go side over the
existing `kiss.Manager` + `send.Sender`:

1. **ANON_REQ handling + ECDH session** — `PAYLOAD_TYPE_ANON_REQ` (payload
   type 0x07) is not yet routed in `kiss_router.go` (only advert + group
   text). Add a parser + a client/session table (the ACL analogue).
2. **Crypto offload to the modem** — the KISS `SetHardware` extensions
   (`kiss_modem_protocol.md`) already expose `KeyExchange` (X25519, 0x07),
   `EncryptData`/`DecryptData` (AES-128 + 2-byte HMAC-SHA256 MAC, 0x05/0x06),
   `SignData`/`VerifySignature` (Ed25519, 0x04/0x03), and `Hash` (SHA-256,
   0x08). A Go room-server can drive these via `Manager.registry` request/response
   instead of reimplementing the crypto natively. This keeps KAM a thin,
   auditable bridge.
3. **Post store + push state machine** — cyclic buffer + per-client
   `sync_since`/`pending_ack`/`push_failures` + round-robin push loop. A
   natural new goroutine alongside the echo worker (§2, goroutine 8).
4. **Path learning** — `onPeerPathRecv` stores `out_path`; KAM must handle
   `PAYLOAD_TYPE_PATH` (0x08) and `PAYLOAD_TYPE_ACK` (0x03) to advance sync.
5. **Region scoping** — preserve transport codes on flood replies so a KAM
   room server's traffic stays within its region.
6. **Operator metrics** — a KAM client can `REQ_TYPE_GET_STATUS` to room
   servers/repeaters it sees and aggregate `ServerStats` into the §8 surface,
   giving cross-node congestion/airtime/dup visibility without each node
   self-reporting.