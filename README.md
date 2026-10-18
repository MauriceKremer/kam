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

- Go 1.22+ (matches `go.mod` directive)
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
cp kam.yml kam.yml.local   # or edit kam.yml in place
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

See [Channel Credentials Storage Design](#channel-credentials-storage-design-step-1) below for the full `key_strategy` semantics (`public` / `derived` / `explicit`).

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

## Backend Monitoring API

KAM exposes a separate loopback API on `127.0.0.1:8766`, independent of the
script API on port 8765. It serves `GET /v1/status` and `GET /v1/events`,
read-only channel history (`GET /v1/channels`, `GET /v1/channels/{name}/messages`)
and read-only room surfaces (`GET /v1/rooms`, `GET /v1/rooms/{id}/clients`,
`GET /v1/rooms/{id}/posts`). Each listener also answers `GET /` with the routes
it serves. No dashboard is required: KAM continues its radio work without API
clients. Modem-control, room-client removal and dry-run endpoints remain planned;
parameters and responses are documented in [`docs/APIdetails.md`](docs/APIdetails.md).

```yaml
monitor:
  enabled: true
  listen: "127.0.0.1:8766"
  event_ring: 8192
```

These are the defaults when the block is omitted. `enabled: false` removes the
listener and event recorder. Invalid monitoring settings or a busy port are
logged without stopping radio ingestion. Changes to this block require a restart.
The API has no authentication and serves loopback hosts only; KAMmon owns its
web authentication. JSON events contain metadata, never message bodies or keys.

```sh
curl --fail-with-body http://127.0.0.1:8766/v1/status
curl --fail-with-body 'http://127.0.0.1:8766/v1/events?after=0&limit=1000'
```

Continue using the returned `epoch` and `next_after`, not `last_seq` (the newest
event in KAM's ring). An empty `events` array means caught up. `reset: true`
means KAM restarted or unread events were overwritten. Signal readings and failed
TX airtime are `null` when unknown. Events are held in bounded memory only;
restarting KAM loses the ring. Endpoint parameters and responses are documented
in [`docs/APIdetails.md`](docs/APIdetails.md); see also [the backend API manual](manuals/FeatureMonitorApi.html)
and [the contract](docs/FeatureMonitorApi.md).

## Automation (Starlark scripts)

KAM runs operator-managed [Starlark](https://github.com/google/starlark-go) scripts for every decoded group text. Scripts live in SQLite (`automation_scripts`), not in `kam.yml`, and are managed at runtime through a local HTTP API. Design and status: [`docs/FeatureStarlark.md`](docs/FeatureStarlark.md).

```python
def on_group_text(message):
    if message["channel"] == "#public" and "ping" in message["body"]:
        reply(message["channel"], "pong, %d hops" % message["hop_count"])
```

- `message` is a read-only dict: `channel`, `sender`, `body`, `timestamp_unix`, `hop_count`, `hop_stations`, `has_rx_meta`, `snr_quarter_db`, `rssi_dbm`, `time_drift_seconds`.
- `reply(channel, text)` is the only host function. Scripts have no filesystem, network, or process access, and keep no state between events.
- Events are queued (128) and run one at a time, so a slow script never blocks packet routing; a full queue drops events and logs them.
- Each script has a `timeout_ms` (default 500). A script that exceeds it becomes `FROZEN`; one that fails to compile on load becomes `BROKEN`. Neither runs until you recover or fix it. Other script errors are only logged.

### Local Management API

`127.0.0.1:8765`, loopback only, no authentication. Requests need a loopback `Host` header and, for `POST`/`PUT`, `Content-Type: application/json`. Updates carry the revision you last read (`expected_revision`; `0`/omitted when creating) and fail with `409` if it is stale. Both loopback APIs answer `GET /` with the routes they serve; endpoint details live in [`docs/APIdetails.md`](docs/APIdetails.md).

```sh
curl -s localhost:8765/scripts
curl -s -X POST localhost:8765/scripts -H 'Content-Type: application/json' \
  -d '{"script":{"id":"ping","name":"ping","enabled":true,"source":"def on_group_text(message):\n    reply(message[\"channel\"], \"pong\")\n"}}'
curl -s -X POST localhost:8765/scripts/ping/disable -H 'Content-Type: application/json' -d '{"expected_revision":1}'
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
  | curl -s -X POST localhost:8765/scripts -H 'Content-Type: application/json' -d @-
```

A database that already ran the former one-time migration keeps its `builtin.echo` script; it is an ordinary script you can edit or delete.

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
| `id` | required | Lowercase letters, digits, `-`, `_` (at most 32). Names the identity file `rooms/<id>.key`. |
| `name` | required | Advert name, at most 31 bytes (23 when a `location` is set). |
| `admin_password` | required | At most 15 bytes (clients truncate longer ones); never `password`. |
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

At most 4 rooms run. A room with invalid settings is reported in the log and stays disabled; the other rooms and radio ingestion keep running.

Things to know:

- Passwords live in plain text in `kam.yml`, the same as explicit channel keys. The only secret outside `kam.yml` is each room's identity key, a file KAM generates in `rooms/` next to the database. Back it up with the database: if it is lost, the room gets a new address. `.gitignore` excludes it.
- A post is stored before it is acknowledged, so an acknowledged post survives a restart or power loss. Posts are limited to 150 bytes, a client receives one push at a time, and each push costs roughly half a second of airtime at SF7 / 62.5 kHz, so a 32-post catch-up takes about 17 seconds per client.
- KAM trusts the host clock. Run NTP or an RTC: if the clock steps back across a restart, clients ignore adverts and posts that are not newer than the last ones they stored, and nothing on KAM's side shows it.
- Not yet implemented: status, telemetry and access-list requests (no reply), the over-the-air CLI, role changes and forced adverts through the Local Management API, and `mesh` changes without a restart.

## Architecture TL;DR

For the full, language-agnostic runtime architecture (with Mermaid diagrams),
see [`docs/DESIGN.md`](docs/DESIGN.md). The Go-specific module/goroutine map
lives in [`docs/SYSTEM_DESIGN.md`](docs/SYSTEM_DESIGN.md).

### Device Limitations
The KISS infrastructure interfaces with constrained hardware devices (e.g., TNCs, LoRa radios). The primary limitations are:
1. **Queue Overload Prevention**: While the hardware device may maintain an internal queue, sending too many commands in rapid succession before responses are received can overwhelm its limited memory buffer. Furthermore, because this API is specifically designed to service a single primary consumer at a time, enforcing an application-level bottleneck acts as a strict preventative measure against inadvertent queue flooding.
2. **Missing Transaction IDs**: The hardware protocol does not natively support transaction IDs tagging requests to responses. This means multiple clients waiting for a generic "OK" (0xF0) response cannot be safely differentiated based solely on the incoming byte stream.

### Architecture Approach
To provide a clean, thread-safe, and asynchronous Go API (e.g., `cmd_radio.go`), while protecting the constrained hardware, the system employs the following:

1. **Hardware Mutex (`hwTxMutex`)**: The `Manager` holds a `sync.Mutex` that locks access to the hardware. Any API call requesting bidirectional interaction with the hardware must acquire this lock. 
   - *Impact*: Enforces that only one command is "in-flight" at a time, protecting the hardware buffer from overrun.
   - *Impact*: Alleviates the need for transaction IDs, as any incoming hardware response (e.g., `OK` or `Error`) is guaranteed to belong to the single goroutine currently holding the lock.
2. **Event-Driven Response Registry (`internal/kiss/registry.go`)**: Incoming serial frames are processed continuously by a background parser loop. When a response byte is detected, the parser uses a registry map (`waiters map[byte]chan []byte`) to route the payload. 
   - *Safety*: The registry uses buffered channels (`make(chan []byte, 1)`) to ensure the fast-running parser loop never blocks, even if the API caller abandoned the request due to a context timeout.
3. **Context-Aware API**: High-level Go clients interact via synchronous interfaces using standard `context.Context` semantics (`func Set(ctx, ...)`). If the hardware lock cannot be acquired or the hardware fails to respond within the expected deadline, the API quickly unwinds and returns an error without stalling the caller.
4. **Payload Router (`internal/kiss/kiss_router.go`)**: Incoming `CMD_DATA` frames are decoded and dispatched by `routeLoop`. It extracts the header bits (`routeType`, `payloadType`, `version`) and routes to a dedicated per-type handler (e.g., `internal/kiss/parsers/advert.go`). `kiss_router.go` owns no parsing logic itself — it is a pure dispatcher. Each payload type lives in its own `internal/kiss/parsers/<type>.go` file, keeping parsing isolated and individually testable.

## Channel Credentials Storage Design (Step 1)

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

### Planned Go models

```go
type KamConfig struct {
    Version  int             `yaml:"version"`
    NodeName string          `yaml:"node_name"`
    Radio    RadioConfig     `yaml:"radio"`
    Channels []ChannelConfig `yaml:"channels"`
}

type RadioConfig struct {
    Port            string `yaml:"port"`
    Baud            int    `yaml:"baud"`
    FrequencyHz     uint32 `yaml:"frequency_hz"`
    BandwidthHz     uint32 `yaml:"bandwidth_hz"`
    SpreadingFactor uint8  `yaml:"spreading_factor"`
    CodingRate      uint8  `yaml:"coding_rate"`
}

type ChannelConfig struct {
    Name        string `yaml:"name"`
    KeyStrategy string `yaml:"key_strategy"`
    KeyHex      string `yaml:"key_hex,omitempty"`
    Enabled     bool   `yaml:"enabled"`
}
```

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

The SQLite file (`kam.db`) is used for adverts and group texts. WAL mode is enabled at open time, along with a 5-second busy timeout and NORMAL synchronous mode. This keeps reads responsive while allowing concurrent writes. Channel configuration is **not** stored here — it lives only in `kam.yml`.

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
  received_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_adverts_received_at ON adverts(received_at);
CREATE INDEX IF NOT EXISTS idx_adverts_public_key ON adverts(public_key);
CREATE INDEX IF NOT EXISTS idx_adverts_node_hash ON adverts(node_hash);
CREATE INDEX IF NOT EXISTS idx_adverts_node_type ON adverts(is_chat_node, is_repeater, is_room_server, is_sensor);
```

The parser stores the full raw advert payload and the first byte of the public key (`node_hash`) so every persisted row remains self-contained and can be replayed or inspected later. RSSI/SNR are not stored because the MeshCore advert payload does not contain them; they would need to be captured from the radio's frame metadata and threaded into the parser.

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

If bursts consistently overflow the queue, increase capacity or reduce flush interval. If SQLite contention becomes the bottleneck, move the adverts table to a separate database file and adjust the store path in `main.go`.

### Duplicate suppression note

The router deduplicates packets by raw payload hash before dispatch. If the same advert is received twice, only the first reaches the parser and is persisted. This is intentional: the duplicate advert is not useful for mesh node tracking.
