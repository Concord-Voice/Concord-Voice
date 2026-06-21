# API Documentation

OpenAPI 3.0 specification for the Concord Voice Control Plane API.

## Files

- [openapi.yaml](./openapi.yaml) — OpenAPI spec (232 documented ops across 187 paths — full live-route coverage, drift-gated)

> **Drift gate (#822):** the spec is kept in lockstep with `services/control-plane/internal/api/router.go` by `scripts/api/check-openapi-coverage.sh` (pr-ci `verify-openapi-coverage` job) — CI fails when a route is added/removed without a matching spec edit, in either direction. Inventory tooling: `scripts/api/extract-routes.py` (`--list` / `--missing` / `--stale` / `--check`).

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
```

## Authentication Flow

1. **Register** or **Login** → receive JWT access token (15 min) + refresh token cookie (30 days)
2. Pass `Authorization: Bearer <token>` on all protected endpoints
3. When a 401 is returned, call `POST /auth/refresh` (sends cookie automatically) → new access token
4. **Logout** → invalidates the refresh token server-side

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
| Voice | `/voice/{join,leave,signal}` | Bearer |

## Base URL

```text
http://localhost:8080/api/v1
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

Obtain a 30-second single-use ticket via `POST /auth/ws-ticket`, then connect:

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

```json
{
  "error": "Error message description"
}
```

Common status codes: `200 OK`, `201 Created`, `400 Bad Request`, `401 Unauthorized`, `403 Forbidden`, `404 Not Found`, `429 Too Many Requests`, `500 Internal Server Error`.

## Postman Collection

Import the Postman collection from: `docs/api/Concord.postman_collection.json`

## Related Docs

- [Architecture](../architecture.md) — System diagrams, database ERD, message flows
- [Development Guide](../development.md) — Local setup, running tests
