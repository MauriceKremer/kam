# Feature: Starlark periodic posts

| | |
|---|---|
| Status | Implemented: all six phases. Compile-time interval and entry-point rules, `rest_get`, `room_post` and room system posts, timer scheduling, the API field, and the example plus docs. |
| Scope | Timer-driven scripts, the `room_post` host function, the `rest_get` host function, `json`/`time` predeclared modules, room service system posts, scripts API exposure, docs and examples. |
| Related | [FeatureStarlark.md](FeatureStarlark.md) (runtime and scripts API), [FeatureRoomServer.md](FeatureRoomServer.md) (room posts and push), [FeatureUnifiedApi.md](FeatureUnifiedApi.md) (transport rules). |

## Summary

Room content today only comes from clients. This feature lets a Starlark script
post into a room on its own schedule, and fetch a URL to decide what to post:

- A script declares a run interval in its source with one module-level
  assignment: `timer_interval_minutes = 60`. The runtime then calls the
  script's `on_timer(message)` handler about every that-many minutes.
- A script posts into a room with `room_post("lobby", "text")`. The post is
  authored by the room's own identity and reaches clients through the normal
  backlog and push path, exactly like a client post.
- A script fetches a URL with `body, status = rest_get("https://…")`.
- The standard Starlark `json` and `time` modules from `go.starlark.net` are
  predeclared in every script, so a fetched JSON body can be decoded without
  any extra glue.

All three capabilities are general: they are available to `on_group_text`
handlers too. Timers are the motivating use, not a special mode.

The scripts API keeps its endpoints; the script object gains one read-only
field, `timer_interval_minutes`, derived from the source. There is no database
migration: the interval lives in the script source, like everything else about
a script.

## Research notes

Everything is implemented against code that exists today (2026-11-06): the
automation runtime (`internal/automation/runtime.go`), the automation
repository (`internal/db/automation.go`), the room service
(`internal/room/room.go`), and the node wiring (`internal/node/node.go`).
Findings this plan relies on:

- `compileWithContext(parent, id, source, timeoutMS)` (with the small wrapper
  `compile()` at `runtime.go:418`) in `internal/automation/runtime.go` builds a
  Starlark program with `starlark.SourceProgram(id, source, allow)`, where
  `allow` admits only names in the package-level `hostFunctions` map, then
  runs `program.Init` under a deadline with stub builtins and checks that
  `globals["on_group_text"]` is callable. `SourceProgram` also returns the
  parsed `*syntax.Module`, which the current code discards — the parse tree
  is where the interval literal will be read.
- The scheduler work sits behind the existing worker: one goroutine, a bounded
  queue (default 128), sequential invokes in stable script-id order, a
  per-invocation timeout that freezes the script on overrun, and non-blocking
  enqueue with a drop counter. Timer fires must respect all of that, so they
  travel through the same queue instead of invoking directly.
- `hostReply` shows the host-function shape: unpack args, honor the shared
  invocation deadline from the context, call the seam. `room_post` and
  `rest_get` follow it.
- `go.starlark.net` at the pinned version (`v0.0.0-20261005163335-bcb1a1a55bf9`,
  in [go.mod](../go.mod)) already ships `lib/json` (module value with
  `encode`/`decode`) and `lib/time` (module value). Predeclaring them needs no
  new dependency and no filesystem `load()`.
- Room posts: `room.Service.addPost` truncates to `maxPostTextLen` (150),
  retries on `db.ErrRoomPostTimestampTaken` using `uniqueNow`, calls
  `RoomRepository.AppendPost`, appends to the in-memory backlog, and arms
  `nextPush`. Room state (clients, backlog, push cursor) is touched only by
  the service's single worker goroutine, so a script post must enter through
  the worker, not through the store directly.
- A server-side post is an anticipated shape: the `room_posts.author` comment
  (`internal/db/rooms.go:17-18`) already says the room's own key is the author
  for system posts, and push skips only posts authored by the requesting
  client, so room-key posts are delivered to every client.
- Starlark name resolution binds a module-level assignment in module scope, so
  a function body may read `timer_interval_minutes` without the compiler
  treating it as an undefined predeclared name. No resolver change is needed
  for the interval; the resolver only needs to admit `json` and `time`.
- MeshCore-side precedent for the *idea* of scheduled jobs exists
  (meshcore.js `JobBuilder` intervals that pause while the radio is down), but
  KAM's model is simpler: intervals are wall-clock coarse, best-effort, and
  documented as such in this file.

## Goals

- A stored script can run periodically without any configuration outside its
  own source: `timer_interval_minutes = N` plus `def on_timer(message)`.
