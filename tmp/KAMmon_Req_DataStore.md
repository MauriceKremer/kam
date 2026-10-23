# KAMmon Requirements: In-Memory Data Store and Poller

**Historical draft:** Superseded by [KAMmon DataStore requirements](../../kammon/docs/requirements/DataStore.md). Use `next_after`, not the ring head `last_seq`, to continue event pagination.

**Status:** Requirements only. Nothing is built.
**Scope:** How KAMmon keeps the data it shows, so that its pages never depend on KAM's API speed or on SQLite. Design context: [KAMmon_Design.md](KAMmon_Design.md). KAM side: [docs/FeatureKamApi.md](../docs/FeatureKamApi.md) (`API-EV-*`, `API-ST-1`).

## 1. Summary

KAM is the system of record and works without KAMmon. KAMmon is a client with a short memory. One goroutine, the **poller**, asks KAM for news every one to two seconds and writes what it learns into an in-memory **store**. Every page reads from the store. If KAM is slow, restarting or gone, the pages still render from what the store holds and say how old it is.

Nothing is written to disk. When KAMmon restarts, it asks KAM for the events still in KAM's ring and rebuilds the last stretch of history. Older history is simply not there, and the screen says so.

Plain words: a **bucket** is the total of one minute of events, kept so charts do not need to loop over every event. A **cursor** is "the number of the last event I have already seen".

## 2. Goals and non-goals

**Goals**

- GL-1. A page view never waits for KAM, and never reads KAM's database.
- GL-2. KAMmon recovers by itself after a restart of KAMmon or of KAM, with no manual step.
- GL-3. Memory use is bounded and small enough for a Raspberry Pi class computer.
- GL-4. The screen always shows how fresh the data is, and where history has a gap.

**Non-goals**

- Writing history to disk, a snapshot file or a database (decision D2).
- More than 24 hours of history.
- Asking KAM to remember anything for KAMmon: KAM keeps no cursor, session or subscription for it.
- Sharing the store between several KAMmon processes.

## 3. What the store holds

| Data | Shape | Size limit | Fed by |
|---|---|---|---|
| **Recent events** | Ring of the newest events (rx, tx, room, script, modem) | 4096 events, about 1 MB | `GET /v1/events` |
| **Minute buckets** | One entry per minute, last 24 hours (1440): per packet class, count and estimated airtime, plus counts for duplicates, room posts and script runs | 1440 buckets, under 200 KB | Built from events |
| **Status snapshot** | Latest `/v1/status` | One | Poll every 2 s |
| **Modem snapshot** | Latest `/v1/modem` | One | Poll every 10 s |
| **Channel list** | Latest `/v1/channels` | One | Poll every 30 s |
| **Room list and clients** | Latest `/v1/rooms` and per-room `/clients` | Up to 4 rooms, 100 clients each | Poll every 10 s |
| **Script list** | Latest `GET /scripts` (port 8765) | One | Poll every 10 s |
| **Text cache** | Newest page of channel messages and room posts, for admin pages | 10 pages, 5 s lifetime | On demand |
| **Command log** | Lines from modem commands | 200 lines | Admin actions |
| **History marks** | "history starts at", "KAM restarted at", "gap from, to" | 50 marks | Poller |

Total expected size: under 5 MB.

## 4. Requirements

### 4.1 Poller

| ID | Requirement | Level |
|---|---|---|
| MON-DS-1 | One poller goroutine owns all calls that read from KAM. Pages and admin actions do not poll. | MUST |
| MON-DS-2 | Each read has its own timeout (3 s) and its own interval (§3). A failure of one read does not stop the others. | MUST |
| MON-DS-3 | After a failure the poller waits 1, 2, 4, 8 seconds, then stays at 10 seconds, and returns to normal after the first success. | MUST |
| MON-DS-4 | The poller never sends a request body and never changes anything in KAM. | MUST |
| MON-DS-5 | KAMmon starts and serves pages even when KAM is down. The pages show "Waiting for KAM" until the first answer. | MUST |

### 4.2 Events and the cursor

