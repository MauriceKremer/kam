# Feature: Monitor API for KAMmon

**Status:** Phases 1-2 implemented (2026-10-12): loopback status/events, bounded RX/TX/room event ring, read-only channel and room resources, persisted signal metadata and client last-heard times. Phases 3-5 remain planned. Real-radio capture and hardware timing verification remain pending.
**Scope:** The REST interface KAM offers on the local device so KAMmon, a separate web monitor running on the same computer, can read live radio, channel, room and script data, manage room clients, and change the KISS modem radio settings.
**Written from:** KAMmon's point of view. The feature needs are in `tmp/KAMmon_Req_*.md`; the UI is shown in `tmp/kammon-prototype.html`. KAMmon is a separate Go project and never opens KAM's SQLite file.

## 1. Summary

### Phase 1 Delivery

Only `GET /v1/status` and `GET /v1/events` are available on the monitoring listener.
RX observations include duplicate and unhandled packets with nullable RxMeta;
TX observations capture submission settings/time and completion or abandonment.
Existing room address hashes provide probable attribution; collisions remain null.
Room TX records retain their producer/id through the scheduler. Legacy group-text
callers without producer metadata use `origin: "other"`, not a guessed automation identity.
Status reports existing runtime counters and cached startup identity. Optional
identity and disabled automation measurements may be null. Room-post/script-event
streams and the remaining resource endpoints are not part of this delivery.

Tests are in `internal/monitor/api_test.go`, `internal/kiss/monitor_test.go`,
`internal/models/monitor_test.go` and `internal/node/monitor_test.go`. No new
dependency, client registry, event database or event-processing worker is added.

### Phase 2 Delivery

The loopback listener now serves configured channel summaries and bounded,
searchable message history, plus room summaries, remembered clients and retained
posts. Channel rows never include keys; signal data is stored only when RxMeta
was present. Room snapshots are copied by the room worker, with per-room
counters; last-heard timestamps are persisted through the existing batched
client writes. Room activity events feed the same bounded event ring. The API
remains read-only for these resources; client removal stays in phase 4.

KAM has a Local Management API on `127.0.0.1:8765` that manages Starlark scripts. Phases 1-2 add status, recent RX/TX/room events and read-only channel/room resources on port 8766. Later phases provide modem control and client removal. Existing adverts and group texts remain in SQLite, and room client activity follows the existing room-service rules; the monitoring ring is not a persistent packet archive.

This plan adds one small package, `internal/monitor`, with three parts:

1. **An event ring.** A fixed-size, in-memory list of recent events (packets heard, packets sent, room activity, script runs). Each event has a sequence number. Nothing is written to SQLite.
2. **A second loopback listener** (default `127.0.0.1:8766`) with read endpoints for status, events, channels, rooms and the modem, and write endpoints for room clients and the modem radio.
3. **Small additions to the script API on port 8765:** a dry run and in-memory run information.

KAMmon polls `GET /v1/events?after=N` every one to two seconds, keeps its own in-memory history, and serves its own web pages. KAM never pushes and never authenticates anyone: it stays loopback only. KAMmon adds the web login.

**KAM is an open system.** Its purpose is to run the radio, the channels, the rooms and the scripts, unhindered, whether or not anything is watching. KAMmon is one client of this API. It supplies the screens and the authentication, and KAM does not know it exists. Any other program (a shell script with `curl`, a different dashboard) can use the same API in the same way. The rules in §5.3 follow from this.

Plain words for the terms used below:

- **Loopback** means the address `127.0.0.1`, reachable only from the same computer.
- **Event ring** means a list with a fixed size where the oldest entry is dropped when a new one arrives.
- **Epoch** means a random id KAM creates each time it starts, so a client can tell that KAM restarted.

## 2. Goals and non-goals

**Goals**

- GL-1. KAMmon can draw airtime, packet types, channel and room activity without reading SQLite.
- GL-2. KAMmon recovers by itself after a restart of either program.
- GL-3. Every write goes through the part of KAM that already owns that data (room worker, radio client, config writer, script runtime).
- GL-4. The API never returns a secret: channel keys, room passwords and room identity seeds stay in KAM.
- GL-5. Monitoring adds only short, bounded recording work to packet ingestion; no HTTP or storage operation runs under the event-ring lock.
- GL-6. KAM behaves exactly as it does today when no client is connected, when a client is slow, and when a client disappears without saying goodbye.
- GL-7. The API describes KAM's own things (channels, rooms, the modem, scripts), not KAMmon's pages. No field exists only to suit one client.

**Non-goals**

- Authentication, TLS, CORS, WebSocket or Server-Sent Events on the KAM side. KAMmon owns the web side.
- Any knowledge of KAMmon inside KAM: no KAMmon address in `kam.yml`, no registration, no list of clients or sessions, no health check of the client, no per-client state.
- Replacing the existing ways to run KAM. Editing `kam.yml` by hand, `kam channel upsert` and `kam room show` keep working exactly as before; the API is an extra way, never the only one.
- Storing events in SQLite, or any long-term history in KAM. Long charts live in KAMmon memory.
- Editing channels or their keys, creating rooms, forcing a room advert, `Reboot`, modem crypto commands, or adding room clients by public key before they log in.
- Anything reachable from Starlark. Scripts cannot call this API or touch the radio client.
- A block list. Removing a client does not stop it logging in again with a valid password (§7.4).

## 3. Sources and method

Everything here is based on the KAM source in this repository, read 2026-10-11. The MeshCore firmware is the authority for radio behaviour and is documented in [FeatureKissModem.md](FeatureKissModem.md) and [FeatureRoomServer.md](FeatureRoomServer.md).

