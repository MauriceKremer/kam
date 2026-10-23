# KAM REST API details

This is the reference for the REST endpoints KAM serves **today**. It lists only
implemented endpoints, with their parameters and responses. Endpoints that are
still planned live in [FeatureKamApi.md](FeatureKamApi.md) §7 and §10;
they are not described here.

KAM serves one small REST API, loopback-only and without authentication, at
`monitor.listen` (default `127.0.0.1:8766`). It hosts the read-only data
surfaces (status, events, channels, rooms) and, on the same listener, Starlark
script management. The separate script listener on port `8765` is retired; scripts live
on the address above since the two APIs merged.

Quick check that it is up:

```sh
curl -s http://127.0.0.1:8766/v1/status
curl -s http://127.0.0.1:8766/scripts
```

`monitor.enabled: false` in `kam.yml` closes the listener and turns the event
recorder off: nothing answers on the port, and scripts keep running but can no
longer be managed or observed. If the automation database cannot be opened,
automation stays off: the read-only surfaces work, every script route answers
`503 {"error":"automation is disabled"}`, and KAM logs the reason.

## Rules

- **Loopback only.** The `Host` header must be `localhost` or a loopback IP.
  Anything else gets `403`. This is a DNS-rebinding guard, not a login.
- **Errors are JSON.** Every error is `{"error": "message"}` with a suitable
  status code (`400`, `403`, `404`, `405`, `409`, `415`, `422`, `500`, `503`).
- **Wrong method** on a known path returns `405` with an `Allow` header.
- **JSON bodies.** `POST` and `PUT` need `Content-Type: application/json`
  (otherwise `415`); request bodies with unknown JSON fields are rejected with
  `400`.
- **Optimistic locking.** Script changes carry an `expected_revision` and fail
  with `409` when it is stale.
- **Bounded listener.** At most 8 requests run at once (more get `503
  {"error":"busy"}`), each request has a 5-second deadline (`503` with
  `{"error":"request timed out"}` when exceeded). The one exception is
  `PUT /v1/modem/radio`, which may legitimately take about 15 s and gets a
  longer deadline.
- **Times** are RFC 3339 UTC, usually with milliseconds. Script timestamps
  (`created_at`, `updated_at`) are Unix epoch seconds.
- **No secrets.** No response contains a channel key or a room password.

## Discovery — GET /

The listener answers `GET /` with the routes it serves, so a client can look up
what is available without reading this document first:

```sh
curl -s http://127.0.0.1:8766/
```

Each entry shows `method`, `path` and `purpose`; the response also carries
`api_version` and `docs`. Script routes are listed even when automation is off
(their routes then answer `503`); channel routes appear when the database is
open. The example below is abridged; the live list always matches the endpoints
documented here.

```json
{
  "api": "kam",
  "api_version": 1,
  "endpoints": [
    {"method": "GET", "path": "/v1/status", "purpose": "one snapshot of the running node"},
    {"method": "GET", "path": "/v1/events", "purpose": "event ring; poll with after, epoch, limit"}
  ],
  "docs": "docs/APIdetails.md"
}
```

## Scripts (Starlark automation)

Script routes share the listener with the read-only data surfaces below and use the
same rules. With the defaults they are at `127.0.0.1:8766`.

### The script object

Every read and write returns a script in this shape:

| Field | Meaning |
|---|---|
| `id` | Unique short identifier, used in the URL. |
| `name` | Display name; unique across scripts. |
| `source` | The Starlark source code, defining at least `on_group_text`. |
| `enabled` | Desired state. A `FROZEN` or `BROKEN` script stays inactive even when enabled. |
| `timeout_ms` | Maximum run time in milliseconds; default 500. |
| `runtime_status` | `READY`, `BROKEN` (compile or run failure) or `FROZEN` (persist failed). |
| `last_error` | Last error text; omitted when there is none. |
| `revision` | Counter that goes up on every change; pass it as `expected_revision` to make a change. |
| `created_at`, `updated_at` | Unix epoch seconds. |

### Endpoints

