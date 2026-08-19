# Development Guide

This guide helps you run Concord Voice locally for development.

**IMPORTANT:** Before you start developing, install git hooks to prevent committing secrets:

```bash
cd /path/to/Concord
./scripts/install-git-hooks.sh
```

See [SETUP_GITHUB.md](./SETUP_GITHUB.md) for full GitHub collaboration workflow.

## Prerequisites

### Required

- **Node.js** 24+ and npm 10+ for the desktop and media-plane workspaces.
  `client/admin` needs **Node.js >= 24.15.0** (Node 26 included). Go-only work
  does not require Node.js
- **Go** 1.26.1+
- **Docker** and **Docker Compose**
- **Git**
- **Python 3** (for `pre-commit` hooks framework and mediasoup build)
- **lsof** (preinstalled on macOS, or `sudo apt-get install lsof` on Debian/Ubuntu)

### Optional

- **PostgreSQL** 16+ (if not using Docker)
- **Redis** 7+ (if not using Docker)

## Quick Start

### 1. Clone the Repository

```bash
git clone <repository-url>
cd concord
```

### 2. Start Infrastructure Services

Start PostgreSQL and Redis using Docker Compose:

```bash
docker-compose up -d postgres redis nats
```

Check their status:

```bash
docker-compose ps
```

You should see `concord-postgres`, `concord-redis`, and `concord-nats` running.

### 3. Set Up Control Plane (Go)

```bash
cd services/control-plane

# Install Go dependencies
go mod download

# Create .env file (optional, has defaults)
cat > .env << EOF
DATABASE_URL=postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable
REDIS_URL=redis://:concord_dev_redis@localhost:6379
JWT_SECRET=dev_jwt_secret_change_in_production
PORT=8080
EOF

# Run the service
go run cmd/server/main.go
```

The control plane starts on `http://localhost:8080`.

Test it:
```bash
curl http://localhost:8080/health
# Should return: {"service":"control-plane","status":"healthy"}
```

### 4. Set Up Media Plane (Node.js)

In a new terminal:

```bash
cd services/media-plane

# Install dependencies
npm install

# Create .env file (optional)
cat > .env << EOF
PORT=3000
ANNOUNCED_IP=127.0.0.1
RTC_MIN_PORT=40000
RTC_MAX_PORT=49999
EOF

# Run the service
npm run dev
```

The media plane starts on `http://localhost:3000`.

Test it:
```bash
curl http://localhost:3000/health
# Should return: {"service":"media-plane","status":"healthy"}
```

### 5. Set Up Desktop Client

In a new terminal:

```bash
cd client/desktop

# Install dependencies
npm install

# Run the client
npm run dev
```

The Electron app should launch automatically.

### 6. Set Up Admin Portal

In a new terminal:

```bash
cd client/admin
npm ci
npm run test
npm run build
npx vite preview --host 127.0.0.1 --port 4181 --strictPort
```

Open `http://127.0.0.1:4181/admin/`. Vite has no API proxy: development and
preview builds send `/admin/api/v1/*` requests to their own origin. Unit and
browser tests intercept those requests. Production serves the built assets and
API from the control plane at the same origin. See
[`client/admin/README.md`](../client/admin/README.md) for the full command set
and the named `admin_ui` Docker build context.

## Development Workflow

### Making Changes

**Control Plane (Go)**:
- Edit files in `services/control-plane/`
- Stop and restart: `go run cmd/server/main.go`
- Or use `air` for hot reload: `go install github.com/cosmtrek/air@latest && air`

**Media Plane (Node.js)**:
- Edit files in `services/media-plane/src/`
- Hot reload runs through `tsx watch`
- Changes apply automatically

**Desktop Client**:
- Edit files in `client/desktop/src/`
- Hot reload runs through Vite
- Changes apply automatically to renderer process
- Main process changes require restart

**Admin Portal**:
- Edit files in `client/admin/src/`
- `npm run dev` starts the frontend at `/admin/` with hot reload
- There is no development API proxy. Use tests for mocked API flows, or the
  control-plane production image for same-origin integration