| Key | File | Used for |
|---|---|---|
| `API` | [internal/automation/api.go](../internal/automation/api.go) | Existing listener guards, route style, error format |
| `ROUTE` | [internal/kiss/kiss_router.go](../internal/kiss/kiss_router.go) | Where inbound packets are parsed, deduplicated and dispatched |
| `MGR` | [internal/kiss/kiss_manager.go](../internal/kiss/kiss_manager.go) | Packet tap, group-text tap, modem events |
| `SCHED` | [internal/kiss/scheduler.go](../internal/kiss/scheduler.go) | Transmit counters and duty-cycle accounting |
| `AIR` | [internal/kiss/airtime.go](../internal/kiss/airtime.go) | `LoRaAirtime` estimate |
| `RADIO` | [internal/kiss/cmd_radio.go](../internal/kiss/cmd_radio.go), [cmd_diagnostics.go](../internal/kiss/cmd_diagnostics.go) | Typed modem commands and the request gate |
| `GT` | [internal/db/groupTexts_store.go](../internal/db/groupTexts_store.go) | Stored channel messages and nullable RxMeta signal columns |
| `ADV` | [internal/db/adverts_store.go](../internal/db/adverts_store.go) | `nickname` per `public_key` |
| `RDB` | [internal/db/rooms.go](../internal/db/rooms.go) | Room, post and client tables |
| `ROOM` | [internal/room/room.go](../internal/room/room.go), [service.go](../internal/room/service.go) | Roles, login, activity, counters |
| `RT` | [internal/automation/runtime.go](../internal/automation/runtime.go) | Script runtime and the `reply` host function |
| `CFG` | [internal/models/models.go](../internal/models/models.go) | `RadioConfig`, `SaveKamConfig` |
| `RES` | `tmp/kammon-research.md` | Room activity semantics (read 2026-10-11) |

## 4. Decisions

Recorded 2026-10-11 from the planning questions.

| ID | Decision | Outcome |
|---|---|---|
| D1 | How KAMmon receives live data | **Cursor pull over REST.** `GET /v1/events?after=N`, polled every 1-2 s. No streaming. |
| D2 | What survives a KAMmon restart | **Nothing on disk.** KAMmon backfills from the KAM event ring and labels its history start. |
| D3 | Local admin versus web viewer | **One listener, roles by user**, in KAMmon. Admin actions are accepted only from loopback. Not a KAM concern. |
| D4 | Where the endpoints live | **A separate monitoring listener** on KAM, default `127.0.0.1:8766`. The script routes stay on 8765. |
| D5 | Modem changes | **Apply, then save to `kam.yml`.** One source of truth; the reset probe reuses the saved values. |
| D6 | Room client management | **List and remove clients.** Existing password-granted radio roles are shown read-only; no add-by-key or role-editing API. Revised during clarification. |
| D7 | What web viewers see | **Per-channel allow list** in KAMmon. KAM always returns full data; KAMmon filters. |
| D8 | Chart history | **Event ring of 8192 events**; the 24 h range fills while KAMmon runs. |
| D9 | KAMmon UI | **Go templates and htmx.** KAM is not affected. |
| D10 | Starlark build tools | **Dry run plus in-memory run information.** |
| D11 | Web roles versus MeshCore room roles | **Preserve existing MeshCore room behavior.** KAMmon owns web authentication and web-user roles. KAM keeps room passwords and radio sessions; the previously proposed pinned roles are withdrawn. |
| D12 | Default state of the monitoring listener | **On by default**, reads and changes both, loopback only (the recommendation was off by default). `monitor.enabled: false` removes it completely. |
| D13 | List responses | **Named envelopes**, such as `{"channels":[]}`. Empty lists are arrays, never `null`. |
| D14 | Event pagination | **Keep `last_seq` and add `next_after`.** The first describes KAM's ring, the second tells a client where to continue. |
| D15 | Failed modem changes | **Restore on failure**, including save failure. If restoration fails, report uncertainty and pause TX while RX continues. |
| D16 | Signal reporting | **Always enabled**, as today. Read-only status; no new YAML field or editable switch. |
| D17 | Recording cost | **Short lock, bounded copy.** Encode JSON and write responses after releasing it; test contention rather than promise zero waiting. |
| D18 | Dry runs | **Synchronous and bounded:** 500 ms default, 3000 ms maximum, one at a time, at most 16 replies and 4096 UTF-8 bytes in captured channel names and text. |

**Consequences of the decisions that go beyond the recommendation or change existing text**

- **D4 versus [FeatureRoomServer.md](FeatureRoomServer.md) RS-OPS-2.** RS-OPS-2 expected room endpoints on the Local Management API. They are on the monitoring listener instead. KAMmon therefore holds two base URLs: scripts on 8765, everything else on 8766. The two listeners share no code path other than the guards in §5.
- **D5 versus [FeatureKissModem.md](FeatureKissModem.md) D8 and the hot-reload non-goal.** Both said "no HTTP surface" and "radio settings stay startup-only". This plan replaces them. `APP_STRUCTURE.md` must be updated when it ships.
- **D5 and comments.** `models.SaveKamConfig` rewrites `kam.yml` and drops comments. Channel upserts already do this. The manual must say so.
- **D11 preserves RS-AUTH-10.** The earlier pinned-role proposal is superseded. Room roles still come from existing radio login rules, not from a KAMmon web role.
- **D12, on by default.** Every KAM carries the event ring and an unauthenticated listener that can change the radio and remove room sessions. Both are loopback only, the same posture as port 8765, and a node that wants neither sets `monitor.enabled: false`. The cost is accepted; it is not a reason to add tokens.
- **Open to local programs.** Because KAM is an open system, any program on the device can call the listener. That matches the project's stance for a hobby node.

## 5. Conventions

### 5.1 Listener and configuration

```yaml
monitor:
  enabled: true              # default true; false removes the listener and the ring
  listen: "127.0.0.1:8766"   # must be a loopback address
  event_ring: 8192           # 256-65536 events
```

- The listener binds only to a loopback address and fails closed otherwise (same check as `listenLoopback` in `API`).
- Requests need a loopback `Host` header. `POST`, `PUT` and `DELETE` with a body need `Content-Type: application/json`. Unknown JSON fields are rejected.
- If the listener cannot start, KAM logs the error and keeps ingesting, as it does for automation and rooms.
- Request bodies are limited to 1 MiB.

### 5.2 Format

| Item | Rule |
|---|---|
| Paths | `/v1/...`. Changes inside `v1` only add fields; KAMmon ignores fields it does not know. |
| Lists | Named envelopes with arrays: `channels`, `rooms`, `clients`, `messages`, `posts`. Empty arrays are `[]`. Client and post responses also contain `room`; message responses contain `channel`. |
| Time | RFC 3339, UTC, millisecond precision. Durations are integer milliseconds. |
| Keys | Public keys are 64 lowercase hex characters. Channel names are lowercase with a leading `#`, URL-encoded in paths (`%23emmen`). |
| Radio units | Hertz, as in `kam.yml` (`frequency_hz`, `bandwidth_hz`). KAMmon converts for display. |
| Errors | `{"error": "message"}` with `400` bad request, `404` not found, `409` conflict or busy, `422` validation failed, `502` modem rejected the command, `503` subsystem disabled or link down. |
| Failure details | Errors may add `field` and operation-specific details. A timed-out modem operation uses `504`. Never discard partial apply or recovery outcomes behind a plain success/failure flag. |
| Untrusted text | Names, senders and message bodies come from the radio. KAMmon must escape them. |

