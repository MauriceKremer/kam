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

Planned file name: `channels.yml`

```yaml
version: 1
channels:
  - name: "#public"
    key_strategy: "public"
    enabled: true

  - name: "#mesh-dev"
    key_strategy: "derived"
    enabled: true

  - name: "#ops"
    key_strategy: "explicit"
    key_hex: "00112233445566778899aabbccddeeff"
    enabled: true
```

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

### Planned Go models for Step 2+

```go
type ChannelsConfig struct {
    Version  int             `yaml:"version"`
    Channels []ChannelConfig `yaml:"channels"`
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
    KeyHex      *string
    Enabled     bool
    Source      string
}
```

### Startup and runtime behavior

1. Startup:
   - Parse `channels.yml`.
   - Normalize + validate.
   - Upsert into SQLite (`channels.db`) with `source='seed'` for YAML-provided rows.
   - Load #public key and all `enabled=1` non-public rows from SQLite into the in-memory topic registry.
2. Runtime changes:
   - Insert/update rows in SQLite with `source='runtime'`.
   - Reload only changed channel(s) in memory without restart via `config.UpsertRuntimeChannelAndApply(...)`.