| Method & path | Purpose |
|---|---|
| `GET /` | Show the routes this listener serves. |
| `GET /scripts` | List all scripts. |
| `POST /scripts` | Create a script. |
| `GET /scripts/{id}` | Read one script. |
| `PUT /scripts/{id}` | Update source, name, enabled state or timeout. |
| `DELETE /scripts/{id}?revision=N` | Delete a script. |
| `POST /scripts/validate` | Compile source without saving. |
| `POST /scripts/{id}/enable` | Turn the desired state on. |
| `POST /scripts/{id}/disable` | Turn the desired state off. |
| `POST /scripts/{id}/recover` | Recompile a `FROZEN` or `BROKEN` script. |

Requests that create lists use an empty JSON array (`[]`); all
create/update/action requests carry `expected_revision` (create uses `0`),
except `validate`, which changes nothing.

**POST /scripts** — body:

```json
{
  "script": {
    "id": "echo", "name": "echo", "enabled": true,
    "source": "def on_group_text(message):\n    reply(message[\"channel\"], message[\"body\"])\n",
    "timeout_ms": 500
  },
  "expected_revision": 0
}
```

Returns `201` with the saved script. `timeout_ms` may be omitted (defaults to
500). Errors: `409` when the id or name is already in use, `422` when the
source does not compile, `400` for malformed or unknown fields.

**PUT /scripts/{id}** — same body as create, with the current `revision` as
`expected_revision`. Returns the saved script. `409` when the revision is
stale or the new name is taken; `404` for an unknown id.

**DELETE /scripts/{id}?revision=N** — returns `204`. N must be the current
revision. `404` for an unknown id, `400` when the revision is missing or
invalid, `409` when stale.

**POST /scripts/validate** — body `{"id": "scratch", "source": "...",
"timeout_ms": 500}` (`id` optional, `timeout_ms` optional). Returns
`{"valid": true}`, or `422` with the compile error.

**POST /scripts/{id}/enable|disable** — body `{"expected_revision": N}`.
Changes only the desired state: a `FROZEN` or `BROKEN` script stays inactive
until `recover` or a source update. Returns the saved script.

**POST /scripts/{id}/recover** — body `{"expected_revision": N,
"timeout_ms": 500}` (`timeout_ms` optional). Recompiles the stored source and
clears the error. Returns the saved script, or `422` when the source still
fails.

Example session:

```sh
curl -s localhost:8766/scripts
curl -s -X POST localhost:8766/scripts -H 'Content-Type: application/json' \
  -d '{"script":{"id":"ping","name":"ping","enabled":true,"source":"def on_group_text(message):\n    reply(message[\"channel\"], \"pong\")\n"}}'
curl -s -X POST localhost:8766/scripts/ping/disable -H 'Content-Type: application/json' -d '{"expected_revision":1}'
```

## Status, events, channels and rooms — /v1

All routes in this section live under `/v1`.

### GET /v1/status

One snapshot of the running node; no parameters. It serves counters and cached
values; it never talks to the modem.

