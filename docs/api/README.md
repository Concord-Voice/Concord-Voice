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

| Tag | Endpoints | Auth |
| --- | --- | --- |
| Auth (public) | `/auth/{register,login,refresh,logout}` | Public (rate-limited) |
| Auth (protected) | `/auth/ws-ticket` | Bearer + verified email |
| Users | `/users/me`, `/users/me/keys`, `/users/me/password`, `/users/me/preferences`, `/users/{id}/public-key` | Bearer |
| Sessions | `/sessions`, `/sessions/{id}` | Bearer |
| Servers | `/servers`, `/servers/unread-status`, `/servers/{id}` | Bearer |
| Channels | `/channels`, `/channels/{id}`, `/channels/{id}/{messages,read,keys}` | Bearer |
| Messages | `/messages`, `/messages/{id}` | Bearer |
| Members | `/servers/{id}/members`, `/servers/{id}/members/{userId}` | Bearer |
| Invites | `/servers/{id}/invites`, `/invites/join`, `/invites/{code}` | Bearer |
| E2EE | `/e2ee/pending-keys` | Bearer |
| WebSocket | `/ws` | Ticket or Bearer |
| MFA (public) | `/auth/mfa/{verify,email/send}` | Challenge token (from login) |
| MFA (protected) | `/mfa/{status,totp/*,webauthn/*,backup-codes/*,trusted-devices/*,recovery-*}` | Bearer |
| RBAC | `/servers/{id}/roles`, `/servers/{id}/roles/{roleId}` | Bearer |
| DMs | `/dm/conversations`, `/dm/conversations/{id}/messages` | Bearer |
| Friends | `/friends/codes` | Bearer |
| Activity History | `/users/me/presence-history`, `/users/me/presence-history/settings` | Bearer; authenticated subject only |
| Voice | `/voice/{join,leave,signal}` | Bearer |
| Platform (public) | `/client/config`, `/server/capabilities` | Public (rate-limited) |

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

Import the Postman collection from: `docs/api/Concord.postman_collection.json`

## Related Docs

- [Architecture](../architecture.md) — System diagrams, database ERD, message flows
- [Development Guide](../development.md) — Local setup, running tests
