# Contributing to Concord Voice

Thank you for your interest in contributing to Concord Voice! This document provides guidelines and instructions for contributing.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [Pull Request Process](#pull-request-process)
- [Coding Standards](#coding-standards)
- [Testing](#testing)
- [Quality Gates](#quality-gates)

## Code of Conduct

This project adheres to the [Contributor Covenant](CODE_OF_CONDUCT.md) Code of Conduct. By participating, you are expected to uphold it — please report unacceptable behavior to security@concordvoice.com.

## Getting Started

1. **Fork the repository** on GitHub
2. **Clone your fork** locally
3. **Add upstream remote**:
   ```bash
   git remote add upstream https://github.com/Concord-Voice/Concord-Voice-Alpha.git
   ```
4. **Create a feature branch** from `main`:
   ```bash
   git checkout -b feature/my-feature
   ```

## Development Setup

See [GETTING_STARTED.md](../docs/GETTING_STARTED.md) for detailed setup instructions.

### Prerequisites

- Node.js 24+
- Go 1.26.2+
- Python 3
- pre-commit (`pip install pre-commit` or `brew install pre-commit`)
- Docker Desktop
- PostgreSQL 16
- Redis 7

### Quick Start

```bash
# MANDATORY: Install git hooks (uses Python pre-commit framework with 22 hooks)
pip install pre-commit          # if not already installed
./scripts/install-git-hooks.sh

# Start everything (Docker + services + client)
./scripts/concord-dev.sh up
```

> **Note:** `install-git-hooks.sh` is **mandatory** for all contributors. It installs the Python `pre-commit` framework which runs 22 hooks on every commit (security scanning, linting, formatting, type checking, and commit message validation). Commits will be rejected without these hooks installed.

**Migration note for existing contributors:** Before the pre-commit framework was fully activated, a legacy bash hook was running at `.git/hooks/pre-commit`. If you cloned before this migration, your local checkout may still have the legacy hook installed. Run this once to reinstall:

```bash
rm .git/hooks/pre-commit .git/hooks/commit-msg 2>/dev/null
./scripts/install-git-hooks.sh
```

If you have python3.9 missing (required for the Semgrep hook due to protobuf incompatibility with Python 3.14), install it with `brew install python@3.9` on macOS or from your distribution's package manager on Linux.

## Making Changes

### Branch Naming

- `feature/` - New features
- `fix/` - Bug fixes
- `docs/` - Documentation changes
- `chore/` - Maintenance tasks
- `test/` - Test additions or changes

### Commit Messages

Follow **Conventional Commits** format:

```
type: subject

body

footer
```

> **Note:** The `commit-msg` pre-commit hook rejects parenthesized scopes (`type(scope): subject`). Use the bare `type:` form only.

**Types:**

- `feat` - New feature
- `fix` - Bug fix
- `docs` - Documentation
- `style` - Formatting, missing semicolons, etc.
- `refactor` - Code restructuring
- `test` - Adding tests
- `chore` - Maintenance

**Examples:**

```
feat: implement chat message editing
fix: resolve auth token refresh bug
docs: update WebSocket API documentation
```

### Co-Authoring with AI

When using AI assistance (Claude, Copilot, etc.), add co-author attribution:

```
feat: implement new feature

Implementation details here.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
```

## Pull Request Process

1. **Update your branch** with latest changes:

   ```bash
   git fetch upstream
   git rebase upstream/main
   ```

2. **Run tests** locally:

   ```bash
   # Backend tests
   cd services/control-plane
   go test ./...

   # Frontend tests
   cd client/desktop
   npm test
   ```

3. **Lint your code**:

   ```bash
   # Backend (Go)
   cd services/control-plane
   golangci-lint run --timeout=5m

   # Frontend (TypeScript/React)
   cd client/desktop
   npm run lint
   ```

4. **Build successfully**:

   ```bash
   # Backend
   go build ./cmd/server

   # Frontend
   npm run build
   ```

5. **Create Pull Request**:
   - Use descriptive title
   - Fill out PR template completely
   - Link related issues
   - Add screenshots if UI changes
   - Request review from maintainers

6. **Address Review Feedback**:
   - Make requested changes
   - Push updates to your branch
   - Re-request review when ready

## Coding Standards

### Go (Backend)

- Follow [Effective Go](https://golang.org/doc/effective_go.html)
- Use `gofmt` for formatting
- Write godoc comments for exported functions
- Maximum line length: 120 characters
- Use meaningful variable names
- Handle errors explicitly

#### Go Linting

We use `golangci-lint` with comprehensive checks for code quality and security. Configuration is in `services/control-plane/.golangci.yml`.

**Enabled Linters:**

- `errcheck` - Check for unchecked errors (CRITICAL)
- `gosec` - Security checker (REQUIRED)
- `staticcheck` - Advanced Go static analysis
- `govet` - Official Go static analyzer
- `revive` - Code quality and style
- `ineffassign`, `misspell`, `unconvert`, `nakedret`, `prealloc` - Additional quality checks

**Installation:**

```bash
# macOS
brew install golangci-lint

# Linux
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin
```

**Running Locally:**

```bash
cd services/control-plane
golangci-lint run --timeout=5m
```

**Pre-commit Hook:**
Go linting runs automatically on staged Go files before each commit. If linting fails, the commit will be blocked. Fix the issue — never bypass with `--no-verify`.

**CI/CD:**
All PRs must pass golangci-lint checks before merging. CI runs via `build.yml` on every PR, and pre-commit hooks run linting automatically on staged files locally.

### TypeScript/React (Frontend)

- Follow [Airbnb Style Guide](https://github.com/airbnb/javascript)
- Use TypeScript for all new code
- Prefer functional components with hooks
- Use explicit types (no `any`)
- Maximum line length: 100 characters
- Name components using PascalCase
- Name hooks using camelCase with `use` prefix

### CSS

- Use CSS custom properties (variables)
- Follow BEM naming convention for complex components
- Maintain existing Concord Voice design system
- Keep styles scoped to components

## Testing

### Backend Tests

```bash
cd services/control-plane

# Unit tests
go test ./internal/...

# Integration tests
go test ./tests/integration/...

# With coverage
go test -cover ./...
```

### Frontend Tests

```bash
cd client/desktop

# Unit tests
npm test

# E2E tests
npm run test:e2e

# With coverage
npm test -- --coverage
```

### Writing Tests

- Write tests for new features
- Update tests for bug fixes
- Maintain >80% code coverage
- Include both positive and negative test cases
- Use descriptive test names
- Coverage provider: Istanbul (not v8) — avoids OOM/hangs with large files

## Quality Gates

All contributions must pass the following quality gates before merge.

### Pre-commit Hooks (Local, Mandatory)

22 hooks are configured in `.pre-commit-config.yaml` via the Python `pre-commit` framework. They run automatically on every commit.

| Category   | Hooks                                                                                                                                                 |
| ---------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| Security   | detect-secrets, trufflehog, gitleaks, detect-private-key, detect-aws-credentials                                                                      |
| SAST       | semgrep (`.semgrep/` custom rules)                                                                                                                    |
| General    | check-added-large-files, check-case-conflict, check-merge-conflict, check-yaml, check-json, end-of-file-fixer, trailing-whitespace, mixed-line-ending |
| Go         | golangci-lint, go vet, gofmt, go build                                                                                                                |
| TypeScript | prettier, eslint, tsc --noEmit                                                                                                                        |
| Commit     | commit-msg-format (conventional commits)                                                                                                              |

### CI: GitHub Actions (Mandatory)

`build.yml` runs on every push to `main` and on **all PRs**. It executes parallel jobs:

```text
changes --> desktop (test + coverage + build)     --> sonarqube (scan)
        --> control-plane (test + coverage + DB)   -->
        --> media-plane (lint + typecheck)          -->
```

The SonarQube job downloads coverage artifacts from the parallel jobs and runs the scan.

### SonarQube Quality Gate (Mandatory)

SonarQube Quality Gate is a **mandatory CI check on all PRs**. Requirements:

- New code must meet **>= 80% test coverage**
- No new bugs, vulnerabilities, or security hotspots
- No excessive code duplication

If a pre-commit hook or CI check fails, fix the issue -- never bypass with `--no-verify`.

## Security

- Never commit secrets or credentials
- Use `.env.example` templates
- Follow OWASP security guidelines
- Report security issues privately to security@concordvoice.com
- See [SECURITY.md](SECURITY.md) for details

## Documentation

Update documentation when:

- Adding new features
- Changing API endpoints
- Modifying configuration
- Updating dependencies

Documentation locations:

- API docs: `docs/api/`
- Architecture: `docs/architecture.md`
- User guides: `docs/`

## License

By contributing, you agree that your contributions will be licensed under the Concord Voice Source License 1.0 (CVSL 1.0), which converts to AGPL-3.0-or-later on 2030-02-15.

See [LICENSE](../LICENSE) for full details.

## Questions?

- Check [FAQ.md](../FAQ.md) first
- Open a discussion on GitHub
- Development questions: dev@concordvoice.com
- General inquiries: contact-us@concordvoice.com

## Recognition

Contributors will be recognized in:

- GitHub contributors list
- Release notes
- [internal] development log (for significant contributions)

Thank you for contributing to Concord Voice!