- `N` is in whole minutes and must be at least 1. This keeps a mistake in a
  source file from becoming a tight loop against the radio, and it makes the
  floor plain: one fire a minute, at the fastest, without unit conversion.
- A script can post to a running room from any handler with one call,
  `room_post(room_id, text)`, and posts follow the normal room rules
  (150-byte truncate, dedupe timestamp, backlog, push).
- A script can fetch over HTTP(S) with `rest_get(url, headers=…)` and gets the
  body text and status code; network errors surface as script errors, not
  crashes.
- Every script has `json` and `time` available as predeclared modules.
- Timer-only scripts are possible (no `on_group_text`); message-only scripts
  keep working exactly as before.
- All execution keeps the existing invariants: one worker, bounded queue,
  sequential invokes, per-script timeout with FROZEN on overrun, intake never
  blocks, shutdown order unchanged.
- The transport package `internal/api` stays free of KAM capabilities: it sees
  only one new read-only struct field, delivered by the existing `Scripts`
  seam.

## Non-goals

- No cron expressions, date/DST-aware schedules, or catch-up runs. An interval
  missed while the runtime was busy or restarting is skipped; the next fire is
  one interval after the current time.
- No per-room or per-script concurrency: a timer fire and a group-text event
  for the same script still run one after the other on the single worker.
- No HTTP methods beyond GET, no response headers returned, no response
  history, no caching. `rest_get` is the one-call fetch a status line needs.
- No access control on `rest_get`: no allowlist, no private-address blocking, no
  proxy policy. KAM runs on the operator's own machine and the operator writes
  the script; this matches the project philosophy ([README.md](../README.md),
  Philosophy & intent). The scheme/userinfo/size limits below are correctness
  guards, not security hardening.
- No secrets plumbing for `rest_get`. Headers to a trusted endpoint (including
  an API key header) are written inline in the script, like channel keys in
  `kam.yml`.
- No `room_post` on behalf of a client key, no role checks — the script acts
  as the room itself.
- No new persistence for timers: no schedule table, no last-fire timestamps, no
  drift correction across restarts. After a restart, a timer's first fire is
  one full interval after startup.
- No weather script in scope; the post-v1 weather item in
  [FeatureStarlark.md](FeatureStarlark.md) Phase 5 stays where it is (with
  `rest_get`, that work becomes a script instead of a provider adapter — a
  follow-up note is recorded in that doc).

## Sources

| Key | Source |
|---|---|
| AUTO | `internal/automation/runtime.go` (pinned by `git log -1` on implementation day) |
| DB | `internal/db/automation.go`, `internal/db/rooms.go` |
| ROOM | `internal/room/room.go`, `internal/room/service.go`, `internal/room/push.go`, `internal/room/send.go` |
| ADAPT | `internal/node/node.go`, `internal/node/scripts.go`, `internal/node/monitor.go` |
| API | `internal/api/api.go`, `internal/api/scripts.go` |
| MODELS | `internal/models/rooms.go`, `internal/models/monitor.go` |
| LIB | `go.starlark.net v0.0.0-20261005163335-bcb1a1a55bf9` (`lib/json`, `lib/time`, `starlarkstruct`, `syntax`) |
| FS-STARLARK | [FeatureStarlark.md](FeatureStarlark.md) |
| FRS | [FeatureRoomServer.md](FeatureRoomServer.md) |

## Design

### 1. Declaring a timer (source-defined interval)

A script opts in with one module-level assignment next to its other globals:

```starlark
timer_interval_minutes = 60

def on_timer(message):
    room_post("lobby", body_text)
```

Rules, enforced at compile time (save and validate both compile):

- `timer_interval_minutes` must be a plain integer literal assignment at the
  top level of the module. Anything else (float, string, expression, function
  call) is a compile error: `timer_interval_minutes must be an integer literal number`.
- Missing assignment means "no timer" (0). No error.
- 0 is treated as "no timer" as well; negative values and values below 1 are
  compile errors: `timer_interval_minutes must be at least 1`.
- If the interval is set, the script must define `on_timer`:
  `script sets timer_interval_minutes but does not define callable on_timer(message)`.
- If `on_timer` exists, the interval must be set:
  `script defines on_timer but does not set timer_interval_minutes`.
- The old rule changes shape: a script must now define `on_group_text` *or* a
  timer. Neither → compile error
  `script must define callable on_group_text(message) or a timer`.
- A second top-level assignment (`timer_interval_minutes = 5` then
  `timer_interval_minutes = 9`) never reaches the interval reader: Starlark's
  resolver rejects rebinding a global, so the error is
  `cannot reassign global timer_interval_minutes declared at <file>:1:1`.
  One assignment is all a script can have.

