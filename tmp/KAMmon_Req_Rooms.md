# KAMmon Requirements: Room Monitor

**Status:** Requirements only. Nothing is built.
**Scope:** Watching the room servers KAM hosts: how active each room is, which clients it remembers and when it last heard them, and the posts in it. Managing roles is in [KAMmon_Req_RoomAccounts.md](KAMmon_Req_RoomAccounts.md). Design: [KAMmon_Design.md](KAMmon_Design.md) §4.2, §8.2. Findings behind the wording: `tmp/kammon-research.md`.

## 1. Summary

A **room** is a message board hosted by KAM. Radios log in with a password, post, and receive posts they missed. A room is not a channel.

KAM remembers a client after it logs in, and does **not** forget it when the radio goes quiet. So the honest numbers are *when KAM last heard a client* and *how many posts arrived recently*, not "how many are online". KAMmon shows exactly that, with an activity indicator that makes silence visible: green, amber, grey, and an explicit **Stale Node** after 24 hours.

## 2. Goals and non-goals

**Goals**

- GL-1. Show each room's state, last activity and posting rate.
- GL-2. Show each remembered client's last activity with a clear indicator (admin).
- GL-3. Show recent posts (admin).
- GL-4. Never present remembered sessions as online users.

**Non-goals**

- Presence ("who is online right now"). KAM cannot know.
- Sending posts from KAMmon.
- Creating, deleting or renaming rooms (those are `kam.yml` settings read at start).

### 2.1 Plain words

- **Remembered client:** a client KAM has a session for.
- **Last heard:** the last valid request KAM received from that client.
- **Stale Node:** a remembered client not heard for over 24 hours. The session still exists; KAM does not expire sessions.
- **Unknown:** KAM has no last-heard time for the client.

## 3. What the page shows

| Area | Content |
|---|---|
| **Room list** (left) | Name, last activity, posts in the last hour. |
| **Detail header** | Room name, "Serving", the room's short public key with a copy control. |
| **Metric strip** (3) | Last activity; Posts last hour; Remembered clients, with "N heard in the last hour" below. |
| **Caption** | "Last heard is radio activity, not a live connection. Stale Node means a stored client was not heard for over 24 hours; its session has not expired. Unknown means no reliable last-heard time is available." |
| **Legend** | The five activity states ([KAMmon_Design.md](KAMmon_Design.md) §8.2). |
| **Client activity** (admin) | Table: label or key, role, last activity with indicator. |
| **Delivery** | Pushes acked and timed out in the last hour; clients paused after repeated failures. |
| **Room feed** (admin) | Newest posts: author, time, text. |

### 3.1 Activity indicator

| State | Heard | Look |
|---|---|---|
| Fresh | under 5 minutes ago | green dot |
| Recent | 5 to 60 minutes ago | amber dot |
| Older | 1 to 24 hours ago | hollow grey dot |
| Stale Node | over 24 hours ago | grey dotted line, text "Stale Node" |
| Unknown | no time | hollow grey dot, text "Unknown" |

Boundaries: under 300 s is Fresh; 300 to 3599 s is Recent; 3600 to 86400 s is Older; above 86400 s is Stale Node.

## 4. Requirements

| ID | Requirement | Level |
|---|---|---|
| MON-RM-1 | The room list and counters come from `GET /v1/rooms` (polled every 10 s). Posts in the last hour come from `room` events with `event = post`. | MUST |
| MON-RM-2 | "Remembered clients" is the room's `clients_remembered`. The words "online" and "connected" are not used. | MUST |
| MON-RM-3 | "N heard in the last hour" counts clients in `GET /v1/rooms/{id}/clients` with `last_activity_at` inside the hour. | MUST |
| MON-RM-4 | The activity indicator follows §3.1 and is computed from KAM time, not KAMmon's clock. | MUST |
| MON-RM-5 | A client with no `last_activity_at` is **Unknown**, never Stale. | MUST |
| MON-RM-6 | The legend is always shown above a client table, and every indicator has a title that explains it in words. | MUST |
| MON-RM-7 | The indicator never relies on colour alone: it also has a shape (dot, hollow dot, dotted line) and text. | MUST |
| MON-RM-8 | Admin only: the client table shows label or key, role, last activity. Without a label the full key is available on hover and in a copy control. | MUST |
| MON-RM-9 | Admin only: the room feed is fetched from `GET /v1/rooms/{id}/posts` (20 posts) when the room is opened, then refreshed every 10 s while the room is open. | MUST |
| MON-RM-10 | Admin only: a post shows the author's label or short key, time and text. Text is escaped. | MUST |
| MON-RM-11 | Delivery shows pushes acked and timed out from `room` events, and the number of clients whose `push_state` is `paused`. | SHOULD |
| MON-RM-12 | A viewer sees the room list, the metric strip, the caption and the legend, and a line "Client activity and posts are visible to the administrator." No client or post data is fetched for a viewer. | MUST |
| MON-RM-13 | A room that is disabled or failed validation in `kam.yml` does not appear. The page says nothing about it. | MUST |
| MON-RM-14 | When `GET /v1/rooms` answers `503` (rooms disabled), the Rooms navigation item shows "Rooms are not enabled". | MUST |

## 5. What KAMmon needs from KAM

| Need | Endpoint | KAM requirement |
|---|---|---|
| Rooms, public key, remembered clients, counters, last activity | `GET /v1/rooms` | API-RM-1 |
| Clients with role, last activity, push state, label | `GET /v1/rooms/{id}/clients` | API-RM-2, API-RM-8 |
| Recent posts | `GET /v1/rooms/{id}/posts` | API-RM-3 |
| Posts and logins as they happen | `room` events | API-EV-5 |
| Last-heard time that survives a KAM restart | `last_heard_at` stored with the 5-second client write | API-RM-9 |

Without API-RM-9, every client reads Unknown after a KAM restart until it is heard again. KAMmon must cope: it shows "Unknown" and the caption explains it.

## 6. Visibility

| Element | Viewer | Admin |
|---|---|---|
| Room list, public key, counters, last activity | Yes | Yes |
| Heard in the last hour (a number) | Yes | Yes |
| Client table with keys and labels | No | Yes |
| Room posts | No | Yes |

## 7. Tests

- Indicator boundaries: 0, 299, 300, 3599, 3600, 86400, 86401 seconds and no time.
- Unknown is never rendered as Stale.
- The viewer page does not contain a client key list or a post.
- Rooms disabled and a room list with zero rooms.
- A client list with 100 clients renders within the page time budget.

## 8. Open points

- Whether "heard in the last hour" should count the 60-minute boundary inclusive.
- Whether Stale Node clients should be sorted last.