### 5.3 Open-system rules

These rules keep KAM independent of any client.

| Rule | Meaning in practice |
|---|---|
| **No client, no change** | With the monitor off, or on but unused, KAM behaves as before. Existing tests pass unchanged. |
| **KAM stays in charge** | KAM validates every request itself. A client's own checks only give friendlier messages. A client can never put KAM into a state its own config file could not. |
| **Stateless requests** | Each request stands alone. KAM keeps nothing per client; a poller that disappears leaves nothing behind. The cursor (`after`) lives in the client. |
| **Bounded cost** | A request has a deadline and a response size limit, and only a few run at once. A client that reads slowly or never reads cannot hold a lock, a queue slot or a goroutine for long. |
| **Failure stays inside the monitor** | A panic, a full ring or a listener error is logged and contained. It never reaches the route loop, the writer, the room worker or the script worker. |
| **Generic and documented** | Names follow KAM's own concepts. The API is described in the README next to the script API and works with `curl`. |
| **Web authentication is the client's job** | KAM has no REST users or tokens. KAMmon authenticates web users. Existing MeshCore room password checks and radio client roles remain KAM's responsibility. |

## 6. Event model

### 6.1 Ring and cursor

- Each event has `seq` (a number that starts at 1 for each epoch and only grows) and `at` (when KAM recorded it).
- The ring holds `event_ring` events and drops the oldest. Record size must be measured in Go rather than assumed; memory is bounded by capacity and bounded text fields. Copy a requested page under the lock, then release it before JSON encoding or network writes.
- `GET /v1/events?after=<seq>&epoch=<epoch>&limit=<n>` returns events with `seq` greater than `after`, oldest first. `limit` defaults to 1000, maximum 2000.

```json
{
  "epoch": "7f3a9c2e41b8d605",
  "first_seq": 5931,
  "last_seq": 8123,
  "next_after": 8123,
  "reset": false,
  "events": [
    { "seq": 8123, "at": "2026-10-11T14:59:58.412Z", "kind": "rx",
      "payload_type": "GRP_TXT", "route": "flood", "hops": 2, "len": 87,
      "snr_db": 8.5, "rssi_dbm": -97, "airtime_ms": 312, "duplicate": false,
      "channel": "#emmen", "name": "PA3M" }
  ]
}
```

- `first_seq` and `last_seq` describe the retained ring at the instant its page is copied. They are not paging cursors.
- `next_after` is the sequence of the last event returned. The client sends this as `after` next time. For example, a ring ending at 5000 can return events 1-1000 with `last_seq: 5000` and `next_after: 1000`.
- The first request uses `after=0` without an epoch, starts at the oldest retained event, and returns `reset: false`.
- Subsequent requests send the epoch from the previous response. Check an epoch change before checking whether the old cursor is in range: a restart returns `reset: true` and starts at the oldest available event, even if the previous sequence exceeds the new ring's newest sequence.
- In the current epoch, an `after` older than `first_seq - 1` returns `reset: true` and starts at `first_seq`. Clients mark the missing history; they do not silently treat it as complete.
- When caught up, return `events: []` and `next_after: after`. An empty ring returns `first_seq: 0`, `last_seq: 0`, `next_after: 0`, and `events: []`; an epoch change is still reported by `reset`.
- Reject malformed, negative or overflowing `after`, a non-positive or over-2000 `limit`, and an `after` beyond `last_seq` in the current epoch with `400`. Missing `after` means 0; missing `limit` means 1000. A nonzero `after` requires an epoch. No long polling or server-side client cursors.

### 6.2 Event kinds

All kinds carry `seq`, `at` and `kind`. No event carries payload bytes or a message body.

| Kind | Fields | When it is recorded |
|---|---|---|
| `rx` | `payload_type`, `route`, `hops`, `len`, `snr_db`, `rssi_dbm`, `airtime_ms`, `duplicate`, `dest_hash`, `room`, `channel`, `name`, `advert_role` | Every inbound radio packet that parses, **before** duplicate suppression, including payload types KAM does not handle |
| `tx` | `payload_type`, `route`, `len`, `airtime_ms`, `ok`, `origin`, `room`, `channel` | The modem reports `TxDone`, or the writer gives up (`ok: null`) |
| `room` | `room`, `event`, `client`, `role` | `login_ok`, `login_failed`, `login_rate_limited`, `post`, `push_sent`, `push_acked`, `push_timeout`, `client_removed` |
| `script` | `script`, `ok`, `duration_ms`, `replies`, `error` | A script run ends (including a timeout that freezes it) |
| `modem` | `event` | `link_lost`, `link_restored`, `reset_detected`, `settings_reapplied`, `tx_abandoned` |

Field notes:

- `payload_type` is the MeshCore name (`ADVERT`, `GRP_TXT`, `TXT_MSG`, `REQ`, `RESPONSE`, `ACK`, `PATH`, `ANON_REQ`, `TRACE`, `MULTIPART`, `CONTROL`, `GRP_DATA`, `RAW_CUSTOM`) or `UNKNOWN_n`.
- `route` is `flood`, `direct`, `transport_flood` or `transport_direct`.
- `snr_db` and `rssi_dbm` are `null` when the modem sent no RxMeta frame. Zero must not be used for "unknown" (`has_rx_meta` in `GroupTextMessage`).
- `airtime_ms` is an estimate from `LoRaAirtime` with the active radio settings, or `null` when they are unknown. It counts duplicates: a repeated packet still occupied the channel.
- `duplicate` is `true` when the deduplication cache would drop the packet.
- `channel` is set only for a group text that KAM decrypted. `name` is the decoded sender for such a text, or the advert nickname for an `ADVERT`.
- `room` on `rx` is the room whose one-byte address hash equals `dest_hash`, or `null`. One byte can collide between two nodes, so KAMmon calls this "probable".
- `origin` on `tx` is `automation`, `room`, `advert` or `other`.
- `client` is a public key; for `login_failed` it is the key that tried.
- RX observation happens before deduplication, but publication may wait briefly for RxMeta and decoded attribution. Copy the packet facts into a bounded pending record; append one immutable event after metadata arrives or times out. Preserve the original receipt time in `at`, and assign `seq` at publication. This does not change the existing packet-processing decisions. Duplicates use previously known channel attribution when available, otherwise `channel: null`.
- TX metadata is captured when the writer submits the frame, not reconstructed from `TxDone` alone. Retain the active settings and submission time until completion; `at` is the submission time. Abandoned or failed TX has uncertain RF airtime and returns `airtime_ms: null` rather than counting a guaranteed transmission.
- Optional measurements and attribution are explicitly `null` when unknown. For a matching room-address collision, set `room: null` rather than selecting an arbitrary room. Names are at most 64 UTF-8 bytes and error summaries at most 256 bytes; truncate at a character boundary. Error summaries must not include message bodies or secrets.