### Database Migrations

Current migrations run automatically on startup. For production, use a migration tool:

```bash
# Install golang-migrate
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# Create new migration
migrate create -ext sql -dir services/control-plane/migrations -seq add_users_table

# Run migrations
migrate -path services/control-plane/migrations -database "postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable" up
```

### Accessing Database

**Via psql**:
```bash
docker exec -it concord-postgres psql -U concord -d concord
```

**Via pgAdmin** (Web UI):
```bash
docker-compose --profile tools up -d pgadmin
```

Then open http://localhost:5050
- Email: `admin@concord.local`
- Password: `admin`

### Accessing Redis

**Via redis-cli**:
```bash
docker exec -it concord-redis redis-cli
```

**Via Redis Commander** (Web UI):
```bash
docker-compose --profile tools up -d redis-commander
```

Then open http://localhost:8081

## Testing

### Control Plane (Go)

```bash
cd services/control-plane

# Run all tests (unit + integration)
# -p 1 is required for anything touching PostgreSQL or NATS — see
# [internal]rules/tests.md § What `-p 1` is actually for. Without it, concurrent
# package binaries contend on the shared test database and fail widely.
go test -p 1 ./...

# Run with race detection (recommended)
go test -race -p 1 ./...

# Run with coverage report
go test -p 1 -coverprofile=coverage.out ./...
go tool cover -func=coverage.out     # Terminal summary
go tool cover -html=coverage.out     # Browser report

# Run specific package
go test ./internal/auth/...
go test ./internal/channels/...

# Run unit tests only (no DB required)
go test ./internal/auth/ -run "TestHashPassword|TestValidatePassword|TestValidateUsername"
go test ./pkg/config/...

# Run integration tests (requires PostgreSQL + Redis via Docker)
# -p 1 for the same reason as above — these touch the shared PostgreSQL.
go test -p 1 ./internal/auth/ -run "Integration"
go test -p 1 ./internal/servers/... ./internal/channels/... ./internal/messages/...
```

Integration tests use the `testhelpers` package. It auto-connects to PostgreSQL and Redis, runs migrations, and creates user, server, and channel helpers. See `services/control-plane/tests/README.md` for details.

**Redis is isolated per test process, automatically.** Each `go test` process
allocates its own Redis logical database, so two worktrees — or two package
binaries in one run — cannot flush each other's keys. You do not need to set
anything, and there is nothing to forget. Full convention in
[`[internal]rules/tests.md`](../[internal]rules/tests.md) § Test isolation.

One thing worth knowing: `CONCORD_TEST_REDIS_DB=<n>` pins one index and skips
allocation, for inspecting a specific database while debugging. It **disables**
isolation rather than redirecting it, and in a way that reaches further than it
first looks: skipping allocation also skips the ticket, so the pinned index is
never reserved. A pinned process therefore collides not only with another
process sharing the same pin, but with any ordinary unpinned process the
allocator happens to hand that same index — and neither can tell, because both
computed it legitimately and `Reset` permits each to flush the other. Export it
for one foreground command, never in a shell you then run a second `go test`
from; if you need a pin while other tests are running, point that run at its own
Redis instance. `0` and any non-positive value are rejected; `0` holds the
allocator's counter and the dev app's own data.

The pool is `databases - 1`, read live from the server with a fallback of 15 —
so 15 usable indices against a stock Redis. Isolation holds while the number of
test processes **simultaneously live** stays inside that pool; past it the
allocator wraps and hands a second process an index the first is still using,
and they flush each other exactly as they did before #2680. `-p 1` and the
handful of worktrees one machine realistically runs keep this well out of reach,
but it is the ceiling: a large parallel fan-out, or many concurrent worktrees,
needs separate Redis instances rather than a bigger pool.

There is deliberately no runtime wrap warning, which is a different question
from the ceiling above. The ticket counts *cumulative* allocations rather than
concurrent ones, and one full suite takes ~60 of them, so such a warning would
be permanently true after the first ordinary run and would then fire forever
while telling you nothing about whether anything is actually colliding.

