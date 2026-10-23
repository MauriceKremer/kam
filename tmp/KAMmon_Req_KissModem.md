# KAMmon Requirements: KISS Modem

**Historical draft:** Superseded by [KAMmon KissModem requirements](../../kammon/docs/requirements/KissModem.md). Signal reporting is now status-only and always enabled; failed modem changes attempt restoration.

**Status:** Requirements only. Nothing is built.
**Scope:** Seeing the state of the KISS modem, running read-only diagnostic commands, and changing the radio settings. Design: [KAMmon_Design.md](KAMmon_Design.md) §4.2 (form plus snapshot). KAM side: [docs/FeatureKamApi.md](../docs/FeatureKamApi.md) §7.5. Protocol and firmware behaviour: [docs/FeatureKissModem.md](../docs/FeatureKissModem.md) and [manuals/FeatureKissModem.html](../manuals/FeatureKissModem.html).

## 1. Summary

The **KISS modem** is the small radio board KAM talks to over a serial port. It does the LoRa radio work. KAM controls it with typed commands, mostly `SetHardware` (`0x06`) sub-commands.

The page has three parts: a **snapshot** of what the modem reports, a **query** box for read-only commands, and a **form** that changes the radio settings. A change is applied by KAM, checked by reading back the modem's stored copy, and then saved into `kam.yml` so that the next start and the reset probe use the same values (decision D5).

Three facts decide the wording on this page (from the firmware):

1. The modem answers `OK` to `SetRadio` and `SetTxPower` after only a length check. **OK means received, not applied.**
2. `GetRadio` and `GetTxPower` return a copy the host stored earlier. **A matching read-back is an echo, not proof that the radio chip accepted the values.**
3. Only RSSI, noise floor, channel-busy and the counters read real hardware.

You are responsible for choosing a frequency and power that is legal where you operate.

## 2. Goals and non-goals

**Goals**

- GL-1. Show the modem's identity, link and readings with their age.
- GL-2. Run any read-only command and show its reply.
- GL-3. Change radio settings with a preview, a clear result and a saved config.
- GL-4. Never say more than the modem proved.

**Non-goals**

- `Reboot`, the modem's crypto and identity commands, `TXtail` (unused by the firmware).
- Changing the serial port or baud rate (they need a restart).
- Full duplex. LoRa is half-duplex; KAMmon shows the value and does not let it be switched on.
- Presets by region or country.

## 3. What the page shows

| Area | Content |
|---|---|
| **Radio form** | LoRa parameters and channel-access (CSMA) parameters, each with a hint naming the KISS command and the allowed range. |
| **Notice** | "Echo, not hardware verification. SetRadio and SetTxPower answer OK when received. GetRadio and GetTxPower return stored copies. Use settings that are legal for your radio and your region." |
| **Buttons** | "Review commands" and "Restore current values". |
| **Snapshot** (right) | Device name, serial link, cached radio, RSSI, noise floor, channel busy, MCU temperature, battery, counters. |
| **Query box** | A select of read-only commands, extra fields for `GetAirtime` (packet length) and `GetSensors` (permissions), and a Run button. |
| **Command log** | Time-stamped request and reply lines, newest at the bottom. |
| **Dialog** | "Review commands": the exact steps that will be sent, with Cancel and Apply. |

### 3.1 Form fields

| Field | Unit in the form | Sent to KAM | Range | KISS command |
|---|---|---|---|---|
| Frequency | MHz, 6 decimals | `frequency_hz` | 300 to 2500 MHz | `SetRadio` `0x09` |
| Bandwidth | kHz | `bandwidth_hz` | 7.8 to 500 kHz | `SetRadio` |
| Spreading factor | SF5 to SF12 | `spreading_factor` | 5 to 12 | `SetRadio` |
| Coding rate | 4/5 to 4/8 | `coding_rate` | 5 to 8 | `SetRadio` |
| TX power | dBm | `tx_power_dbm` | 1 to 22 | `SetTxPower` `0x0A` |
| Signal metadata | checkbox | `signal_report` | on or off | `SetSignalReport` `0x19` |
| TX delay | ms, steps of 10 | `csma.txdelay_ms` | 0 to 2550 | KISS `0x01` |
| Persistence | number | `csma.persistence` | 0 to 255 | KISS `0x02` |
| Slot time | ms, steps of 10 | `csma.slot_time_ms` | 0 to 2550 | KISS `0x03` |
| Full duplex | read-only text | `csma.full_duplex` | off | KISS `0x05` |

Persistence note: the modem transmits when a random number from 0 to 255 is at most the value, so the chance is (value + 1) / 256.

### 3.2 Read-only commands in the query box

`Ping`, `GetRadio`, `GetTxPower`, `GetCurrentRssi`, `IsChannelBusy`, `GetAirtime`, `GetNoiseFloor`, `GetVersion`, `GetStats`, `GetBattery`, `GetMCUTemp`, `GetSensors`, `GetDeviceName`, `GetSignalReport`.

## 4. Requirements

### 4.1 Snapshot and queries

