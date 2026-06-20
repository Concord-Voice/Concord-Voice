# Control Plane Service

The control plane handles authentication, authorization, server/channel management, billing, and user presence for Concord Voice.

## Tech Stack

- **Go** 1.26.1+
- **Gin** - HTTP web framework
- **PostgreSQL** 16
- **Redis** 7 - Caching, rate limiting, ephemeral state
- **NATS** 2.x - Inter-service messaging
- **JWT** - Authentication tokens (15min access + 30d refresh)
- **WebSocket** - Real-time signaling (ticket-based auth)

## Architecture

The control plane is responsible for:
- User authentication (Argon2id, JWT, session management)
- E2EE key management (RSA-OAEP 4096-bit, AES-256-GCM channel keys)
- Server and channel CRUD operations
- Channel groups and ordering
- Membership and permissions management
- WebSocket hub (real-time messaging, presence, typing indicators)
- DM system (conversations, friend codes, privacy controls)
- Voice coordination via NATS (room signaling with media-plane)
- Server invites with configurable expiration
- Message read states and unread tracking

## Project Structure

```
control-plane/
├── cmd/
│   └── server/           # Application entry point (main.go)
├── internal/
│   ├── api/              # API router setup
│   ├── auth/             # Authentication (Argon2id, JWT, sessions)
│   ├── channels/         # Channel management and groups
│   ├── clientconfig/     # Client configuration endpoint
│   ├── database/         # Database connections and migrations
│   ├── dm/               # Direct messaging system
│   ├── email/            # Email verification service
│   ├── friends/          # Friend codes and privacy
│   ├── invites/          # Server invite system
│   ├── media/            # Media file handling (MinIO/S3)
│   ├── members/          # Server membership
│   ├── messages/         # Message CRUD with E2EE enforcement
│   ├── mfa/              # Multi-factor authentication (TOTP, WebAuthn)
│   ├── middleware/        # Auth, rate limiting, CORS
│   ├── models/           # Data models
│   ├── ownership/        # Server ownership transfer
│   ├── rbac/             # Role-based access control
│   ├── servers/          # Server CRUD
│   ├── sessions/         # Session management and device tracking
│   ├── storage/          # Object storage abstraction
│   ├── testhelpers/      # Shared test infrastructure
│   ├── updates/          # Client update management
│   ├── users/            # User profiles, preferences, privacy
│   ├── voice/            # Voice coordination (NATS ↔ media-plane)
│   ├── websocket/        # WebSocket hub (messaging, presence, DMs)
│   └── klipy/            # Klipy GIF integration and privacy settings
├── pkg/
│   ├── config/           # Configuration management
│   └── logger/           # Structured logging
├── migrations/           # 57 SQL migration pairs (000001-000057)
├── tests/                # Integration tests (90 test files)
├── Dockerfile
├── go.mod
└── go.sum
```

## Development

### Prerequisites

- Go 1.26.1+
- PostgreSQL 16
- Redis 7+
- NATS 2.x

### Setup

1. **Install dependencies**
   ```bash
   go mod download
   ```

2. **Set up environment variables**
   ```bash
   cp .env.example .env
   # Edit .env with your configuration
   ```

3. **Start infrastructure** (using Docker Compose from project root)
   ```bash
   cd ../..
   docker-compose up -d postgres redis
   ```

4. **Run the service**
   ```bash
   go run cmd/server/main.go
   ```

The service will start on port 8080 by default.

### Environment Variables

```bash
ENVIRONMENT=development
PORT=8080
DATABASE_URL=postgres://concord:password@localhost:5432/concord?sslmode=disable
REDIS_URL=redis://localhost:6379
JWT_SECRET=your-secret-key
NATS_URL=nats://localhost:4222
MEDIA_PLANE_URL=http://localhost:3000
LICENSING_AUTHORITY_URL=http://localhost:8082

# TRUSTED_PROXY_CIDRS — Comma-separated IPv4/IPv6 CIDRs whose X-Forwarded-For
# and X-Real-IP headers c.ClientIP() honors. REQUIRED in production (startup
# fails if unset); in non-production (development/staging/CI) falls back to
# 172.16.0.0/12 (broad RFC1918 Docker range). Malformed CIDRs fail startup in
# all envs. Two-layer default: the Go code falls back to 172.16.0.0/12; the
# deployment surface (.env.example + docker-compose.yml) pins the tighter
# 172.19.0.0/16 that matches the docker-compose bridge network — docker-compose.production.yml
# requires an explicit value (no default) to avoid silently inheriting a dev CIDR.
TRUSTED_PROXY_CIDRS=172.19.0.0/16
```