## 7. Endpoints

Implemented endpoints from this section are documented with parameters and
responses in [APIdetails.md](APIdetails.md), and each listener serves a
self-describing index at `GET /`. Update APIdetails.md and the index lists when
an endpoint ships; this section keeps the whole plan, including parts still
planned (§7.5, the §7.6 additions).

### 7.1 Status

`GET /v1/status` returns everything KAMmon shows in its header and on the Overview page.

```json
{
  "api_version": 1,
  "epoch": "7f3a9c2e41b8d605",
  "now": "2026-10-11T15:00:00.120Z",
  "started_at": "2026-10-08T00:33:41Z",
  "node_name": "kam",
  "modem": { "link": "up", "device_name": "Emmen KISS", "firmware_version": 1 },
  "radio": { "frequency_hz": 868000000, "bandwidth_hz": 125000, "spreading_factor": 8,
             "coding_rate": 5, "tx_power_dbm": 14, "source": "modem_echo" },
  "duty_cycle": { "configured_percent": 1.0, "used_ms": 21600, "window_s": 3600 },
  "tx": { "queued": 0, "submitted": 38, "sent": 38, "completed": 38, "rejected": 0,
          "evicted": 0, "deferred": 0, "gate_timeouts": 0 },
  "persistence": { "adverts": { "depth": 0, "dropped": 0 }, "group_texts": { "depth": 0, "dropped": 0 } },
  "rooms": { "enabled": true, "count": 3 },
  "automation": { "enabled": true, "scripts_enabled": 2, "queue_depth": 0, "events_dropped": 0 },
  "event_ring": { "capacity": 8192, "first_seq": 5931, "last_seq": 8123 }
}
```

`radio.source` is always `modem_echo`: the values come from the modem's stored copy, not from the radio chip.

### 7.2 Channels

| Endpoint | Purpose |
|---|---|
| `GET /v1/channels` | Configured channels: `name`, `key_strategy` (`public`, `derived`, `explicit`), `enabled`, `stored_messages`, `last_message_at`. Never a key. |
| `GET /v1/channels/{name}/messages?limit=&before_id=&q=` | Stored group texts, newest first: `id`, `received_at`, `sender`, `body`, `hop_count`, and `snr_db` / `rssi_dbm` when stored. `limit` defaults to 50, maximum 200. `q` is a plain substring match on sender and body. |

Message counts and airtime per hour come from `rx` events, not from SQLite.

Response examples:

```json
{"channels":[{"name":"#emmen","key_strategy":"derived","enabled":true,"stored_messages":3120,"last_message_at":"2026-10-11T14:59:58.000Z"}]}
```

```json
{"channel":"#emmen","messages":[{"id":410,"received_at":"2026-10-11T14:59:58.000Z","sender":"PA3M","body":"Signal check","hop_count":2,"snr_db":8.5,"rssi_dbm":-97}],"next_before_id":410}
```

Message pages are ordered by descending stored `id`, and `before_id` is exclusive. Default `limit` is 50, maximum 200; invalid values return `400`. `q` is at most 200 UTF-8 bytes, uses case-insensitive substring matching, and is bound as a SQL parameter with wildcard characters escaped. Fetch one extra row to detect more results: `next_before_id` is the last returned id when more rows exist, otherwise `null`. An empty page returns `messages: []` and `next_before_id: null`. Missing stored signal metadata is returned as `null`. Counts and histories describe retained records, not a complete record of everything on air.

### 7.3 Rooms

| Endpoint | Purpose |
|---|---|
| `GET /v1/rooms` | Per room: `id`, `name`, `enabled`, `public_key`, `max_clients`, `clients_remembered`, `last_activity_at`, `last_post_at`, `retained_posts`, and counters (`logins_ok`, `logins_failed`, `logins_rate_limited`, `posts_accepted`, `posts_rejected`, `pushes_sent`, `pushes_acked`, `push_timeouts`). |
| `GET /v1/rooms/{id}/clients` | Per client: `public_key`, password-granted `role`, `last_activity_at`, `push_state`, `label`. No pin or role-editing control. |
| `GET /v1/rooms/{id}/posts?limit=` | Latest retained posts, newest first: `id`, `created_at`, `author_key`, `body`. Maximum 200. |
| `DELETE /v1/rooms/{id}/clients/{key}` | Forgets the client's session, cancels its pending pushes and deletes its persisted client record. Returns `204`; unknown clients return `404`. |

- `role` is `guest`, `read_only`, `read_write` or `admin`. Only `read_write` and `admin` can post. This describes radio-side room behavior; it does not grant or restrict REST access.
- `last_activity_at` is the last time a valid request from that client was heard. `null` means KAM has no record of hearing this client. See API-RM-9.
- `push_state` is `idle`, `awaiting_ack` or `paused` (three push failures in a row).
- `label` is the newest advert nickname for that public key, or `null`. It is a hint, not an identity.

Response examples:

```json
{"rooms":[{"id":"lobby","name":"Emmen local","enabled":true,"public_key":"8e2cb91aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","max_clients":20,"clients_remembered":9,"last_activity_at":"2026-10-11T14:59:48.000Z","last_post_at":"2026-10-11T14:59:00.000Z","retained_posts":412,"counters":{"logins_ok":41,"logins_failed":2,"logins_rate_limited":0,"posts_accepted":131,"posts_rejected":1,"pushes_sent":320,"pushes_acked":311,"push_timeouts":9}}]}
```