| ID | Requirement | Level |
|---|---|---|
| MON-MD-1 | The snapshot is read from `GET /v1/modem` every 10 s and shows the time it was read. | MUST |
| MON-MD-2 | A value KAM reports as unsupported shows "Not supported by this board" and is not an error. A noise floor of 0 shows "Not calibrated yet". | MUST |
| MON-MD-3 | The radio line is labelled "Cached by the modem", never "Current radio setting". | MUST |
| MON-MD-4 | A query sends `POST /v1/modem/query` and appends the request and reply to the command log. | MUST |
| MON-MD-5 | `GetAirtime` takes a length of 1 to 255 bytes; `GetSensors` takes permissions 1 to 7. Other fields are checked in the browser and again by KAM. | MUST |
| MON-MD-6 | The query result shows the value, the raw reply bytes in hex, and the time taken. | SHOULD |
| MON-MD-7 | When the link is down the form and query box are disabled with "The modem is not connected." The snapshot shows its last values marked stale. | MUST |

### 4.2 Changing the radio

| ID | Requirement | Level |
|---|---|---|
| MON-MD-8 | The form starts with the current values from KAM. "Restore current values" resets it. | MUST |
| MON-MD-9 | The browser checks every range in §3.1 and shows the message beside the field. KAM checks again; a `422` message is shown against the named field. | MUST |
| MON-MD-10 | "Review commands" sends `PUT /v1/modem/radio?preview=true` and shows the steps returned by KAM in a dialog. KAMmon builds no protocol frames itself. | MUST |
| MON-MD-11 | The dialog says "Review only until you press Apply" and lists what has no reply (the CSMA frames). | MUST |
| MON-MD-12 | Apply sends `PUT /v1/modem/radio` without `preview`. The dialog shows "Applying" and cannot be closed by a second click. | MUST |
| MON-MD-13 | The result lists every step with its outcome and says one of: "Applied and saved. The modem echoed the same values. The radio chip itself is not checked." or "Not applied" with the reason. | MUST |
| MON-MD-14 | A `409` (another change running) or `503` (link down) shows its message and leaves the form as entered. | MUST |
| MON-MD-15 | After a successful change the snapshot is refreshed at once, and the node block and Overview show the new radio line. | MUST |
| MON-MD-16 | If KAM reports a rollback, the page says which values the modem has again. | MUST |
| MON-MD-17 | The page notes that saving rewrites `kam.yml` and removes any comments in it. | MUST |
| MON-MD-18 | The legal-use sentence from §1 is shown next to the Apply control. | MUST |
| MON-MD-19 | If `full_duplex` is on in KAM, the page shows a warning that this is harmful on a half-duplex LoRa radio, and does not let it be changed. | SHOULD |

### 4.3 Command log

| ID | Requirement | Level |
|---|---|---|
| MON-MD-20 | The log keeps the last 200 lines in KAMmon memory, shared by admin sessions, lost at restart. | MUST |
| MON-MD-21 | Each line shows the time, the command name and code, and the reply or "no reply". Lines never claim a setting was verified. | MUST |
| MON-MD-22 | The log is an `aria-live` region that scrolls inside its box. | MUST |

## 5. What KAMmon needs from KAM

| Need | Endpoint | KAM requirement |
|---|---|---|
| Snapshot | `GET /v1/modem` | API-MD-1, API-MD-10 |
| One read-only command | `POST /v1/modem/query` | API-MD-2 |
| Preview and apply | `PUT /v1/modem/radio` (`?preview=true`) | API-MD-3, API-MD-4 |
| One change at a time, safe timing | Behaviour | API-MD-5 |
| Saved only after a matching echo; rollback | Behaviour | API-MD-6 |
| New values reach the probe, airtime estimate and scheduler | Behaviour | API-MD-7 |
| Wording of results | `modem_echo` | API-MD-8 |
| Link and reset events | `modem` events | API-EV-7 |

KAM keeps the radio client away from Starlark (`API-MD-9`) and rejects bad input itself (`API-F-12`).

## 6. Visibility

| Element | Viewer | Admin |
|---|---|---|
| Snapshot and radio values | Yes | Yes |
| Radio form, query box, command log | No | Yes |

## 7. Tests

- Field range checks at the edges: 299 and 300 MHz, 7.7 and 7.8 kHz, SF4 and SF5, CR4 and CR5, 0 and 1 dBm, 22 and 23 dBm, 2550 and 2560 ms, persistence 255 and 256.
- A fake KAM: preview, apply ok, `422`, `409`, `502` with partial steps, `503`, rollback reported.
- Wording tests: no page text contains "verified" next to an echo.
- Viewer routes return `403`.
- The form is disabled when the link is down.

## 8. Corrections to the prototype

The prototype applies changes in the browser only and prints simulated replies. It builds the command list itself; KAMmon must take it from KAM. The prototype's sample battery line says "Unsupported"; the real snapshot uses `null` plus the `unsupported` list.

## 9. Open points

- Whether a "Restore previous values" button should offer the values before the last successful change (KAMmon would keep them in memory only).
- Whether the radio form should warn when the frequency is outside common licence-free bands. KAM cannot know the region.
