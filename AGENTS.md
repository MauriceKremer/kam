# AGENTS.md

KAM is a Go service that connects a MeshCore KISS radio to SQLite storage and Starlark automation. [README.md](README.md) covers build, run and configuration.

## Philosophy

KAM is for enthusiasts: [README.md](README.md#philosophy--intent) puts pragmatism and easy setup ahead of strict design and security hardening. Apply it to every code, plan and doc change:

- Choose the simplest thing that works. Few settings, sane defaults, one `kam.yml`, no new dependency unless it clearly pays off.
- Passwords and similar secrets you write by hand are plain text in `kam.yml`, like channel keys. Do not add file-mode checks, permission warnings, "keep the file private" notes, secrets files or environment-variable plumbing unless asked.
- Keep cheap correctness safeguards (input validation, bounded queues, not logging secrets). Skip hardening aimed at a threat model beyond a hobbyist's local node.
- A requirement that asks for such hardening gives way to this section. Record the outcome as a decision in the plan's decisions table (as D11 in [FeatureRoomServer.md](docs/FeatureRoomServer.md)), not as a deviation, and state its consequence once instead of repeating warnings.
- Write docs for beginners, in plain words.

## Project docs

Read the doc that matches the task before changing code:

- [docs/APP_STRUCTURE.md](docs/APP_STRUCTURE.md): package layout, runtime flows, data ownership.
- [docs/SYSTEM_DESIGN.md](docs/SYSTEM_DESIGN.md): goroutines, locks, shutdown order, end-to-end data flows.
- [docs/APIdetails.md](docs/APIdetails.md): reference for the REST endpoints KAM serves today (implemented ones only), with parameters and responses. Whenever you implement a new endpoint, or change or remove an existing one, update this doc **and** the `GET /` index (`endpoints` in [internal/api/api.go](internal/api/api.go)) in the same change. Planned-but-unimplemented endpoints stay in the feature document.
- The API's transport layer ([internal/api](internal/api/)) implements no KAM capability: it imports only the standard library and [internal/models](internal/models/), capabilities are handed in through its seams (`Scripts`, `EventsSource`, the channel/room readers), and capability packages never import it. A guard test in that package fails the build on a violating import. Keep the rule when extending the API; see [docs/FeatureUnifiedApi.md](docs/FeatureUnifiedApi.md) §4.

Before creating, editing, styling, or reviewing `manuals/*.html`, read and follow the [KAM manuals style skill](.github/skills/kam-manuals-style/SKILL.md).

Plans live in `docs/Feature*.md` with a Status/Scope header, Summary, Goals/Non-goals, a sources table with short keys, and a corrections table. Model new ones on [FeatureKissModem.md](docs/FeatureKissModem.md) §3 and §9.

## Researching MeshCore capabilities

Primary sources, all in `https://github.com/meshcore-dev/MeshCore`. Fetch files as `https://raw.githubusercontent.com/meshcore-dev/MeshCore/main/<path>`.

| Question | Source |
|---|---|
| KISS modem commands, frames, error codes, data formats | `docs/kiss_modem_protocol.md` |
| What each KISS command actually does, defaults, TX state machine, output queue | `examples/kiss_modem/KissModem.h`, `KissModem.cpp`, `main.cpp` |
| Whether the radio accepted a setting; noise floor, airtime, RX/TX state | `src/helpers/radiolib/RadioLibWrappers.cpp`, `CustomSX1262Wrapper.h` |
| Chip limits (frequency, power) | RadioLib `src/modules/SX126x/SX1262.cpp` at `github.com/jgromes/RadioLib` |
| Packet header, route types, path encoding, size limits | `docs/packet_format.md` |
| Payload layouts (advert, request, text, ACK, group) | `docs/payloads.md` |
| Radio parameter ranges, repeater/room server settings | `docs/cli_commands.md` |
| Companion app protocol | `docs/companion_protocol.md` |
| Suggested regional radio presets | `docs/radio_presets.md`, `https://api.meshcore.nz/api/v1/config` |
| Room server state machine | `examples/simple_room_server/MyMesh.cpp`, `MyMesh.h` |
| Mesh receive rules, ACL, crypto, identity | `src/Mesh.cpp`, `src/Utils.cpp`, `src/Identity.cpp`, `src/helpers/ClientACL.*`, `src/helpers/BaseChatMesh.cpp` |
| Sensor payloads from `GetSensors` | CayenneLPP: `https://docs.mydevices.com/docs/lorawan/cayenne-lpp` |

Other files in `docs/`: `number_allocations.md`, `stats_binary_frames.md`, `qr_codes.md`, `terminal_chat_cli.md`, `faq.md`, `nrf52_power_management.md`.

Lead: `https://deepwiki.com/meshcore-dev/MeshCore/4.6-kiss-modem` (and its sibling pages). It is generated from an older commit shown on the page; confirm every claim in the source files above.

### Method

- Firmware source decides behaviour; the protocol doc supplies names and layouts. When they differ, the source wins and the difference goes in the doc's corrections table.
- Pin each source: `https://api.github.com/repos/meshcore-dev/MeshCore/commits?path=<path>&per_page=5`, first entry newest. Cite that commit in the doc header and build permalinks from it.
- Page fetches return condensed excerpts. Ask for one constant or function per request, and re-fetch the raw file when an exact value matters.
- Mark each fact as read in source, inferred, or needing a hardware capture.