Extraction reads the parse tree, not executed globals: `compile()` currently
discards the `*syntax.Module` from `SourceProgram`; the new helper

```go
// parseTimerInterval returns the int literal assigned to
// timer_interval_minutes at module top level, or 0 when absent.
func parseTimerInterval(mod *syntax.Module) (int64, error)
```

walks `mod.Stmts`, matches `*syntax.AssignStmt` with an identifier LHS
`timer_interval_minutes` and a `*syntax.LiteralExpr` int RHS, and errors on a
non-conforming assignment to that name. A valid script whose top level merely
*reads* the global elsewhere keeps working, because module-level assignments
bind in module scope.

One wrinkle from the current code: compile runs the top-level statements
against stub builtins (a `program.Init(thread, stubs)` at
`runtime.go:444`), which means a script whose globals *use* a host function
at top level (`name = reply("#public", "hi")`) already executes host calls
during compile with stubs. The interval extraction is a parse-tree read and
needs no execution, but the compile-time execution of host calls means
`room_post`/`rest_get` stubs must return a benign value (like today's
`starlark.None`) so top-level use stays compilable, not callable.

`compileWithContext(parent, id, source, timeoutMS)` — the package entry point
is `compile()` (`runtime.go:418`), which wraps it with a background context —
becomes the returning struct and callers:  

```go
// compiledScript gains fields: interval int64, timerEntry bool
// (scripts map in runtime). compileWithContext returns a second value:
func compileWithContext(parent context.Context, id, source string, timeoutMS int64) (*starlark.Program, compiledProgram, error)

type compiledProgram struct {
    interval      int64 // timer_interval_minutes value, 0 = none
    messageEntry  bool  // on_group_text is defined and callable
    timerEntry    bool  // on_timer is defined and callable
}
```

Callers to adjust: `compile` (runtime.go:418 signature carries the new value
too) and its calls inside `NewRuntimeWithQueueSize` (message load), `Save`,
and `Validate` (ignores the struct fields). Nothing else touches the program.

### 2. Predeclared `json` and `time`

Every script can use `json` and `time` as plain globals:

```starlark
data = json.decode(body, "text/plain")
stamp = time.from_timestamp(message["timestamp_unix"])
```

- Allowed predeclared names = `hostFunctions` keys ∪ {`json`, `time`}. The
  name-allowing closure in `compile()` and the stub/predeclared StringDict
  builders should share one small helper so the three places can't drift.
- Validation (compile) stubs: host functions as today; `json`/`time` are
  predeclared as the real module values from `go.starlark.net/lib/json` and
  `go.starlark.net/lib/time`. These are pure libraries, so real values under
  the validation deadline are fine.
- Runtime predeclared: same values.
- No `load()`; no new go.mod entry (both packages live in the pinned module).

### 3. Timer scheduling in the runtime

New machinery, all inside `internal/automation`:

```go
type timerFire struct { id string }

type runtimeEvent struct {
    message *parsers.GroupTextMessage // group-text path
    timer   *timerFire                // timer path
}
```

- The worker queue widens from `chan parsers.GroupTextMessage` to
  `chan runtimeEvent` (same size constant, 128). `Handle` enqueues a message
  event; everything downstream switches on which field is set.
- A `timerLoop` goroutine starts next to the worker and stops on the runtime
  context, so `Stop()` order is untouched.
- A `timerChanged chan struct{}` (buffered, size 1) lets the mutation paths —
  `Save`, `SetEnabled`, `Recover`, `Delete` — nudge the loop after they update
  the scripts map. Nudge is non-blocking, and it is a nudge, not a payload.
- Snapshot semantics, mirroring `dispatch`: a timer applies to scripts that
  are in the map, `Enabled`, and `READY` (resident status wins; FROZEN and
  BROKEN scripts have no timer scheduled).

Loop algorithm:

```
next := map[string]time.Time{}          // script id → next fire
loop:
    scripts := snapshot of timer scripts (id → interval minutes)
    drop `next` entries for ids no longer eligible
    for each new eligible id: next[id] = now + interval*minute
    wake := soonest of next (none → wait on ctx/timerChanged only)
    timer := time.NewTimer(wake)
    select:
        <-ctx.Done: return
        <-timerChanged: NewTimer discarded; continue loop
        <-timer.C:
            for id in sorted(next) where next[id] <= now:
                enqueue timerFire{id}   (non-blocking; on full:
                    Warn "[Automation] timer fire dropped: queue full",
                    increment the existing dropped counter)
                next[id] = now.Add(interval * time.Minute)   // no catch-up
            continue loop
```

Timer semantics, to document verbatim for operators:

