# Concord Voice Frequently Asked Questions (FAQ)

**Last Updated:** 2026-05-01

This document answers common questions about Concord Voice's licensing, security, development, deployment, and usage.

---

## 📋 Table of Contents

- [Licensing & Legal](#-licensing--legal)
- [Security & Privacy](#-security--privacy)
- [Development & Contributing](#-development--contributing)
- [Deployment & Self-Hosting](#-deployment--self-hosting)
- [Features & Usage](#-features--usage)
- [Troubleshooting](#-troubleshooting)

---

## 📜 Licensing & Legal

### What is CVSL 1.0?

The Concord Voice Source License 1.0 (CVSL) is a custom source-available license — structurally inspired by the Functional Source License 1.1 (FSL) and the Business Source License 1.1 (BSL) — that allows you to:

- ✅ View and audit the source code
- ✅ Use the software for personal or non-profit purposes, free of charge
- ✅ Modify the code for your own use within those purposes
- ✅ Self-host with unlimited users (personal or non-profit)

But **restricts** you from:

- ❌ Using it to compete with Concord Voice or the hosted Concord Voice service
- ❌ Self-hosting for for-profit or governmental use without a commercial license
- ❌ Misrepresenting the software as your own original work
- ❌ Using it in violation of export-control or economic-sanctions laws

After **the Change Date** (February 15, 2030, or the fourth anniversary of a specific version's first public release, whichever comes first), each version automatically converts to **AGPL-3.0-or-later** (fully open source).

See the full text in [LICENSE](./LICENSE).

---

### Can I use Concord Voice for free?

**Yes!** You can use Concord Voice for free if any of these apply:

- You're an **individual** using it for personal, non-commercial purposes (homelab, family, hobby)
- You're a **non-profit organization** (501(c)(3) or foreign equivalent), educational institution, or public-research organization
- You're using the **hosted SaaS** at [www.concordvoice.com](https://www.concordvoice.com) (subject to its [Terms of Service](./docs/legal/terms-of-service.md))

If you're a **for-profit business** or **governmental entity** that wants to self-host, you need a commercial license. See [Commercial Licensing](./docs/legal/commercial-license.md).

---

### Can I self-host Concord Voice for my business?

**Yes**, with a commercial self-hosted license. For-profit and governmental self-hosting is governed by the **Enterprise** track (single server or custom) or the **MSP & OEM** track (Fleet 10/25/50/100+ for resellers and hosting providers).

You **don't need a license** if you're:

- A non-profit organization (501(c)(3) or foreign equivalent)
- An educational institution
- An individual using it for personal/family use

See [docs/legal/commercial-license.md](./docs/legal/commercial-license.md) for program details.

---

### Can I modify the code?

**Yes!** You can modify Concord Voice for your own use, subject to the License terms:

- ✅ Modify for personal self-hosted instance (Personal Use)
- ✅ Modify for non-profit deployment (Non-Profit Use)
- ✅ Modify under a commercial license (Enterprise or MSP & OEM)
- ✅ Contribute improvements back (encouraged!)
- ❌ Create a competing platform with your modifications (Competing Use prohibition)
- ❌ Distribute modified versions as your own original work (anti-impersonation)

---

### Can I contribute to Concord Voice?

**Absolutely!** We welcome contributions. When you contribute code:

- Your contributions are licensed under the same CVSL 1.0 terms
- On the Change Date, they become AGPL-3.0-or-later (fully open source)
- You retain copyright to your original work
- For substantial contributions, we may request a Contributor License Agreement (CLA)
- See [CONTRIBUTING.md](./.github/CONTRIBUTING.md) for guidelines

**How to contribute:**

1. Fork the repository
2. Create a feature branch
3. Make your changes with tests
4. Submit a Pull Request
5. Participate in code review

---

### What happens on the Change Date (February 15, 2030)?

On the applicable Change Date, each version of Concord Voice becomes additionally licensed under **AGPL-3.0-or-later**:

- Anyone can use that version for any purpose (including competing services)
- Modifications must be shared (AGPL network-copyleft)
- This ensures long-term sustainability and community ownership

**Per-version timing.** Each version has its own Change Date, calculated as the earlier of (a) February 15, 2030 or (b) the fourth anniversary of that version's first public release under the License. Versions released before February 2026 reach AGPL on their fourth anniversary; versions released after February 2026 all converge on 2030-02-15.

---

### Why a custom license? Why not just use FSL, Polyform, AGPL, or MIT?

We chose CVSL 1.0 because it combines properties no off-the-shelf license offered cleanly:

- **Protects development investment** — Competing Use prohibition prevents free-riding while we build the platform
- **Free for the right users** — Personal and non-profit use is free, indefinitely, with no quotas or limits
- **Commercial path for everyone else** — For-profit and government use require a license, with clear self-serve and negotiated tiers
- **Ensures eventual openness** — Guarantees full open source under AGPL-3.0-or-later on the Change Date
- **Transparent from day one** — Source code is available for security audit immediately
- **Anti-impersonation built in** — Explicit protection against forks presenting Concord Voice as their own work
- **Sanctions and interop compliance** — Built-in OFAC/EU/UK/UN sanctions clauses and DMCA § 1201(f) reverse-engineering carve-out

CVSL is structurally inspired by the Functional Source License 1.1 (Sentry) and the Business Source License 1.1 (MariaDB), but is a distinct license authored specifically for Concord Voice's model.

---

### Can I fork Concord Voice?

**Yes**, with restrictions:

- ✅ Fork for personal use (Personal Use)
- ✅ Fork to contribute improvements back
- ✅ Fork to self-host for your non-profit organization
- ✅ Fork to self-host for your commercial organization (with a commercial license)
- ❌ Fork to create a competing platform (Competing Use prohibition)
- ❌ Fork to distribute under a different brand without permission (anti-impersonation)

After the Change Date, the converted version may be used under AGPL-3.0-or-later for any purpose, including competing services.

---

### What's the difference between CVSL and "open core"?

**Traditional Open Core:**

- Core features: Open source (e.g., MIT)
- Premium features: Proprietary forever
- Creates permanent split between free and paid

**CVSL 1.0 (Concord Voice's approach):**

- All features: Source-available immediately
- All code: Becomes fully open source on the Change Date (AGPL-3.0-or-later)
- Commercial use requires a license, but **the entire codebase is the same** — there is no proprietary core or premium-only fork

We believe this is **more open** long-term than traditional open core models, while preserving a sustainable business path.

---

### Do I need a license to self-host for my family?

**No!** Personal, family, educational, and non-profit use is always free:

- ✅ Set up a server for your gaming group
- ✅ Use it for your family chat
- ✅ Deploy it for your school, college, or non-profit
- ✅ Run it for your hobby project

**You need a commercial license if:**

- You're a for-profit business self-hosting for internal use
- You're a governmental agency or state-owned enterprise self-hosting
- You're offering Concord Voice as a hosted or managed service to third-party customers (Enterprise + MSP/OEM tracks)
- You're embedding Concord Voice in a commercial product you sell to customers

For-profit internal-team self-hosting requires a commercial license under CVSL 1.0.

---

### Can universities and schools use Concord Voice?

**Yes, completely free!** Educational institutions can:

- Self-host with unlimited users
- Modify the code for research
- Use it for teaching purposes
- No commercial license required

Educational institutions qualify under Non-Profit Use in CVSL 1.0. We encourage educational use and welcome academic contributions.

---

### What if I want to compete with Concord Voice?

Under CVSL 1.0, you **cannot** use Concord Voice (or any modification or derivative) to develop, market, distribute, host, or operate a product or service that competes with Concord Voice or the hosted Concord Voice service. This Competing Use prohibition is in effect for the duration of the License.

**However:**

- After the Change Date (2030-02-15 or per-version 4-year anniversary), the version becomes AGPL-3.0-or-later, and you can use that version to compete freely
- You can always build complementary services (bots, plugins, integrations) that don't substitute for the platform
- The MSP & OEM track is **not** a Competing Use — partners reselling Concord Voice under license are operating within the framework, not competing with it

If you're unsure whether your intended use is a Competing Use, contact contact-us@concordvoice.com before deploying.

---

### Can I use Concord Voice in my commercial product?

**It depends:**

| Use Case                                  | License Required?                   |
| ----------------------------------------- | ----------------------------------- |
| Internal team communication (for-profit)  | **Yes** (Enterprise license)        |
| Internal team communication (non-profit)  | No (Non-Profit Use)                 |
| Hosting Concord Voice for your customers  | **Yes** (MSP & OEM license — authorized resellers are NOT a Competing Use) |
| Embedding in your product (for customers) | **Yes** (MSP & OEM with Livery)     |
| Building integrations/bots                | No (complementary services allowed) |
| Using the hosted SaaS API                 | Subject to [Terms of Service](./docs/legal/terms-of-service.md) |

**When in doubt**, email contact-us@concordvoice.com for clarification, or see the full [commercial license program documentation](./docs/legal/commercial-license.md).

---

### How does CVSL 1.0 compare to other licenses?

| License      | Source Available? | Can Self-Host?                                                         | Can Compete?                       | Eventually Open?                    |
| ------------ | ----------------- | ---------------------------------------------------------------------- | ---------------------------------- | ----------------------------------- |
| **CVSL 1.0** | ✅ Yes            | ✅ Personal/non-profit free; commercial license for for-profit/government | ❌ No (until Change Date 2030-02-15) | ✅ Yes (AGPL-3.0-or-later)         |
| MIT          | ✅ Yes            | ✅ Yes                                                                 | ✅ Yes                             | ✅ Already open                     |
| AGPL-3.0     | ✅ Yes            | ✅ Yes                                                                 | ✅ Yes (must share changes)        | ✅ Already open                     |
| BSL 1.1      | ✅ Yes            | ✅ Yes (no commercial-status restriction)                              | ❌ Multi-tenant SaaS resale only   | ✅ Yes (after Change Date)          |
| FSL 1.1      | ✅ Yes            | ✅ Yes (any internal use)                                              | ❌ No (Competing Use prohibition)  | ✅ Yes (typically 2 years; varies by FSL variant) |
| Proprietary  | ❌ No             | ❌ No                                                                  | ❌ No                              | ❌ No                                |
| Open Core    | ✅ Partial        | ⚠️ Partial                                                             | ⚠️ Partial                         | ⚠️ Core only                        |

CVSL 1.0 balances immediate transparency, sustainable development, free use for the right users (personal and non-profit), and eventual full openness.

---

## 🔒 Security & Privacy

### Is Concord Voice secure?

Yes. We implement industry-standard security practices:

**Cryptography:**

- 🔐 **E2EE:** RSA-OAEP 4096-bit + AES-256-GCM encryption
- 🔑 **Password Hashing:** Argon2id (OWASP recommended)
- 🎫 **Tokens:** JWT access tokens (15min) + HttpOnly refresh tokens (30d)
- 🔐 **Key Derivation:** PBKDF2 600k iterations
- 🔒 **Transport:** TLS 1.3

**Infrastructure:**

- 🚫 **Rate Limiting:** Redis-based, per-IP and per-user
- 🛡️ **Pre-commit Hooks:** Prevents committing secrets
- **Pre-commit Scanning:** 22 hooks via Python pre-commit framework (TruffleHog, detect-secrets, Gitleaks, Semgrep, golangci-lint, ESLint, etc.)
- **CI/CD:** GitHub Actions (build.yml) with parallel test + coverage + SonarQube Quality Gate (mandatory)
- 🗄️ **Database:** PostgreSQL with row-level security
- 🔐 **Redis:** Authentication enabled
- 🐳 **Docker:** Container isolation

---

### What is End-to-End Encryption (E2EE)?

E2EE means your messages are encrypted on your device before being sent. Only you and the recipient can read them—not even Concord Voice can decrypt them.

**How it works:**

1. Your device generates a unique key pair (public + private keys)
2. Your private key is encrypted with your password and never leaves your device unencrypted
3. Messages are encrypted with the recipient's public key
4. Only the recipient's private key can decrypt the message

**Important:** If you forget your password, we **cannot** reset it or recover your encrypted data.

---

### Can Concord Voice read my messages?

**For E2EE messages:** No. Messages are encrypted on your device with the recipient's public key. Concord Voice servers only see encrypted ciphertext.

**For non-E2EE messages:** Yes, technically. If you join a non-E2EE server (self-hosted or opted-out), messages are stored in plaintext and server admins can access them.

**Recommendation:** Always use E2EE for private conversations.

---

### How do I report a security vulnerability?

**DO NOT** create a public GitHub issue.

**Preferred methods:**

1. **GitHub Security Advisories (Recommended)**
   - Go to [Security tab](https://github.com/Concord-Voice/Concord-Voice-Alpha/security/advisories)
   - Click "Report a vulnerability"
   - Fill out the form

2. **Email:** security@concordvoice.com
   - Use PGP encryption if possible (key available on request)
   - Include "SECURITY" in subject line

**Response timeline:**

- 24 hours: Initial acknowledgment
- 72 hours: Preliminary assessment
- 7 days: Detailed response and timeline
- 90 days: Public disclosure (coordinated)

See [.github/SECURITY.md](./.github/SECURITY.md) for full policy.

---

### What data does Concord Voice collect?

**Hosted SaaS:**

- ✅ Email address (for account recovery)
- ✅ Username
- ✅ Encrypted password (Argon2id hash)
- ✅ E2EE public key
- ✅ Session metadata (IP, user agent, last used)
- ❌ **Not collected:** Biometric data, location, message content (E2EE), call recordings

**Self-Hosted:**

- You control all data
- Concord Voice receives no data from self-hosted instances (except license validation pings)

**Telemetry:**

- No telemetry or crash reporting is collected

---

### Is multi-factor authentication (MFA) supported?

**Status:** ✅ Implemented (Phase 2A)

**Supported methods:**

- TOTP (Google Authenticator, Authy)
- WebAuthn/FIDO2 (hardware security keys like YubiKey, passkeys)
- Backup codes
- Recovery circles
- Trusted devices

MFA is available now. Enable it in your account security settings.

---

### Can I sign in with Google or Apple?

**Status:** ✅ Implemented — both providers shipped (Google via #808, Apple via #824).

The desktop app's Login screen renders both `Sign in with Google` and `Sign in with Apple` buttons (`client/desktop/src/renderer/components/Auth/Login.tsx`, `SSOButton.tsx`), wired through the loopback OAuth flow in `client/desktop/src/main/ssoLoopback.ts`. Apple's privacy-relay email addresses are detected server-side and excluded from auto-link to avoid attaching an existing account to an opaque relay alias.

---

### What about vendor lock-in?

Concord Voice is designed to **prevent vendor lock-in**:

- ✅ Full source code available (audit anytime)
- ✅ Self-host option (data portability)
- ✅ Standard protocols (WebRTC, PostgreSQL, Redis)
- ✅ Export your data anytime (feature planned)
- ✅ Becomes fully open source after 4 years

If we shut down or change direction, you can always self-host.

---

## 👨‍💻 Development & Contributing

### How do I set up the development environment?

**Quick start:**

```bash
# 1. Clone the repository
git clone https://github.com/Concord-Voice/Concord-Voice-Alpha.git
cd Concord-Voice-Alpha

# 2. Install git hooks (prevents committing secrets)
./scripts/install-git-hooks.sh

# 3. Start everything (Docker + services + client)
./scripts/concord-dev.sh up

# That's it! The app will open in Electron
```

See [docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md) for detailed instructions.

---

### What are the prerequisites for development?

**Required:**

- **Node.js** 20+
- **Go** 1.26.2+
- **Docker** & **Docker Compose**
- **Git**

**Optional:**

- **PostgreSQL** client (psql) for database inspection
- **Redis** CLI for cache debugging

---

### What's the project structure?

```
Concord/
├── client/desktop/          # Electron desktop application
├── services/
│   ├── control-plane/       # Go backend (auth, channels, billing)
│   ├── media-plane/         # Node.js WebRTC SFU (voice/video)
│   └── licensing-authority/ # Go licensing service (planned)
├── docs/                    # Documentation
├── scripts/                 # Development scripts
└── infrastructure/docker/   # Docker Compose configs
```

See [README.md](./README.md) for full breakdown.

---

### What coding standards should I follow?

**Go (Backend):**

- Use `gofmt` and `golangci-lint`
- Follow [Effective Go](https://go.dev/doc/effective_go)
- Handle all errors explicitly
- Add comments for exported functions
- Keep functions small and focused

**TypeScript (Frontend):**

- Use ESLint and Prettier
- Follow Airbnb style guide
- Use TypeScript strict mode
- Prefer interfaces over types
- Avoid `any` type
- Use functional components with hooks

**Commit Messages:**

```bash
<type>: <description>

Types: feat, fix, refactor, docs, test, chore, perf, style

Examples:
git commit -m "feat: add username validation"
git commit -m "fix: resolve token expiration bug"
```

See [.github/CONTRIBUTING.md](./.github/CONTRIBUTING.md) for full guidelines.

---

### How do I run tests?

**Backend tests:**

```bash
cd services/control-plane
go test -v ./...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

**Frontend tests:**

```bash
cd client/desktop
npm test
```

> SonarQube enforces ≥ 80% coverage on new code as a mandatory Quality Gate; see [[internal]](.[internal]) for current test-file counts.

---

### How do I create a Pull Request?

1. **Create a branch:**

   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make changes:**
   - Write code
   - Add tests
   - Update documentation

3. **Test locally:**

   ```bash
   ./scripts/concord-dev.sh down
   ./scripts/concord-dev.sh rebuild
   ```

4. **Commit:**

   ```bash
   git add .
   git commit -m "feat: add my feature"
   ```

5. **Push and create PR:**

   ```bash
   git push origin feature/my-feature
   # Then open a Pull Request on GitHub
   ```

6. **PR review:**
   - Respond to all comments
   - Make requested changes
   - Wait for approval
   - Squash and merge

---

### What should I NOT commit?

**Never commit:**

- ❌ Passwords or API keys
- ❌ Private keys (.pem, .key)
- ❌ .env files with secrets
- ❌ Database credentials
- ❌ JWT secrets
- ❌ Large binary files (>10MB)

**Git hooks will catch these**, but be careful.

**Always use:**

- ✅ Environment variables
- ✅ .env.example templates
- ✅ GitHub Secrets for CI/CD

---

## 🚀 Deployment & Self-Hosting

### How do I self-host Concord Voice?

**Status:** Self-hosting installer is planned for Phase 3 (issue #210); narrative deployment guide tracked at #819.

**Current workaround** (for developers):

```bash
# Use Docker Compose for local deployment (from project root)
docker-compose up -d

# Or use the dev script for the full stack
./scripts/concord-dev.sh up

# Access at http://localhost:8080
```

Full self-hosting guide with single-command setup coming soon.

---

### What are the system requirements for self-hosting?

**Minimum (for testing):**

- 2 CPU cores
- 4 GB RAM
- 20 GB disk space
- Docker + Docker Compose

**Recommended (for production):**

- 4 CPU cores
- 8 GB RAM
- 100 GB SSD
- PostgreSQL 16
- Redis 7
- Reverse proxy with TLS (nginx, Caddy)

**For voice (Media Plane):**

- Add 2 CPU cores per 25 concurrent voice users
- Add 1 GB RAM per 25 concurrent voice users

---

### Can I run Concord Voice on Raspberry Pi?

**Maybe** - depends on usage:

- ✅ **Raspberry Pi 4 (4GB+)**: Should work for small groups (<10 users)
- ⚠️ **Raspberry Pi 3**: Marginal, text-only might work
- ❌ **Voice chat**: Not recommended on Pi due to CPU requirements

We haven't officially tested Pi deployment, but community reports welcome!

---

### What about Kubernetes/cloud deployment?

Kubernetes manifests, Helm charts, and cloud provider templates are not currently shipped. Production deployment uses a single VM via systemd timer + Docker Compose (see `[internal]`).

For now, Docker Compose is the supported method.

---

## 🎯 Features & Usage

### What platforms are supported?

**Current:**

- ✅ **Desktop:** Electron app (Windows, macOS, Linux)
- 📋 **Mobile:** Planned (Phase 3, issue #204)

---

### Can I use Concord on mobile?

**Not yet.** Mobile apps (iOS + Android) are planned for Phase 3 (issue #204).

The desktop Electron app works on laptops/tablets, but no native mobile UI yet.

---

### How is Concord different from mainstream chat platforms?

| Feature            | Mainstream platforms | Concord                             |
| ------------------ | -------------------- | ----------------------------------- |
| **E2EE**           | ❌                   | ✅ Yes                              |
| **Self-Hosting**   | ❌                   | ✅ Yes (Phase 3)                    |
| **Source Code**    | ❌ Proprietary       | ✅ CVSL 1.0 (→ AGPL-3.0)            |
| **Privacy**        | ⚠️ Collects data     | ✅ No telemetry                     |
| **Business Model** | Subscription upsells | À-la-carte + self-host              |
| **Voice Quality**  | Good (Opus)         | ✅ Hi-Fi (256kbps+, 7 quality tiers) |
| **Bots**           | ✅ Yes              | 📋 Planned (post-GA)                 |

**Concord Voice's focus:** Privacy, sovereignty, eventual open source.

---

### Does Concord support screen sharing?

**Status:** ✅ Implemented (Phase 1C, PR #114).

Screen sharing uses WebRTC via the mediasoup SFU. Features include:

- Full-screen and window/tab selection via ScreenSharePicker
- AV1 codec for screen share (high quality at low bitrate)
- IGNIS decoder budget system for managing stream limits
- Opt-in screen share viewing to conserve bandwidth

---

### Can I upload files?

**Status:** Shipped in the v0.2.0-Beta chat-enhancements series (#168 series), with file & image attachments delivered in [#470](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/470).

Supported uploads:

- Image uploads (with two-tier media access via the MinIO object-storage layer, [#325](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/325))
- General file attachments via the chat composer
- Profile and server image assets (avatars, banners, server icons)

---

### Is there a file size limit?

Yes — configurable per-deployment via the `UPLOAD_MAX_SIZE` env var on the control-plane (defaults to 25 MB for general attachments). Profile and server image assets have separate caps (avatars and server icons 5 MB; banners 10 MB) defined in `services/control-plane/internal/media/handlers.go`.

---

### Can I create bots?

**Status:** Planned for post-GA (no issue tracker entry yet).

Anticipated scope:

- Bot user type
- Public API with webhooks
- Bot permission system
- Bot directory/marketplace

For now, focus is on core chat and voice features.

---

## 🛠️ Troubleshooting

### The dev environment won't start

**Check:**

1. Docker is running: `docker ps`
2. Ports are free: `lsof -ti:8080` (should be empty)
3. Logs: `tail -f logs/control-plane.log`

**Fix:**

```bash
# Stop everything cleanly
./scripts/concord-dev.sh down

# Start fresh
./scripts/concord-dev.sh rebuild
```

---

### I'm getting "port already in use" errors

**Find and kill the process:**

```bash
# For port 8080 (control-plane)
lsof -ti:8080 | xargs kill -9

# For port 5173 (Vite dev server)
lsof -ti:5173 | xargs kill -9

# Then restart
./scripts/concord-dev.sh up
```

---

### Database migrations aren't running

The migration system uses `golang-migrate/v4` and runs automatically on server startup. See [[internal]](.[internal]) § Key Counts for the current migration count.

**Check migration status:**

```bash
cd services/control-plane
make migrate-version
```

**Run migrations manually:**

```bash
cd services/control-plane
make migrate-up
```

**Rollback:**

```bash
cd services/control-plane
make migrate-down
```

**If migrations fail**, ensure PostgreSQL is running and accessible:

```bash
docker exec -it concord-postgres psql -U concord -d concord -c "SELECT version FROM schema_migrations;"
```

---

### WebSocket connection fails

**Check:**

1. Control Plane is running: `curl http://localhost:8080/health`
2. JWT token is valid (not expired)
3. Browser console for errors

WebSocket support is fully implemented with ticket-based authentication. Ensure you obtain a ticket via `POST /api/v1/auth/ws-ticket` before connecting.

---

### Tests are failing

**For Go tests:**

```bash
cd services/control-plane

# Run with verbose output
go test -v ./...

# Check for race conditions
go test -race ./...
```

**For frontend tests:**

```bash
cd client/desktop

# Clear cache and retry
rm -rf node_modules package-lock.json
npm install
npm test
```

---

### How do I reset my local database?

**WARNING:** This deletes all local data.

```bash
# Stop services
./scripts/concord-dev.sh down

# Remove Docker volumes (from project root)
docker-compose down -v

# Restart
./scripts/concord-dev.sh up
```

---

### The Electron app won't open

**Check:**

1. Vite dev server is running: `curl http://localhost:5173`
2. Node modules installed: `cd client/desktop && npm install`
3. Console output: Look for errors in terminal

**Rebuild:**

```bash
cd client/desktop
npm run build
npm run dev
```

---

### Where can I get help?

**Resources:**

- **Documentation:** [docs/](./docs/)
- **GitHub Discussions:** [Ask questions](https://github.com/Concord-Voice/Concord-Voice-Alpha/discussions)
- **GitHub Issues:** [Report bugs](https://github.com/Concord-Voice/Concord-Voice-Alpha/issues)
- **Security Issues:** security@concordvoice.com
- **Data Privacy:** privacy@concordvoice.com
- **General Inquiries:** contact-us@concordvoice.com
- **Development Team:** dev@concordvoice.com

---

## 📚 Additional Resources

- **README:** [README.md](./README.md) - Project overview
- **TODO:** [[internal]](.[internal]) - Current priorities
- **ROADMAP:** [ROADMAP.md](./ROADMAP.md) - Long-term plan
- **Contributing:** [.github/CONTRIBUTING.md](./.github/CONTRIBUTING.md) - Development guidelines
- **Security:** [.github/SECURITY.md](./.github/SECURITY.md) - Security policy
- **Architecture:** [docs/architecture.md](./docs/architecture.md) - System design
- **Getting Started:** [docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md) - Setup guide
- **AI Context:** [[internal]](.[internal]) - For AI assistants

---

## 💬 Still Have Questions?

If your question isn't answered here:

1. **Search GitHub Issues:** Your question may have been asked before
2. **Check Documentation:** Browse the [docs/](./docs/) folder
3. **Ask on Discussions:** Start a conversation on GitHub Discussions
4. **Email Us:**
   - General: contact-us@concordvoice.com
   - Security: security@concordvoice.com
   - Privacy: privacy@concordvoice.com
   - Development: dev@concordvoice.com

---

**Last Updated:** 2026-05-01
