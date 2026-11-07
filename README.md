# kam

MIT-licensed Go bridge for MeshCore KISS TNC / LoRa radios. Built to make MeshCore truly KickAss (KAM) by providing a robust, local brain for decoding, persistence, and automation.

New to KAM or MeshCore? Start with the [KAM manual](manuals/KAM.html) for
purpose, design philosophy, radio-network basics, setup, and everyday operation.

## License

MIT — see [LICENSE](LICENSE).

## Philosophy & Intent

KAM is built for enthusiasts. We take a down-to-earth approach: we prioritize ease of setup and maintenance so you can spend your time on the mesh, not fighting your software.

We value pragmatism over overly strict design or extreme security hardening. KAM is meant to be a reliable, local "brain" for your radio—not a fortified vault. It's built to be tinkered with, extended, and used to contribute to something bigger.

## Quickstart

### Prerequisites

- Go 1.25+ (matches `go.mod` directive)
- A MeshCore-compatible KISS TNC / LoRa radio (e.g. Heltec) connected over USB serial
- OS serial driver present (most platforms auto-load; Linux may need the `dialout` group: `sudo usermod -aG dialout $USER`)

### Install

```sh
git clone https://github.com/<you>/kam.git
cd kam
go build .
```

Or, if the module path is resolvable from your GOPROXY:

```sh
go install kam.bytecoder.nl@latest
```

### Configure

Copy the example config and edit the radio port for your OS:

```sh
cp kam.yml kam.yml.local   # run with `-config kam.yml.local` to use the copy
```

```yaml
version: 1
node_name: "kam"
db_path: "kam.db"
radio:
  port: "/dev/cu.usbserial-0001"   # macOS; Linux usually /dev/ttyUSB0
  baud: 115200
  frequency_hz: 869618000
  bandwidth_hz: 62500
  spreading_factor: 7
  coding_rate: 5
channels:
  - name: "#public"
    key_strategy: "public"
    key_hex: "8b3387e9c5cdea6ac9e5edbaa115cd72"  # MeshCore public PSK, public by design
    enabled: true
```