**PostgreSQL is not isolated the same way.** All test processes share one
database, and the advisory lock guarding it is held only for each test's
setup→cleanup window — so a concurrent worktree's cleanup can truncate rows your
test is still using, **at any migration version**. Matching migration counts does
not prevent this; it only prevents the separate schema-divergence hazard. Give the
second worktree its own `DATABASE_URL`, or run the two serially. See the same
rules file, and [#2790](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues/2790)
for the fix.

### Media Plane

```bash
cd services/media-plane

# Run tests (when implemented)
npm test

# Run with coverage
npm run test:coverage
```

### Desktop Client (React/TypeScript)

```bash
cd client/desktop

# Run all unit tests
npx vitest run

# Watch mode (re-runs on file changes)
npx vitest

# Run with coverage
npx vitest run --coverage

# Run specific test file
npx vitest run tests/unit/stores/chatStore.test.ts

# Run tests matching a pattern
npx vitest run -t "renders login form"

# Run E2E tests (requires running dev server + backend)
npx playwright test

# Type checking
npm run typecheck

# Linting
npm run lint
```

Tests use Vitest + Testing Library + MSW. See `client/desktop/tests/README.md` for full details on test infrastructure and patterns.

## Debugging

### Go Services

**VS Code**:

Create `.vscode/launch.json`:

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Control Plane",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/services/control-plane/cmd/server",
      "env": {
        "DATABASE_URL": "postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable"
      }
    }
  ]
}
```

**Delve** (CLI):

```bash
cd services/control-plane
dlv debug cmd/server/main.go
```

### Node.js Services

**VS Code**:

```json
{
  "name": "Media Plane",
  "type": "node",
  "request": "launch",
  "runtimeExecutable": "npm",
  "runtimeArgs": ["run", "dev"],
  "cwd": "${workspaceFolder}/services/media-plane",
  "console": "integratedTerminal"
}
```

**Chrome DevTools**:

```bash
cd services/media-plane
node --inspect dist/index.js
# Open chrome://inspect
```

### Electron Client

The Electron app automatically opens DevTools in development mode.

**Main Process**:
```bash
# Add --inspect flag to Electron
# Edit package.json dev script
```

**Renderer Process**:
- DevTools opens automatically
- Or: View → Toggle Developer Tools

## Common Tasks

### Reset Database

```bash
docker-compose down -v postgres
docker-compose up -d postgres
# Wait a few seconds for postgres to start
cd services/control-plane && go run cmd/server/main.go
# Migrations will run automatically
```

### Clear Redis

```bash
docker exec -it concord-redis redis-cli FLUSHALL
```

### View Logs

```bash
# All services
docker-compose logs -f

# Specific service
docker-compose logs -f postgres
docker-compose logs -f redis
```

### Port Conflicts

If ports are in use:

**Change ports** in:
- `docker-compose.yml` for infrastructure
- `.env` files for services
- `vite.config.ts` for client dev server

**Or stop verified Concord processes**:
```bash
./scripts/concord-dev.sh down --force

