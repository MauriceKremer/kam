# KAMmon Requirements: Automations (Starlark Workspace)

**Historical draft:** Superseded by [KAMmon Automations requirements](../../kammon/docs/requirements/Automations.md). Dry runs are now bounded independently of live-script timeout settings.

**Status:** Requirements only. Nothing is built.
**Scope:** Creating, testing, saving and managing the Starlark scripts that KAM runs when it hears a channel message. Design: [KAMmon_Design.md](KAMmon_Design.md) §4.2 (master-detail). KAM side: [docs/FeatureMonitorApi.md](../docs/FeatureMonitorApi.md) §7.6. Language and runtime: [docs/FeatureStarlark.md](../docs/FeatureStarlark.md).

## 1. Summary

**Starlark** is a small scripting language that looks like Python. A script reacts to a channel message and may send a reply. KAM already has a Local Management API on `127.0.0.1:8765` to list, create, validate, save, enable, disable, recover and delete scripts, with revision checks. KAMmon becomes the screen for it.

KAMmon adds two things the API lacked: a **dry run**, which runs a script against a made-up message and shows what it would have said without sending anything, and **run information** (how often a script ran, how long the last run took, how many replies it queued).

A script can only do what KAM's `reply(channel, text)` allows. It cannot reach the radio, the files or the network. KAMmon must not suggest otherwise.

## 2. Goals and non-goals

**Goals**

- GL-1. See every script with its state and last run.
- GL-2. Write and edit a script in the browser, validate it, test it with a dry run, and save it.
- GL-3. Enable, disable, recover (after a timeout) and delete scripts safely.
- GL-4. Teach a beginner what a script receives and what it may do.

**Non-goals**

- Syntax highlighting, completion or a debugger in version 1.
- Running a script against real traffic from KAMmon.
- Version history beyond KAM's current revision number.
- Importing or exporting a script file (copy and paste is enough).

## 3. What the page shows

| Area | Content |
|---|---|
| **Script list** (left) | Name, state (READY, disabled, FROZEN, BROKEN), revision, a state dot, and a new-script button (admin). |
| **Editor** | Title with revision; toolbar with Validate, Dry run, Save; a monospace text area. |
| **Settings row** | Enabled checkbox, timeout in milliseconds, save state ("Saved revision 4", "Unsaved changes"). |
| **Console** | Result of the last Validate, Dry run or Save, as plain text. |
| **Last run** | "Last run 14:59:48 · 2 ms · 1 reply" from the `run` object. |
| **Fixture form** | A small form for the dry run: channel, sender, text, hops, SNR, RSSI, clock drift. Sensible defaults. |
| **Reference** | A collapsed panel: the `on_group_text(message)` signature, the fields of `message`, and `reply(channel, text)`. |
| **Recover** | For a FROZEN script: the last error and a Recover button. |

### 3.1 What a script receives

`message` is a read-only dictionary:

| Key | Meaning |
|---|---|
| `channel` | Normalised name such as `#emmen` |
| `sender` | Sender name |
| `body` | Message text |
| `timestamp_unix` | Sender's timestamp |
| `hop_count`, `hop_stations` | How far it travelled and the one-byte path hashes |
| `has_rx_meta`, `snr_quarter_db`, `rssi_dbm` | Signal data; SNR is in quarter dB (divide by 4) |
| `time_drift_seconds` | Difference between sender and KAM clocks |

Starter template offered for a new script:

```python
def on_group_text(message):
    if message["body"] == "!ping":
        reply(message["channel"], "pong")
```

## 4. Requirements

### 4.1 Reading

| ID | Requirement | Level |
|---|---|---|
| MON-AU-1 | The script list is read from `GET /scripts` (port 8765) every 10 s and when the page opens. | MUST |
| MON-AU-2 | The list shows `runtime_status` in words. A FROZEN script shows the last error and a Recover button; a BROKEN script shows the last error. | MUST |
| MON-AU-3 | When `run` is present the page shows last run time, duration and reply count; otherwise it shows nothing in its place. | MUST |
| MON-AU-4 | Viewers see names, state, revision and run information. They never see source, and no source is fetched for them. | MUST |
| MON-AU-5 | When KAM's script API is down, the list shows the last known state with the stale-data bar. | MUST |

### 4.2 Editing (admin)