```json
{"room":"lobby","clients":[{"public_key":"ed8342bcaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","role":"admin","last_activity_at":"2026-10-11T14:59:48.000Z","push_state":"idle","label":"Maurice"}]}
```

```json
{"room":"lobby","posts":[{"id":91,"created_at":"2026-10-11T14:59:00.000Z","author_key":"ed8342bcaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","body":"Hello Emmen"}]}
```

Clients are ordered by public key; posts by descending stored id (default 20, maximum 200). Invalid limits return `400`. Lists use `[]` when empty. Disabled rooms are not listed; unknown or inactive room ids return `404`; a disabled room subsystem returns `503`. Counters reset at KAM start. Last activity follows the existing room worker's accepted-input rules, not database update time or a sender's replay timestamp.

### 7.4 Room protocol and web permissions

- Keep current room login behavior: the admin password grants `admin`, the guest password or configured open-room login grants `read_write`, and `allow_read_only` grants `guest` for other passwords. Without that fallback, an unmatched password is refused. Known-client blank-password resumes behave as they do today.
- No `role_pin` column, role setter or new room-account model is introduced by this feature. The earlier pinned-role proposal is withdrawn.
- KAM's local REST callers all have full access to the endpoints this version offers. KAMmon alone handles web logins, web roles and data visibility. Those rules never change the permissions of radios logging into a room.
- **Remove** is not a block. A radio can authenticate again under the existing room rules. Removing a session does not change passwords, revoke previously shared passwords or make an open/read-only room private.

### 7.5 Modem

| Endpoint | Purpose |
|---|---|
| `GET /v1/modem` | Last cached snapshot (refreshed by KAM every 10 s): link, device name, firmware version, radio echo, TX power, signal-report state, CSMA values from the config, and readings (RSSI, noise floor, channel busy, MCU temperature, battery, RX/TX/error counters), plus a list of commands the board does not support. |
| `POST /v1/modem/query` | Runs one read-only command now: `{"command":"GetCurrentRssi"}`. `GetAirtime` takes `length` (1-255); `GetSensors` takes `permissions` (1-7). |
| `PUT /v1/modem/radio` | Validates, applies, checks the echo, then saves `radio:` to `kam.yml`. `?preview=true` returns the planned steps and sends nothing. Signal reporting is always enabled. |

```json
{
  "frequency_hz": 868000000, "bandwidth_hz": 125000,
  "spreading_factor": 8, "coding_rate": 5,
  "tx_power_dbm": 14,
  "csma": { "txdelay_ms": 500, "persistence": 63, "slot_time_ms": 100, "full_duplex": false }
}
```

`tx_power_dbm` and `csma` are optional; leaving one out leaves that setting alone. The serial port and baud rate are not changeable through the API.

Frequency, bandwidth, spreading factor and coding rate are required; omission is not a reset to zero. CSMA, when supplied, is a complete object with all four fields. Use existing KAM validation, not chip-specific or region presets. Signal reporting stays enabled after startup, apply and modem-reset recovery; `signal_report` in a mutation body is an unknown field, rejected with `400`.

`GET /v1/modem` example (readings may be absent without making the snapshot fail):

```json
{
  "at":"2026-10-11T15:00:00.000Z",
  "link":"up",
  "device_name":"Emmen KISS",
  "firmware_version":1,
  "radio":{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5,"tx_power_dbm":14,"source":"modem_echo"},
  "signal_report":true,
  "csma":{"txdelay_ms":500,"persistence":63,"slot_time_ms":100,"full_duplex":false},
  "readings":{"rssi_dbm":-106,"noise_floor_dbm":-114,"noise_floor_calibrated":true,"channel_busy":false,"mcu_temp_c":32.4,"battery_mv":null,"stats":{"rx":1246,"tx":38,"errors":0}},
  "unsupported":["GetBattery"],
  "errors":{},
  "tx_paused":false,
  "last_change":null
}
```

`at` is when the snapshot was refreshed, not the HTTP request time. `errors` maps command names to their latest transient error (for example a timeout); an error is not the same as unsupported hardware. Keep the last successful value on a transient failure and do not advance `at` for a wholly failed refresh. Snapshot reads do not trigger modem commands. Refresh hardware readings at most every 10 s; cache device/version at startup and update settings after an apply or reset. Refreshes and the existing reset probe share the radio-operation gate and yield to an explicit setting change.

The query allow list is `Ping`, `GetRadio`, `GetTxPower`, `GetCurrentRssi`, `IsChannelBusy`, `GetAirtime`, `GetNoiseFloor`, `GetVersion`, `GetStats`, `GetBattery`, `GetMCUTemp`, `GetSensors`, `GetDeviceName`, `GetSignalReport`. No raw command bytes or setting commands are accepted. `GetAirtime` requires integer `length` 1-255; `GetSensors` requires integer `permissions` 1-7; other commands accept only `command`. Missing or invalid arguments return `422`; unknown commands or unknown fields return `400`.

Query responses always include `command`, `at`, `duration_ms`, `supported`, and `value`. The value is typed: RSSI/noise floor/temperature/airtime/battery are numbers in dBm/dBm/degrees C/ms/mV; busy and signal report are booleans; device name is a string; version is `{version,reserved}`; stats is `{rx,tx,errors}`; GetRadio is the radio tuple; sensors is `{permissions,data_hex}`; Ping is `"pong"`. Readback values are cached modem state, never chip verification.

```json
{"command":"GetCurrentRssi","at":"2026-10-11T15:00:00.000Z","duration_ms":8,"supported":true,"value":-106}
```

```json
{"command":"GetBattery","at":"2026-10-11T15:00:00.000Z","duration_ms":8,"supported":false,"value":null,"reason":"NoCallback"}
```

`NoCallback`, `UnknownCmd`, and an empty GetSensors response mean unsupported and return `200`; transport failures still return `502`, `503`, or `504` as appropriate. Unknown commands submitted by the caller are not silently sent to discover support.

### 7.5.1 Apply and recovery

