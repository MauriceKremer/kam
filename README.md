# kam

## Architecture TL;DR

### Device Limitations
The KISS infrastructure interfaces with constrained hardware devices (e.g., TNCs, LoRa radios). The primary limitations are:
1. **Queue Overload Prevention**: While the hardware device may maintain an internal queue, sending too many commands in rapid succession before responses are received can overwhelm its limited memory buffer. Furthermore, because this API is specifically designed to service a single primary consumer at a time, enforcing an application-level bottleneck acts as a strict preventative measure against inadvertent queue flooding.
2. **Missing Transaction IDs**: The hardware protocol does not natively support transaction IDs tagging requests to responses. This means multiple clients waiting for a generic "OK" (0xF0) response cannot be safely differentiated based solely on the incoming byte stream.

### Architecture Approach
To provide a clean, thread-safe, and asynchronous Go API (e.g., `cmd_radio.go`), while protecting the constrained hardware, the system employs the following:

1. **Hardware Mutex (`hwTxMutex`)**: The `Manager` holds a `sync.Mutex` that locks access to the hardware. Any API call requesting bidirectional interaction with the hardware must acquire this lock. 
   - *Impact*: Enforces that only one command is "in-flight" at a time, protecting the hardware buffer from overrun.
   - *Impact*: Alleviates the need for transaction IDs, as any incoming hardware response (e.g., `OK` or `Error`) is guaranteed to belong to the single goroutine currently holding the lock.
2. **Event-Driven Response Registry (`kiss_registry.go`)**: Incoming serial frames are processed continuously by a background parser loop. When a response byte is detected, the parser uses a registry map (`waiters map[byte]chan []byte`) to route the payload. 
   - *Safety*: The registry uses buffered channels (`make(chan []byte, 1)`) to ensure the fast-running parser loop never blocks, even if the API caller abandoned the request due to a context timeout.
3. **Context-Aware API**: High-level Go clients interact via synchronous interfaces using standard `context.Context` semantics (`func Set(ctx, ...)`). If the hardware lock cannot be acquired or the hardware fails to respond within the expected deadline, the API quickly unwinds and returns an error without stalling the caller.
4. **Payload Router (`kiss_router.go`)**: Incoming `CMD_DATA` frames are decoded and dispatched by `routeLoop`. It extracts the header bits (`routeType`, `payloadType`, `version`) and routes to a dedicated per-type handler (e.g., `kiss_parse_advert.go`). `kiss_router.go` owns no parsing logic itself — it is a pure dispatcher. Each payload type lives in its own `kiss_parse_<type>.go` file, keeping parsing isolated and individually testable.

## Channel Credentials Storage Design (Step 1)

This project will use a two-layer model:
1. A human-editable YAML bootstrap file for defaults and seed data.
2. SQLite as the runtime source of truth.

At startup, the YAML file is parsed and upserted into SQLite.
During operation, additions/updates are written to SQLite and then reloaded into the in-memory parser registry.

### YAML file format

File name: `kam.yml`

```yaml
version: 1
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

1. Channel names are trimmed, lowercased, and must start with `#`.
2. Empty name and bare `#` are invalid.
3. `#public` must use `key_strategy: public`.
4. Names are unique after normalization.
5. `enabled: false` keeps records in DB but excludes them from in-memory decryption registry.

### SQLite schema

```sql
CREATE TABLE IF NOT EXISTS channels (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  key_strategy TEXT NOT NULL CHECK (key_strategy IN ('public', 'derived', 'explicit')),
  key_hex TEXT,
  enabled INTEGER NOT NULL DEFAULT 1,
  source TEXT NOT NULL DEFAULT 'runtime',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_channels_enabled ON channels(enabled);
```

### Planned Go models

```go
type KamConfig struct {
    Version  int             `yaml:"version"`
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

type ChannelRecord struct {
    ID          int64
    Name        string
    KeyStrategy string
    KeyHex      string
    Enabled     bool
    Source      string
}
```

### Startup and runtime behavior

1. Startup:
   - Parse `kam.yml`.
   - Normalize + validate channels and radio settings.
   - Upsert channels into SQLite (`kam.db`) with `source='seed'` for YAML-provided rows.
   - Load #public key and all `enabled=1` non-public rows from SQLite into the in-memory topic registry.
   - Open the advert queue against the same SQLite file.
   - Open the KISS manager using the configured `radio.port` and `radio.baud`, then push `radio.frequency_hz`, `radio.bandwidth_hz`, `radio.spreading_factor`, and `radio.coding_rate` to the hardware.
2. Runtime changes:
   - Insert/update channel rows in SQLite with `source='runtime'`.
   - Reload only changed channel(s) in memory without restart via `config.UpsertRuntimeChannelAndApply(...)`.

## Advert Persistence

Validated `ADVERT` packets are persisted to SQLite without blocking the packet router. A dedicated `AdvertQueue` decouples ingestion from storage.

### Pipeline

1. `kiss_router.go` dispatches `PAYLOAD_TYPE_ADVERT` to `kiss_parse_advert.go`.
2. The parser validates the packet, including Ed25519 signature verification.
3. The parser enqueues the `AdvertPacket` onto `AdvertQueue` via the `AdvertPersistence` interface.
4. Enqueue is non-blocking; if the queue is full, the advert is dropped and a counter is incremented.
5. A single background writer goroutine drains the queue, accumulates a batch, and inserts it transactionally on batch size or flush interval.
6. `Manager.Stop()` flushes the remaining batch and closes the queue.

### SQLite write tuning

The same SQLite file (`kam.db`) is used for channels and adverts. WAL mode is enabled at open time, along with a 5-second busy timeout and NORMAL synchronous mode. This keeps reads responsive while allowing concurrent writes.

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

### Metrics and tuning knobs

`AdvertQueue.Metrics()` returns:
- `Enqueued` — total adverts accepted by the queue.
- `Dropped` — total adverts dropped because the queue was full.
- `Flushed` — total adverts successfully persisted.
- `Failed` — total adverts lost due to insert errors.
- `Depth` — current queued items not yet flushed.

Current defaults (hard-coded constants in `internal/config/adverts.go`):
- Queue capacity: 256
- Batch size: 50
- Flush interval: 1 second

If bursts consistently overflow the queue, increase capacity or reduce flush interval. If SQLite contention becomes the bottleneck, move the adverts table to a separate database file and adjust the store path in `main.go`.

### Duplicate suppression note

The router deduplicates packets by raw payload hash before dispatch. If the same advert is received twice, only the first reaches the parser and is persisted. This is intentional: the duplicate advert is not useful for mesh node tracking.