- The first fire after a start, save, or enable is one full interval later.
- Fires are best-effort: a fire that lands on a full queue is dropped (logged
  and counted), and the next fire is still one interval after the current
  time — intervals never pile up into bursts.
- Changing a script's interval restarts that script's cycle from now.
- Disabling, freezing, or deleting a script removes it from the schedule; a
  recovered (or re-saved) timer script rejoins at one full interval later.
- Scripts with equal intervals fire together after a restart; the single
  worker serializes the invocations.

Firing invokes the same `invoke()` used for messages, with a frozen
`{"timestamp_unix": now.Unix()}` dict as the message argument, and the same
timeout, freeze-on-timeout, and logging. A dedicated `dispatchTimer(fire)`
does the per-id snapshot and skip when the script is no longer eligible (a
fire can race a disable/save).

Testing seam for the 1-minute floor: package-level
`var minTimerInterval = time.Minute`, overridden in tests; the floor is
applied at compile time as a duration comparison against the parsed minutes.

`MonitorStats()` keeps its shape: timer-fire drops increment the existing
`dropped` counter (they *are* dropped automation events) and get their own log
line. No status API change is needed; the scripts object below is the only
surface change.

### 4. `room_post` — posting from a script

New host function entry in the `hostFunctions` map:

```go
"room_post": hostRoomPost,   // room_post(room_id, text)
```

Behavior at the runtime boundary:

- Args: `room_post(room_id, text)` — two required strings, unpacked the way
  `hostReply` unpacks, with `room_post:` in error messages.
- Empty `text` → error `room_post: text is empty` (no empty posts in the
  backlog). Empty room id → `room_post: room id is required`.
- Text is passed verbatim; the room service truncates to 150 bytes exactly as
  it does for client posts (scripts cannot and must not manage the length).
- Errors reach the script as ordinary errors; they log like any invocation
  failure and never freeze a script.

The runtime reaches the room service through a seam, so the packages stay
decoupled:

```go
// internal/automation/runtime.go
type RoomPoster interface {
    Post(ctx context.Context, roomID, text string) error
}
```

`NewRuntimeWithQueueSize` gains a `poster RoomPoster` parameter. With `nil`,
calling `room_post` fails at invoke time with
`room_post: rooms are not running` — compile still accepts the name, so an
operator's script remains valid for a node that later gains rooms.

```go
// internal/node (adapter, next to the send.Sender wiring)
type roomPoster struct{ service *room.Service }
func (r roomPoster) Post(ctx context.Context, roomID, text string) error {
    return r.service.AddSystemPost(ctx, roomID, text)
}
```

Room service side (`internal/room`) — the post must go through the worker
because room state is worker-owned:

```go
type systemPostRequest struct {
    roomID string
    text   string
    reply  chan error   // buffered 1, so the worker never blocks
}

// AddSystemPost is safe from any goroutine. Non-blocking enqueue into a
// bounded channel (cap 16), then wait for the worker's verdict or the
// caller's context.
func (s *Service) AddSystemPost(ctx context.Context, roomID, text string) error
```

