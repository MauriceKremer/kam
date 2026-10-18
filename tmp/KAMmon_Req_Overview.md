# KAMmon Requirements: Overview Page

**Status:** Requirements only. Nothing is built.
**Scope:** The first page an operator sees: how busy the radio channel is, what kinds of packets are being heard, and whether rooms and scripts are healthy. Shown in the prototype as "Network overview". Design: [KAMmon_Design.md](KAMmon_Design.md) §4.2. Data: [KAMmon_Req_DataStore.md](KAMmon_Req_DataStore.md).

## 1. Summary

The Overview answers three questions in one screen: *Is the radio busy? What is being sent? Is my node healthy?* It is built from the event feed that KAM publishes (every packet heard or sent, with an estimated airtime) and from KAM's status snapshot. It is read-only for everyone.

Plain words: **airtime** is how long a packet occupies the radio channel. LoRa is slow, so a few packets already fill a noticeable part of every hour, and a shared channel can only carry so much.

## 2. Goals and non-goals

**Goals**

- GL-1. Show channel load and the mix of packet types for the last 15 minutes, hour or day.
- GL-2. Show node health: duty cycle, transmit queue, room and script activity.
- GL-3. Work from the in-memory store only.

**Non-goals**

- Total radio-frequency occupancy. KAM hears only what reaches its antenna.
- Alerts and notifications.
- A map of nodes.

## 3. What the page shows

| Area | Content |
|---|---|
| **Metric strip** (4) | Observed packet airtime (%); Node TX duty cycle (%) against its limit; Packets observed (RX and TX, with how many were repeats); Room posts in the last hour. |
| **Airtime chart** | Stacked bars, one per time slot, by packet class. Range switch 15m / 1h / 24h and a pause control. |
| **Packet mix** | The same classes as a thin bar and a list with counts and percentages. |
| **Recent packets** | The six newest: time, type, source or destination. A link to the Traffic page. |
| **Rooms** | Each room: serving state, last activity, posts in the last hour, remembered clients. Admin: the two most recent clients with the activity indicator. |
| **Automation** | Scripts ready and disabled; time of the last run. |
| **Node block** (sidebar) | Node name, link state, radio summary, uptime. |
| **Stale-data bar** | Shown above the page when KAM has not answered for over 10 s. |

### 3.1 Packet classes

| Class | Which events |
|---|---|
| Group text | `GRP_TXT` and `GRP_DATA`, received or sent |
| Adverts | `ADVERT`, received, or sent by KAM or a room |
| Room traffic | `REQ`, `RESPONSE`, `TXT_MSG`, `ACK`, `ANON_REQ`, `PATH`, `MULTIPART` whose `room` is set, and anything KAM sends for a room |
| Other | Everything else: peer packets for other nodes, `TRACE`, `CONTROL`, `RAW_CUSTOM`, unknown types |

### 3.2 Definitions

- **Observed packet airtime (%)** = the sum of `airtime_ms` of all `rx` and `tx` events in a slot, divided by the slot length. Repeats (`duplicate: true`) count because they occupied the channel.
- **Node TX duty cycle** = `used_ms` divided by the window, from `/v1/status`, shown beside the configured limit. It counts only what this node sent.
- **Packets observed** = all `rx` and `tx` events in the range. The caption shows RX and TX counts and how many RX events were repeats.
- **Chart slots**: at most 60 bars. 15m uses 15 one-minute bars; 1h uses 60 one-minute bars; 24h uses 60 bars of 24 minutes.

## 4. Requirements

| ID | Requirement | Level |
|---|---|---|
| MON-OV-1 | The page renders from the store alone; opening it makes no call to KAM. | MUST |
| MON-OV-2 | The four metrics and their captions follow §3.2. The airtime metric is labelled "Observed packet airtime", never "occupancy". | MUST |
| MON-OV-3 | The chart is drawn on the server as SVG, stacked by the four classes in §3.1, with the class colours from the design. | MUST |
| MON-OV-4 | Changing the range refreshes the chart, the packet mix and the metric strip together. | MUST |
| MON-OV-5 | If the store holds less than the range, the chart caption says "History starts at HH:MM". KAM restarts and gaps appear as marks on the axis. | MUST |
| MON-OV-6 | The chart has a text alternative with the range, the average and the peak. | MUST |
| MON-OV-7 | Recent packets show at most six rows with the same name rules as the Traffic page. | MUST |
| MON-OV-8 | Each room line shows "Last activity" from KAM's room list and posts in the last hour from `room` events. The wording says "remembered", not "online". | MUST |
| MON-OV-9 | Admin sees the two newest clients per room with the activity indicator ([KAMmon_Req_Rooms.md](KAMmon_Req_Rooms.md)). Viewers see no client names. | MUST |
| MON-OV-10 | The automation block shows counts from `GET /scripts` and the newest `run.last_run_at`. When `run` is absent it shows only counts. | SHOULD |
| MON-OV-11 | The metric strip and the recent-packets table refresh on their own every 2 seconds (htmx). The page's pause control stops them. | MUST |
| MON-OV-12 | When the duty cycle is above 80 % of its limit the caption turns amber and says so in words. | SHOULD |
| MON-OV-13 | When the link is `down`, the metric strip keeps the last values and the stale-data bar explains why. | MUST |

## 5. What KAMmon needs from KAM

| Need | Source | KAM requirement |
|---|---|---|
| Every packet heard or sent, with airtime | `rx` and `tx` events | API-EV-3, API-EV-4, API-EV-8 |
| Room posts and activity | `room` events and `GET /v1/rooms` | API-EV-5, API-RM-1 |
| Duty cycle, queue, link, radio summary | `GET /v1/status` | API-ST-1 |
| Script counts and last run | `GET /scripts` with `run` | API-AU-2 |
| Script queue depth | `GET /v1/status` (`automation`) | API-AU-5 |

## 6. Visibility

Viewers and admins see the same numbers. The admin also sees client names in the rooms block. No text from channels or rooms appears on this page for anyone.

## 7. Tests

- Bucket arithmetic: a known event list gives the expected percentage, mix and counts.
- Class mapping for each payload type, including `room` set and unset.
- A range switch with less history than the range shows the caption.
- Rendering with the store empty, with KAM down, and with only some snapshots filled.
- A snapshot test of the SVG for a fixed input.

## 8. Open points

- Whether "Room traffic" should also include `ACK` packets that KAM cannot tie to a room. Today they fall into Other.
- Whether the duty-cycle limit should be shown in the metric title or only in the caption.
