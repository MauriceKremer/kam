# KAMmon Requirements: Traffic Page

**Status:** Requirements only. Nothing is built.
**Scope:** A live table of what the radio hears and sends, with its type, route, strength and airtime. It answers "what kind of messages are being sent?". Design: [KAMmon_Design.md](KAMmon_Design.md) §4.2 (filter and table). Data: [KAMmon_Req_DataStore.md](KAMmon_Req_DataStore.md).

## 1. Summary

Every packet KAM hears or sends becomes an event with its type, route, hop count, signal strength and an estimated airtime, **but never its payload**. The Traffic page shows those events from KAMmon's memory, newest first, and lets the operator filter and export them. It includes repeats and the packet types KAM does not otherwise process, which is why it can show the real mix on the air.

Plain words: **route** says how a packet travels. A *flood* is repeated by every repeater that hears it; a *direct* packet follows a known path. **Hops** is how many repeaters it passed. **SNR** (signal-to-noise ratio) and **RSSI** (received signal strength) say how well KAM heard it.

## 2. Goals and non-goals

**Goals**

- GL-1. Show recent traffic as a table that updates by itself.
- GL-2. Let the operator find a packet type, a name or a direction quickly.
- GL-3. Export what is on screen as CSV.

**Non-goals**

- Payload bytes, decoded message text or a packet capture file.
- Traffic older than the store holds (the most recent 4096 events).
- Filtering by a time range beyond what the store holds.

## 3. What the page shows

| Area | Content |
|---|---|
| **Filter bar** | Search box (source or destination), packet type select, direction select (all, RX, TX), "Hide repeats" toggle, Export CSV. |
| **Type summary** | One line per packet type in the current filter: count and percentage. |
| **Table** | Time, direction, type tag, source / destination, route and hops, SNR, estimated airtime. Repeats are marked "repeat". |
| **Footnote** | "Observed packets only. Unattributed or undecryptable payloads keep their visible packet type." |

### 3.1 Source and destination text

| Event | Text |
|---|---|
| `rx` `GRP_TXT`, decoded | "PA3M → #emmen" (sender, channel) |
| `rx` `GRP_TXT`, no channel key | "Unknown channel" |
| `rx` `ADVERT` | The advert name, or "Unnamed advert" |
| `rx` peer packet with `room` set | "→ Emmen local (probable)" |
| `rx` other | The type name and `dest_hash` when present |
| `tx` | "KAM → #emmen", "KAM → Emmen local", "KAM advert" from `origin` |

## 4. Requirements

| ID | Requirement | Level |
|---|---|---|
| MON-TR-1 | The table is built from the store's event ring. Opening the page makes no call to KAM. | MUST |
| MON-TR-2 | Rows are newest first, 100 on first load, with a "Show 100 more" control up to what the store holds. | MUST |
| MON-TR-3 | New rows appear at the top every 2 seconds. A "Pause" control stops the refresh so a row can be read. | MUST |
| MON-TR-4 | Every `rx` event is shown, including repeats and packet types KAM does not handle. | MUST |
| MON-TR-5 | Filters: free text on the source / destination text, packet type, direction, hide repeats. They combine with AND. | MUST |
| MON-TR-6 | Filter state is in the URL query string so a view can be shared and survives a reload. | SHOULD |
| MON-TR-7 | The type summary reflects the filter and the store contents, not just the 100 visible rows. | SHOULD |
| MON-TR-8 | SNR and RSSI show a dash when the event has none; zero is never used to mean unknown. | MUST |
| MON-TR-9 | Airtime is labelled estimated. | MUST |
| MON-TR-10 | Export CSV writes the filtered rows that the store holds (not only the visible ones). Columns match the table. The file is named `kammon-traffic-<time>.csv`. | MUST |
| MON-TR-11 | In CSV, a value that starts with `=`, `+`, `-` or `@` gets a leading apostrophe, because names come from the radio. | MUST |
| MON-TR-12 | The page shows "History starts at HH:MM" when the store holds fewer events than the user asked for. | MUST |
| MON-TR-13 | The table sits in a scroll region; the page never scrolls sideways. | MUST |

## 5. What KAMmon needs from KAM

| Need | Source | KAM requirement |
|---|---|---|
| Event feed with type, route, hops, SNR, RSSI, airtime, repeat flag, channel, name, `room` | `GET /v1/events` (`rx`, `tx`) | API-EV-2, API-EV-3, API-EV-4, API-EV-8 |
| Events for unhandled types | `rx` events for every parsed packet | API-EV-3 |
| No payload in events | Metadata only | API-EV-9 |

## 6. Visibility

| Element | Viewer | Admin |
|---|---|---|
| Time, direction, type, route, hops, SNR, airtime | Yes | Yes |
| Advert names | Yes (adverts are public over the air) | Yes |
| Group-text sender | Only for channels in `viewer_text_channels` | Yes |
| Channel name | Yes | Yes |
| Export CSV | Yes, with the same hidden fields | Yes |

Hidden senders show as "Sender hidden". Filtering by text never matches hidden fields, so a viewer cannot find a sender by searching.

## 7. Tests

- Filter combinations against a fixed event list.
- Text for every event kind in §3.1.
- Viewer view model: hidden sender, unmatched search.
- CSV: escaping, formula guard, viewer columns.
- Store with fewer events than requested; paging control.

## 8. Open points

- Whether a "dup" counter per type is useful next to the type summary.
- Whether to show the one-byte path hashes of the hops. The events do not carry them today.