### Available Endpoints

#### Health Check
```
GET /health
```

#### Authentication (Phase 1A) ✅ IMPLEMENTED
```
POST /api/v1/auth/register
  - Registers new user with Argon2id password hashing
  - Validates username (profanity filtering, leetspeak detection)
  - Stores E2EE public key
  - Returns JWT access token + refresh token (HttpOnly cookie)

POST /api/v1/auth/login
  - Authenticates user
  - Returns JWT access token + refresh token
  - Returns E2EE keys (wrapped_private_key, key_derivation_salt)

POST /api/v1/auth/refresh
  - Refreshes access token using refresh token cookie

POST /api/v1/auth/logout
  - Revokes refresh token

GET /api/v1/auth/sessions (authenticated)
  - Lists user's active sessions

DELETE /api/v1/auth/sessions/:id (authenticated)
  - Revokes a specific session
```

#### Users (Phase 1A) ✅ IMPLEMENTED
```
GET /api/v1/users/me (authenticated)
  - Returns current user info
```

#### Servers (Phase 1B) ✅ IMPLEMENTED
```
GET    /api/v1/servers
POST   /api/v1/servers
GET    /api/v1/servers/:id
PATCH  /api/v1/servers/:id
DELETE /api/v1/servers/:id
```

#### Channels (Phase 1B) ✅ IMPLEMENTED
```
POST   /api/v1/servers/:server_id/channels
GET    /api/v1/servers/:server_id/channels
GET    /api/v1/channels/:id
PATCH  /api/v1/channels/:id
DELETE /api/v1/channels/:id
```

#### Messages, Members, Invites (Phase 1B) ✅ IMPLEMENTED
```
GET/POST /api/v1/channels/:id/messages
PATCH/DELETE /api/v1/messages/:id
POST /api/v1/servers/:id/invites
POST /api/v1/invites/:code/accept
GET/POST/PATCH/DELETE /api/v1/servers/:id/members
```

#### WebSocket (Phase 1B) ✅ IMPLEMENTED
```
POST /api/v1/auth/ws-ticket  → obtain single-use ticket (30s TTL)
GET  /api/v1/ws?ticket=...   → ticket-based auth (no JWT in URL)
Events: message, typing, presence, dm_message, voice signaling
```

#### DMs, Friends, Privacy (Phase 1C) ✅ IMPLEMENTED
```
GET/POST /api/v1/dm/conversations
POST /api/v1/dm/conversations/:id/messages
GET/POST/DELETE /api/v1/friends/codes
GET/PATCH /api/v1/users/me/privacy
POST /api/v1/dm/conversations/:id/voice/join|leave|signal
```

#### Voice & E2EE (Phase 1C) ✅ IMPLEMENTED
```
POST /api/v1/voice/join|leave|signal
GET/POST /api/v1/channels/:id/keys
POST /api/v1/channels/:id/keys/rotate
```

**API routes:** the canonical count lives in [[internal]](../..[internal]) Key Counts. See `docs/api/openapi.yaml` for the OpenAPI spec (covers partial routes; Phase 2A additions pending spec update).

### Testing

```bash
# Run tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests with race detector
go test -race ./...
```

### Building

```bash
# Build binary
go build -o bin/control-plane cmd/server/main.go

# Or use Make
make build

# Build Docker image
docker build -t concord-control-plane .
```

## Database Migrations