| ID | Requirement | Level |
|---|---|---|
| MON-AU-6 | Opening a script loads it with `GET /scripts/{id}` and remembers its `revision`. | MUST |
| MON-AU-7 | Validate sends `POST /scripts/validate` with the current text and timeout and shows KAM's message verbatim (it contains line and column). | MUST |
| MON-AU-8 | Save sends the text, timeout, enabled state and `expected_revision`. Creating sends `expected_revision` 0 or none. | MUST |
| MON-AU-9 | A `409` shows "Changed elsewhere. Reload to see the latest version." and keeps the user's text in the editor so nothing is lost. | MUST |
| MON-AU-10 | Save does not enable a script by itself. Enabled is an explicit checkbox and uses the enable and disable endpoints with the current revision. | MUST |
| MON-AU-11 | Recover sends `POST /scripts/{id}/recover` with the revision and optional new timeout. | MUST |
| MON-AU-12 | Delete asks for confirmation and sends `DELETE /scripts/{id}?revision=N`. | MUST |
| MON-AU-13 | The timeout field takes a positive whole number of milliseconds. KAM sets no upper limit; a value above 5000 shows a note that a long script blocks other scripts, because scripts run one at a time. | SHOULD |
| MON-AU-14 | Leaving the page with unsaved changes asks for confirmation. | SHOULD |

### 4.3 Dry run

| ID | Requirement | Level |
|---|---|---|
| MON-AU-15 | Dry run sends `POST /scripts/dry-run` with the editor text, the timeout and the fixture message. It works on unsaved text. | MUST |
| MON-AU-16 | The result lists each reply as "would send to #channel: text", the duration, or the error. | MUST |
| MON-AU-17 | The console says "Nothing was sent." on every dry run. | MUST |
| MON-AU-18 | The fixture form keeps its values while the page is open and has a reset button. | SHOULD |
| MON-AU-19 | The reference panel states that a dry run uses the same engine and the same timeout rules but cannot reproduce load. | SHOULD |
| MON-AU-20 | While KAM answers `404` for the dry-run route, the button is absent and a line says "Needs a newer KAM". | MUST |

### 4.4 Honesty

| ID | Requirement | Level |
|---|---|---|
| MON-AU-21 | The page never implies that a script can reach anything but `reply`. | MUST |
| MON-AU-22 | A replying script uses airtime. The page shows a line: "Replies are real radio transmissions and count against the duty cycle." | MUST |
| MON-AU-23 | Source text is escaped when shown and is never run in the browser. | MUST |

## 5. What KAMmon needs from KAM

| Need | Endpoint | KAM requirement |
|---|---|---|
| List, get, create, update, delete | `GET/POST /scripts`, `GET/PUT/DELETE /scripts/{id}` | API-AU-1 (exists) |
| Validate | `POST /scripts/validate` | API-AU-1 (exists) |
| Enable, disable, recover | `POST /scripts/{id}/enable|disable|recover` | API-AU-1 (exists) |
| Run information | `run` object on each script | API-AU-2 |
| Dry run | `POST /scripts/dry-run` | API-AU-3, API-AU-4 |
| Queue depth and dropped events | `GET /v1/status` (`automation`) | API-AU-5 |
| Script run events | `script` events | API-EV-6 |

KAMmon uses two base URLs: scripts on port 8765, everything else on 8766.

## 6. Visibility

| Element | Viewer | Admin |
|---|---|---|
| Script names, state, revision, run information | Yes | Yes |
| Source, dry run, save, enable, delete, recover | No | Yes |

## 7. Tests

- Fake KAM: validate ok and error, save ok and `409`, enable and disable with a stale revision, recover, delete, dry run ok and error and `404`.
- A `409` keeps the user's text.
- A viewer cannot fetch a script's source.
- The dry-run console always says nothing was sent.
- Starter template and fixture defaults match `message` keys in §3.1.

## 8. Corrections to the prototype

The prototype's sample scripts use `msg.text` and `reply(msg, ...)`. Real scripts use `on_group_text(message)`, `message["body"]` and `reply(channel, text)`. The prototype's Validate and Dry run only print simulated text.

## 9. Open points

- Whether KAMmon should show an event list of recent `script` events per script.
- Whether the editor needs a line-number gutter in version 1.
