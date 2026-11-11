# Feature: Node API Composition

**Status:** Implemented; checks recorded in §5.
**Scope:** Split node API composition into focused files and rename internal listener lifecycle identifiers, without changing the `monitor:` configuration or HTTP behavior.

## 1. Summary

`internal/node` is the composition root for the local API. Keep it in one package, but separate listener wiring, status assembly, and the event, channel, and room adapters. The transport remains in `internal/api`; this is an organizational change with no new package or runtime layer.

## 2. Goals and non-goals

**Goals**

- Keep listener composition and lifecycle in `api.go`, status assembly in `status.go`, and event, channel, and room adapters in their own files.
- Rename `startMonitor` and `monitorServer` to `startAPI` and `apiServer`.
- Update architecture documentation to reflect the composition.

**Non-goals**

- Change routes, responses, lifecycle behavior, or the user-facing `monitor:` settings and event-recording terminology.
- Extract modem management into a new package or add dependencies.

## 3. Sources and method

| Key | Source | Used for |
|---|---|---|
| `NODE` | [internal/node/node.go](../internal/node/node.go) | Node startup and shutdown |
| `API` | [internal/api/api.go](../internal/api/api.go), [internal/api/status.go](../internal/api/status.go) | Transport seams and status types |
| `SYSTEM` | [internal/systemstats/systemstats.go](../internal/systemstats/systemstats.go) | System-stat collection |
| `ARCH` | [docs/APP_STRUCTURE.md](APP_STRUCTURE.md), [docs/SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) | Package responsibilities and runtime flow |

## 4. Corrections to existing notes

| Where | Previous description | Current description |
|---|---|---|
| Node listener | `startMonitor` and `monitorServer` in `monitor.go` | `startAPI` and `apiServer` in the node API composition |
| Node status | Built alongside listener startup | Assembled in `status.go`; system-stat mapping is there too |
| Node adapters | Events, channels, and rooms were together in `monitor.go` | Each has a focused file in `internal/node` |

## 5. Verification

- `go build ./...`
- `go vet ./internal/node`
- `go test ./internal/...`
- `git diff --check`

These checks pass. The API routes, wire shapes, disabled behavior, and shutdown order remain unchanged.