- Enqueue `default` branch (queue full) → error `system post queue is full`.
- If the service is stopped → error `room service is stopped`.
- If the caller's context ends first → `context error`; the worker may still
  store the post afterwards (documented in AddSystemPost's comment).
- The worker's select gains the channel case and handles the request with a
  new `addSystemPost(roomID, text, now)`:

  - Unknown room id → error `unknown room`.
  - Author = the room's own public key (`r.pub`, copied), text truncated with
    `mesh.TruncateUTF8(text, maxPostTextLen)`, timestamp from `s.uniqueNow`
    with the same 3-retry dance on `db.ErrRoomPostTimestampTaken` as
    `addPost`.
  - One `RoomRepository.AppendPost` call with a nil client key (no
    `last_timestamp` update — the room is not a logged-in client).
  - Then the normal in-memory append + backlog trim, `cnt.PostsAccepted++`,
    `nextPush` arm, and `recordRoomEvent(roomID, "post", "", "system", now)`
    so the event stream shows system posts with an empty client and role
    `system`.
  - Persistence errors are returned to the waiting script and logged
    (`[Room] system post failed for <id>: …`).

Delivery is free: the post is in the backlog, so logins get it via
`LatestPosts`, and `tickPush` offers it to clients (the "skip own post" filter
compares against the *client's* key, not the room's).

### Sample script against WeatherAPI

A real, working sample pins the whole design to one concrete case. Fetched
2026-11-06 with a free-tier key:

```
GET https://api.weatherapi.com/v1/current.json?key=[secret]&q=Emmen
```

Response (851 bytes, HTTP 200, `content-type: application/json`):

```json
{
  "location": {
    "name": "Emmen",
    "region": "Drenthe",
    "country": "Netherlands",
    "lat": 52.7792,
    "lon": 6.9069,
    "tz_id": "Europe/Amsterdam",
    "localtime_epoch": 1793991141,
    "localtime": "2026-11-06 19:52"
  },
  "current": {
    "last_updated_epoch": 1793989800,
    "last_updated": "2026-11-06 19:30",
    "temp_c": 4.9,
    "temp_f": 40.9,
    "is_day": 0,
    "condition": {"text": "Clear", "icon": "//cdn.weatherapi.com/weather/64x64/night/113.png", "code": 1000},
    "wind_mph": 8.7,
    "wind_kph": 14.0,
    "wind_degree": 90,
    "wind_dir": "E",
    "pressure_mb": 1025.0,
    "pressure_in": 30.27,
    "precip_mm": 0.0,
    "precip_in": 0.0,
    "humidity": 80,
    "cloud": 0,
    "feelslike_c": 1.8,
    "feelslike_f": 35.2,
    "windchill_c": 1.8,
    "windchill_f": 35.3,
    "heatindex_c": 4.9,
    "heatindex_f": 40.9,
    "dewpoint_c": 1.8,
    "dewpoint_f": 35.3,
    "vis_km": 10.0,
    "vis_miles": 6.0,
    "uv": 0.0,
    "gust_mph": 18.8,
    "gust_kph": 30.3,
    "will_it_rain": 0,
    "chance_of_rain": 10,
    "will_it_snow": 0,
    "chance_of_snow": 0
  }
}
```

What the shape tells the design (all confirmed on the real response):

- Plain GET, no headers, no auth header — the key rides in the query string,
  so `rest_get(url)`'s single-argument form covers it. `json`/`time`
  predeclaration (Phase 1) is what makes the payload decodable without glue.
- 851 bytes ≪ the 1 MiB cap; the cap never interferes with free-tier APIs.
- The interesting numbers are top-level scalars inside `current` —
  `temp_c`, `condition.text`, `wind_kph`, `humidity` — so `json.decode`
  yields a dict whose values read directly into a format string.
- Float formatting: Starlark's `%` operator has no precision or width
  specifiers (`"%.1f" % 4.9` is `unknown conversion %.`), and there is no
  `round()`. `str()`/`%s` on a float prints its shortest form (`4.9`, `4.0`),
  so the sample formats temperatures with `%s` and leaves the value as the API
  reported it. `%d` accepts a float too and truncates it (`14.0` → `14`), which
  is why `wind_kph` reads as a whole number.
- The API can return an error body with a non-200 status (bad key, exceeded
  quota); the `(body, status)` tuple lets the script branch on `status` and
  stay quiet on failures rather than post error pages into the room.

The example script for Phase 6, `docs/examples/periodic.star`:

```starlark
timer_interval_minutes = 15

ROOM = "lobby"
API_URL = "https://api.weatherapi.com/v1/current.json?key=YOUR_KEY&q=Emmen"

def on_timer(message):
    body, status = rest_get(API_URL)
    if status != 200:
        return  # stay quiet and try again next interval; a bad key is not room news
    weather = json.decode(body)
    current = weather["current"]
    text = "Emmen %s°C, feels %s°C, %s, wind %d km/h, humidity %d%% (%s)" % (
        current["temp_c"],
        current["feelslike_c"],
        current["condition"]["text"],
        current["wind_kph"],
        current["humidity"],
        time.from_timestamp(message["timestamp_unix"]).format("15:04"),
    )
    room_post(ROOM, text)
```

Posting the line into room `lobby` produces, on air and in the backlog:

```
Emmen 4.9°C, feels 1.8°C, Clear, wind 14 km/h, humidity 80% (19:52)
```

The script's stored shape: `timeout_ms = 30000` (a cold HTTPS roundtrip can
exceed the 500 ms default), everything else default. The key placeholder
`YOUR_KEY` is substituted by the operator at save time; the sample URL with
the working key above is what this spec was verified against.

### 5. `rest_get` — fetching a URL

New host function entry:

```go
"rest_get": hostRestGet,   // rest_get(url, headers=None)
```

- Args: `url` (string), optional `headers` (None or a dict of string →
  string). Non-string keys or values → error `rest_get: headers must map strings to strings`.
- Returns a 2-tuple: `body, status = rest_get(url)`. `body` is the raw
  response text (bytes as-is; use `json.decode` for JSON APIs), `status` is
  the int HTTP status. Non-2xx responses are *not* errors — the script decides
  what 503 from its own URL means.
- Scheme gate: only `http` and `https` parse and pass. Anything else →
  `rest_get: only http and https URLs are supported`.
