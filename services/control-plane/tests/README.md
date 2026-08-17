# Control Plane Tests

Test suite for the Concord Voice Control Plane service (Go).

## Structure

```
internal/
├── testhelpers/                    # Shared test infrastructure
│   ├── testdb.go                   # SetupTestDB, TruncateAllTables, RunMigrations
│   ├── testredis.go                # SetupTestRedis — delegates to redistest/
│   ├── redistest/                  # Per-process Redis logical-DB allocator (#2680)
│   ├── testserver.go               # TestServer with CreateTestUser/Server/Channel helpers
│   └── fixtures.go                 # TestUser struct, E2EETestKeys, ValidCiphertext
├── auth/
│   ├── password_test.go            # Argon2id hashing, password strength validation
│   ├── username_test.go            # Username validation, profanity filter, normalization
│   ├── tokens_test.go              # JWT generation/validation, refresh tokens
│   ├── ws_ticket_test.go           # WebSocket ticket generation/validation
│   ├── verification_test.go        # Email verification logic
│   ├── verification_unit_test.go   # Email verification unit tests
│   ├── verification_integration_test.go  # Email verification integration
│   ├── verification_integration_2_test.go
│   ├── recovery_integration_test.go # Account recovery flows
│   ├── handlers_integration_test.go # Register, login, refresh, logout HTTP tests
│   └── handlers_integration_2_test.go
├── channels/
│   ├── handlers_test.go            # CRUD, unread tracking, E2EE key distribution
│   ├── handlers_integration_2_test.go
│   ├── groups_test.go              # Channel group unit tests
│   └── groups_integration_test.go  # Channel group integration tests
├── clientconfig/
│   └── handlers_test.go            # Client configuration endpoint
├── database/
│   └── redis_test.go               # Redis connection and operations
├── dm/
│   └── handlers_test.go            # DM conversations, messages, voice
├── email/
│   └── service_test.go             # Email service
├── friends/
│   └── handlers_test.go            # Friend codes, requests, privacy
├── media/
│   ├── handlers_test.go            # Media upload/download
│   ├── handlers_integration_2_test.go
│   ├── cleanup_test.go             # Media cleanup
│   ├── processing_test.go          # Media processing
│   └── mock_store_test.go          # Mock storage for tests
├── messages/
│   ├── handlers_test.go            # Send/edit/delete, pagination, E2EE enforcement
│   └── handlers_integration_2_test.go
├── mfa/
│   ├── handlers_test.go            # MFA setup, verify, disable
│   ├── totp_test.go                # TOTP generation/validation
│   └── challenge_test.go           # MFA challenge flow
├── servers/
│   ├── handlers_test.go            # CRUD, role-based access, membership
│   └── handlers_integration_2_test.go
├── members/
│   ├── handlers_test.go            # Add/remove/update roles, kick, leave
│   └── handlers_integration_test.go
├── invites/
│   ├── handlers_test.go            # Create/revoke/join, expiry, max uses
│   └── handlers_integration_2_test.go
├── models/
│   └── user_test.go                # User model validation
├── ownership/
│   ├── handlers_test.go            # Ownership transfer
│   └── handlers_integration_2_test.go
├── rbac/
│   ├── audit_test.go               # RBAC audit logging
│   ├── cache_test.go               # Permission cache
│   ├── handlers_integration_test.go # RBAC integration tests
│   ├── middleware_test.go          # Permission middleware
│   ├── permissions_integration_test.go
│   ├── resolver_test.go            # Permission resolver
│   ├── resolver_visibility_test.go # Visibility resolver
│   └── types_test.go               # RBAC type tests
├── users/
│   ├── handlers_test.go            # Profile, preferences, password change, public keys
│   └── handlers_integration_test.go
├── sessions/
│   ├── ip_test.go                  # IP masking (IPv4/IPv6)
│   ├── handlers_test.go            # Session listing, revocation
│   └── handlers_integration_test.go
├── updates/
│   └── handler_test.go             # Client update endpoint
├── middleware/
│   ├── middleware_test.go          # Auth required, rate limiting
│   ├── auth_test.go                # Auth middleware
│   ├── cors_test.go                # CORS middleware
│   ├── ratelimit_test.go           # Rate limiting
│   └── validate_headers_test.go    # Custom header validation
├── voice/
│   ├── handlers_test.go            # Voice coordination
│   └── handlers_integration_2_test.go
├── websocket/
│   ├── hub_test.go                 # WebSocket hub
│   ├── hub_epoch_test.go           # Key epoch enforcement
│   ├── client_test.go              # WebSocket client
│   ├── handler_test.go             # WebSocket handler
│   ├── checkorigin_test.go         # Origin validation
│   └── mentions_test.go            # @mention routing
pkg/
├── config/
│   ├── config_test.go              # Config defaults, env overrides, validation
│   ├── spa_live_test.go            # SPA live mode config
│   └── turn_test.go                # TURN config
└── logger/
    └── logger_test.go              # Logger tests
```