This service uses [golang-migrate](https://github.com/golang-migrate/migrate) for versioned database migrations with rollback support.

### Quick Start

```bash
# Apply all pending migrations
make migrate-up

# Rollback last migration
make migrate-down

# Check current version
make migrate-version

# Create new migration
make migrate-create NAME=add_user_status
```

### How Migrations Work

- Migrations run automatically when the server starts
- Each migration has an UP (apply) and DOWN (rollback) SQL file
- Migrations are versioned sequentially (000001, 000002, etc.)
- Migration state is tracked in the `schema_migrations` table

### Creating New Migrations

1. Use the Make command to generate migration files:
   ```bash
   make migrate-create NAME=add_user_profile_fields
   ```

2. Edit the generated files in `migrations/`:
   - `000004_add_user_profile_fields.up.sql` - Forward migration
   - `000004_add_user_profile_fields.down.sql` - Rollback migration

3. Apply the migration:
   ```bash
   make migrate-up
   ```

### Migration Best Practices

- Always create both UP and DOWN migrations
- Test rollbacks to ensure they properly reverse changes
- One logical change per migration
- Never modify committed migrations (create new ones instead)
- Migrations run in transactions automatically

For detailed migration documentation, see [migrations/README.md](migrations/README.md).

## Database Schema

### Users (Phase 1A) ✅
- id (UUID, PK)
- email (unique)
- username (unique, lowercase)
- password_hash (Argon2id)
- age_confirmed (boolean)
- created_at
- updated_at

### Public Keys (Phase 1A) ✅
- id (UUID, PK)
- user_id (FK → users)
- public_key (BYTEA, base64-encoded SPKI format)
- key_version (integer, default 1)
- created_at
- updated_at
- UNIQUE(user_id, key_version)

### Refresh Tokens (Phase 1A) ✅
- id (UUID, PK)
- user_id (FK → users)
- token_hash (SHA256)
- user_agent (text)
- ip_address (text)
- created_at
- expires_at

### Servers (Phase 1B) ✅
- id, name, owner_id, icon_url, header_image_url, created_at, updated_at

### Channels (Phase 1B) ✅
- id, server_id, name, type (text/voice/announcement), position, topic, slowmode_seconds, group_id, voice_settings (JSONB)

### Server Members (Phase 1B) ✅
- server_id, user_id, role (owner/admin/member), joined_at, nickname

### Messages (Phase 1B) ✅
- id, channel_id, user_id, content, encrypted_key, key_epoch, edited_at, created_at

### Channel Groups (Phase 1C) ✅
- id, server_id, name, position, is_collapsed

### DM System (Phase 1C) ✅
- dm_conversations, dm_participants, dm_messages, dm_read_states, dm_channel_keys, dm_key_recipients, dm_voice_participants, dm_voice_signals

### Additional Tables ✅
- user_keys, server_invites, channel_keys, channel_key_recipients, channel_read_states, voice_participants, user_preferences, friend_codes, privacy_settings

**57 migrations total** (000001-000057). See `migrations/` for full schema.

## Current Status

### Phase 1A: Authentication & E2EE ✅ COMPLETE
- [x] User registration with Argon2id password hashing
- [x] Login with JWT access tokens (15 min) + refresh tokens (30 day)
- [x] Username validation with profanity filtering and leetspeak detection
- [x] E2EE key management (RSA-OAEP 4096-bit)
- [x] Session management (list/revoke, device tracking, machine-ID dedup)
- [x] Rate limiting (Redis-based, per-IP and per-user)
- [x] Password strength validation
- [x] Ticket-based WebSocket authentication (30s single-use)

### Phase 1B: Channels & Text Chat ✅ COMPLETE
- [x] Server CRUD with icon and header image uploads
- [x] Channel CRUD with encryption, topics, slowmode
- [x] WebSocket real-time messaging with E2EE enforcement
- [x] User presence tracking (online/offline/typing)
- [x] Message history, pagination, edit, delete
- [x] Basic permissions (owner, admin, member)
- [x] Server invites with configurable expiration
- [x] Channel read states and unread tracking
- [x] Channel-level E2EE key management and rotation

### Phase 1C: Voice & Media ✅ COMPLETE
- [x] Voice coordination via NATS (join/leave/signal)
- [x] Channel groups with drag-and-drop ordering
- [x] DM system (8 tables, conversations, messages, voice calls)
- [x] Friend codes and privacy controls
- [x] DM E2EE enforcement

### Phase 2+: Advanced Features

- [x] RBAC/SBAC permissions (#82) ✅ DONE
- [x] Token theft detection + MFA (#89) ✅ DONE
- [x] Klipy GIF integration + privacy settings (#168–#172) ✅ DONE
- [x] Server mute/deafen (#84 partial) ✅ DONE (migration 000054)
- [ ] File uploads (S3 + CDN) — partially implemented (`media_files` table, MinIO storage)
- [ ] Message search
- [ ] Billing integration (Stripe)
