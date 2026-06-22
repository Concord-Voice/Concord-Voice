# Concord Voice

<a href="https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha"><img align="right" src="https://sonarcloud.io/api/project_badges/quality_gate?project=Concord-Voice-Alpha&token=170bdb3864bdffb9834f9341a37e3a708e9d7287" alt="Quality gate" /></a>

<!-- SonarCloud badges use a read-only image-rendering token (renders for anyone; grants no API/data access). CI badges point at this repo's curated public CI. -->
[![Lines of Code](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=ncloc&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=coverage&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![Security Rating](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=security_rating&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![Reliability Rating](https://sonarcloud.io/api/project_badges/measure?project=Concord-Voice-Alpha&metric=reliability_rating&token=170bdb3864bdffb9834f9341a37e3a708e9d7287)](https://sonarcloud.io/summary/new_code?id=Concord-Voice-Alpha)
[![CI (Go)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-go.yml/badge.svg)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-go.yml)
[![CI (Frontend)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-frontend.yml/badge.svg)](https://github.com/Concord-Voice/Concord-Voice/actions/workflows/public-ci-frontend.yml)

<!-- Project info -->
[![License: CVSL 1.0](https://img.shields.io/badge/license-CVSL%201.0-blue.svg)](./LICENSE)
[![Security Policy](https://img.shields.io/badge/security-policy-green.svg)](./.github/SECURITY.md)
[![E2EE](https://img.shields.io/badge/E2EE-AES--256--GCM%20%2F%20RSA--OAEP%204096-success)](./docs/architecture.md)
[![Self-hosted](https://img.shields.io/badge/self--hosted-supported-blue)](./docs/GETTING_STARTED.md)

A privacy-first, hybrid SaaS + self-hosted real-time communications platform — voice, video, and chat with end-to-end encryption, deployable to the cloud or to your own hardware.

> **This is the public, source-available mirror of Concord Voice.** It is a read-only
> snapshot published from our canonical repository — issues and pull requests are not
> tracked here. Please use the channels under [Contact](#contact) to reach us.

**Website:** [www.concordvoice.com](https://www.concordvoice.com) | **Security:** security@concordvoice.com

---

## Overview

Concord Voice combines a Go control plane, a mediasoup WebRTC SFU, and an Electron desktop client. Messages are end-to-end encrypted client-side (AES-256-GCM with RSA-OAEP 4096-bit key wrapping). Identity is hosted, but accounts can connect to self-hosted servers. The roadmap targets v0.2.0-Beta (Phase 2 in flight) with v1.0.0 to follow.

For the high-level system design, see **[docs/architecture.md](./docs/architecture.md)**.

---

## Getting Started

### Prerequisites

- **Node.js** 24+
- **Go** 1.26+
- **Python 3** + **pre-commit** (`pip install pre-commit`)
- **Docker** & **Docker Compose**

### Quick Start

```bash
git clone https://github.com/Concord-Voice/Concord-Voice.git
cd Concord-Voice
pip install pre-commit
docker compose up
```

For detailed setup, see [docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md) and [docs/development.md](./docs/development.md).

---

## Repository Layout

```text
client/desktop/          Electron + React + TypeScript desktop client
services/control-plane/  Go backend (auth, channels, messaging, RBAC)
services/media-plane/    Node.js mediasoup WebRTC SFU
docs/                    Architecture, API, policies
```

---

## Development

```bash
# Backend tests
cd services/control-plane && go test -race ./...

# Frontend tests
cd client/desktop && npm test
```

Branch and commit conventions live in [CONTRIBUTING.md](./.github/CONTRIBUTING.md).

---

## Roadmap

Long-term vision and phase scope live in **[ROADMAP.md](./ROADMAP.md)**.

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
- [.github/ISSUE_TEMPLATE/](./.github/ISSUE_TEMPLATE/) — bug report and feature request templates

---

## License

Concord Voice Source License 1.0 (CVSL 1.0) — source-available, free for personal and non-profit use; commercial self-hosted use by for-profit or governmental entities requires a [commercial license](./docs/legal/commercial-license.md); converts to AGPL-3.0-or-later on 2030-02-15.

See [LICENSE](./LICENSE) and [FAQ.md](./FAQ.md) for details.

---

## Documentation

- **[ROADMAP.md](./ROADMAP.md)** — long-term vision and milestones
- **[FAQ.md](./FAQ.md)** — licensing, security, development questions
- **[docs/GETTING_STARTED.md](./docs/GETTING_STARTED.md)** — setup guide
- **[docs/architecture.md](./docs/architecture.md)** — system design, data flows
- **[docs/api/](./docs/api/)** — OpenAPI 3.0 spec
- **[docs/privacy-policy.md](./docs/privacy-policy.md)** — privacy policy

---

## Contact

- **Website:** [www.concordvoice.com](https://www.concordvoice.com)
- **Security:** security@concordvoice.com
- **Privacy:** privacy@concordvoice.com
- **General:** contact-us@concordvoice.com