# If a port remains, inspect its owner before taking manual action
lsof -nP -iTCP:8080 -sTCP:LISTEN
```

Signal a remaining PID manually only after you verify its command and working
directory belong to the intended service.

## Environment Variables Reference

### Control Plane

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `development` | Environment mode |
| `HSTS_HEADER_VALUE` | empty → `max-age=63072000; includeSubDomains; preload` | Optional production STS policy. Nonempty values must pass the structural STS grammar |
| `PORT` | `8080` | HTTP server port |
| `DATABASE_URL` | (see above) | PostgreSQL connection |
| `REDIS_URL` | `redis://:concord_dev_redis@localhost:6379` | Redis connection. Compose starts Redis with `--requirepass "$REDIS_PASSWORD"`, so the URL must carry that password (repo-root `.env`); an uncredentialed URL fails every Redis call with `NOAUTH` |
| `JWT_SECRET` | (dev secret) | JWT signing key |
| `INSTANCE_TYPE` | `saas` | Deployment/entitlement mode (`saas` or `self-hosted`) |
| `SERVER_VERSION` | `dev` | Advertised server version for capability discovery |
| `NATS_URL` | `nats://localhost:4222` | NATS connection |
| `ACTIVITY_HISTORY_CLUSTER_ENABLED` | `false` | Global Activity History rollout gate. Keep false for ordinary development unless you test the guarded single-replica path |
| `CONTROL_PLANE_REPLICA_COUNT` | unset (Compose: `1`) | Explicit control-plane replica contract. Since #2178 it is enforced **unconditionally at startup**, not only under the Activity History gate: any explicitly set value other than `1` fails `Load()` with a fatal error, on every environment. The WebSocket hub holds per-connection session state and channel interest maps in process memory, so a second replica silently drops cross-replica fanout. Horizontal scaling is tracked in #2757. Leaving it unset is fine — the guard only fires on an explicit value |
| `ACTIVITY_HISTORY_OPERATOR_NAME` | empty | Self-host disclosure operator. Required before new Activity History opt-in becomes available (SaaS uses `Concord Voice LLC`) |
| `ACTIVITY_HISTORY_PRIVACY_POLICY_URL` | empty | Self-host disclosure URL. Absolute HTTPS applies outside loopback development and test |

On managed hosts, `concord-ctl.sh nginx-reload` reads the canonical Compose-quoted value from `/opt/concord/.env`. It validates that value before sudo. It then renders nginx as the public HSTS owner. The nginx config hides Go's upstream STS field and emits one server-level policy. It repeats that policy in the three locations whose own `add_header` directives suppress inheritance.

Main and admin config activation is transactional. Syntax, reload or start, and an exact one-field local HTTPS probe must all pass, or the controller restores the prior files. The direct self-host renderer keeps the committed default for scoped nginx responses, while Go owns proxied responses.

Activity History stays off by default. For SaaS, the binary fixes the operator and
privacy URL. For self-hosted development, invalid or missing disclosure keeps new
opt-in unavailable. It does not prevent existing history reads or deletion.

### Operations Metrics (Control Plane, Media Plane, and Ops Agent)

| Variable | Default | Description |
|----------|---------|-------------|
| `OPS_METRICS_ENABLED` | `false` | Enables the aggregate-only metrics pipeline |
| `OPS_METRICS_NODE_ID` | (empty) | Opaque assigned `cvn_` node token. Required when enabled |
| `OPS_METRICS_SHARED_SECRET` | (empty) | Snapshot-signing secret of at least 32 bytes. Required when enabled |
| `OPS_METRICS_INTERVAL` | `15s` | Sampling interval from 5 seconds through 5 minutes |
| `OPS_METRICS_ROLE` | `local` | Local storage role. Reserve `aggregator` for #1504 |

### Media Plane

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `development` | Environment mode |
| `PORT` | `3000` | HTTP server port |
| `ANNOUNCED_IP` | `127.0.0.1` | Public IP for WebRTC |
| `RTC_MIN_PORT` | `40000` | Min RTC port |
| `RTC_MAX_PORT` | `49999` | Max RTC port |
| `NUM_WORKERS` | `4` (code); Compose sets its own literal per file — `4` in `docker-compose.yml`, `3` in `docker-compose.production.yml` | Mediasoup workers. The production value matches that file's `cpus: '3'` limit — a mediasoup worker is a single-threaded subprocess pinned to one core, and nothing enforces agreement between the two values, so change them together. Validated fail-closed at startup (#2178): anything that is not an integer `>= 1` logs `FATAL:` and exits 1 rather than falling back to the default. `0` would otherwise build an empty worker pool that starts and answers `/health`, then crashes on the first voice join |

### STUN/TURN (coturn)