| ID | Requirement | Level |
|---|---|---|
| MON-DS-6 | KAMmon keeps its own cursor (`after`) and the KAM `epoch`, in memory only. | MUST |
| MON-DS-7 | First start: ask with `after=0` and repeat with the new `last_seq` until the reply holds fewer than `limit` events. This is the backfill. | MUST |
| MON-DS-8 | If a reply has `reset: true`, keep the existing buckets, add a **gap** mark at the reply time, and continue from the new cursor. | MUST |
| MON-DS-9 | A new `epoch` means KAM restarted. Add a "KAM restarted at" mark, take the new `epoch`, and start again from `after=0`. | MUST |
| MON-DS-10 | Events are placed in buckets by their KAM time (`at`), not by KAMmon's clock. | MUST |
| MON-DS-11 | A late event (older than the newest bucket) goes into its own bucket; buckets older than 24 hours are dropped. | MUST |
| MON-DS-12 | Events carry no message text, so the store holds none from the event feed. | MUST |

### 4.3 Freshness and health

| ID | Requirement | Level |
|---|---|---|
| MON-DS-13 | Each snapshot remembers when it was last refreshed. A page shows data as **fresh** (under 10 s old), **stale** (10 s to 2 min) or **down** (older, or never). | MUST |
| MON-DS-14 | Stale and down data stay on screen with the stale-data bar: "KAM has not answered for 12 s. Showing data from 14:59:46." | MUST |
| MON-DS-15 | KAMmon measures the difference between KAM's `now` (from `/v1/status`) and its own clock, and shows KAM time on every screen. | SHOULD |
| MON-DS-16 | A chart caption says "History starts at HH:MM" when the store holds less than the selected range, and shows marks for gaps and KAM restarts. | MUST |
| MON-DS-17 | The status area of the sidebar shows the link as `up`, `stale` or `down` from the freshness rule. | MUST |

### 4.4 Memory and concurrency

| ID | Requirement | Level |
|---|---|---|
| MON-DS-18 | All sizes in §3 are hard limits. The oldest entry is dropped when a limit is reached. | MUST |
| MON-DS-19 | The poller is the only writer. Pages read an immutable snapshot (an atomic pointer swap), never a structure that is being changed. | MUST |
| MON-DS-20 | The store has no lock that a page view can hold while it waits for KAM or for the network. | MUST |
| MON-DS-21 | The text cache is only filled by admin pages and expires after 5 seconds. Viewers never trigger a text fetch. | MUST |
| MON-DS-22 | A KAMmon restart loses the command log, sessions and marks. This is accepted. | MUST |

## 5. What KAMmon needs from KAM

| Need | Endpoint | Interval |
|---|---|---|
| New events and the cursor | `GET /v1/events?after=&epoch=&limit=` | 1-2 s |
| Status, radio, duty cycle, counters | `GET /v1/status` | 2 s |
| Modem snapshot | `GET /v1/modem` | 10 s |
| Channel list | `GET /v1/channels` | 30 s |
| Rooms and their clients | `GET /v1/rooms`, `GET /v1/rooms/{id}/clients` | 10 s |
| Scripts | `GET /scripts` | 10 s |

KAM requirements: `API-EV-1`, `API-EV-2`, `API-ST-1`, `API-F-7`..`API-F-9`.

## 6. Behaviour table

| Situation | What KAMmon does |
|---|---|
| KAMmon starts, KAM running | Backfill from the ring; history starts at the oldest event in the ring |
| KAMmon starts, KAM down | Serve pages with "Waiting for KAM"; poll with backoff |
| KAM restarts while KAMmon runs | New epoch; add a restart mark; keep buckets; continue |
| KAMmon was away longer than the ring covers | `reset: true`; gap mark; continue |
| One read keeps failing (for example `/v1/modem`) | Only that panel goes stale; the rest stays fresh |
| KAM answers `404` for an endpoint | That feature shows "Needs a newer KAM" and the poller stops asking for 10 minutes |
| KAM is slow (over 3 s) | Treated as a failure for that read |
| Browser asks for a page while KAM is down | The page renders from the store with the stale-data bar |

## 7. Tests

- A fake KAM (an `httptest` server behind the `kam` client interface) drives table tests for: first backfill, caught up, gap, new epoch, 404, slow answer, malformed JSON.
- Bucket tests: events in order, out of order, across the 24-hour edge, duplicates counted separately.
- A race test: 50 page reads while the poller writes.
- A memory test: feed 1 million events and check that the sizes in §3 hold.
- An optional `--demo` mode serves generated data through the same store, for development and screenshots. This replaces the prototype's sample data.

## 8. Open points

- Whether the demo mode ships in the release binary or only in a test build.
- Whether 24 hours of minute buckets should become 5-minute buckets beyond the first hour. Not needed at this size.