1. Validate the entire request before touching the modem. Acquire the single radio-change gate; another change or direct query gets `409` while it is held. Capture previous runtime settings and the current config file contents.
2. Pause new scheduler dispatches (do not discard its bounded queue), wait for the in-flight frame to finish or be abandoned, and suspend reset-probe and diagnostic setting work. Continue RX and normal room/automation intake.
3. Send SetRadio and compare GetRadio; optionally set and compare TX power; ensure SetSignalReport(true) succeeds; optionally send the four CSMA settings followed by Ping. CSMA is `sent_no_reply`, not verified.
4. Re-read `kam.yml` before saving. Preserve non-radio values; if the file changed during this operation, return `409` and restore rather than overwrite somebody else's edit. Atomically save with the existing configuration writer. A save error initiates recovery, not a successful temporary change.
5. Publish the new runtime defaults only after save succeeds, including reset-probe defaults, airtime estimates, gate timeout and scheduler accounting for future frames. Existing airtime entries retain the estimate made when they were submitted. Resume TX and probe work.
6. On any failure after touching the modem, including cancellation, try restoring the previous radio, TX power, signal reporting and CSMA settings. Leave `kam.yml` unchanged. Report the failed step and recovery outcome. A confirmed radio/power echo plus sent CSMA settings permits TX to resume; CSMA is still not independently verified.
7. If recovery fails, retain the previous saved configuration as the desired state, mark current radio state uncertain, keep TX paused and continue RX. Log the error and expose it through `GET /v1/modem`. Do not let the reset probe silently clear this state. A later successful explicit apply, or a KAM restart that successfully reapplies saved startup settings, clears the pause.

Normal monitor requests have a 5 s deadline. The radio apply is the bounded exception: at most 10 s for waiting/applying/saving and a separate maximum 5 s for recovery, independent of a disconnected HTTP caller. No job registry or client session is introduced. Clients allow at least 15 s for apply; after a disconnect they read the snapshot instead of automatically retrying. The snapshot's `last_change` records the most recent operation only, in memory.

Preview returns `{"preview":true,"steps":[...]}` with the ordered command names and details, and performs validation but no send, pause or save. The actual apply revalidates; preview is not a reservation.

Result of a real apply:

```json
{
  "applied": true, "saved": true, "verified": "modem_echo", "restored": false, "tx_paused": false,
  "steps": [
    { "command": "SetRadio", "detail": "868000000 Hz, 125000 Hz, SF8, CR5", "result": "ok" },
    { "command": "GetRadio", "result": "echo_matches" },
    { "command": "SetTxPower", "detail": "14 dBm", "result": "ok" },
    { "command": "TXDELAY", "detail": "500 ms", "result": "sent_no_reply" },
    { "command": "Ping", "result": "pong" }
  ]
}
```

Snapshot fields that cannot be read return `null` with the command name in `unsupported`, never an error. A noise floor of `0` means "not calibrated yet" and is returned as `null` with `noise_floor_calibrated: false`.

Failed-change examples (returned at the relevant error status, and also stored in `last_change`):

```json
{"error":"Could not save kam.yml","applied":false,"saved":false,"restored":true,"tx_paused":false,"verified":"modem_echo","steps":[{"command":"save","result":"failed"},{"command":"restore","result":"echo_matches"}]}
```

```json
{"error":"Apply and restoration failed","applied":false,"saved":false,"restored":false,"tx_paused":true,"verified":null,"steps":[{"command":"SetRadio","result":"timeout"},{"command":"restore","result":"timeout"}]}
```

`applied: true` means the entire requested change succeeded, not merely that some commands reached the modem. File-write failure is `500`; echo mismatch or modem rejection is `502`; timeout is `504`. Failure responses expose best-known state, never promise that interrupted changes did nothing.

### 7.6 Scripts (existing port 8765)

The routes `GET/POST /scripts`, `GET/PUT/DELETE /scripts/{id}`, `POST /scripts/validate` and `POST /scripts/{id}/enable|disable|recover` stay exactly as they are, with the same revision checks (`expected_revision`, `409` when stale). Two additions, both backward compatible:

- Each script object gains `run`: `{ "runs": 12, "errors": 0, "last_run_at": "...", "last_duration_ms": 2, "last_replies": 1 }`. In memory only; it resets when KAM restarts.
- `POST /scripts/dry-run` runs one script against a fixture without sending anything.

```json
{
  "source": "def on_group_text(message):\n    reply(message[\"channel\"], message[\"body\"])\n",
  "timeout_ms": 500,
  "message": { "channel": "#emmen", "sender": "PA3M", "body": "@kam hello", "hop_count": 2,
               "has_rx_meta": true, "snr_quarter_db": 34, "rssi_dbm": -97, "time_drift_seconds": 0 }
}
```

```json
{ "ok": true, "duration_ms": 1, "replies": [ { "channel": "#emmen", "text": "@kam hello" } ] }
```

A failure returns `ok: false` with `error`. The status is `200` in both cases because the dry run itself worked.

### 7.6.1 Bounded dry-run contract

- `source` is required. `timeout_ms` defaults to 500 when omitted; explicit values must be whole milliseconds from 1 to 3000. This test limit does not cap a live script's configured timeout. Validation errors return `422`; malformed JSON or unknown fields return `400`.
- `message` is required and its `channel` must be an enabled configured channel. Optional fields default to: `sender: "test"`, `body: ""`, `hop_count: 0`, `hop_stations: []`, `timestamp_unix` equal to server time, `has_rx_meta: false`, `snr_quarter_db: 0`, `rssi_dbm: 0`, `time_drift_seconds: 0`. These are fixture values, not measurements. Accept all documented live message fields; validate types and non-negative hop counts, with at most 63 hop stations of 1-3 bytes encoded as hex.
- Use the same compiler, frozen message dictionary, host capabilities and execution semantics as live automation, but a separate invocation with a captured-reply sender. The dry run does not update live run counters, persisted source, enabled state, last_error or FROZEN/BROKEN state.
- At most one dry run executes at a time; another gets `409` immediately, not a queue entry. Respect the shorter of the requested timeout and request cancellation. Compilation and execution share the 3 s maximum execution budget inside the 5 s request deadline.
- Capture at most 16 replies and 4096 total UTF-8 bytes across their channel names and text. The next reply exceeding either limit stops the run with `ok: false` and a clear error; return already captured replies as test output, never transmit them. Each reply still uses normal channel and message-size validation.
- `ok: false` covers compilation failure, script exceptions, timeout and reply-limit failure. The response always contains `ok`, `duration_ms`, `replies` (possibly `[]`) and `error` on failure. HTTP `200` means the test completed and reported its outcome, not that the script passed. Client disconnection cancels work; no asynchronous test job survives it.

## 8. Requirements

IDs are stable. "From" lists the KAMmon feature document that needs the item (`tmp/KAMmon_Req_<name>.md`).

