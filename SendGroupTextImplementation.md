# Send Group Text Implementation Plan

Target: generate outbound `PAYLOAD_TYPE_GRP_TXT` packets through the existing KISS manager.

## MeshCore packet layout

Full radio packet for flood-routed group text:

```text
[header:1][path_length:1][payload]
```

- `header` = `(PAYLOAD_VERSION_V1 << 6) | (PAYLOAD_TYPE_GRP_TXT << 2) | ROUTE_TYPE_FLOOD`
  - `0x15`
- `path_length` = `0x00` (zero hops, 1-byte hashes)

Payload:

```text
[channel_hash:1][cipher_mac:2][ciphertext:n]
```

Plaintext format:

```text
[timestamp:4 LE][flags:1][message]
```

- `timestamp` = Unix seconds, little-endian
- `flags` = `(txt_type << 2) | attempt`
  - Plain text group message uses `txt_type = 0x00`, `attempt = 0`
  - Therefore `flags = 0x00`
- `message` = `"<sender>: <body>"` (e.g., `"kam: hello"`)

## Crypto operations

- **AES-128-ECB** key = first 16 bytes of 32-byte channel secret.
- **Padding** = zero bytes to next 16-byte boundary.
- **MAC** = first 2 bytes of **HMAC-SHA256** over the ciphertext, keyed with the full 32-byte secret (encrypt-then-MAC).
- **Channel hash** = first byte of SHA256(secret).
  - For 16-byte keys (trailing 16 bytes zero), hash only the first 16 bytes. This matches `BaseChatMesh::setChannel` behavior.

## Size limits

- `MAX_PACKET_PAYLOAD` = 184 bytes.
- With channel hash (1), MAC (2), and worst-case zero padding (15), plaintext max = **168 bytes**.
- KISS `CMD_DATA` frame max = 255 bytes; generated raw packet fits easily.
- Truncate body so `len("<sender>: <body>") + 5` does not exceed 168 bytes.

## Current code gaps

- `internal/kiss` only decrypts/parses inbound group texts (`kiss_parse_groupText.go`).
- No outbound `CMD_DATA` sender; only `RadioClient` uses `SetHardware`.
- Topic registry stores `secret` but not `channel_hash`.
- No configured node name/sender for the `"<sender>: "` prefix.
- `main.go` blocks after radio init; no caller for outbound messages.

## Implementation steps

### 1. Add sender name to config

- Add `node_name` to `models.KamConfig`.
- Validate non-empty in `normalizeAndValidate`.
- Add `node_name: "kam"` to `kam.yml`.
- Pass into `Manager` from `main.go`.

### 2. Crypto helpers

New file `internal/kiss/group_text_crypto.go` (or extend `kiss_parse_groupText.go`):

```go
func aesECBEncrypt(key, src []byte) []byte
func encryptThenMAC(secret32, plaintext []byte) []byte   // returns [mac:2][ciphertext]
func channelHash(secret32 []byte) byte
```

- `aesECBEncrypt` mirrors existing `aesECBDecrypt`.
- `encryptThenMAC` mirrors existing `macThenDecrypt` in reverse.
- `channelHash` selects 16- or 32-byte hash input based on whether `secret32[16:]` is all zero.

### 3. Extend topic registry

In `kiss_parse_groupText.go`:

```go
type topicStorage struct {
    name   string
    secret []byte
    hash   byte
}
```

- Compute `hash` in `RegisterTopic` and `RegisterTopicWithKeyHex`.
- Add `lookupTopic(name string) (topicStorage, bool)`.

### 4. Outbound builder

New file `internal/kiss/group_text_sender.go`:

```go
type GroupTextSender struct {
    manager  *Manager
    nodeName string
}

func (m *Manager) NewGroupTextSender(nodeName string) *GroupTextSender
func (s *GroupTextSender) Send(ctx context.Context, channelName, body string) ([]byte, error)
```

`Send` does:

1. Normalize channel name.
2. Look up `topicStorage` (secret + hash).
3. Build message: `"<nodeName>: <body>"`.
4. Build plaintext: 4-byte LE timestamp + `0x00` flags + message.
5. Truncate body so plaintext ≤ 168 bytes.
6. Encrypt-then-MAC with secret.
7. Assemble raw packet: `header | path_length | channel_hash | mac | ciphertext`.
8. Enforce raw packet ≤ 255 bytes.
9. Build KISS frame: `{CMD_DATA, rawPacket...}`.
10. Push frame to `manager.txChan`.

### 5. Manager wiring

Add to `Manager` in `kiss_manager.go`:

```go
NodeName string
```

Or expose `NewGroupTextSender` taking the name directly.

### 6. Main integration

In `main.go`:

- Set `manager.NodeName = kamCfg.NodeName` after creating the manager.
- Expose `GroupTextSender` to callers.

Phase 1 leaves the trigger (HTTP endpoint, periodic message, stdin, etc.) to the caller. Calling `manager.NewGroupTextSender("kam").Send(ctx, "#public", "hello")` sends the message.

### 7. Optional: TxDone confirmation

Phase 1 is fire-and-forget.

Phase 2 may wait for the `TxDone (0xF8)` unsolicited event by registering in the response registry before sending and waiting on a buffered channel.

## Tests

1. **Round-trip**: encrypt a group text with public/derived secrets, feed the resulting `CMD_DATA` frame through `handleGroupText`, and assert decryption yields the same sender/body.
2. **Channel hash vectors**: verify `channelHash` for `#public` and a known derived channel match expected first bytes.
3. **Length enforcement**: body is truncated when plaintext exceeds 168 bytes.
4. **Frame builder**: KISS output starts with `0x00` (command byte) and ends with `0xC0`, with correct escapes.
