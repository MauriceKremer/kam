# KAMmon Requirements: Room Accounts

**Historical draft:** Superseded by [KAMmon RoomAccounts requirements](../../kammon/docs/requirements/RoomAccounts.md). Existing MeshCore room roles remain read-only; the pinned-role proposal below is withdrawn.

**Status:** Requirements only. Nothing is built.
**Scope:** Maintaining who is an admin, a contributor or a viewer in each room. This is a local-administrator page. Design: [KAMmon_Design.md](KAMmon_Design.md). KAM side: [docs/FeatureKamApi.md](../docs/FeatureKamApi.md) §7.3 and §7.4.

## 1. Summary

In KAM today a client's role comes from the **password** it logs in with: the admin password gives *admin*, the guest password gives *read-write*, and a read-only fallback gives *guest*. There is no way to change one client without changing the password for everyone.

This page adds a per-client **pinned role**. KAM stores a role for one client in one room, and from then on that role wins at login. The client still has to know a correct password. The page can also **remove** a client, which forgets its session.

KAMmon's room roles are separate from KAMmon's own login accounts ([KAMmon_Req_Access.md](KAMmon_Req_Access.md)).

Plain words:

| In KAMmon | In KAM | Can post | Notes |
|---|---|---|---|
| Admin | `ADMIN` | Yes | Protected when the room is full |
| Contributor | `READ_WRITE` | Yes | |
| Viewer | `GUEST` | No | Receives posts |
| Follow password | no pin | by password | Removes the pin |

## 2. Goals and non-goals

**Goals**

- GL-1. List the remembered clients of a room with role, last activity and label.
- GL-2. Pin a role (admin, contributor, viewer) or clear the pin.
- GL-3. Remove a client.
- GL-4. Say clearly what a role change and a removal do and do not do.

**Non-goals**

- Adding a client by public key before it ever logs in (decision D6).
- A block list. A removed client can log in again with a valid password.
- Changing room passwords or room settings. Those stay in `kam.yml`.
- Rooms' creation or deletion.

## 3. What the page shows

| Area | Content |
|---|---|
| **Room selector** | A select with the rooms. |
| **Notice** | "Changes affect only this room. A pinned role wins over the password the client uses. Removing a client does not stop it logging in again with a valid password. To keep someone out, change the room password in kam.yml." |
| **Role legend** | The table above, in short. |
| **Client table** | Label or key, public key (short, copy control), role select, last activity with indicator, remove button. A "pinned" marker when a pin exists. |
| **Filter** | Search by label or key; sort by last activity. |
| **Capacity** | "9 of 20 remembered" (`max_clients`). |
| **Help** | "How to give someone access": the admin and guest passwords live in `kam.yml`; pin a role here to override the password for one client. |

## 4. Requirements

### 4.1 Reading

| ID | Requirement | Level |
|---|---|---|
| MON-RA-1 | The page is for admins on a local request only. It is not in the navigation for viewers and its routes answer `403`. | MUST |
| MON-RA-2 | The client table is read from `GET /v1/rooms/{id}/clients` when the page opens and every 10 seconds while it is open. The page does not refresh while a role select or dialog is open. | MUST |
| MON-RA-3 | A row shows the label when KAM sent one and the short key otherwise; the full key is always reachable. The label is marked "from advert" and is not treated as identity. | MUST |
| MON-RA-4 | The role select shows the effective role. A `pinned` marker shows whether it is a pin or comes from the password. | MUST |
| MON-RA-5 | The last-activity cell uses the activity indicator and legend from [KAMmon_Req_Rooms.md](KAMmon_Req_Rooms.md). | MUST |
| MON-RA-6 | "Remembered" capacity shows `clients_remembered` against `max_clients`. | SHOULD |

### 4.2 Changing

| ID | Requirement | Level |
|---|---|---|
| MON-RA-7 | Choosing a role sends `PUT /v1/rooms/{id}/clients/{key}/role` with `admin`, `read_write`, `guest` or `null` (Follow password). | MUST |
| MON-RA-8 | The row changes only after KAM confirms, using the client returned by KAM. On an error the old role returns and the message is shown. | MUST |
| MON-RA-9 | Demoting the last pinned admin of a room asks for confirmation. KAM does not require it; it is a guard against a slip. | SHOULD |
| MON-RA-10 | Remove opens a dialog: "Forget this client? Its session and pinned role are deleted. It can log in again with a valid password." Confirming sends `DELETE`. | MUST |
| MON-RA-11 | A `404` after a change (the client was just evicted) reloads the table and says so. | MUST |
| MON-RA-12 | Each successful change adds a line to a small "Recent changes" list on the page (kept in KAMmon memory, lost at restart). | SHOULD |
| MON-RA-13 | The label field is display only. KAMmon has no list of names of its own. | MUST |

### 4.3 Honesty

| ID | Requirement | Level |
|---|---|---|
| MON-RA-14 | The page never says "ban", "block" or "kick". It says "remove". | MUST |
| MON-RA-15 | While KAM does not offer the role endpoints (`404`), the page is read-only with the line "Needs a newer KAM" and no controls. | MUST |

## 5. What KAMmon needs from KAM

| Need | Endpoint | KAM requirement |
|---|---|---|
| Clients with role, pin, last activity, label | `GET /v1/rooms/{id}/clients` | API-RM-2, API-RM-8, API-RM-9 |
| Pin or clear a role | `PUT /v1/rooms/{id}/clients/{key}/role` | API-RM-4, API-RM-6 |
| Remove a client | `DELETE /v1/rooms/{id}/clients/{key}` | API-RM-5 |
| Changes applied on the room worker and saved first | Behaviour | API-RM-7 |
| Change events | `room` events `role_changed`, `client_removed` | API-EV-5 |

KAM decides what a pin means at login and rejects bad input itself (`API-F-12`). KAMmon never relies on its own checks.

## 6. Visibility

Admin only. A viewer gets none of this page.

## 7. Tests

- A fake KAM: role set ok, role set `404`, role set `422`, delete ok, delete `404`.
- The row updates after confirmation only.
- The last-admin guard and the removal dialog.
- A viewer request to every route of this page returns `403`.
- The page in read-only mode when the endpoints answer `404`.

## 8. Open points

- Whether the "Recent changes" list should be written to a log file, since KAMmon keeps nothing on disk.
- Whether to offer a one-click "Pin as viewer" for a Stale Node client.