### 8.1 Foundation

| ID | Requirement | Level | From |
|---|---|---|---|
| API-F-1 | A `monitor` config block with `enabled`, `listen` and `event_ring`; a loopback-only listener; loopback `Host` check; JSON-only bodies; best-effort start (§5.1). | MUST | all |
| API-F-2 | Routes live under `/v1`; changes inside `v1` only add fields. | MUST | all |
| API-F-3 | Errors use `{"error": "..."}` and the status codes in §5.2. | MUST | all |
| API-F-4 | No response contains a channel key, room password or identity seed. | MUST | Access |
| API-F-5 | Handlers read bounded snapshots; all JSON encoding, database work and network writes are outside event-ring locks. Writes go through the owner of the data. | MUST | all |
| API-F-6 | Times are RFC 3339 UTC with milliseconds; public keys are lowercase hex. | MUST | all |
| API-F-7 | KAM holds no client state: no registry, no sessions, no outbound calls to a client, no "client connected" flag. With no client attached KAM behaves as before. | MUST | DataStore |
| API-F-8 | Normal requests have a 5 s deadline and at most 8 MiB encoded response; radio apply has the bounded 10 s operation plus 5 s recovery budget in §7.5.1. Header and write timeouts are set on the listener. No network write holds a shared runtime lock. | MUST | all |
| API-F-9 | At most 8 requests run at once; more get `503` with `{"error":"busy"}`. Writes are serialized by the owner of the data. | MUST | all |
| API-F-10 | A panic or error inside a handler or the recorder is logged and contained. It never reaches the route loop, writer, room worker or script worker. | MUST | all |
| API-F-11 | With `monitor.enabled: false` there is no listener and no ring, and the recording hooks do nothing (a nil check). | MUST | all |
| API-F-12 | KAM validates all input itself, whatever the client. Hand-editing `kam.yml` and the existing commands keep working unchanged. | MUST | all |
| API-F-13 | The API is described in the README beside the script API, with `curl` examples. | SHOULD | all |
| API-ST-1 | `GET /v1/status` as in §7.1, including `epoch` and `now`. | MUST | Overview, DataStore |

### 8.2 Events

| ID | Requirement | Level | From |
|---|---|---|---|
| API-EV-1 | An in-memory ring of `event_ring` events with `seq` and an `epoch` regenerated at start; the oldest event is dropped when full. | MUST | DataStore |
| API-EV-2 | `GET /v1/events` with `after`, `epoch`, `limit`, `reset` and `next_after` as in §6.1. `last_seq` describes the ring, not the delivered page. | MUST | DataStore |
| API-EV-3 | An `rx` event for every inbound packet that parses, recorded before duplicate suppression and for unhandled payload types too. | MUST | Overview, Traffic |
| API-EV-4 | A `tx` event when `TxDone` arrives or the writer abandons a frame. | MUST | Overview, Traffic |
| API-EV-5 | `room` events for the kinds in §6.2. | MUST | Rooms |
| API-EV-6 | `script` events at the end of each run. | SHOULD | Automations |
| API-EV-7 | `modem` events for link loss, restore and reset. | SHOULD | KissModem |
| API-EV-8 | `airtime_ms` from `LoRaAirtime` with the active radio settings; `null` when unknown. Duplicates count. | MUST | Overview |
| API-EV-9 | Events contain no payload bytes and no message bodies. | MUST | Access |
| API-EV-10 | Recording uses a preallocated bounded ring and a short lock for copying only. Readers copy at most a page before releasing it. No separate recording worker is needed. Benchmark lock hold time and packet-routing latency under concurrent reads; do not claim zero waiting. | MUST | all |

### 8.3 Channels

| ID | Requirement | Level | From |
|---|---|---|---|
| API-CH-1 | `GET /v1/channels` as in §7.2, never returning key material. | MUST | Channels |
| API-CH-2 | `GET /v1/channels/{name}/messages` with paging by `before_id`, a bounded `limit` and a plain substring `q` (wildcards escaped). | MUST | Channels |
| API-CH-3 | Add nullable `snr_quarter_db` and `rssi_dbm` columns to `group_texts` and store them from `RxMeta`. | SHOULD | Channels |
| API-CH-4 | A disabled channel is listed with `enabled: false`; its stored history stays readable. | MUST | Channels |

### 8.4 Rooms

| ID | Requirement | Level | From |
|---|---|---|---|
| API-RM-1 | `GET /v1/rooms` with **per-room** counters. Today `room.Stats` is one total for all rooms. | MUST | Rooms |
| API-RM-2 | `GET /v1/rooms/{id}/clients` with existing protocol role, last activity, push state and label. | MUST | Rooms, RoomAccounts |
| API-RM-3 | `GET /v1/rooms/{id}/posts`; `RoomPost` gains `created_at` (already stored in `room_posts`). | MUST | Rooms |
| API-RM-4 | Withdrawn: no role-editing endpoint in this feature (D11). | WITHDRAWN | RoomAccounts |
| API-RM-5 | `DELETE .../clients/{key}` forgets the session and cancels its pushes; this is not a ban. | MUST | RoomAccounts |
| API-RM-6 | Withdrawn: no pinned-role schema or override (D11). | WITHDRAWN | RoomAccounts |
| API-RM-7 | Removals run on the room worker; delete the database record before removing runtime state, and prevent later batch writes from resurrecting it. A persistence failure leaves the session intact and returns an error. | MUST | RoomAccounts |
| API-RM-8 | `label` comes from the newest `adverts.nickname` for the key; `null` when none. | SHOULD | Rooms |
| API-RM-9 | Persist the last time each client was heard (`last_heard_at` in `room_clients`) with the existing 5-second batch write, so a restart does not turn every client into "unknown". | SHOULD | Rooms |

### 8.5 Modem

