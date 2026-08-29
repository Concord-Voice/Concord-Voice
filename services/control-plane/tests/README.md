# Control Plane Tests

Test suite for the Concord Voice Control Plane service (Go).

## Structure

Every package under `internal/` carries `_test.go` files, and so do `pkg/config` and
`pkg/logger`. Run `ls internal/` for the current package set, and
`go test ./internal/<pkg>/...` to run one package. The shared test infrastructure is the
only part worth enumerating here.

```
internal/
└── testhelpers/                    # Shared test infrastructure
    ├── testdb.go                   # SetupTestDB, TruncateAllTables, RunMigrations
    ├── testredis.go                # SetupTestRedis — delegates to redistest/
    ├── redistest/                  # Per-process Redis logical-DB allocator (#2680)
    ├── testserver.go               # TestServer with CreateTestUser/Server/Channel helpers
    └── fixtures.go                 # TestUser struct, E2EETestKeys, ValidCiphertext
```

Test files follow three naming conventions:

- `*_test.go` — unit tests, no external services
- `*_integration_test.go` — needs PostgreSQL and Redis
- `mock_*_test.go` — in-package fakes

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
- `CreateTestChannel(t, serverID, name)` — Inserts channel
- `CreateTestUserUnverified(t, username)` — Inserts a user whose email is not verified
- `CreateTestRole(t, serverID, name, position, permissions)` — Inserts a role
- `CreateTestMessage(t, channelID, user, content)` — Inserts a message
- `DoRequest(method, path, body, headers)` — HTTP request via `httptest.ResponseRecorder`

`AuthHeaders(accessToken)` is a package-level function, not a `TestServer` method. It takes an
access token and returns an `Authorization: Bearer <token>` header.

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
