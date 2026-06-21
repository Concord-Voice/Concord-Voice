# Concord Voice

<a href="https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha"><img align="right" src="https://sonarcloud.io/api/project_badges/quality_gate?project=Concord-Voice-Alpha&token=170bdb3864bdffb9834f9341a37e3a708e9d7287" alt="Quality gate" /></a>

<!-- Code quality (SonarQube) badges use a read-only image-rendering badge token (SonarCloud project_badges tokens grant image rendering only, not API/data access), so they render for anyone and are safe to publish. CI status badges point at the public Concord-Voice repo's curated CI (#1666). -->
[![Lines of Code](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=ncloc&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=coverage&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![Security Rating](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=security_rating&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![Reliability Rating](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=reliability_rating&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha) 
[![CI (Go)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-go.yml/badge.svg)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-go.yml)
[![CI (Frontend)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-frontend.yml/badge.svg)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-frontend.yml)

<!-- Security & supply chain — release/last-commit/commit-activity badges were removed in #835. The OpenSSF Scorecard public badge stays unavailable: Scorecard runs in the private canonical repo with publish_results: false (#1428, SARIF → Code Scanning), and securityscorecards.dev would index the public mirror only if Scorecard published there — a separate-repo concern outside this mirror (#1666). -->
[![AI Code Assurance](https://sonarcloud.io/api/project_badges/ai_code_assurance?project=Concord-Voice-Alpha&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![Dependabot](https://img.shields.io/badge/Dependabot-enabled-success?logo=dependabot)](./.github/dependabot.yml)
[![OpenSSF Best Practices](https://img.shields.io/badge/OpenSSF%20Best%20Practices-TODO%3A%20register-lightgrey)](https://bestpractices.coreinfrastructure.org/)
<!-- TODO: After registering at https://bestpractices.coreinfrastructure.org/, replace the line above with the live badge:
     [![OpenSSF Best Practices](https://bestpractices.coreinfrastructure.org/projects/PROJECT_ID/badge)](https://bestpractices.coreinfrastructure.org/projects/PROJECT_ID) -->

<!-- Project info -->
[![License: CVSL 1.0](https://img.shields.io/badge/license-CVSL%201.0-blue.svg)](./LICENSE)
[![Security Policy](https://img.shields.io/badge/security-policy-green.svg)](./.github/SECURITY.md)
[![E2EE](https://img.shields.io/badge/E2EE-AES--256--GCM%20%2F%20RSA--OAEP%204096-success)](./docs/architecture.md)
[![Self-hosted](https://img.shields.io/badge/self--hosted-supported-blue)](./infrastructure/deploy)

A privacy-first, hybrid SaaS + self-hosted real-time communications platform — voice, video, and chat with end-to-end encryption, deployable to the cloud or to your own hardware.

**Website:** [www.concordvoice.com](https://www.concordvoice.com) | **Security:** security@concordvoice.com

---

## Overview

Concord Voice combines a Go control plane, a mediasoup WebRTC SFU, and an Electron desktop client. Messages are end-to-end encrypted client-side (AES-256-GCM with RSA-OAEP 4096-bit key wrapping). Identity is hosted, but accounts can connect to self-hosted servers. The roadmap targets v0.2.0-Beta (Phase 2 in flight) with v1.0.0 to follow.

For the full tech-stack table, key counts, and release roadmap, see **[[internal]](.[internal])** — the single source of truth for project ground truth.

---

## Getting Started

### Prerequisites

- **Node.js** 20+
- **Go** 1.26.2+
- **Python 3** + **pre-commit** (`pip install pre-commit`)
- **Docker** & **Docker Compose**

### Quick Start

```bash
git clone https://github.com/Concord-Voice/Concord-Voice-Alpha.git
cd Concord-Voice-Alpha
pip install pre-commit
./scripts/install-git-hooks.sh
./scripts/concord-dev.sh up
```

For detailed setup, see [docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md).

---

## Repository Layout

```text
client/desktop/        Electron + React + TypeScript desktop client
services/control-plane/  Go backend (auth, channels, messaging, RBAC)
services/media-plane/    Node.js mediasoup WebRTC SFU
infrastructure/        Docker Compose, deployment scripts, K8s (planned)
scripts/               Dev scripts, build scripts, git hooks
docs/                  Architecture, API, policies, runbooks
```

For the canonical directory tree and package list, see [[internal] → Key Directories](.[internal]).

---

## Development

```bash
# Backend tests
cd services/control-plane && go test -race ./...

# Frontend tests
cd client/desktop && npm test
```

Linting, hook setup, and CI details live in [[internal] → CI / Code Quality](.[internal]). Branch and commit conventions live in [CONTRIBUTING.md](./.github/CONTRIBUTING.md).

---

## Project Roadmap

Phases, milestones, and active issues are tracked in **[[internal] → Release Roadmap](.[internal])** and on GitHub:

- **[ROADMAP.md](./ROADMAP.md)** — long-term vision and phase scope
- **[[internal]](.[internal])** — current sprint tasks
- **[GitHub milestones](https://github.com/Concord-Voice/Concord-Voice-Alpha/milestones)** — release tracking

---

## Security

Concord Voice uses end-to-end encryption (AES-256-GCM + RSA-OAEP 4096), Argon2id password hashing, MFA/WebAuthn, RBAC/SBAC permissions, and OS-keychain-backed credential storage via Electron `safeStorage`.

For the full security feature list, threat model, and disclosure process, see [SECURITY.md](./.github/SECURITY.md).

**Report vulnerabilities:** security@concordvoice.com

---

## Contributing

We welcome contributions. Start with:

- [CONTRIBUTING.md](./.github/CONTRIBUTING.md) — development workflow, branch and commit conventions
- [CODE_OF_CONDUCT.md](./.github/CODE_OF_CONDUCT.md) — community standards (Contributor Covenant v2.1)
- [FAQ.md](./FAQ.md) — common development and licensing questions
- [.github/ISSUE_TEMPLATE/](./.github/ISSUE_TEMPLATE/) — bug reports and feature requests

Install git hooks before your first commit: `./scripts/install-git-hooks.sh`.

---

## Team

- **markdrogersjr** — Co-Founder
- **m.prime95** — Co-Founder
- **HeimD0S** — Collaborator

---

## License

Concord Voice Source License 1.0 (CVSL 1.0) — source-available, free for personal and non-profit use; commercial self-hosted use by for-profit or governmental entities requires a [commercial license](./docs/legal/commercial-license.md); converts to AGPL-3.0-or-later on 2030-02-15.

See [LICENSE](./LICENSE) and [FAQ.md → Licensing & Legal](./FAQ.md#-licensing--legal) for details.

---

## Documentation

- **[[internal]](.[internal])** — project ground truth (tech stack, counts, roadmap)
- **[ROADMAP.md](./ROADMAP.md)** — long-term vision and milestones
- **[FAQ.md](./FAQ.md)** — licensing, security, development questions
- **[docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md)** — 15-minute setup guide
- **[docs/architecture.md](./docs/architecture.md)** — system design, ERD, data flows
- **[docs/api/](./docs/api/)** — OpenAPI 3.0 spec
- **[docs/release/desktop-release-checklist.md](./docs/release/desktop-release-checklist.md)** — desktop release engineering checklist (signing, version bump, smoke test)

---

## Contact

- **Website:** [www.concordvoice.com](https://www.concordvoice.com)
- **Security:** security@concordvoice.com
- **Privacy:** privacy@concordvoice.com
- **General:** contact-us@concordvoice.com