| ID | Requirement | Level | From |
|---|---|---|---|
| API-MD-1 | `GET /v1/modem` serves a cached snapshot refreshed every 10 s, never reading the modem inside the request. | MUST | KissModem |
| API-MD-2 | `POST /v1/modem/query` accepts only the read-only commands listed in §7.5. | MUST | KissModem |
| API-MD-3 | `PUT /v1/modem/radio` runs `SetRadio`, `GetRadio` echo check, optional `SetTxPower`, `SetSignalReport`, optional CSMA frames and a final `Ping`; `?preview=true` lists the steps without sending. | MUST | KissModem |
| API-MD-4 | Reuse the existing `RadioConfig` validation. A bad value returns `422` naming the field. | MUST | KissModem |
| API-MD-5 | A shared radio-operation gate serializes changes and suspends new TX dispatch, reset-probe work and diagnostic refresh during apply/recovery. Wait for the active frame before changing settings; preserve queued packets. | MUST | KissModem |
| API-MD-6 | Apply/save/recovery follows §7.5.1. Any failure after a modem change attempts restoration, including YAML-save failure. Failed restoration leaves TX paused and reports uncertainty while RX continues. | MUST | KissModem |
| API-MD-7 | After a successful change, the reset probe, the airtime estimator and the transmit scheduler use the new values. | MUST | KissModem |
| API-MD-8 | Results say `modem_echo`, never "hardware verified". | MUST | KissModem |
| API-MD-9 | The radio client stays unreachable from Starlark. `Reboot` and crypto commands are not exposed. | MUST | KissModem |
| API-MD-10 | An unsupported command (`NoCallback`, empty sensors) returns `supported: false`, not an error. | MUST | KissModem |
| API-MD-11 | Signal reporting is always enabled at startup, apply and reset; expose status only, not a persisted or editable option. | MUST | KissModem |

### 8.6 Scripts

| ID | Requirement | Level | From |
|---|---|---|---|
| API-AU-1 | Existing `/scripts` routes and their revision rules do not change. | MUST | Automations |
| API-AU-2 | Each script object gains an in-memory `run` object (§7.6). | MUST | Automations |
| API-AU-3 | `POST /scripts/dry-run` with a fixture message; `reply` is captured and never sent. | MUST | Automations |
| API-AU-4 | A dry run changes no live script state, runs separately from the live worker and follows the timeout, fixture, reply and concurrency limits in §7.6.1. | MUST | Automations |
| API-AU-5 | `GET /v1/status` includes script queue depth and dropped events. | SHOULD | Overview |

## 9. Corrections to existing text

| # | Existing statement | Finding |
|---|---|---|
| C1 | `tmp/kammon-research.md`: client names are unavailable as structured data. | The `adverts` table stores `nickname` per `public_key` (`ADV`). A label can be offered, but it may be missing or stale. |
| C2 | [FeatureKissModem.md](FeatureKissModem.md) D8: no HTTP surface; non-goal: no hot reload of radio settings. | Replaced by D5 and API-MD-3. |
| C3 | [APP_STRUCTURE.md](APP_STRUCTURE.md): radio settings are read at startup and need a restart. | No longer true once API-MD-3 ships. |
| C4 | Earlier draft: override password roles with pinned client roles. | Withdrawn by D11. Existing MeshCore room authentication remains unchanged; web permissions belong to KAMmon. |
| C5 | [FeatureRoomServer.md](FeatureRoomServer.md) RS-OPS-2: management endpoints on the Local Management API. | They are on the monitoring listener (D4). |
| C6 | Prototype sample scripts use `msg.text` and `reply(msg, ...)`. | Real scripts define `on_group_text(message)`, read `message["body"]` and call `reply(channel, text)` (`RT`, [echo.star](examples/echo.star)). |
| C7 | `room.Stats` looks per room. | It is one set of counters for the whole service (`ROOM`). |

## 10. Phases

| Phase | Content | Unlocks | Done when |
|---|---|---|---|
| 1 (implemented; hardware verification pending) | `internal/monitor` package, listener, `/v1/status`, event ring, `rx` and `tx` events, `epoch`/`next_after` | Overview, Traffic, DataStore | Automated transport/HTTP/cursor/lifecycle tests pass; real-radio acceptance remains pending |
| 2 (implemented) | Channels endpoints; room list, clients, posts, `room` events, per-room counters, persisted `last_heard_at` | Channels, Rooms | Focused repository, handler, worker-snapshot and race tests pass |
| 3 | Modem snapshot, query and `PUT /v1/modem/radio` with save | KissModem | A change survives a KAM restart and a simulated modem reset |
| 4 | Client removal endpoint; protocol roles remain read-only | RoomAccounts | Removal cannot resurrect a client via batch persistence; existing room-login tests remain unchanged |
| 5 | Script `run` info and dry run | Automations | A dry run never transmits and cannot freeze a live script |

**Tests**

- With `monitor.enabled: false` no second listener is opened and the existing test suite passes unchanged.
- A client that connects and never reads, and one that sends a slow request, never retain the event-ring lock; benchmark route latency during concurrent page-copy requests.
- The monitor listener failing to bind leaves ingestion, rooms and scripts running.
- Table tests for the cursor: first poll, pagination without skipping, empty ring, caught up, gap, wrong epoch, ring wrap, invalid and future cursor.
- A router test that proves duplicates and unhandled payload types produce `rx` events.
- Handler tests with a fake room worker and fake radio client: busy, echo mismatch, save failure, successful restoration, failed restoration with TX pause, disconnect, concurrent config edit, invalid field and link down.
- A test that no response contains a configured key or password.
- Existing room-login tests (including `allow_read_only` and blank resumes) pass unchanged. Removal tests cover pending pushes and delayed batch writes.
- Parse every JSON example as a fixture. Test the dry-run 3000 ms boundary, busy response, defaults, reply-count and byte limits, and prove no send or live-state transition occurs.

## 11. Risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | The route loop calls the event recorder inline. | One lock and a fixed-size copy (API-EV-10); benchmark with a burst of 1000 packets. |
| R2 | A radio change while a frame is on the air. | API-MD-5: wait for `TxDone` or the gate timeout. |
| R3 | `SaveKamConfig` drops comments from `kam.yml`. | Documented; same behaviour as `kam channel upsert`. |
| R4 | Echo matches but the chip rejected a value. | Reported as `modem_echo`; startup RSSI and noise-floor readings remain hints ([FeatureKissModem.md](FeatureKissModem.md) G1, G2). |
| R5 | Any local program can call the unauthenticated listener, which is on by default (D12). | Accepted for a hobby device, same as port 8765. Keep the loopback bind check; `monitor.enabled: false` turns it off. |
| R7 | A slow or misbehaving local client. | API-F-8 and API-F-9: deadlines, size limits, at most 8 requests at once, no per-client state. |
| R6 | A one-byte room hash collides with another node. | `room` on `rx` is labelled probable. |