See [Channel Credentials Storage Design](#channel-credentials-storage-design) below for the full `key_strategy` semantics (`public` / `derived` / `explicit`).

### Run

```sh
./kam                       # uses ./kam.yml and ./kam.db
./kam -port /dev/ttyUSB0    # override serial port for this run
./kam -db /tmp/kam.db        # override SQLite path for this run
./kam -config ~/kam-prod.yml
DEBUG=1 ./kam               # enable debug logs
```

Press `Ctrl+C` to stop. The manager flushes pending SQLite batches and closes the port on shutdown.

### Run tests

```sh
go test ./...               # all packages
go test -race ./...         # with race detector (CI runs this)
gofmt -l .                  # should print nothing
go vet ./...
```

### Flags

| Flag      | Default     | Description |
|-----------|-------------|-------------|
| `-config` | `kam.yml`   | Path to YAML config file |
| `-db`     | _empty_     | SQLite path; overrides `kam.yml db_path`; falls back to `kam.db` |
| `-port`   | _empty_     | Override `radio.port` for this run |

DB path precedence: `-db` flag > `kam.yml db_path` > `kam.db`.

Other commands: `kam channel upsert ...` (add or change a channel in `kam.yml`) and `kam room show` (print room keys and adverts, see [Room servers](#room-servers)).

## Local REST API

KAM serves one loopback REST API — the **KAM API** — on `127.0.0.1:8766`. It
hosts the read-only data surfaces — `GET /v1/status` and `GET /v1/events`,
channel history (`GET /v1/channels`, `GET /v1/channels/{name}/messages`) and
room data (`GET /v1/rooms`, `GET /v1/rooms/{id}/clients`,
`GET /v1/rooms/{id}/posts`) — plus, on the same listener, script management
([Automation](#automation-starlark-scripts)) and modem monitoring and radio
control: `GET /v1/modem` (a cached snapshot refreshed every 10 s),
`POST /v1/modem/query` (one read-only command) and `PUT /v1/modem/radio`
(validate, apply, check the echo, save `radio:` to `kam.yml`; `?preview=true`
lists the steps without sending). The listener answers `GET /` with the routes
it serves. No dashboard is required: KAM continues its radio work without API
clients. Room-client removal and dry-run endpoints remain planned; parameters
and responses are documented in [`docs/APIdetails.md`](docs/APIdetails.md).

```yaml
monitor:
  enabled: true
  listen: "127.0.0.1:8766"
  event_ring: 8192
  system_interval_sec: 5
```

These are the defaults when the block is omitted. `enabled: false` closes the
listener and turns the event recorder off; automation scripts keep running, but
they can no longer be managed or observed until the listener is back.
`GET /v1/status` serves an extra `system` section (machine memory, CPU load,
disk and database size) when a client asks with `?system=true`; the section is
collected at most once every `system_interval_sec` seconds (`0` disables the
throttle). Invalid
`monitor` settings or a busy port are
logged without stopping radio ingestion. Changes to this block require a restart.
The API has no authentication and serves loopback hosts only; KAMmon, the
separate web-monitor project, owns its web authentication. JSON events contain
metadata, never message bodies or keys.

```sh
curl --fail-with-body http://127.0.0.1:8766/v1/status
curl --fail-with-body http://127.0.0.1:8766/v1/modem
curl --fail-with-body 'http://127.0.0.1:8766/v1/events?after=0&limit=1000'
```

Continue using the returned `epoch` and `next_after`, not `last_seq` (the newest
event in KAM's ring). An empty `events` array means caught up. `reset: true`
means KAM restarted or unread events were overwritten. Signal readings and failed
TX airtime are `null` when unknown. Events are held in bounded memory only;
restarting KAM loses the ring. A radio change through `PUT /v1/modem/radio`
may take about 15 s (and rewrites `kam.yml`, dropping comments just like
`kam channel upsert`); its `?preview=true` form answers instantly. Endpoint
parameters and responses are documented
in [`docs/APIdetails.md`](docs/APIdetails.md); see also [the KAM API manual](manuals/FeatureKamApi.html)
and [the contract](docs/FeatureKamApi.md).

## Automation (Starlark scripts)

KAM runs operator-managed [Starlark](https://github.com/google/starlark-go) scripts for every decoded group text and, if a script asks for one, on a timer. Scripts live in SQLite (`automation_scripts`), not in `kam.yml`, and are managed at runtime through a local HTTP API. Design and status: [`docs/FeatureStarlark.md`](docs/FeatureStarlark.md).

```python
def on_group_text(message):
    if message["channel"] == "#public" and "ping" in message["body"]:
        reply(message["channel"], "pong, %d hops" % message["hop_count"])
```

- `message` is a read-only dict: `channel`, `sender`, `body`, `timestamp_unix`, `hop_count`, `hop_stations`, `has_rx_meta`, `snr_quarter_db`, `rssi_dbm`, `time_drift_seconds`.
- Host functions: `reply(channel, text)` answers on air, `room_post(room_id, text)` writes into one of KAM's own rooms, and `rest_get(url, headers=...)` fetches an HTTP(S) URL and returns `(body, status)`. `json` and `time` are available as modules. Scripts have no filesystem or process access, and keep no state between events.
- Set `timer_interval_minutes` at the top of the source and define `def on_timer(message)` to run on a timer instead of (or as well as) on group text. One minute is the shortest interval; the timer only runs while the script is enabled and `READY`. [`docs/examples/periodic.star`](docs/examples/periodic.star) is a working weather post built this way.
- Events are queued (128) and run one at a time, so a slow script never blocks packet routing; a full queue drops events and logs them.
- Each script has a `timeout_ms` (default 500). A script that exceeds it becomes `FROZEN`; one that fails to compile on load becomes `BROKEN`. Neither runs until you recover or fix it. Other script errors are only logged.

### Managing scripts over the local REST API

Script management lives on the same listener as the read-only data surfaces:
`monitor.listen` (default `127.0.0.1:8766`), loopback only, no authentication.
Requests need a loopback `Host` header and, for `POST`/`PUT`, `Content-Type: application/json`. Updates carry the revision you last read (`expected_revision`; `0`/omitted when creating) and fail with `409` if it is stale. The listener answers `GET /` with the routes it serves; endpoint details live in [`docs/APIdetails.md`](docs/APIdetails.md).

```sh
curl -s localhost:8766/scripts
curl -s -X POST localhost:8766/scripts -H 'Content-Type: application/json' \
  -d '{"script":{"id":"ping","name":"ping","enabled":true,"source":"def on_group_text(message):\n    reply(message[\"channel\"], \"pong\")\n}}'
curl -s -X POST localhost:8766/scripts/ping/disable -H 'Content-Type: application/json' -d '{"expected_revision":1}'
```

| Method and path | Purpose |
|---|---|
| `GET /` | Show the routes this listener serves. |
| `GET /scripts`, `GET /scripts/{id}` | List or read scripts, including `runtime_status` and `last_error`. |
| `POST /scripts/validate` | Compile a source without saving it. |
| `POST /scripts`, `PUT /scripts/{id}` | Create or replace a script (`{"script": {...}, "expected_revision": N}`); invalid source is rejected and never activated. |
| `POST /scripts/{id}/enable`, `.../disable` | Change the desired state only; a `FROZEN` or `BROKEN` script stays inactive. |
| `POST /scripts/{id}/recover` | Recompile the stored source, return to `READY`, optionally set a new `timeout_ms`. |
| `DELETE /scripts/{id}?revision=N` | Delete a script. |

### ECHO as a script

The mention responder (`heard alice in 2 hops, SNR 7.25 dB, RSSI -64 dBm, drift 3s`) is the example script [`docs/examples/echo.star`](docs/examples/echo.star), not a built-in service, and `kam.yml` has no `echo:` section (an old block is ignored). Edit the three settings at the top of the file (`CHANNELS`, `MENTION`, `NODE_NAME`), then install it:

```sh
jq -n --rawfile source docs/examples/echo.star '{script: {id: "echo", name: "echo", enabled: true, source: $source}}' \
  | curl -s -X POST localhost:8766/scripts -H 'Content-Type: application/json' -d @-
```

### Backups

`kam.db` now holds your script source and room posts. Include it in backups, using `sqlite3 kam.db ".backup kam-backup.db"` while running, or copying `kam.db` with its `-wal` and `-shm` files while stopped. If you run room servers, also back up the `rooms/` directory next to the database: it holds each room's identity key, and losing it changes the room's address.

## Room servers

KAM can host one or more MeshCore **room servers**: bulletin boards that stock MeshCore apps and radios can find, log in to, post in, and receive pushed posts from. Requirements, decisions and the places where KAM differs from the reference firmware are in [`docs/FeatureRoomServer.md`](docs/FeatureRoomServer.md); the [room server manual](manuals/FeatureRoomServer.html) is the beginner-friendly version.

Add a `rooms:` list (and optionally a `mesh:` block) to `kam.yml` and restart KAM; room settings are read at start-up only.

```yaml
mesh:
  default_scope: "#region"     # region for adverts; leave out to send unscoped floods
  duty_cycle_percent: 10       # airtime cap for pushes and adverts (0 = off)
rooms:
  - id: lobby
    name: "KAM Lobby"
    admin_password: "change-me-now"
    guest_password: "letmein"     # read-write; or set open_room: true and leave it out
    allow_read_only: false        # any other password logs in as a guest who can read but not post
```

```sh
./kam room show              # prints each room's public key, contact URI and signed advert
./kam room show -config ~/kam-prod.yml -db /var/lib/kam/kam.db
```

`kam room show` creates a missing identity key, so you can run it before the first start. Import the room into a MeshCore client with the printed `meshcore://contact/add?...` URI (as a QR code or link) or let it discover the advert; KAM sends one advert at start-up, a zero-hop advert every 2 minutes and a flood advert every 47 hours by default.

| Role | How a client gets it | Can read | Can post |
|---|---|---|---|
| Admin | `admin_password` | yes | yes |
| Read-write | `guest_password` (or a blank password with `open_room: true`) | yes | yes |
| Guest | any other password, only with `allow_read_only: true` | yes | no |

A wrong password gets no answer at all (the client times out); that is how MeshCore works.

| Setting | Default | Meaning |
|---|---|---|
| `id` | required | Lowercase letters, digits, `-`, `_` (at most 32); must start with a letter or digit. Names the identity file `rooms/<id>.key`. |
| `name` | required | Advert name, at most 31 bytes (23 when a `location` is set). |
| `admin_password` | required | At most 15 bytes (clients truncate longer ones); never `password`. |
| `enabled` | `true` | `false` leaves the room out of KAM's config entirely (key file untouched). |
| `guest_password`, `open_room`, `allow_read_only` | none / `false` / `false` | See the table above. `open_room: true` needs an empty `guest_password`. |
| `location` | none | `latitude` and `longitude` advertised to clients. |
| `max_clients` | 20 | Client table size (1-100). The least recently active non-admin is evicted when full. |
| `advert_interval_min` | 2 | Zero-hop advert interval; 0 turns it off. |
| `flood_advert_interval_hours` | 47 | Flood advert interval; 0 turns it off, otherwise 3-168. |
| `max_posts`, `max_age_days` | 1000, 30 | Retention; 0 means unlimited. A background job prunes every 10 minutes. |
| `backlog_posts` | 32 | The newest posts a returning client is sent, however old its cursor is (1-256). |
| `mesh.path_hash_size` | 1 | Bytes per path hash on floods KAM sends. Keep 1 unless every repeater runs firmware 1.14 or newer. |
| `mesh.default_scope`, `mesh.regions` | none | Region used for adverts and for replies whose request scope is unknown; extra regions recognised on inbound floods. |
| `mesh.duty_cycle_percent` | 10 | Estimated airtime per hour allowed for pushes and adverts. ACKs and login replies are never held back. |

At most 10 rooms run. A room with invalid settings is reported in the log and stays disabled; the other rooms and radio ingestion keep running.

Things to know:

- Passwords live in plain text in `kam.yml`, the same as explicit channel keys. The only secret outside `kam.yml` is each room's identity key, a file KAM generates in `rooms/` next to the database. Back it up with the database: if it is lost, the room gets a new address. `.gitignore` excludes it.
- A post is stored before it is acknowledged, so an acknowledged post survives a restart or power loss. Posts are limited to 150 bytes, a client receives one push at a time, and each push costs roughly half a second of airtime at SF7 / 62.5 kHz, so a 32-post catch-up takes about 17 seconds per client.
- KAM trusts the host clock. Run NTP or an RTC: if the clock steps back across a restart, clients ignore adverts and posts that are not newer than the last ones they stored, and nothing on KAM's side shows it.
- Not yet implemented: status, telemetry and access-list requests (no reply), the over-the-air CLI, role changes and forced adverts over the local REST API, and `mesh` changes without a restart.

## Architecture TL;DR

The Go-specific module/goroutine map lives in
[`docs/SYSTEM_DESIGN.md`](docs/SYSTEM_DESIGN.md). The full KAM API surface is
[`docs/APIdetails.md`](docs/APIdetails.md).

### Device Limitations
The KISS infrastructure interfaces with constrained hardware devices (e.g., TNCs, LoRa radios). The primary limitations are:
1. **Queue Overload Prevention**: While the hardware device may maintain an internal queue, sending too many commands in rapid succession before responses are received can overwhelm its limited memory buffer. Furthermore, because this API is specifically designed to service a single primary consumer at a time, enforcing an application-level bottleneck acts as a strict preventative measure against inadvertent queue flooding.
2. **Missing Transaction IDs**: The hardware protocol does not natively support transaction IDs tagging requests to responses. This means multiple clients waiting for a generic "OK" (0xF0) response cannot be safely differentiated based solely on the incoming byte stream.

### Architecture Approach
To provide a clean, thread-safe, and asynchronous Go API (e.g., `cmd_radio.go`), while protecting the constrained hardware, the system employs the following:

1. **Request Gate (`requestGate`)**: `internal/kiss` serializes radio commands with a capacity-one channel semaphore. Any API call that talks to the hardware must take the single request slot before sending.
   - *Impact*: Enforces that only one command is "in-flight" at a time, protecting the hardware buffer from overrun.
   - *Impact*: Alleviates the need for transaction IDs, as any incoming hardware response (e.g., `OK` or `Error`) is guaranteed to belong to the single request that currently holds the slot.
2. **Pending-Request Slot**: The `Manager` tracks at most one pending radio request (`requestMu`/`pending` in `kiss_manager.go`, `pendingRequest` in `cmd_radio.go`). The frame router hands the response bytes of that one request type to its channel (buffered to 1) and records the result there.
   - *Safety*: The result channel is buffered so the fast-running router loop never blocks, even if the API caller abandoned the request due to a context timeout.
3. **Context-Aware API**: High-level Go clients interact via synchronous interfaces using standard `context.Context` semantics (`func Set(ctx, ...)`). If the request slot cannot be acquired or the hardware fails to respond within the expected deadline, the API quickly unwinds and returns an error without stalling the caller.
4. **Payload Router (`internal/kiss/kiss_router.go`)**: Incoming `CMD_DATA` frames are decoded and dispatched by `routeLoop`. It extracts the header bits (`routeType`, `payloadType`, `version`) and routes to a dedicated per-type handler (e.g., `internal/kiss/parsers/advert.go`). `kiss_router.go` owns no parsing logic itself — it is a pure dispatcher. Each payload type lives in its own `internal/kiss/parsers/<type>.go` file, keeping parsing isolated and individually testable.

## Channel Credentials Storage Design

System configuration lives in **one place**: the human-editable YAML file
`kam.yml`. There is no channel state in SQLite — the durable store is used
only for ingested traffic (adverts, group texts).

At startup the YAML file is parsed, normalized, and validated, and the
configured channels are loaded directly into the in-memory parser topic
registry. When the application needs to add or change a channel at runtime,
it **writes the change back into the YAML file** (atomically) and applies it
to the registry without a restart. The YAML file is therefore both the
bootstrap and the runtime source of truth.

### YAML file format

File name: `kam.yml`

```yaml
version: 1
node_name: "kam"
db_path: "kam.db"
radio:
  port: "/dev/cu.usbserial-0001"
  baud: 115200
  frequency_hz: 869618000
  bandwidth_hz: 62500
  spreading_factor: 7
  coding_rate: 5
channels:
  - name: "#public"
    key_strategy: "public"
    key_hex: "8b3387e9c5cdea6ac9e5edbaa115cd72"
    enabled: true

  - name: "#mesh-dev"
    key_strategy: "derived"
    enabled: true

  - name: "#ops"
    key_strategy: "explicit"
    key_hex: "00112233445566778899aabbccddeeff"
    enabled: true
```

### Radio settings

The `radio` block configures the default serial port and LoRa parameters used at startup:
- `port` — serial device path.
- `baud` — serial baud rate.
- `frequency_hz` — center frequency in Hz.
- `bandwidth_hz` — LoRa bandwidth in Hz.
- `spreading_factor` — LoRa spreading factor.
- `coding_rate` — LoRa coding rate.
- `tx_power_dbm` — TX power in dBm, 1–22 (optional; the modem keeps its current power when omitted).
- `csma` — optional channel-access timing: `txdelay_ms` and `slot_time_ms` (multiples of 10, 0–2550; defaults 500 / 100), `persistence` (0–255, default 63), `full_duplex` (default `false`). Supplied, it must be the complete object.

These values are pushed to the hardware during `main.go` initialization.

### Key strategy semantics

1. `public`
   - Reserved channel `#public`.
   - Uses the current built-in public secret behavior.
   - `key_hex` is required and must decode to 16 bytes.
2. `derived`
   - Secret is derived from normalized channel name using current SHA256-based logic.
   - `key_hex` must be empty.
3. `explicit`
   - `key_hex` is required.
   - Accept 32 hex chars (16 bytes) or 64 hex chars (32 bytes).
   - If 16 bytes are provided, runtime expands to a 32-byte secret by zero-padding the remaining 16 bytes to preserve current behavior.

### Normalization and validation rules

1. `node_name` is required (trimmed, non-empty).
2. Channel names are trimmed, lowercased, and must start with `#`.
3. Empty name and bare `#` are invalid.
4. `#public` must use `key_strategy: public`.
5. Names are unique after normalization.
6. `enabled: false` keeps the channel in the YAML file but excludes it from the in-memory decryption registry.

### Runtime channel upsert

Channels can be added or updated at runtime via `node.UpsertChannel(...)`
(or `config.UpsertRuntimeChannelAndApply(configPath, channel)` directly).
The channel is normalized, merged into the loaded config (inserted or
replaced by normalized name), and the whole `kam.yml` is rewritten atomically
(temp file + rename). The updated channel is then applied to the parser
registry live. `#public` is structural and cannot be upserted at runtime —
edit the file directly.

To update channels from a separate terminal while KAM is running, use the CLI
subcommand. The running node watches the YAML file and applies valid channel
changes without restarting the radio or database:

```sh
./kam channel upsert -config kam.yml -name '#ops' -key-strategy derived
./kam channel upsert -config kam.yml -name '#ops' -key-strategy explicit \
  -key-hex 00112233445566778899aabbccddeeff -enabled=true
./kam channel upsert -config kam.yml -name '#ops' -key-strategy derived -enabled=false
```

The watcher checks for changes every 500 ms, validates the full YAML, then
replaces the in-memory channel registry atomically. Invalid YAML is logged and
ignored, so the last valid channel registry remains active. This live reload
currently applies channel settings only; radio, node, database, and echo
settings still require a restart. `#public` must still be edited directly.

### Go models

The parsed `kam.yml` is the `KamConfig` in [`internal/models/models.go`](internal/models/models.go), which also holds `RadioConfig` (with `tx_power_dbm` and the `csma` block), `ChannelConfig`, `MeshConfig`, `RoomConfig` and `MonitorConfig`. Read that file for the current field list instead of a copy here.

### Startup and runtime behavior

1. Startup:
   - Parse `kam.yml`.
   - Normalize + validate channels and radio settings.
   - Load `#public` key and all enabled non-public channels from the config directly into the in-memory topic registry (no DB involved).
   - Open the advert queue against the SQLite file (`kam.db`).
   - Open the KISS manager using the configured `radio.port` and `radio.baud`, then push `radio.frequency_hz`, `radio.bandwidth_hz`, `radio.spreading_factor`, and `radio.coding_rate` to the hardware.
2. Runtime changes:
  - Normalize the channel and merge it into `kam.yml` (inserted or replaced by name).
  - Rewrite `kam.yml` atomically, either through `node.UpsertChannel(...)` or `kam channel upsert`.
  - The running node detects the change, validates the YAML, and atomically replaces the in-memory channel registry.

## Advert Persistence

Validated `ADVERT` packets are persisted to SQLite without blocking the packet router. A dedicated `AdvertQueue` decouples ingestion from storage.

### Pipeline

1. `internal/kiss/kiss_router.go` dispatches `PAYLOAD_TYPE_ADVERT` to `internal/kiss/parsers/advert.go`.
2. The parser validates the packet, including Ed25519 signature verification.
3. The parser enqueues the `AdvertPacket` onto `AdvertQueue` via the `kiss.Persistence` interface.
4. Enqueue is non-blocking; if the queue is full, the advert is dropped and a counter is incremented.
5. A single background writer goroutine drains the queue, accumulates a batch, and inserts it transactionally on batch size or flush interval.
6. `Manager.Stop()` flushes the remaining batch and closes the queue.

### SQLite write tuning

The SQLite file (`kam.db`) holds ingested adverts and group texts, automation scripts, and room data. WAL mode is enabled at open time, along with a 5-second busy timeout and NORMAL synchronous mode. This keeps reads responsive while allowing concurrent writes. Channel configuration is **not** stored here — it lives only in `kam.yml`.

### Adverts schema

```sql
CREATE TABLE IF NOT EXISTS adverts (
  id INTEGER PRIMARY KEY,
  node_hash INTEGER NOT NULL,
  public_key BLOB NOT NULL,
  advert_timestamp INTEGER NOT NULL,
  signature BLOB NOT NULL,
  signature_valid INTEGER NOT NULL,
  is_chat_node INTEGER NOT NULL,
  is_repeater INTEGER NOT NULL,
  is_room_server INTEGER NOT NULL,
  is_sensor INTEGER NOT NULL,
  nickname TEXT,
  has_location INTEGER NOT NULL,
  latitude REAL,
  longitude REAL,
  time_drift_seconds INTEGER NOT NULL,
  raw_payload BLOB NOT NULL,
  hop_count INTEGER NOT NULL DEFAULT 0,
  hop_stations TEXT NOT NULL DEFAULT '[]',
  received_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_adverts_received_at ON adverts(received_at);
CREATE INDEX IF NOT EXISTS idx_adverts_public_key ON adverts(public_key);
CREATE INDEX IF NOT EXISTS idx_adverts_node_hash ON adverts(node_hash);
CREATE INDEX IF NOT EXISTS idx_adverts_node_type ON adverts(is_chat_node, is_repeater, is_room_server, is_sensor);
```

The parser stores the full raw advert payload and the first byte of the public key (`node_hash`) so every persisted row remains self-contained and can be replayed or inspected later. Adverts carry no RSSI/SNR because the MeshCore advert payload does not contain them; group texts do (`snr_quarter_db`, `rssi_dbm` below, captured from the radio's frame metadata).

The parser extracts the node type from the lower nibble of the appdata flags byte per the MeshCore spec:
- `0x01` — chat node
- `0x02` — repeater
- `0x03` — room server
- `0x04` — sensor

## Group Text Persistence

Decrypted `PAYLOAD_TYPE_GRP_TXT` messages are persisted to the same SQLite database via a batched, non-blocking queue. The pipeline mirrors the advert persistence path: parse, enqueue, batch, flush.

### Group texts schema

```sql
CREATE TABLE IF NOT EXISTS group_texts (
  id INTEGER PRIMARY KEY,
  channel_name TEXT NOT NULL,
  sender TEXT,
  body TEXT,
  txt_type INTEGER NOT NULL,
  attempt INTEGER NOT NULL,
  public_key_prefix BLOB,
  message_timestamp INTEGER NOT NULL,
  time_drift_seconds INTEGER NOT NULL,
  hop_count INTEGER NOT NULL DEFAULT 0,
  hop_stations TEXT NOT NULL DEFAULT '[]',
  snr_quarter_db INTEGER,
  rssi_dbm INTEGER,
  received_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_group_texts_received_at ON group_texts(received_at);
CREATE INDEX IF NOT EXISTS idx_group_texts_channel ON group_texts(channel_name);
CREATE INDEX IF NOT EXISTS idx_group_texts_sender ON group_texts(sender);
```

Persisted fields include the channel name, sender/body split, plaintext flags (`txt_type` and `attempt`), the public-key prefix for signed messages, the message's own timestamp, and the local time drift.

### Shared persistence interface

`Manager.Store` implements `kiss.Persistence`, which exposes:
- `EnqueueAdvert(advert)` — routes to the advert queue.
- `EnqueueGroupText(msg)` — routes to the group-text queue.
- `Close()` — flushes both queues and closes the shared database handle.

This keeps both persistence paths over a single SQLite handle (`kam.db`) with WAL mode and a 5-second busy timeout.

### Metrics and tuning knobs

`AdvertQueue.Metrics()` returns:
- `Enqueued` — total adverts accepted by the queue.
- `Dropped` — total adverts dropped because the queue was full.
- `Flushed` — total adverts successfully persisted.
- `Failed` — total adverts lost due to insert errors.
- `Depth` — current queued items not yet flushed.

Current defaults (hard-coded constants in `internal/db/adverts.go`):
- Queue capacity: 256
- Batch size: 50
- Flush interval: 1 second

If bursts consistently overflow the queue, increase capacity or reduce flush interval. If SQLite contention becomes the bottleneck, move the adverts table to a separate database file and adjust the store path (`db.NewPersistence` in `internal/db`, called by `internal/node`).

### Duplicate suppression note

The router deduplicates packets by raw payload hash before dispatch. If the same advert is received twice, only the first reaches the parser and is persisted. This is intentional: the duplicate advert is not useful for mesh node tracking.
