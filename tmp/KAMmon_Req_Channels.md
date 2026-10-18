# KAMmon Requirements: Channels Page

**Status:** Requirements only. Nothing is built.
**Scope:** Monitoring the channels KAM is configured to decode: how active each one is, and what was said on it. Design: [KAMmon_Design.md](KAMmon_Design.md) §4.2 (master-detail). KAM side: [docs/FeatureMonitorApi.md](../docs/FeatureMonitorApi.md) §7.2.

## 1. Summary

A **channel** is a shared conversation, for example `#public`. Everyone who holds the channel's key can read it. KAM decodes the channels listed in `kam.yml` and stores the messages in SQLite. KAMmon lists those channels, shows their activity from the event feed, and, for those allowed to read it, the stored message text.

A channel is **not** a room. A room is a message board hosted by KAM with logins; see [KAMmon_Req_Rooms.md](KAMmon_Req_Rooms.md). All channels share the one radio and one frequency, so KAMmon shows channels as conversations, not as radio settings.

Keys are secrets. KAMmon never shows them and does not edit channels in version 1. A channel is changed in `kam.yml` or with `kam channel upsert`; KAM picks up a valid change while running.

## 2. Goals and non-goals

**Goals**

- GL-1. See which channels exist, whether decoding is on, and how active each is.
- GL-2. Read recent messages on a channel and search them (where allowed).
- GL-3. See how much airtime a channel uses.
- GL-4. Notice group texts that no configured channel can read.

**Non-goals**

- Showing or editing keys, adding channels, or changing a channel's enabled state.
- Sending messages from KAMmon.
- Full-text search over the whole database; search is a plain substring on one channel.
- Reading messages for a channel that was never decoded.

## 3. What the page shows

| Area | Content |
|---|---|
| **Channel list** (left) | One item per channel: name, key type (public, derived, explicit), enabled or disabled, messages in the last hour. |
| **Detail header** | Channel name, key type, "Decoding enabled" or "Disabled". |
| **Metric strip** (3) | Decoded messages in the last hour; last decoded message ("2 s ago"); received airtime in the last hour. |
| **Caption** | "Airtime attributed to decoded RX packets, not total radio occupancy. Messages KAM cannot decrypt are not assigned to a channel here." |
| **Search box** | Sender or message text. |
| **Messages** | Newest first: sender, time, text, then route, SNR and RSSI. |
| **Footnote** | "Channel keys and enabled states live in kam.yml. No key material is shown." |
| **Unreadable** | "N group texts in the last hour could not be decoded (no matching channel key)." |

## 4. Requirements

| ID | Requirement | Level |
|---|---|---|
| MON-CH-1 | The channel list comes from `GET /v1/channels` (polled every 30 s). A new or removed channel appears within that time. | MUST |
| MON-CH-2 | Metric values come from `rx` events in the store: count of decoded group texts with `channel = name` and `duplicate = false`, the newest one's time, and the sum of `airtime_ms` of all `rx` events for that channel. | MUST |
| MON-CH-3 | A disabled channel is shown muted with "Decoding disabled". It still opens and shows its stored history. | MUST |
| MON-CH-4 | The key type is shown as a word. The key itself, and any value derived from it, never appears. | MUST |
| MON-CH-5 | The message list for an allowed viewer or an admin is fetched on demand from `GET /v1/channels/{name}/messages` (50 per request) and cached for 5 seconds. | MUST |
| MON-CH-6 | "Older messages" loads the next 50 with `before_id`. | MUST |
| MON-CH-7 | Search sends `q` to KAM and shows the result. Empty results say "No messages match this filter." | MUST |
| MON-CH-8 | For a viewer on a channel that is not allow-listed, the page shows the metrics and a list of rows built from events (time, route, hops, SNR) with "Text hidden". No call is made to the messages endpoint. | MUST |
| MON-CH-9 | SNR and RSSI appear on a message when KAM stored them; otherwise the footer shows the route and hops only. | MUST |
| MON-CH-10 | The unreadable-text line counts `rx` events with `payload_type = GRP_TXT` and no `channel`, per hour. | SHOULD |
| MON-CH-11 | Message text is escaped when rendered. Names and text from the radio are untrusted. | MUST |
| MON-CH-12 | When KAM does not answer, the list shows the last cached page with the stale-data bar; with no cache it says "Messages are unavailable right now." | MUST |

## 5. What KAMmon needs from KAM

| Need | Endpoint | KAM requirement |
|---|---|---|
| Channel names, key type, enabled, stored count, last message | `GET /v1/channels` | API-CH-1, API-CH-4 |
| Stored messages with paging and search | `GET /v1/channels/{name}/messages` | API-CH-2 |
| SNR and RSSI per stored message | Two nullable columns in `group_texts` | API-CH-3 |
| Live counts and airtime | `rx` events with `channel` and `airtime_ms` | API-EV-3, API-EV-8 |

If API-CH-3 is not built, MON-CH-9 shows the route and hops only; the rest of the page is unaffected.

## 6. Visibility

| Element | Viewer | Admin |
|---|---|---|
| Channel list, key type, counts, airtime, last activity | Yes | Yes |
| Message text and sender | Only channels in `viewer_text_channels` | Yes |
| Rows with time, route, hops and SNR | Yes, "Text hidden" where needed | Yes |
| Key material | Never | Never |

## 7. Tests

- Metric arithmetic from a fixed event list, including repeats.
- The three message states: allowed, hidden for a viewer, KAM down.
- A viewer's page source does not contain hidden text.
- Paging with `before_id` and an empty result.
- A disabled channel with and without stored history.

## 8. Open points

- Whether the message list should also update live between fetches (the event feed has no text, so this needs another fetch every few seconds for admins).
- Whether a viewer should be told which channels are on the allow list.