| Field | Meaning |
|---|---|
| `api_version` | Always `1`; new fields are only ever added. |
| `epoch`, `event_ring` | The event ring this process serves: identifier, `capacity`, `first_seq`, `last_seq`. See `GET /v1/events`. |
| `now`, `started_at` | Request time and process start time. |
| `node_name` | The node's name from `kam.yml`. |
| `modem` | `link` (`up`/`down`), `device_name`, `firmware_version`. |
| `radio` | Saved radio settings plus `tx_power_dbm`; `source` is `modem_echo` (the modem's stored copy). |
| `duty_cycle` | Airtime budget: `configured_percent`, `used_ms` in the sliding `window_s` window. |
| `tx` | Transmit scheduler counters: `queued`, `submitted`, `sent`, `completed`, `rejected`, `evicted`, `deferred`, `gate_timeouts`. |
| `persistence` | Queues for `adverts` and `group_texts`, each with `depth` and `dropped`. |
| `rooms` | `enabled` when the room subsystem runs, with the configured room `count`. |
| `automation` | `enabled`, plus `scripts_enabled`, `queue_depth`, `events_dropped`; the last three are `null` when automation is off. |

### GET /v1/events

Live observations from an in-memory ring (no bodies, only metadata). Events
arrive oldest first; the client keeps the cursor.

**Parameters**

| Parameter | Meaning |
|---|---|
| `after` | Sequence number: return events with `seq` greater than this. `0` (or omitted) starts from the ring's oldest event. A nonzero value **requires** `epoch`. |
| `epoch` | The `epoch` from the previous response; guards against a KAM restart. |
| `limit` | Page size, 1-2000, default 1000. |

`next_after` is the cursor to use next time. `reset: true` means the cursor is
useless (epoch changed, or events were dropped from the ring); start over with
`after=0` and the current `epoch`. A cursor beyond `last_seq` returns `400`.

**Event fields** (each event is one of the kinds `rx`, `tx`, `room`):

| Field | Meaning |
|---|---|
| `seq`, `at` | Ring sequence number and time. |
| `kind` | `rx` (packet came in), `tx` (frame went out), `room` (room service activity). |
| `payload_type`, `route` | Packet type (`ADVERT`, `GRP_TXT`, ...) and route (`flood`, `direct`, ...). Room events leave them empty. |
| `hops`, `len` | Hop count and frame size in bytes (packets only). |
| `snr_db`, `rssi_dbm` | Signal readings; `null` until the modem reports them (`rx` only). |
| `airtime_ms` | Estimated airtime; `null` when unknown or when a send failed. |
| `duplicate` | `rx` only: the packet was seen before. Recorded for every parsed packet, even unhandled types. |
| `dest_hash`, `room` | Addressed hash prefix and probable room id, for packets addressed to a room. |
| `channel`, `name` | Channel name, sender name (group text) or advert nickname, when known. |
| `advert_role` | Advert role, when the packet carried one. |
| `ok` | `tx` only: the modem acknowledged the send. |
| `origin` | `tx` only: who produced the frame — `advert`, `room`, or `other`. |
| `event`, `client`, `role` | `room` only: one of `login_ok`, `login_failed`, `login_rate_limited`, `post`, `post_rejected`, `push_sent`, `push_acked`, `push_timeout`, the client's public key, and its role. |

Example, abridged:

```json
{
  "epoch": "7f3a9c2e41b8d605", "first_seq": 5931, "last_seq": 5932,
  "next_after": 5932, "reset": false,
  "events": [
    {"seq": 5932, "at": "2026-10-11T15:00:02.120Z", "kind": "rx",
     "payload_type": "GRP_TXT", "route": "flood", "hops": 2, "len": 43,
     "snr_db": 8.5, "rssi_dbm": -97, "airtime_ms": 120, "duplicate": false,
     "channel": "#emmen", "name": "PA3M"}
  ]
}
```

KAMmon-style polling is: `GET /v1/events?after=<next_after>&epoch=<epoch>&limit=1000`, once or twice per second.

### GET /v1/channels

The configured channels; no parameters. Responses never contain a channel key.

| Field | Meaning |
|---|---|
| `name` | Channel name, e.g. `#emmen`. |
| `key_strategy` | `public`, `derived` or `explicit`. |
| `enabled` | Whether the channel is active. |
| `stored_messages` | How many group texts are kept in SQLite. |
| `last_message_at` | Time of the newest stored text; `null` when none. |

```sh
curl -s http://127.0.0.1:8766/v1/channels
```

### GET /v1/channels/{name}/messages

Stored group texts for one channel, newest first. The path uses the channel
name (`/v1/channels/%23emmen/messages` when it starts with `#`).

**Parameters**

| Parameter | Meaning |
|---|---|
| `limit` | Page size, 1-200, default 50. |
| `before_id` | Return only messages with an `id` lower than this; the exclusive cursor for older pages. Must be a positive integer. |
| `q` | Case-insensitive substring filter over sender and body; at most 200 UTF-8 bytes. |

**Response**: `channel` (the name as requested), `messages` (each with `id`,
`received_at`, `sender`, `body`, `hop_count`, and `snr_db`/`rssi_dbm` when
stored), and `next_before_id` — the id to pass as `before_id` for the next
older page, `null` when this is the last one. `sender` or `body` is `null`
when unknown. Unknown channel names return `404`.

```sh
curl -s 'http://127.0.0.1:8766/v1/channels/%23emmen/messages?limit=10'
```

### GET /v1/rooms

All configured rooms with their live counters. When the room subsystem is off,
this (and the two room endpoints below) return `503 {"error": "rooms are
disabled"}`. Counter totals reset when KAM starts.

| Field | Meaning |
|---|---|
| `id`, `name`, `enabled` | Room identity and whether its service is running. |
| `public_key` | Room public key, lowercase hex. |
| `max_clients`, `clients_remembered` | Capacity and current client count. |
| `last_activity_at`, `last_post_at` | Last heard client and newest post; `null` when unknown. |
| `retained_posts` | Posts kept under the room's retention limit. |
| `counters` | `logins_ok`, `logins_failed`, `logins_rate_limited`, `posts_accepted`, `posts_rejected`, `pushes_sent`, `pushes_acked`, `push_timeouts`. |

### GET /v1/rooms/{id}/clients

The remembered clients of one room, ordered by public key; no parameters.
Unknown room ids return `404`.

| Field | Meaning |
|---|---|
| `public_key` | Client key, lowercase hex. |
| `role` | Role granted through the room password: `admin`, `read_write`, `read_only` or `guest`. |
| `last_activity_at` | Last time a valid request from this client was heard; `null` when never heard. |
| `push_state` | `idle`, `awaiting_ack` or `paused` (after three push failures). |
| `label` | Newest advert nickname for that key, or `null`; a hint, not an identity. |

### GET /v1/rooms/{id}/posts

Retained room posts, newest first.

**Parameters**: `limit` — page size, 1-200, default 20. Unknown room ids
return `404`.

**Response**: `room` (the id) and `posts` (each with `id` — the post's
timestamp, `created_at`, `author_key`, `body`).

```sh
curl -s 'http://127.0.0.1:8766/v1/rooms/lobby/posts?limit=5'
```

## Modem monitoring and radio changes — /v1/modem

These routes read KAM's cached modem state and apply radio changes. Readings
come from a refresh loop that runs at most every 10 s; a request never talks to
the modem itself, except `POST /v1/modem/query` and `PUT /v1/modem/radio`,
which each send their own commands.

### GET /v1/modem

The cached snapshot. `at` is when KAM last refreshed it successfully (KAM's
start time before the first refresh). Fields KAM could not read are `null`, and
board limitations are listed in `unsupported`, not in `errors` — `errors` holds
the latest transient failure per command (for example a timeout); a successful
refresh clears both. A wholly failed refresh keeps the last successful values
and does not advance `at`.

| Field | Meaning |
|---|---|
| `at` | Time of the last successful refresh. |
| `link` | `up` or `down` (the modem link is lost). |
| `device_name`, `firmware_version` | Cached at startup; `null` when unknown. |
| `radio` | The modem's echo of the host-set values; `source` is `modem_echo`. |
| `signal_report` | Whether the last successful setting left signal reporting on. |
| `csma` | The configured KISS CSMA values (from `kam.yml`; when absent, the host's runtime view). |
| `readings` | `rssi_dbm`, `noise_floor_dbm`, `noise_floor_calibrated`, `channel_busy`, `mcu_temp_c`, `battery_mv`, `stats` (`rx`, `tx`, `errors`). Noise floor `0` means not calibrated yet: `null` with `noise_floor_calibrated: false`. `null` as a whole before the first refresh. |
| `unsupported` | Commands this board does not answer. |
| `errors` | Command → latest transient error text. |
| `tx_paused` | `true` after a failed change left the radio state uncertain; KAM applies the change on a later successful apply or restart. |
| `last_change` | The latest radio change result, in memory only; `null` before the first one. |

All routes return `503 {"error":"modem is disabled"}` when KAM runs without a
modem capability (there is nothing wired behind the listener).

### POST /v1/modem/query

Runs one read-only command now. The body is `{"command":"GetCurrentRssi"}`;
the allow list is `Ping`, `GetRadio`, `GetTxPower`, `GetCurrentRssi`,
`IsChannelBusy`, `GetAirtime`, `GetNoiseFloor`, `GetVersion`, `GetStats`,
`GetBattery`, `GetMCUTemp`, `GetSensors`, `GetDeviceName`, `GetSignalReport`.
`GetAirtime` takes `length` (integer 1-255), `GetSensors` takes `permissions`
(integer 1-7); every other command accepts only `command`.

Unknown commands or unknown fields are `400`; missing or invalid arguments are
`422`; a change in progress is `409`; a lost modem link is `503`; a timeout is
`504`; other transport failures are `502`.

An unsupported board is **not** an error: the response is `200` with
`supported: false`, `value: null` and a `reason` — `NoCallback`, `UnknownCmd`
for a rejected command, or `NoData` for an empty `GetSensors` payload.

```sh
curl -s -X POST localhost:8766/v1/modem/query -H 'Content-Type: application/json' \
  -d '{"command":"GetCurrentRssi"}'
```

```json
{"command":"GetCurrentRssi","at":"2026-10-11T15:00:00.000Z","duration_ms":8,"supported":true,"value":-106}
```

The value is typed: RSSI and noise floor are dBm numbers, busy and signal
report are booleans, the device name is a string, MCU temperature is degrees C,
airtime is milliseconds, battery is millivolts, `Ping` is `"pong"`, `GetRadio`
is the radio tuple, `GetVersion` is `{version,reserved}`, `GetStats` is
`{rx,tx,errors}` and `GetSensors` is `{permissions,data_hex}`. All readback
values are cached modem state, never a verification of the RF chip.

### PUT /v1/modem/radio

Validates, applies, checks the echo, then saves `radio:` to `kam.yml`:

```json
{
  "frequency_hz": 868000000, "bandwidth_hz": 125000,
  "spreading_factor": 8, "coding_rate": 5,
  "tx_power_dbm": 14,
  "csma": { "txdelay_ms": 500, "persistence": 63, "slot_time_ms": 100, "full_duplex": false }
}
```

The four radio values are required; `tx_power_dbm` and `csma` are optional and
leave that setting alone when omitted. `csma`, when supplied, must be the
complete object. A bad value is `422` naming the field; `signal_report` in the
body is an unknown field (`400`); the serial port and baud rate are not
changeable here. The change pauses new TX dispatches (queued packets are kept),
waits for any frame on the air, sets and echoes the values, and then rewrites
the `radio:` section of `kam.yml` — dropping the file's comments, just like
`kam channel upsert`. If the file changed while the change ran, KAM returns
`409` and restores instead of overwriting the edit.

**Timing.** A change may take about 15 s (10 s for the apply, 5 s for recovery);
the listener gives this one route a longer deadline. Clients allow at least
15 s and, after a disconnect, read `GET /v1/modem` instead of retrying.

`?preview=true` returns `{"preview": true, "steps": [...]}` with the ordered
command list and details — validated, but with nothing sent, paused or saved:

A successful change returns `200` (the steps here are abridged; a real
response names every command, including the power echo, signal reporting, each
CSMA value and the final save):

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

`verified` is always `"modem_echo"` or `null`: the echo confirms receipt, never
that the RF chip accepted a value. A failure returns the same shape with an
`error` at the mapped status — save failure `500`, echo mismatch or modem
rejection `502`, link lost `503`, timeout `504` — and restores the previous
settings. When the restoration itself fails, `tx_paused` is `true`, the radio
state is uncertain and RX continues:

```json
{"error":"Could not save kam.yml","applied":false,"saved":false,"restored":true,"tx_paused":false,"verified":"modem_echo","steps":[{"command":"save","result":"failed"},{"command":"restore","result":"echo_matches"}]}
```

(The steps of a real failure carry the full preceding command list.)

A query during a change or a concurrent change gets `409`; disabled modem
capability gets `503`.

## What is deliberately not here

Room client removal, script run info and dry runs, and per-script event streams
are planned but not implemented yet; see [FeatureKamApi.md](FeatureKamApi.md)
§7 and §10 until they ship.
When an endpoint ships, it moves into this document.