- No user-credentials-in-URL (`user:pass@host`) → error
  `rest_get: credentials in the URL are not supported`. This keeps secrets out
  of error and log lines wholesale.
- Size cap: the body reader stops at 1 MiB (package const
  `restGetMaxResponseBytes = 1 << 20`); hitting the cap is an error
  `rest_get: response exceeds 1048576 bytes`, not a truncation — a silently
  cut JSON body would look valid and decode wrong.
- Deadline: the request context is the invocation context, so a `rest_get`
  shares the script's `timeout_ms` budget exactly like every host call
  (existing policy). Redirects follow the standard library defaults.
- One shared `http.Client` per runtime with a conservative transport
  (`TLSHandshakeTimeout: 10 * time.Second`); no per-request client. No
  proxy/allowlist policy (non-goal). A cancellation or deadline produces
  `rest_get: <cause>` as a script error — logged, never a freeze (freezes stay
  reserved for the script's own timeout overrun, via the existing
  `classifyInvokeError` path).

What this deliberately does *not* do, for the [README](../README.md) philosophy
record: no allowlist, no private-address blocking, no response header access,
no method overrides, no built-in API-key store.

### 6. Scripts API surface

- `api.Script` gains one field, written by the node adapter, never read from
  requests (it is not a stored column; requests that include it are accepted
  but ignored, like other non-stored fields):

```go
TimerIntervalMinutes int `json:"timer_interval_minutes"`
```

- The runtime gets one read helper:

```go
// TimerIntervals maps script id → configured interval minutes for compiled,
// known scripts. 0 for scripts without a timer; missing ids are broken
// scripts still pending recovery.
func (r *Runtime) TimerIntervals() map[string]int64
```

  One snapshot per `List`/`Get` (no per-row locking).
- No new endpoints; the `GET /` index lists the same script routes. Per the
  project rule, [docs/APIdetails.md](APIdetails.md) gets the field documented
  in the script-object section in the same change; the scripts endpoints
  table is unchanged.
- `docs/examples/echo.star` keeps working untouched: the group-text message
  dict is unchanged.

### 7. Wiring and lifecycle

- `node.New` builds `roomPoster{service}` when rooms run, `nil` otherwise, and
  passes it to `automation.NewRuntime(ctx, repository, sender, poster)`.
- Shutdown order in `Run()` is unchanged: the timer loop dies with the runtime
  context and the worker drains whatever is queued, exactly like today.
- The automation repository and its SQLite schema are untouched. BROKEN
  persistence, revision rules, `RuntimeStatus` vocabulary, and restart
  preservation of FROZEN/BROKEN all apply to timer scripts unchanged.
- `kam.yml` gains no settings. Nothing to configure: a script either declares
  an interval or it does not.

## Invariants kept (trace of the existing guarantees)

| Invariant (from [FeatureStarlark.md](FeatureStarlark.md)) | How this feature preserves it |
|---|---|
| Single worker, sequential per-script invokes | Timer fires travel through the same bounded queue as group texts |
| Never block inbound route processing | Both `Handle` and timer enqueue stay non-blocking; room_post enqueues into a separate bounded service channel |
| Per-script timeout, FROZEN on overrun, recover | `invoke()` shared; frozen scripts drop out of both dispatch snapshot and timer snapshot |
| Fresh globals per invocation | Unchanged; the timer message is a frozen fresh dict |
| Host calls honor the invocation deadline | `rest_get` request ctx = invocation ctx; `room_post` bounded by ctx for the enqueue-wait |
| Persist-failure leaves frozen-in-memory until restart | Untouched; no new store failure paths outside existing ones |
| `internal/api` imports only stdlib + models | The only api change is a struct field; the value arrives via the `Scripts` seam |
| Automation never imports `internal/room` and vice versa | New `RoomPoster` interface lives in `internal/automation`; the concrete adapter lives in `internal/node` |
| Transport purity of `internal/db` vs runtime | No db changes at all |

## Implementation plan

Order matters only in that compile changes (Phase 1) come first; everything
after is independently testable against compile's new contract.

### Phase 1: Compile-time interval parsing and entry-point rules

- [x] Update `compile` (`runtime.go:418`) to carry the second value;
      `compileWithContext` returns `(*starlark.Program, compiledProgram,
      error)`. Adjust `Save`, `Validate` (ignores the fields), and the message
      load in `NewRuntimeWithQueueSize`; store interval/flags on
      `compiledScript`.
- [x] Add `parseTimerInterval` over the `*syntax.File` return of
      `SourceProgram` with the literal grammar and the three error texts.
