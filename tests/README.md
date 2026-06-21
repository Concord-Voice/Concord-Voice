# Concord Voice — Testing Overview

This is the project-wide entry point for Concord Voice's test strategy. It explains the testing philosophy, the per-service tooling split, the coverage policy, and how to run tests locally. **Detailed, per-service documentation is linked in [Per-service test docs](#per-service-test-docs)** — this file is the index, not a replacement for those.

> Single source of truth for test conventions: [`[internal]rules/tests.md`](../[internal]rules/tests.md).
> Canonical run commands also live in [`[internal]` → "Test & Lint Commands"](..[internal]).

## Philosophy

- **Test behavior, not implementation details.**
- **Both positive and negative cases** are required — new functions need at least one happy-path and one error-path test.
- **Deterministic** — no reliance on timing, random data, or live external services.
- **No real credentials** in test data — use `test-token-123`-style placeholders.
- **Descriptive names** — e.g. `TestHashCode_SameInput_ReturnsSameHash`.

## Test categories

| Category | What it covers | Where it lives |
|---|---|---|
| **Unit** | Pure logic, single component/function, mocked dependencies | Desktop: `client/desktop/tests/unit/` · Control-plane: `*_test.go` (no DB) · Media-plane: `services/media-plane/tests/` |
| **Integration** | Real datastores and cross-module behavior | Desktop: `client/desktop/tests/integration/` · Control-plane: `*_test.go` against real PostgreSQL + Redis |
| **E2E** | Full user flows through the running app | Desktop: `client/desktop/tests/e2e/` (Playwright) |

Shell/operational scripts have their own lightweight test convention (no framework) under [`scripts/tests/`](../scripts/tests/README.md) and `[internal]tests/`.

## Tooling per service

| Service | Runner & libraries |
|---|---|
| **Desktop** (`client/desktop`) | Vitest · `@testing-library/react` · `@testing-library/user-event` · `@testing-library/jest-dom` · MSW v2 (API mocking) · jsdom · `@vitest/coverage-istanbul` · Playwright (E2E) |
| **Control-plane** (`services/control-plane`) | Go stdlib `testing` · `stretchr/testify` (`assert` / `require`) · shared `internal/testhelpers` · `-race` enabled |
| **Media-plane** (`services/media-plane`) | Vitest (`services/media-plane/tests/*.test.ts`) |
| **Scripts / deploy** | Roll-your-own bash assertions — `PASS:` / `FAIL:` + `exit 1`, no framework |

## Coverage policy

- **Provider: Istanbul**, not v8 — the v8 provider has caused OOM in this suite; Istanbul is the project standard (`@vitest/coverage-istanbul`).
- **SonarQube Quality Gate enforces ≥ 80% coverage on new code** (mandatory — see [`[internal]quality-gate-definition.md`](../[internal]quality-gate-definition.md)).
- Every new source file needs a corresponding test file; every new function needs at least a happy-path and an error-path test.

## Running tests locally

Canonical commands (from [`[internal]` → "Test & Lint Commands"](..[internal])):

```bash
# Backend (Go) — control-plane
cd services/control-plane && go test -race ./...
cd services/control-plane && golangci-lint run

# Frontend (Electron/React) — desktop
cd client/desktop && npm test          # vitest run
cd client/desktop && npm run lint
```

Per service, the most common invocations:

- **Desktop** — `npm test` (full Vitest suite — unit + integration), `npm run test:watch`, `npm run test:coverage`, `npm run test:unit` (unit only), `npm run test:integration` (integration only), `npm run test:e2e` (Playwright; renderer-only specs need only the Vite dev server, full-stack specs need the backend running). Full command set: [`client/desktop/tests/README.md`](../client/desktop/tests/README.md).
- **Control-plane** — `go test ./...`, `go test -race ./...`; integration tests require PostgreSQL + Redis (`docker-compose up -d postgres redis`). Full command set + `testhelpers` reference: [`services/control-plane/tests/README.md`](../services/control-plane/tests/README.md).
- **Media-plane** — `cd services/media-plane && npm test`.
- **Whole stack for E2E** — `./scripts/concord-dev.sh up` then run Playwright from `client/desktop`.

## CI/CD

- [`.github/workflows/build.yml`](../.github/workflows/build.yml) runs on every PR, invoked via `workflow_call` from `pr-ci.yml`: desktop, control-plane, and media-plane test suites run in parallel with coverage, then results upload to SonarQube for Quality Gate enforcement.
- The Playwright E2E specs run **manually** via `npm run test:e2e` in `client/desktop` — the CI workflow was removed in #1435 (deferred-to-manual posture; visual-regression diffs were non-blocking advisory noise).

See the desktop README's [CI/CD section](../client/desktop/tests/README.md#cicd) for the full E2E job semantics.

## Per-service test docs

- [`client/desktop/tests/README.md`](../client/desktop/tests/README.md) — desktop unit/component/hook/service + Playwright E2E, structure, mocking patterns, known jsdom limitations.
- [`services/control-plane/tests/README.md`](../services/control-plane/tests/README.md) — Go test layout across packages, `internal/testhelpers`, integration prerequisites and env vars.
- [`scripts/tests/README.md`](../scripts/tests/README.md) — bash assertion convention for `scripts/concord-dev.sh` and deploy scripts.

> **Gap:** `services/media-plane/` has tests (`services/media-plane/tests/`) but no per-service `README.md` yet. Authoring it is tracked separately (out of scope for this overview).
