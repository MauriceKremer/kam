# Feature: Node API Composition

**Status:** Implemented; checks recorded in §7.
**Scope:** Clarify the local API composition root in `internal/node` by separating listener startup, status assembly, and capability adapters into focused files. Rename internal API lifecycle identifiers without changing the user-facing `monitor:` configuration or HTTP behavior.

## 1. Summary

`internal/node/monitor.go` currently combines four jobs: adapting events, channels, and rooms to the transport seams; assembling node status; collecting optional system statistics; and wiring and starting the HTTP listener. Split these responsibilities into files within the existing `node` package. Rename `startMonitor` and `monitorServer` to `startAPI` and `apiServer`, which describe the current unified listener.

This is a file-level organization change, not a new package or runtime layer. The capability packages continue to own their behavior, `internal/api` remains transport-only, and `internal/node` remains the composition root.

## 2. Goals and non-goals

**Goals**

- Keep listener configuration and lifecycle in `internal/node/api.go`.
- Keep status snapshot assembly and `systemstats`-to-HTTP mapping in `internal/node/status.go`.
- Put the events, channels, and rooms adapters in their own focused node files.
- Rename internal listener lifecycle identifiers to API terminology.
- Preserve all routes, wire shapes, disabled behavior, lifecycle order, and `monitor:` configuration compatibility.
- Update architecture documentation and provide checks that cover behavior and package dependencies.

**Non-goals**

- Rename `monitor:` YAML settings, `models.MonitorSink`, the event recorder, or historical `/v1` endpoint names.
- Change any route, response, error mapping, HTTP policy, stats collection, or modem behavior.
- Extract modem management into a new package in this change. `modemController` currently uses HTTP request, response, and error types throughout its implementation; separating that capability from the transport needs its own domain interface and behavior-preserving design.
- Create more Go packages or introduce new dependencies.

## 3. Sources

| Key | Source | Used for |
|---|---|---|
| `NODE` | [internal/node/node.go](../internal/node/node.go) | Node construction, API start, and shutdown lifecycle |
| `ADAPTERS` | [internal/node/api.go](../internal/node/api.go), [internal/node/status.go](../internal/node/status.go), [internal/node/events.go](../internal/node/events.go), [internal/node/channels.go](../internal/node/channels.go), [internal/node/rooms.go](../internal/node/rooms.go), [internal/node/scripts.go](../internal/node/scripts.go) | Node-to-transport adapters and status assembly after the split |
| `TRANSPORT` | [internal/api/api.go](../internal/api/api.go), [internal/api/status.go](../internal/api/status.go) | API seams, listener composition, and status wire types |
| `SYSTEM` | [internal/systemstats/systemstats.go](../internal/systemstats/systemstats.go) | Host-stat collection and throttling remain in their capability package |
| `APP` | [docs/APP_STRUCTURE.md](APP_STRUCTURE.md) | Package responsibilities |
| `DESIGN` | [docs/SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) | Runtime inventory and API shutdown |

## 4. Target layout

| File | Responsibility |
|---|---|
| `internal/node/api.go` | Connect API seams, create the system-stat collector, start and retain the HTTP server |
| `internal/node/status.go` | Build the node status snapshot and map system-stat snapshots into API wire types |
| `internal/node/events.go` | Adapt the event ring to `api.EventsSource` |
| `internal/node/channels.go` | Adapt persistence channel reads to `api.ChannelReader` |
| `internal/node/rooms.go` | Adapt room and persistence reads to `api.RoomSource` |
| `internal/node/scripts.go` | Continue adapting the automation runtime to `api.Scripts` |
| `internal/node/modem.go` | Remains in `node` for now; a transport-independent modem domain interface is a separate follow-up |

All files stay in `package node`; they share the private `Node` implementation without creating package coupling.

## 5. Decisions

| ID | Question | Outcome |
|---|---|---|
| D1 | Rename the configuration and event recording terminology along with the API listener? | No. `monitor:` is user-facing configuration and `MonitorSink` names event recording; neither describes only the HTTP listener and both have compatibility or domain meaning. |
| D2 | Split adapters into additional packages? | No. These adapters are small wiring code owned by the composition root. Focused files improve locality without adding package interfaces. |
| D3 | Extract modem management at the same time? | No. The controller's interface and implementation currently use `api` request/result/error types. First define a transport-independent modem interface and results in a separate design; do not disguise the dependency by moving the file. |

## 6. Delivery steps and checks

1. **Record this change plan.** Confirm the current API routes and node lifecycle; use the existing dependency structure as the constraint.
   - Check: plan identifies scope, target files, decisions, behavior invariants, and verification.
2. **Separate node API responsibilities.** Move event, channel, and room adapters into focused files; move status assembly and system-stat mapping to `status.go`; keep listener wiring in `api.go`.
   - Check: `gofmt` and `go test ./internal/node ./internal/api ./internal/systemstats`.
3. **Use API terminology for listener lifecycle.** Rename `startMonitor` to `startAPI` and `monitorServer` to `apiServer` in startup, shutdown, and tests.
   - Check: `rg 'startMonitor|monitorServer' internal/node` has no matches.
4. **Align architecture documentation.** Update package responsibility and runtime descriptions to name the API composition root and its focused files; leave the `monitor:` configuration and event terminology intact.
   - Check: search docs and Go code for stale references to the old node file/function/field; verify documented API routes are unchanged.
5. **Validate behavior and dependencies.**
   - Checks: `gofmt -w` on touched Go files; `go test ./internal/node ./internal/api ./internal/systemstats`; `go test ./...`; inspect `git diff --check`.
   - Invariants: API import guard passes; tests continue to cover disabled/bind-failure behavior, status with and without `?system=true`, secret exclusion, cached system stats, and shutdown.

## 7. Corrections and verification

| Item | Previous description | Correction |
|---|---|---|
| Node API startup | `startMonitor` in `internal/node/monitor.go` starts a unified local REST API | `startAPI` in `internal/node/api.go` composes and starts the one local API listener |
| Node status | Status closure and system mapping live beside listener startup | Status assembly and system mapping live in `internal/node/status.go` |
| Node adapters | Events, channels, and rooms adapters are bundled into `monitor.go` | Each adapter is in a focused file in `internal/node`; scripts remain in `scripts.go` |

Verification:

- [x] `gofmt` completed for touched Go files.
- [x] Targeted package tests pass.
- [x] Full `go test ./...` passes.
- [x] `git diff --check` passes.
- [x] API dependency guard passes as part of the API tests.