- [x] New compile rules: message-or-timer requirement; interval-without-
      `on_timer`; `on_timer`-without-interval; 1-minute floor via
      `minTimerInterval` package var (test seam).
- [x] Predeclare `json` (`go.starlark.net/lib/json`.Module) and `time`
      (`go.starlark.net/lib/time`.Module) in resolver, validation stubs, and
      runtime builtins, through one shared name/value helper.
- [x] Tests: literal grammar (all error cases, absent → 0); entry-point rule
      matrix (message-only, timer-only, both, neither, mismatched pairs);
      `json`/`time` usable at top level and inside handlers; existing compile
      tests still pass. Duplicate assignment is a parser error, covered as
      such.

### Phase 2: `rest_get`

- [x] Add `hostRestGet` to `hostFunctions`; shared `http.Client` on the
      runtime; scheme, userinfo, header-dict, and size-cap checks; tuple
      return `(body, status)`; request ctx = invocation ctx.
- [x] Tests with `httptest`: status passthrough, headers param, body as text,
      redirect, size cap (exactly at cap, one byte over), scheme rejection,
      userinfo rejection, malformed URL, invocation-deadline cancellation,
      header value type errors; a READY-status check that an ordinary fetch
      error does not freeze.

### Phase 3: `room_post` and room service system posts

- [x] `room.Service`: `systemPostRequest` type, bounded channel (16),
      `AddSystemPost` (non-blocking enqueue, ctx-bounded wait, stopped-service
      and full-queue errors), worker select case, `addSystemPost` (lookup,
      truncate, uniqueNow retry, `AppendPost` with nil client key, in-memory
      backlog + trim + `nextPush` + counter + `recordRoomEvent(..., "system")`).
- [x] `internal/automation`: `RoomPoster` interface, nil-poster refusal error
      in `hostRoomPost`, `room_post` entry in `hostFunctions` with arg
      unpacking and empty-text/empty-id checks.
- [x] `internal/node`: `roomPoster` adapter, new `NewRuntime` parameter,
      nil when no rooms run.
- [x] Tests: room package (unknown room, busy queue, stopped service, own-key
      author, 150-byte truncation, backlog visibility, push to a client,
      timestamp-retry path); automation package (arg errors, nil poster, fake
      poster call-through); node package (adapter wiring into a real room
      service).

### Phase 4: Timer scheduling

- [x] Widen the queue to `runtimeEvent`; switch `Handle`, worker, and
      `dropQueuedEvents` to the event type; timer drops merged into the
      dropped counter with their own log text.
- [x] `timerLoop` per the algorithm above: snapshot from compiled scripts,
      stagger-free first fire, `timerChanged` nudge from `Save`/`SetEnabled`
      /`Recover`/`Delete`, no catch-up, non-blocking fire enqueue.
- [x] `dispatchTimer`: per-id eligibility snapshot, frozen dict
      `{"timestamp_unix": now.Unix()}`, shared `invoke`, freeze-on-timeout.
- [x] `TimerIntervals()` snapshot helper.
- [x] Tests: fire cadence with an overridable minute floor; interval change
      resets; disabled/frozen/deleted scripts stop firing; recovered scripts
      rejoin; queue-full fire drop increments the counter; timer-only scripts
      never run on group texts; a script with both entries runs both paths;
      race detector over the loop and Stop.

### Phase 5: API field and node adapter exposure

- [x] `api.Script.TimerIntervalMinutes` field; node adapter fills it from
      `TimerIntervals()` for `List` and `Get`; request copies ignored.
- [x] API package tests: field serialization; malformed-request behavior
      unchanged; guard test (import rule) still passes.

### Phase 6: Examples and documentation

- [x] New example `docs/examples/periodic.star`: every-15-minutes fetch of a
      JSON status URL (`rest_get` + `json.decode` + `time`), formatted line
      posted with `room_post`, with `timeout_ms = 30000` commented why.
- [x] Working sample on the real WeatherAPI endpoint included in this spec
      (§ Sample script against WeatherAPI) and shipped as the example's
      subject: `https://api.weatherapi.com/v1/current.json?key=…&q=Emmen`.
      The example keeps the operator's key in the script source, like channel
      keys in `kam.yml`.
- [x] [docs/APIdetails.md](APIdetails.md): script-object section documents
      `timer_interval_minutes` (read-only, source-derived; 0 when none).
- [x] [FeatureStarlark.md](FeatureStarlark.md): summary and host-capability
      lists point to this doc; the first-release non-goal "Multiple event
      types beyond group text" is annotated as superseded (see Corrections);
      Phase 5 (weather) notes that `rest_get` makes a plain provider script
      possible.
- [x] README scripts section: two sentences (timers + the two new calls) and a
      pointer to the example. Plain words per the projects docs rule.

## Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | The interval is declared in script source as a module-level integer literal (`timer_interval_minutes`), not stored as a column and not configurable in `kam.yml`. | Source already is the single definition of a script's behavior; a column would add a migration and a second place scripts diverge from settings. The literal is machine-checkable at compile time. |
| D2 | Timer floor is 1 minute, compile-enforced. | A typo must not become a tight loop hammering the radio, and minutes make the floor self-evident — no second-to-minute conversion to explain or get wrong. |
| D3 | Timer entry is a separate `on_timer(message)` handler, and the group-text message dict is unchanged. | Reusing `on_group_text` for timer fires would make the name and the argument lie for scripts that never see group texts; a second entry point costs one compile check. |
| D4 | Timer fires go through the existing bounded worker queue; no parallel dispatch. | Preserves the sequential-execution invariant and reuses timeout/freeze/snapshot logic verbatim; fire-and-recompute (no catch-up) keeps bursts impossible. |
| D5 | `rest_get` returns `(body, status)`, GET-only, no allowlist/private-IP rules, no returned headers, 1 MiB cap. | The tuple mirrors how scripts read HTTP; the caps are correctness guards. KAM is single-operator; its scripts are trusted code, matching the README philosophy. |
| D6 | Script posts are authored by the room's own identity, via a new bounded command channel into the room worker. | Room state is worker-owned (existing invariant); the room key is the anticipated author for system posts (db schema comment), and delivery/backlog/push then work with zero new mesh code. |
| D7 | `room_post` resolves success at enqueue, not at persist; persist errors are logged and returned to the waiting script when the worker gets there within the caller's deadline, otherwise the post may still land. | The script's timeout budget should not be hostage to the room worker's push cadence; the documented race (ctx-first → post may still store) is harmless for periodic content. |
| D8 | `json` and `time` are predeclared module values, not `load()`ed, and both host functions are compiled-in names even when the corresponding seam is nil (runtime call fails with a clear message). | No new dependency, no filesystem access, keeps `hostFunctions` a static table; nil-seam scripts fail loudly at the call site, not mysteriously at compile. |
| D9 | The API script object gains one read-only `timer_interval_minutes` field derived from compiled state; no request-side handling. | Operators and dashboards should not parse Starlark to know a script's cadence; requests can't set it because storage never owns it. |
| D10 | No database schema change; no `kam.yml` change. | The interval lives in source (D1); timers need no persistence beyond the script itself (Non-goals). |

## Corrections

| When | Correction |
|---|---|
| 2026-11-06 | [FeatureStarlark.md](FeatureStarlark.md) listed "Multiple event types beyond group text" as a first-release non-goal; this feature intentionally supersedes that line with the timer entry (D3). No other statement in that doc was contradicted: the runtime, its policy table, and the scripts API described there hold as written. |
| 2026-11-06 | §1 said a second top-level `timer_interval_minutes` assignment is allowed and the last one wins. It is not: Starlark's resolver rejects rebinding a global, so the script fails with `cannot reassign global timer_interval_minutes`. One assignment per script; the parser-level error is now part of the grammar table. |
| 2026-11-06 | §Sample script used `"%.1f" %` for temperatures on the belief that Starlark floats print with a trailing dot. Starlark's `%` has no precision/width flags (`unknown conversion %.`) and no `round()`, so the sample uses `%s`, which prints `4.9` for `4.9` and `4.0` for an integral float. |

## Verification

Focused acceptance, in order of the phases:

1. `go test ./internal/automation/... ./internal/room/... ./internal/node/... ./internal/api/...` green, plus
   `go test -race ./...` green — the timer loop, the worker, and the room
   worker are the concurrency surfaces this feature adds.
2. Compile matrix from Phase 1: each rule fires with its error text; a
   message-only echo-style script and a timer-only room-poster script both
   compile and store as READY.
3. Behavior probe with a fake HTTP server and a fake poster:
   `on_timer` receives `{"timestamp_unix": …}` at the configured cadence (tests
   shrink the floor via `minTimerInterval`), `rest_get` reports body/status
   through the caps, and `room_post` lands a room-key-authored post that
   `LatestPosts` and the push path serve to a logged-in client.
4. `GET /scripts/{id}` shows `timer_interval_minutes`; `GET /` index is
   unchanged; `POST /scripts` with an interval script and then
   `POST /scripts/{id}/disable` stops the timer fires.
5. Manual end-to-end on a real node (operator script): store
   `docs/examples/periodic.star`, watch the room backlog receive the fetched
   line each interval, then restart KAM and observe the cycle restart one full
   interval after startup (D1/D4 semantics visible).
