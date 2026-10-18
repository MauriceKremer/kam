# KAMmon Requirements: Access, Roles and Visibility

**Status:** Requirements only. Nothing is built.
**Scope:** Who may use KAMmon, how they log in, what each role may see and do, and how KAMmon enforces it. Design context: [KAMmon_Design.md](KAMmon_Design.md) §3.1 and §9.

## 1. Summary

KAM has no users and no logins. It is an open system that listens on the local computer only. **KAMmon is the part that is shown to the world, so KAMmon does all the authentication.** It has two roles:

- **Viewer.** Logs in with a name and password, can look at the pages, and cannot change anything.
- **Admin.** Everything a viewer can do, plus message text, room posts, client names and every control. An admin must also be on the same computer (a loopback address).

Passwords sit in `kammon.yml` as plain text, like channel keys in `kam.yml`. This is a hobby tool; the setup stays simple.

## 2. Goals and non-goals

**Goals**

- GL-1. A web visitor without a login sees nothing but the login page.
- GL-2. A viewer can never change anything, even with a hand-made request.
- GL-3. Setup is one file and a few lines.
- GL-4. KAM is not involved in logins, sessions or roles.

**Non-goals**

- Single sign-on, password reset, e-mail, two-factor login or a user database.
- Per-user permissions beyond the two roles.
- Protecting against an attacker who already controls the computer.
- Hiding the existence of KAMmon (there is a login page).

## 3. Roles and visibility

| Item | Viewer | Admin (on this computer) |
|---|---|---|
| Overview numbers, airtime chart, packet mix | Yes | Yes |
| Traffic table: type, route, hops, SNR, airtime | Yes | Yes |
| Traffic table: names (senders, advert names) | Advert names and names on allow-listed channels | Yes |
| Channel list, counts, last activity | Yes | Yes |
| Channel message **text** | Only channels in `viewer_text_channels` | Yes |
| Room list, counts, last activity, public key | Yes | Yes |
| Room client list (keys, roles, activity, labels) | No: viewers see room-level numbers only | Yes, with labels |
| Room posts | No | Yes |
| Accounts page (roles) | No | Yes |
| Script names, status, run information | Yes | Yes |
| Script **source**, dry run, save, delete | No | Yes |
| Modem snapshot and radio values | Yes | Yes |
| Modem queries and radio changes | No | Yes |
| Theme switch | Yes | Yes |

Anything marked "No" is not rendered and its routes answer `403`.

## 4. Requirements

### 4.1 Login and sessions

| ID | Requirement | Level |
|---|---|---|
| MON-AX-1 | `kammon.yml` lists users with `name`, `password` and `role` (`viewer` or `admin`). At least one user is required to start. | MUST |
| MON-AX-2 | Every page and API route except the login page and static files requires a session. An unauthenticated request is redirected to the login page. | MUST |
| MON-AX-3 | A session is a random 128-bit id in a cookie: `HttpOnly`, `SameSite=Strict`, and `Secure` when the request came over HTTPS. | MUST |
| MON-AX-4 | Sessions live in memory with an idle timeout (default 8 hours, configurable). A KAMmon restart ends all sessions. | MUST |
| MON-AX-5 | Passwords are compared in constant time. A wrong name and a wrong password give the same message. | MUST |
| MON-AX-6 | Failed logins from one address are slowed: after 5 failures in a minute, answer each further attempt after 2 seconds. | SHOULD |
| MON-AX-7 | A log-out button ends the session. | MUST |

### 4.2 Roles

| ID | Requirement | Level |
|---|---|---|
| MON-AX-8 | A role is attached to the session when it is created. The check `role == admin` and `request from loopback` is made on every admin route, not once at login. | MUST |
| MON-AX-9 | A request is "from loopback" when its connection address is a loopback address **and** it has no `X-Forwarded-For`, `X-Real-IP` or `Forwarded` header. A reverse proxy on the same computer therefore cannot give web visitors admin power. | MUST |
| MON-AX-10 | An admin user on a non-local request gets viewer access and sees a notice: "Signed in as admin, but this is not a local request. View only." | MUST |
| MON-AX-11 | Routes are registered with a required role. A route that needs `admin` answers `403` to anyone else, whatever the page shows. | MUST |
| MON-AX-12 | Navigation and pages leave out what the role cannot use, rather than showing disabled controls. | MUST |

### 4.3 What viewers may see

| ID | Requirement | Level |
|---|---|---|
| MON-AX-13 | `viewer_text_channels` is a list of channel names. Text of other channels is shown to admins only; a viewer sees the row with time, route and SNR and the words "Text hidden". | MUST |
| MON-AX-14 | One function builds every view model and applies the visibility table in §3. Templates never decide visibility. | MUST |
| MON-AX-15 | Names that come from the radio (advert names, senders, message text, labels) are escaped when rendered, and never marked as safe HTML. | MUST |
| MON-AX-16 | KAMmon never shows or logs channel keys, room passwords or its own passwords. KAM does not send the first two, and the config loader redacts the third. | MUST |

### 4.4 Changes made from the browser

| ID | Requirement | Level |
|---|---|---|
| MON-AX-17 | Routes that change anything accept only `POST`, `PUT` and `DELETE`, never `GET`. | MUST |
| MON-AX-18 | Such a request must carry the htmx request header and an `Origin` that matches the host, and the session cookie. | MUST |
| MON-AX-19 | KAMmon calls KAM for the admin only after the checks above. It adds no authentication to KAM, which has none. | MUST |

### 4.5 Serving

| ID | Requirement | Level |
|---|---|---|
| MON-AX-20 | KAMmon listens on one address (`listen`, default `:8080`) over plain HTTP. | MUST |
| MON-AX-21 | Optional `tls_cert` and `tls_key` serve HTTPS directly. A reverse proxy is the other way to get HTTPS. | MAY |
| MON-AX-22 | The login page and error pages reveal nothing about KAM, the node name or the radio. | SHOULD |

## 5. What KAMmon needs from KAM

Nothing for authentication. KAM must keep its listener loopback-only and unauthenticated (`API-F-1`, `API-F-12` in [docs/FeatureMonitorApi.md](../docs/FeatureMonitorApi.md)). KAM returns full data for every request; KAMmon decides what to show (`MON-AX-14`).

## 6. Configuration

```yaml
listen: ":8080"
session_idle_hours: 8
viewer_text_channels: ["#public"]
users:
  - { name: maurice, password: "change-me", role: admin }
  - { name: observer, password: "change-me-too", role: viewer }
```

## 7. Tests

- Table tests for the permission table in §3 for each role and for local and non-local requests.
- A test that every registered route has a required role, and that a route with none fails to start.
- A test that an admin request with `X-Forwarded-For` is treated as a viewer.
- A test that no `GET` route changes state.
- A test that a viewer's page source does not contain the text of a hidden message or a client label.

## 8. Open points

- Confirm assumption A1 in the design: room posts and client labels are admin only, and the allow list covers channel text only.
- Whether the allow list should also apply to advert names (adverts are public over the air; today they are shown to viewers).
