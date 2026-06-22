# Getting Started with Concord Voice Development

Welcome! This guide will get you up and running with Concord Voice in about 15 minutes.

## What is Concord Voice?

Concord Voice is a privacy-first, real-time voice communication platform with two deployment models:
- **SaaS** - Cloud-hosted with freemium pricing
- **Self-Hosted** - Licensed deployment for organizations

### Key Features

✅ **Privacy-First** - No biometric ID, minimal telemetry
✅ **High-Quality Audio** - Opus codec, low-latency WebRTC
✅ **Flexible Deployment** - Cloud or self-hosted
✅ **À-la-carte Pricing** - Pay for features you need
✅ **Open Architecture** - Clear separation of concerns

## Architecture at a Glance

```
Desktop/Web Client
    ↓ (HTTP/WebSocket)
Control Plane (Go) ← Auth, Channels, Users
    ↓
Database (PostgreSQL) + Cache (Redis)

Desktop/Web Client
    ↓ (WebRTC)
Media Plane (Node.js + mediasoup) ← Voice Routing
```

**3 Main Components:**

1. **Client** (Electron + React) - User interface
2. **Control Plane** (Go) - Business logic, auth, state
3. **Media Plane** (Node.js) - WebRTC voice routing

## Prerequisites

Install these first:

