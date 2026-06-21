# Development Guide

This guide will help you get Concord Voice running locally for development.

**IMPORTANT:** Before you start developing, install git hooks to prevent committing secrets:

```bash
cd /path/to/Concord
./scripts/install-git-hooks.sh
```

See [SETUP_GITHUB.md](./SETUP_GITHUB.md) for full GitHub collaboration workflow.

## Prerequisites

### Required

- **Node.js** 20+ and npm 10+
- **Go** 1.26.1+
- **Docker** and **Docker Compose**
- **Git**
- **Python 3** (for `pre-commit` hooks framework and mediasoup build)

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

Verify they're running:

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
REDIS_URL=redis://localhost:6379
JWT_SECRET=dev_jwt_secret_change_in_production
PORT=8080
EOF

# Run the service
go run cmd/server/main.go
```

The control plane will start on `http://localhost:8080`.

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

The media plane will start on `http://localhost:3000`.

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

## Development Workflow

### Making Changes

**Control Plane (Go)**:
- Edit files in `services/control-plane/`
- Stop and restart: `go run cmd/server/main.go`
- Or use `air` for hot reload: `go install github.com/cosmtrek/air@latest && air`

**Media Plane (Node.js)**:
- Edit files in `services/media-plane/src/`
- Hot reload is enabled via `tsx watch`
- Changes apply automatically

**Desktop Client**:
- Edit files in `client/desktop/src/`
- Hot reload is enabled via Vite
- Changes apply automatically to renderer process
- Main process changes require restart

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
go test ./...

# Run with race detection (recommended)
go test -race ./...

# Run with coverage report
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out     # Terminal summary
go tool cover -html=coverage.out     # Browser report

# Run specific package
go test ./internal/auth/...
go test ./internal/channels/...

# Run unit tests only (no DB required)
go test ./internal/auth/ -run "TestHashPassword|TestValidatePassword|TestValidateUsername"
go test ./pkg/config/...

# Run integration tests (requires PostgreSQL + Redis via Docker)
go test ./internal/auth/ -run "Integration"
go test ./internal/servers/... ./internal/channels/... ./internal/messages/...
```

Integration tests use the `testhelpers` package which auto-connects to PostgreSQL/Redis, runs migrations, and provides user/server/channel creation helpers. See `services/control-plane/tests/README.md` for details.

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

**Or kill processes**:
```bash
# macOS/Linux
lsof -ti:8080 | xargs kill -9
lsof -ti:3000 | xargs kill -9
lsof -ti:5432 | xargs kill -9
```

## Environment Variables Reference

### Control Plane

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `development` | Environment mode |
| `PORT` | `8080` | HTTP server port |
| `DATABASE_URL` | (see above) | PostgreSQL connection |
| `REDIS_URL` | `redis://localhost:6379` | Redis connection |
| `JWT_SECRET` | (dev secret) | JWT signing key |
| `NATS_URL` | `nats://localhost:4222` | NATS connection |

### Media Plane

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `development` | Environment mode |
| `PORT` | `3000` | HTTP server port |
| `ANNOUNCED_IP` | `127.0.0.1` | Public IP for WebRTC |
| `RTC_MIN_PORT` | `40000` | Min RTC port |
| `RTC_MAX_PORT` | `49999` | Max RTC port |
| `NUM_WORKERS` | `4` | Mediasoup workers |

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

These ports are used in staging/production deployments with nginx reverse proxy and database replication:

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
# Find process using port
lsof -i :8080

# Kill it
kill -9 <PID>
```

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

- Use Node.js 24+ for better performance
- Enable V8 flags: `--max-old-space-size=4096`
- Profile with Chrome DevTools

### Docker

- Allocate enough resources in Docker Desktop
- Minimum: 4 CPU cores, 8 GB RAM
- Use volumes for faster I/O

## Next Steps

- Read [API Documentation](./api/) — OpenAPI 3.0 spec (partial: 88 operations documented; 269 routes implemented)
- Review [Architecture](./architecture.md) — System diagrams, database ERD, message flows
- Check [Contributing Guidelines](../.github/CONTRIBUTING.md)
- Join the Discord for discussions