**Backend test status:** CI reports current results.

## Running Tests

```bash
cd services/control-plane

# Run all tests
go test -p 1 ./...

# Run with race detection
go test -race -p 1 ./...

# Run with verbose output
go test -p 1 -v ./...

# Run specific package
go test ./internal/auth/...
go test ./internal/channels/...

# Run unit tests only (no DB required)
go test ./internal/auth/ -run "TestHashPassword|TestValidatePassword|TestValidateUsername|TestGenerate|TestValidateAccessToken"
go test ./internal/sessions/ -run "TestMaskIP"
go test ./pkg/config/...

# Run integration tests (requires PostgreSQL + Redis)
go test ./internal/auth/ -run "Integration"
go test ./internal/channels/... ./internal/messages/... ./internal/servers/...

# Coverage report
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go tool cover -html=coverage.out  # Open in browser
```

## Test Infrastructure

### Test Helpers (`internal/testhelpers/`)

**SetupTestDB(t)** — Connects to PostgreSQL via the `DATABASE_URL` env var (default: localhost). Runs all migrations. Returns a DB handle plus a cleanup function that truncates all tables.

**SetupTestRedis(t)** — Delegates to `internal/testhelpers/redistest`, which allocates **this OS process** its own Redis logical database (never DB 0). The signature is unchanged. Cleanup closes the client; where a test needs an explicit flush use `redistest.Reset(ctx, c)` — a direct `FlushDB`/`FlushAll` is rejected by pre-commit and by CI.

**SetupTestServer(t)** — Creates a full test server with Gin router, Hub, DB, Redis, and JWT secret. It offers these convenience methods:
- `CreateTestUser(t, username)` — Inserts user with pre-computed Argon2id hash (avoids ~100ms per user), generates JWT
- `CreateTestServer(t, ownerID, name)` — Inserts server + owner membership
- `CreateTestChannel(t, serverID, name, isEncrypted)` — Inserts channel
- `AuthHeaders(userID)` — Returns `Authorization: Bearer <jwt>` header map
- `DoRequest(method, path, body, headers)` — HTTP request via `httptest.ResponseRecorder`

**Fixtures** — `E2EETestKeys()` for structurally-valid test keys, `ValidCiphertext()` for base64-encoded data passing minimum AES-GCM size validation.

### Prerequisites

Integration tests require running PostgreSQL and Redis:

```bash
docker-compose up -d postgres redis
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable` | Test database |
| `REDIS_URL` | `redis://:concord_dev_redis@localhost:6379` | Test Redis. Must carry the compose `REDIS_PASSWORD`, or every Redis-backed test fails with `NOAUTH`/`WRONGPASS`. The DB segment is ignored — see below |

> **The DB segment is advisory (#2680).** Each `go test` process allocates its own Redis logical database via
> `internal/testhelpers/redistest`, which rewrites whatever index the URL names (path *or* `?db=` query param) and
> never allocates DB 0. Host and credentials are honoured; the index is not.
>
> This inverts the previous guidance, which told you to keep a `/1` suffix because the helper honoured the URL's own
> DB and would otherwise `FLUSHDB` your development keyspace on DB 0. That is no longer true in either direction:
> the pin is gone, and exporting `REDIS_URL` for the control-plane service and then running tests in the same shell
> is now safe.

## CI/CD

Tests run in GitHub Actions via `.github/workflows/build.yml` on every push to `main` and all PRs. The workflow starts PostgreSQL and Redis service containers. It runs `go test` with coverage. It uploads results to SonarQube for Quality Gate enforcement.

Pre-commit hooks (`./scripts/install-git-hooks.sh`) run local Go linting (golangci-lint, go vet, gofmt) before push.

Coverage target: **80%+** on new code, which the SonarQube Quality Gate enforces.