- **Node.js 24+** - [Download](https://nodejs.org/)
- **Go 1.26.1+** - [Download](https://go.dev/dl/)
- **Docker Desktop** - [Download](https://www.docker.com/products/docker-desktop/)
- **Git** - [Download](https://git-scm.com/)
- **Python 3** - [Download](https://www.python.org/downloads/) (for `pre-commit` hooks framework and mediasoup build)

### Verify Installation

```bash
node --version    # Should be v20.x or higher
go version        # Should be go1.26.1 or higher
docker --version  # Should be 20.x or higher
```

## Setup (15 minutes)

### Step 1: Clone the Repository

```bash
git clone <repository-url>
cd concord
```

### Step 2: Install Pre-commit Hooks

```bash
pip install pre-commit        # or: brew install pre-commit / pipx install pre-commit
./scripts/install-git-hooks.sh
```

This installs 22 hooks covering security scanning, linting, formatting, and commit message validation. See [SETUP_GITHUB.md](./SETUP_GITHUB.md) for details.

### Step 3: Start Infrastructure (2 min)

Start PostgreSQL, Redis, and NATS:

```bash
docker-compose up -d
```

Wait about 30 seconds for services to start, then verify:

```bash
docker-compose ps
```

You should see `healthy` status for postgres, redis, and nats.

### Step 4: Start Control Plane (2 min)

Open a new terminal:

```bash
cd services/control-plane
go mod download          # First time only
go run cmd/server/main.go
```

You should see:
```
INFO Starting Control Plane server port=8080 env=development
```

Test it:
```bash
curl http://localhost:8080/health
```

### Step 5: Start Media Plane (3 min)

Open another terminal:

```bash
cd services/media-plane
npm install             # First time only
npm run dev
```

You should see:
```
info: Media Plane server started {"port":3000,"environment":"development"}
```

Test it:
```bash
curl http://localhost:3000/health
```

### Step 6: Start Desktop Client (5 min)

Open another terminal:

```bash
cd client/desktop
npm install             # First time only (takes ~3 min)
npm run dev
```

The Electron app should launch automatically!

## What You Should See

### Terminal Windows

You should have **4 terminals** running:

1. **Docker** - `docker-compose up`
2. **Control Plane** - Go server on port 8080
3. **Media Plane** - Node server on port 3000
4. **Desktop Client** - Electron app with Vite dev server

### Desktop App

The Concord Voice desktop app should show:
- A connection selector (hosted vs self-hosted)
- Registration and login screens with full validation
- Password strength meter
- E2EE information tooltips
- Branding: concordvoice.chat

**Try it out!** You can now register an account and log in. E2EE keys are generated automatically and stored securely.

## Verify Everything Works

Run these checks:

```bash
# Check control plane
curl http://localhost:8080/health
# Expected: {"service":"control-plane","status":"healthy"}

# Check media plane
curl http://localhost:3000/health
# Expected: {"service":"media-plane","status":"healthy"}

# Check database
docker exec -it concord-postgres psql -U concord -d concord -c "SELECT 1;"
# Expected: Returns "1"

# Check Redis
docker exec -it concord-redis redis-cli ping
# Expected: "PONG"
```

## What's Implemented?

### ✅ Phase 1A Complete (Authentication & E2EE)

**Backend (Go - Control Plane)**:
- User registration with Argon2id password hashing
- Login with JWT access tokens (15 min) + refresh tokens (30 day)
- Username validation with profanity filtering and leetspeak detection
- E2EE public key storage in PostgreSQL
- Session management (list sessions, revoke sessions)
- Rate limiting (Redis-based, per-IP and per-user)
- HttpOnly cookie handling for refresh tokens
- Password strength validation

**Frontend (Electron Desktop)**:
- Registration form with full E2EE key generation (RSA-OAEP 4096-bit)
- Login form with key unwrapping (PBKDF2 600k iterations)
- Password strength meter (5-tier system)
- Connection selector UI (hosted vs self-hosted)
- InfoTooltip component for user education
- Client-side encryption (AES-GCM, PBKDF2, RSA-OAEP)
- Token storage and automatic refresh mechanism
- Branding integration (concordvoice.chat)

**DevOps & Security**:
- Pre-commit hooks (backend tests, frontend tests, security scanning)
- Pre-commit hooks (secret detection, large file blocking, commit message format)
- Security scanning (Semgrep, Trivy, TruffleHog)
- Issue and PR templates
- Branch protection rules
- Contributing guide and security policy

### ✅ Phase 1B Complete (Channels & Text Chat)

- Server/channel CRUD with role-based permissions
- WebSocket real-time messaging with E2EE (AES-256-GCM)
- Presence system (online/offline, typing indicators)
- Server invites, session management, unread tracking
- Browser-inspired UI layout (server bar, folder bar, channel panel)
- Custom context menus, ~30 Zustand stores (see `[internal]` for current count)
- 272 frontend test files, 90 Go test files (see `[internal]` for current count)

### ✅ Phase 1C Complete (Voice & Media)

- Media Plane (Node.js + mediasoup SFU) — fully implemented
- Voice channels (join/leave, mute/deafen/PTT, audio devices, 7 quality tiers)
- Screen sharing UI, video calls UI
- Channel groups with drag-and-drop
- Emoji picker (1,800+ emoji), DM frontend
- Electron hardening (safeStorage, ASAR, sandbox)

### ✅ Phase 2A Near-Complete (Foundations & Security)

- MFA / WebAuthn (TOTP + recovery keys + trusted devices)
- Full RBAC/SBAC permission system
- Email verification, ownership transfer
- CI/CD reactivated (GitHub Actions build.yml + SonarQube)
- All major dependency upgrades (React 19, Router 7, Zustand 5, ESLint 10, Vite 8, Go 1.26.1)
- Auto-updater with splash screen, safety checks, and rollback

### 📋 Phase 2B In Progress (Core Features & Polish)

- Chat enhancements: reactions, replies, pinning, search, GIF integration (Klipy)
- Profile editors and avatar management
- File/attachment uploads with E2EE
- Video/screen sharing backend integration
- Server mute/deafen and DM message pinning

> For current issue counts and phase breakdown, see `[internal]`.

**You can now test the full platform end-to-end — auth, text chat, voice channels, and DMs!**

## Making Your First Change

**IMPORTANT:** Before making changes, install git hooks to prevent committing secrets:

```bash
cd /path/to/Concord
./scripts/install-git-hooks.sh
```

### Proper Development Workflow

Follow the GitHub Flow process:

1. **Create a feature branch**
   ```bash
   git checkout main
   git pull origin main
   git checkout -b feature/my-feature
   ```

2. **Make your changes**
   For example, add a custom endpoint to the control plane in `services/control-plane/internal/api/router.go`:
   ```go
   router.GET("/hello/:name", func(c *gin.Context) {
       name := c.Param("name")
       c.JSON(http.StatusOK, gin.H{
           "message": fmt.Sprintf("Hello, %s!", name),
       })
   })
   ```

3. **Test your changes**
   ```bash
   # Restart control plane (Ctrl+C then re-run)
   go run cmd/server/main.go

   # Test the endpoint
   curl http://localhost:8080/hello/Developer
   # Expected: {"message":"Hello, Developer!"}
   ```

4. **Commit and push**
   ```bash
   git add .
   git commit -m "feat: add hello endpoint"
   # Git hooks will run - blocking secrets, large files, etc.
   git push origin feature/my-feature
   ```

5. **Create Pull Request**
   - Go to GitHub and click "Create Pull Request"
   - Fill out the PR template
   - Request review from team member
   - Wait for pre-commit hooks to pass
   - Merge when approved

**Congratulations!** You just contributed to Concord properly!

See [docs/SETUP_GITHUB.md](./SETUP_GITHUB.md) for full GitHub collaboration guide.

## Next Steps

Now that you're set up, here's what to explore:

### 1. Understand the Architecture

Read [Architecture Documentation](./architecture.md) to understand how the 3 services interact (Control Plane, Media Plane, Desktop Client).

### 2. Explore the Codebase

All core features are implemented. Good starting points:
- **Auth flow:** `services/control-plane/internal/auth/` — Argon2id, JWT, E2EE key storage
- **WebSocket:** `services/control-plane/internal/websocket/` — Hub, ticket-based auth, channel subscriptions
- **Voice:** `services/media-plane/src/lib/roomManager.ts` — mediasoup SFU, WebRTC transport management
- **Frontend stores:** `client/desktop/src/renderer/stores/` — ~30 Zustand stores (see `[internal]` for current count)

### 3. Run the Tests

```bash
# Backend (Go)
cd services/control-plane && go test ./...

# Frontend (React/TypeScript)
cd client/desktop && npx vitest run
```

### 4. Pick an Issue

Check [GitHub Issues](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues) for open Phase 2 items. See `[internal]` for the full project status and ground truth.

## Common Issues

### Port already in use

```bash
# Kill process on port
lsof -ti:8080 | xargs kill -9

# Or change ports in .env files
```

### Docker not starting

```bash
# Reset Docker
docker-compose down -v
docker-compose up -d

# Or restart Docker Desktop
```

### npm install fails

```bash
# Clear cache
npm cache clean --force
rm -rf node_modules package-lock.json
npm install
```

### Electron won't start

```bash
# Rebuild native modules
cd client/desktop
npm run postinstall
```

## Development Resources

- **Architecture**: [docs/architecture.md](./architecture.md)
- **Development Guide**: [docs/development.md](./development.md)
- **API Docs**: [docs/api/](./api/) — OpenAPI 3.0 spec
- **Contributing**: [CONTRIBUTING.md](../.github/CONTRIBUTING.md)

## Project Structure Quick Reference

```
concord/
├── client/
│   ├── desktop/           # Electron app
│   └── web/              # Web client (future)
├── services/
│   ├── control-plane/    # Go service - auth, channels
│   ├── media-plane/      # Node.js - WebRTC SFU
│   └── licensing-authority/  # Go - license validation
├── docs/                 # Documentation
├── infrastructure/       # Docker, K8s configs
├── shared/              # Shared types
└── docker-compose.yml   # Local development
```

## Getting Help

**Questions?**
- Check [development.md](./development.md) for detailed guides
- Review [architecture.md](./architecture.md) for design decisions
- Open an issue on GitHub
- Join our Discord (link TBD)

## What's Next?

You're ready to start building! Here are the immediate priorities:

**Phase 1A: Authentication & E2EE** ✅ COMPLETE

- [x] Implement user registration (`POST /api/v1/auth/register`)
- [x] Implement login with JWT (`POST /api/v1/auth/login`)
- [x] Add JWT validation middleware
- [x] E2EE key generation and storage
- [x] Username validation with profanity filtering
- [x] Password strength validation
- [x] Session management
- [x] Rate limiting
- [x] GitHub collaboration setup (CI/CD, pre-commit hooks)

**Phase 1B: Channels & Text Chat** ✅ COMPLETE

- [x] Server/channel CRUD with role-based permissions
- [x] WebSocket real-time messaging with E2EE
- [x] Presence system (online/offline, typing)
- [x] Server invites, unread tracking, session management
- [x] Browser-inspired UI layout with ~30 Zustand stores (see `[internal]`)
- [x] 272 frontend test files, 90 Go test files (see `[internal]`)

**Phase 1C: Voice & Media** ✅ COMPLETE

- [x] Media Plane (mediasoup SFU, NATS, Socket.IO)
- [x] Voice channels (join/leave, mute/deafen/PTT, 7 quality tiers)
- [x] Screen sharing UI, video calls UI
- [x] Channel groups, emoji picker, DM frontend
- [x] Electron hardening (safeStorage, ASAR, sandbox)

**Phase 2A: Foundations & Security** ✅ NEAR-COMPLETE

- [x] MFA / WebAuthn (TOTP + recovery keys + trusted devices)
- [x] RBAC/SBAC permission system
- [x] Email verification, ownership transfer
- [x] CI/CD reactivated (GitHub Actions build.yml + SonarQube)
- [ ] Code signing (macOS notarization, Windows Authenticode)

**Phase 2B: Core Features & Polish** 📋 IN PROGRESS

- [ ] Chat enhancements: reactions, replies, pinning, search, GIF integration (Klipy)
- [ ] Profile editors and avatar management
- [ ] File/attachment uploads with E2EE
- [ ] Video/screen sharing backend integration
- [ ] Server mute/deafen and DM message pinning

See [ROADMAP.md](../ROADMAP.md) for milestones and [[internal]](..[internal]) for current tasks.

---

**Welcome to the Concord Voice project! Let's build something great.** 🎙️
