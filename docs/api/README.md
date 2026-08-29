# API Documentation

OpenAPI 3.0 specifications for the supported Concord Voice control-plane
surfaces. A drift gate mechanically checks the specs against the router, but they do not
cover every control-plane surface.

## Files

- [openapi.yaml](./openapi.yaml) — supported public `/api/v1` operations
- [admin-metrics.openapi.yaml](./admin-metrics.openapi.yaml) — supported admin metrics `/admin/api/v1` GET operations

The specs intentionally exclude other control-plane surfaces, including admin
authentication and enrollment. Explicit method-denial registrations on the four
admin metrics paths are enforcement behavior, not supported operations.

> **Drift gate (#822):** `scripts/api/check-openapi-coverage.sh` runs in the pr-ci `verify-openapi-coverage` job. It independently checks the public router against `openapi.yaml`, and the supported admin metrics GET routes against `admin-metrics.openapi.yaml`, in both drift directions. Unlisted public delegated registrars fail extraction rather than silently dropping routes. Inventory tooling is `scripts/api/extract-routes.py` (`--list` / `--missing` / `--stale` / `--check`). The `--list` flag prints the union of supported operations from both surfaces.

## Viewing the Spec

**Swagger Editor** (browser):

```text
https://editor.swagger.io
# Paste or import the openapi.yaml file
```

**Redoc CLI** (local):

```bash
npx @redocly/cli preview-docs docs/api/openapi.yaml
# Opens interactive docs at http://127.0.0.1:8080
```

**Validate**:

```bash
npx @redocly/cli lint docs/api/openapi.yaml
npx @redocly/cli lint docs/api/admin-metrics.openapi.yaml
```

## Public Authentication Flow

1. **Register** or **Login** creates a refresh session and returns `access_token` plus its `session_id`. A login response that mints a session sets `X-Concord-Session-Issued: true`. It exposes that exact row as `X-Concord-Session-ID`, so a client can clean up even when a successful response body is malformed.
2. Pass `Authorization: Bearer <access_token>` on protected endpoints.
3. **Refresh** with the HttpOnly `refresh_token` cookie, or `X-Refresh-Token` for non-browser clients. Rotation returns the successor `session_id` and the replaced `previous_session_id`. Clients must accept the result only for that exact lineage.
4. **Logout** revokes credentials in this precedence order: explicit `X-Refresh-Token`, then bearer-owned `X-Session-ID`. After those come `X-Session-ID` bound atomically to the presented refresh-token cookie, then the cookie alone. Explicit credentials never clear an ambient cookie. A cookie expires only when its database row matched, so stale cleanup cannot erase a successor session.

The response bodies, security alternatives, header requirements, and error
shapes are canonical in [openapi.yaml](./openapi.yaml). Keep client integrations
bound to those operation definitions rather than duplicating them here.

## Endpoint Groups

The rows below are the OpenAPI **tags** declared in [openapi.yaml](./openapi.yaml),
one row per tag, with representative paths. **This table is a hand-maintained
orientation aid, not the inventory.** For the authoritative list, run the extractor:

```bash
scripts/api/extract-routes.py --list
```

It prints the union of supported operations from both surfaces and the
`verify-openapi-coverage` CI job checks it against the router in both drift
directions, so it cannot fall behind the way this table can.

Counts as of 2026-08-28: 27 tags and 271 operations in `openapi.yaml`, plus 4
`Admin Metrics` operations in `admin-metrics.openapi.yaml`.

| Tag | Ops | Representative paths | Auth |
| --- | ---: | --- | --- |
| Auth | 24 | `/auth/{register,login,refresh,logout,ws-ticket}`, `/auth/register/*`, `/auth/recovery/*`, `/auth/sso/{provider}/*`, `/auth/mfa/{verify,email/send}` | Mixed — public (rate-limited), refresh cookie or `X-Refresh-Token`, MFA challenge token, Bearer for `ws-ticket` |
| MFA | 34 | `/mfa/{status,totp/*,webauthn/*,backup-codes/*,trusted-devices/*,recovery-*,backup-email,email-sms/*}` | Bearer |
| Users | 19 | `/users/me`, `/users/me/{keys,password,preferences,security,saved-gifs,sso-identities,friend-organization}`, `/users/search`, `/users/{user_id}/{profile,public-key,friend-request-eligibility}` | Bearer |
| Sessions | 4 | `/sessions`, `/sessions/{id}`, `/sessions/{revoke-all,revocation-mode}` | Bearer |
| Servers | 20 | `/servers`, `/servers/{id}`, `/servers/unread-status`, `/servers/{id}/{roles,roles/{role_id},roles/reorder,permissions,audit-log,entitlements,mute-states,transfer-ownership}`, `/ownership/reverse/{token}` | Bearer |
| Members | 12 | `/servers/{id}/members`, `/servers/{id}/members/{userId}`, `/servers/{id}/bans`, `/servers/{id}/member-public-keys` | Bearer |
| Invites | 7 | `/servers/{id}/invites`, `/invites/join`, `/invites/{code}`, `/invites/{code}/{icon,preview}` | Mixed — preview/icon public, the rest Bearer |
| Channels | 13 | `/channels`, `/channels/{id}`, `/channels/{id}/{read,overrides}` | Bearer |
| ChannelGroups | 8 | `/servers/{id}/channel-groups`, `/categories/{id}/overrides`, `/categories/{id}/overrides/{override_id}` | Bearer |
| Messages | 13 | `/channels/{id}/messages`, `/channels/{id}/messages/bulk`, `/channels/{id}/pins`, `/messages/{id}` | Bearer |
| DM | 27 | `/dm/conversations`, `/dm/conversations/{personal,group}`, `/dm/conversations/{id}`, `/dm/conversations/{id}/{messages,members,keys,read}` | Bearer |
| Direct Messages | 2 | `/dm/conversations/{id}/members/{userId}` | Bearer — a stray tag spelling of `DM` in the spec, listed so the row count reconciles |
| E2EE | 9 | `/e2ee/pending-keys`, `/channels/{id}/keys`, `/channels/{id}/rotate-key`, `/dm/conversations/{id}/rotate-key` | Bearer |
| Voice | 24 | `/channels/{id}/voice/{join,participants,authorize-action}`, `/servers/{id}/voice/{userId}/{mute,deafen,move,disconnect,temp-access}`, `/dm/conversations/{id}/voice/{join,ring,decline,cancel,participants}` | Bearer |
| Media | 17 | `/media/{file_id}`, `/media/{attachments,avatars,banners,server-icons,server-banners,dm-icons}/…`, `/media/upload/{avatar,banner,attachment,server-icon,server-banner,dm-icon}`, `/media/upload/attachment/session/*` (chunked upload, #2157) | Mixed — reads public where the asset is, uploads Bearer |
| Friends | 6 | `/friends`, `/friends/request`, `/friends/request/{id}` | Bearer |
| FriendCodes | 7 | `/friends/codes`, `/friends/codes/{code}`, `/friends/codes/{code}/{avatar,preview,claim}` | Mixed — preview/avatar public, the rest Bearer |
| Presence | 8 | `/users/me/presence-history`, `/users/me/presence-history/settings` (Activity History), `/users/me/presence-overrides/{category}` (Custom Status) | Bearer; authenticated subject only |
| Privacy | 3 | `/users/me/privacy`, `/privacy/erase-account` | Bearer |
| Notifications | 2 | `/notifications/{mute,preferences}` | Bearer |
| GIFs | 11 | `/klipy/gifs/{items,categories,trending}`, `/klipy/customer-id` | Bearer |
| Age Verification | 2 | `/age/claim`, `/age/status` | Bearer + verified email |
| Attestation | 3 | `/attestation/verify`, `/internal/attestation/publish/{binary,spa}` | Bearer |
| Redemption | 2 | `/redeem`, `/admin/redemption/codes` | Bearer; the admin path additionally requires an admin token |
| Feedback | 1 | `/feedback` | Bearer |
| Platform | 5 | `/client/config`, `/server/capabilities`, `/entitlements`, `/subscriptions/me`, `/updates/{filename}` | Mixed — `client/config` and `server/capabilities` public (rate-limited), the rest Bearer |
| WebSocket | 1 | `/ws` | Ticket or Bearer |
| Admin Metrics | 4 | `/admin/api/v1/*` — see [admin-metrics.openapi.yaml](./admin-metrics.openapi.yaml) | Admin token |

## Base URL

Public API:

```text
http://localhost:8080/api/v1
```

Admin metrics API:

```text
https://{adminHost}/admin/api/v1
http://localhost:8080/admin/api/v1
```

## Quick Reference

### Authentication

```http
POST /api/v1/auth/register
POST /api/v1/auth/login
POST /api/v1/auth/logout
POST /api/v1/auth/refresh
POST /api/v1/auth/ws-ticket        # Protected (Bearer + verified email)
GET  /api/v1/sessions
DELETE /api/v1/sessions/:id
```

### WebSocket

Get a 30-second single-use ticket via `POST /auth/ws-ticket`, then connect:

```text
ws://localhost:8080/api/v1/ws?ticket=<ticket>
```

Do NOT pass raw JWT tokens in the URL.

### Rate Limits

| Endpoint class | Limit |
| --- | --- |
| Auth endpoints | 5–30 requests / 15 min |
| Read operations | 30 requests / min |
| Write operations | 10 requests / min |
| Destructive operations | 5 requests / min |

### Error Responses

There is no universal error envelope. Most legacy handlers and shared auth/rate-limit middleware use:

```json
{
  "error": "Error message description"
}
```

Activity History handler errors instead use a stable code and may include the current disclosure for a consent mismatch:

```json
{
  "code": "activity_history_invalid_request"
}
```

Its bearer-token and rate-limit failures still use the existing `error` envelope. Global header validation, account state, email verification, and client-attestation gates retain their own documented shapes. See each OpenAPI operation for its exact response schema. The Activity History routes do not expose Custom Status delivery or quarantine error codes.

Common status codes: `200 OK`, `201 Created`, `400 Bad Request`, `401 Unauthorized`, `403 Forbidden`, `404 Not Found`, `429 Too Many Requests`, `500 Internal Server Error`.

## Postman Collection

There is no checked-in Postman collection. Both specs are OpenAPI 3.0.3 and Postman
imports OpenAPI directly, so import [openapi.yaml](./openapi.yaml) (or
[admin-metrics.openapi.yaml](./admin-metrics.openapi.yaml)) through
**File → Import → OpenAPI**.

## Related Docs

- [Architecture](../architecture.md) — System diagrams, database ERD, message flows
- [Development Guide](../development.md) — Local setup, running tests