| Variable | Default | Description |
|----------|---------|-------------|
| `TURN_EXTERNAL_IP` | (auto-detect) | Public IP for TURN relay |
| `TURN_SECRET` | (generated) | Shared HMAC secret for ephemeral credentials |
| `TURN_PORT` | `3478` | STUN/TURN listening port (UDP+TCP) |
| `TURN_TLS_PORT` | `5349` | STUN/TURN TLS listening port |
| `TURN_MIN_PORT` | `49152` | Min TURN relay port |
| `TURN_MAX_PORT` | `49252` | Max TURN relay port |

### Desktop Client

No environment variables needed for development.

### Failover/Production Ports

Staging and production deployments use these ports, with an nginx reverse proxy and database replication:

| Port | Service | Notes |
|------|---------|-------|
| 8443 | Control Plane HTTPS | TLS termination via nginx |
| 3443 | Media Plane HTTPS | TLS termination via nginx |
| 443 | TURN over TLS | Bypasses restrictive firewalls (corporate, hotel WiFi) |
| 5433 | PostgreSQL replica | Read replica / hot standby failover |
| 6380 | Redis Sentinel | Coordinates automatic primary failover |
| 6222 | NATS cluster routing | Inter-node communication for multi-instance |
| 8222 | NATS monitoring | HTTP monitoring/metrics endpoint |

## Troubleshooting

### "Cannot connect to database"

```bash
# Check if postgres is running
docker-compose ps postgres

# Check logs
docker-compose logs postgres

# Restart postgres
docker-compose restart postgres
```

### "Port already in use"

```bash
# Stop identity-verified Concord processes first
./scripts/concord-dev.sh down --force

# Inspect any remaining listener without signaling it
lsof -nP -iTCP:8080 -sTCP:LISTEN
```

If cleanup still fails, verify the listener's command and working directory
before you send it `SIGTERM`. Do not blindly signal a PID that `lsof` returned.

### "Module not found" (Node.js)

```bash
# Clean install
rm -rf node_modules package-lock.json
npm install
```

### "Go module not found"

```bash
# Clean module cache
go clean -modcache
go mod download
```

### Mediasoup build failures

Mediasoup requires build tools:

**macOS**:
```bash
xcode-select --install
```

**Linux**:
```bash
sudo apt-get install build-essential python3
```

**Windows**:
```bash
npm install --global windows-build-tools
```

### WebRTC connection issues

- Check firewall allows UDP ports 40000-49999
- Verify `ANNOUNCED_IP` is correct
- Test with localhost first before remote connections
- Check browser console for ICE errors

## IDE Setup

### VS Code Extensions

Recommended:

- **Go** (golang.go)
- **ESLint** (dbaeumer.vscode-eslint)
- **Prettier** (esbenp.prettier-vscode)
- **Thunder Client** (rangav.vscode-thunder-client) - API testing
- **Docker** (ms-azuretools.vscode-docker)

### Settings

```json
{
  "go.useLanguageServer": true,
  "go.lintTool": "golangci-lint",
  "editor.formatOnSave": true,
  "editor.codeActionsOnSave": {
    "source.fixAll.eslint": true
  },
  "[go]": {
    "editor.defaultFormatter": "golang.go"
  },
  "[typescript]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  }
}
```

## Performance Tips

### Go

- Use `go build -race` to detect race conditions
- Profile with `pprof`: import `_ "net/http/pprof"`
- Use `GOMAXPROCS` to limit CPU usage

### Node.js

- Use the Node.js version that each workspace's `package.json` declares. The
  Admin Portal requires Node.js >= 24.15.0, which includes Node 26
- Enable V8 flags: `--max-old-space-size=4096`
- Profile with Chrome DevTools

### Docker

- Allocate enough resources in Docker Desktop
- Minimum: 4 CPU cores, 8 GB RAM
- Use volumes for faster I/O

## Next Steps

- Read [API Documentation](./api/): the OpenAPI 3.0 specification. `scripts/api/check-openapi-coverage.sh` enforces full live-route coverage
- Review [Architecture](./architecture.md): system diagrams, database ERD, message flows
- Check [Contributing Guidelines](../.github/CONTRIBUTING.md)
- Join the Discord for discussions
