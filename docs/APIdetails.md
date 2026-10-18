# KAM REST API details

This is the reference for the REST endpoints KAM serves **today**. It lists only
implemented endpoints, with their parameters and responses. Endpoints that are
still planned live in [FeatureMonitorApi.md](FeatureMonitorApi.md) §7 and §10;
they are not described here.

KAM serves two small REST APIs, both loopback-only and without authentication:

| API | Address | What it manages |
|---|---|---|
| Script API (Local Management API) | `127.0.0.1:8765` | Starlark automation scripts |
| Monitor API | `127.0.0.1:8766` | Status, events, channels, rooms |

Quick check that they are up:

```sh
curl -s http://127.0.0.1:8765/scripts
curl -s http://127.0.0.1:8766/v1/status
```

The monitor listener can be turned off with `monitor.enabled: false` in
`kam.yml`. The script API runs whenever automation started successfully; if the
automation database cannot be opened, automation and its API stay off, and KAM
logs the reason.

## Rules both APIs share

- **Loopback only.** The `Host` header must be `localhost` or a loopback IP.
  Anything else gets `403`. This is a DNS-rebinding guard, not a login.
- **Errors are JSON.** Every error is `{"error": "message"}` with a suitable
  status code (`400`, `403`, `404`, `405`, `409`, `415`, `422`, `500`, `503`).
- **Wrong method** on a known path returns `405` with an `Allow` header.
- **Times** are RFC 3339 UTC, usually with milliseconds. Script timestamps
  (`created_at`, `updated_at`) are Unix epoch seconds.
- **No secrets.** No response contains a channel key or a room password.

Monitor API specifics: at most 8 requests run at once (more get `503
{"error":"busy"}`), each request has a 5-second deadline (`503` with
`{"error":"request timed out"}` when exceeded).

Script API specifics: `POST` and `PUT` need `Content-Type: application/json`
(otherwise `415`); request bodies with unknown JSON fields are rejected with
`400`; changes carry an `expected_revision` and fail with `409` when it is
stale (optimistic locking).

## Discovery — GET /

Both listeners answer `GET /` with the routes they serve, so a client can look
up what is available without reading this document first:

```sh
curl -s http://127.0.0.1:8766/
```

Each entry shows `method`, `path` and `purpose`. The monitor index also carries
`api_version` and `script_api`, a pointer to the other listener; the script
index carries only its own routes. The example below is abridged; the live list
always matches the endpoints documented here.

```json
{
  "api": "kam",
  "api_version": 1,
  "endpoints": [
    {"method": "GET", "path": "/v1/status", "purpose": "one snapshot of the running node"},
    {"method": "GET", "path": "/v1/events", "purpose": "event ring; poll with after, epoch, limit"}
  ],
  "script_api": "http://127.0.0.1:8765/",
  "docs": "docs/APIdetails.md"
}
```

## Script API — 127.0.0.1:8765

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
curl -s localhost:8765/scripts
curl -s -X POST localhost:8765/scripts -H 'Content-Type: application/json' \
  -d '{"script":{"id":"ping","name":"ping","enabled":true,"source":"def on_group_text(message):\n    reply(message[\"channel\"], \"pong\")\n"}}'
curl -s -X POST localhost:8765/scripts/ping/disable -H 'Content-Type: application/json' -d '{"expected_revision":1}'
```

## Monitor API — 127.0.0.1:8766

All monitor routes live under `/v1`.

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

## What is deliberately not here

Modem endpoints, room client removal, script run info and dry runs, and
per-script event streams are planned but not implemented yet; see
[FeatureMonitorApi.md](FeatureMonitorApi.md) §7 and §10 until they ship.
When an endpoint ships, it moves into this document.