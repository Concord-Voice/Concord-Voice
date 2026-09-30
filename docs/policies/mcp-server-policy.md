# MCP Server Security Policy

**Effective Date:** 2026-04-03
**Version:** 1.2
**Last amended:** 2026-09-30 — retired VS Code tooling
**Review Cadence:** Quarterly (last reviewed: 2026-07-11; next review: 2026-10-11)

---

## 1. Purpose

Model Context Protocol (MCP) servers extend AI-assisted development tools with external capabilities such as API access, database queries, browser automation, and code analysis. Each MCP server is an attack surface: it may access filesystems, hold credentials, make network requests, or execute commands on the host. For a privacy-first encrypted communications application, this requires explicit vetting.

This policy defines the approved MCP server allowlist, the vetting process for adding or re-approving servers, and credential scoping requirements for all MCP integrations used in Concord development.

**Configuration:** Project MCP servers are configured in `.mcp.json` (Claude Code App + CLI) and `.codex/config.toml` (Codex App/CLI). SonarQube uses a separate user-scope CLI integration. Credentials come from environment variables or the CLI's secure credential store, never committed values. VS Code support was retired on 2026-09-30; its workspace MCP configuration and extension recommendations are no longer maintained. See §3 for the supported host inventory.

---

## 2. Current MCP Server Inventory

| Server | Maintainer | Status | Data Access | Network Access | Credentials Required | Risk Level | Notes |
|---|---|---|---|---|---|---|---|
| SonarQube (SonarSource) | SonarSource | Approved | Code quality metrics, issues, hotspots via SonarQube Cloud/Server API | Yes -- SonarQube Cloud (sonarcloud.io) | `sonar auth login` → macOS Keychain, through the user-scope CLI integration | Medium | Claude Code and Codex use `sonarqube-cli` (`sonar run mcp`) to launch the SonarSource image `docker.io/sonarsource/sonarqube-mcp`; see §8. This developer integration is separate from CI scanning and quality-gate enforcement. Source-available (SSAL v1.0). Collects anonymous telemetry (no source code). Repo: [SonarSource/sonarqube-mcp-server](https://github.com/SonarSource/sonarqube-mcp-server). |
| Context7 | Upstash | Approved | Read-only library/framework documentation from Context7 database | Yes -- context7.com API | API key (optional, for rate limits) | Low | Read-only documentation lookups. No code or project data sent. Open source, MIT. Repo: [upstash/context7](https://github.com/upstash/context7). |
| Playwright | Microsoft | Approved | Local browser automation via accessibility trees; filesystem restricted to workspace root by default | Configurable -- can reach any URL the browser navigates to | None | Medium | Used for local testing only. Open source, Apache-2.0. DNS rebinding vulnerability (pre-v0.0.40) patched; ensure version >= 0.0.40. Supports origin allow/blocklists. Repo: [microsoft/playwright-mcp](https://github.com/microsoft/playwright-mcp). |
| GitHub | Model Context Protocol | Approved | Repos, issues, PRs, code search, workflows, file contents | Yes -- api.github.com | GitHub Personal Access Token (PAT) | High | Broad repo access. Use fine-grained PATs with minimum scope (repo read, issues, PRs). Classic PATs auto-hide unauthorized tools. Open source, MIT. Package: `@modelcontextprotocol/server-github`. Repo: [modelcontextprotocol/servers](https://github.com/modelcontextprotocol/servers). Approved 2026-04-04. |
| ~~PostgreSQL MCP~~ | — | **Removed** | — | — | — | — | Removed from inventory 2026-05-02 (#778, late-cycle scope addition). Was unused in practice — never loaded successfully in any verification matrix run, and `psql` CLI covers the dev-DB introspection use case. Re-vet via §4 if a future need arises. |
| Stripe | Stripe, Inc. | Approved | Customers, products, payments, invoices, subscriptions | Yes -- api.stripe.com | Stripe API key (secret or restricted) | High | Financial and payment data. **Use restricted API keys (rk_*) with minimum permissions only.** Must never use production keys — test mode only. Open source, MIT. Repo: [stripe/agent-toolkit](https://github.com/stripe/agent-toolkit). Approved 2026-04-04. |
| ~~Terraform MCP~~ | — | **Removed** | — | — | — | — | Removed from inventory 2026-07-11 after Terraform and HCP Terraform were retired. The repository has no active Terraform manifests or HCP-backed deployment workflow. Re-vet via §4 if an infrastructure-as-code need returns. |
| ~~Sentry MCP~~ | — | **Removed** | — | — | — | — | Removed from inventory 2026-04-28 (#778, coordinating with #760's Sentry strip sweep). The Sentry product integration is being unwound entirely; see project memory `Sentry — being removed`. The original 2026-04-09 approval and Sentry MCP package metadata are preserved in the prior commit history if archaeology is needed. |
| ~~SonarQube (2nd instance)~~ | — | **Removed** | — | — | — | — | Confirmed as duplicate config entry of the approved SonarSource SonarQube MCP. Removed from inventory 2026-04-04. |

---

## 3. Configuration

Project MCP servers are configured in two committed files, one per supported host:

- **`.mcp.json`** — Claude Code App + Claude Code CLI. Credentials come from process env (set via shell `export` for CLI, `launchctl setenv` for App; see §8). **SonarQube is deliberately not in this file** — it is a user-scope `sonar run mcp` server whose credential lives in the macOS Keychain; see §8.
- **`.codex/config.toml`** — Codex App/CLI project MCP. Credentials come from the Codex process environment for servers that declare `env_vars`.

These files use only vetted server implementations from §2. Do not substitute a different maintainer/package for an approved server without re-review.

### Servers in `.mcp.json` (Claude Code App + CLI)

| Server | Approved Package | Version | Credentials |
| ------ | ---------------- | ------- | ----------- |
| Context7 | `@upstash/context7-mcp` (Upstash) | 2.1.6 | None (optional API key for rate limits) |
| Playwright | `@playwright/mcp` (Microsoft) | 0.0.70 | None |
| GitHub | `@modelcontextprotocol/server-github` | 2025.4.8 | `GITHUB_PERSONAL_ACCESS_TOKEN` — use fine-grained PAT with minimum scope (note: in `.mcp.json` this env var is read from `${ADMIN_PAT}` in the shell/launchd env — set `ADMIN_PAT`, not `GITHUB_PERSONAL_ACCESS_TOKEN`, via `export` for CLI or `launchctl setenv ADMIN_PAT ...` for App) |
| Stripe | `@stripe/mcp` (Stripe) | 0.3.3 | `STRIPE_SECRET_KEY` — **restricted test-mode keys only (`rk_test_*`)** |

**SonarQube is intentionally absent from this table.** It is registered at *user* scope
(`~/.claude.json`) by `sonar integrate claude --global`, which runs the same SonarSource
image via `sonar run mcp` and reads its token from the macOS Keychain. Two reasons it must
not also live in `.mcp.json`:

1. **Scope precedence shadows it.** Claude Code resolves MCP servers local → project →
   user, matching on server *name*. A `.mcp.json` entry named `sonarqube` silently wins
   over the user-scope one, with no warning at any layer.
2. **The project entry cannot authenticate in a worktree.** It depends on `SONARQUBE_TOKEN`
   being present in the process env. Sessions run from `[internal]worktrees/*` routinely have
   no such env, so the server binds and then fails `CONNECTION_CLOSED`. The user-scope
   Keychain entry has no env dependency and therefore works in every repo and worktree.

### Servers in `.codex/config.toml` (Codex App/CLI project MCP)

| Server | Approved Package | Version | Credentials |
| ------ | ---------------- | ------- | ----------- |
| Context7 | `@upstash/context7-mcp` (Upstash) | 2.1.6 | None |
| Playwright | `@playwright/mcp` (Microsoft) | 0.0.70 | None |
| GitHub | `@modelcontextprotocol/server-github` | 2025.4.8 | `GITHUB_PERSONAL_ACCESS_TOKEN` — use a fine-grained PAT with minimum scope |
| Stripe | `@stripe/mcp` (Stripe) | 0.3.3 | `STRIPE_SECRET_KEY` — **restricted test-mode keys only (`rk_test_*`)** |

**SonarQube is absent here too, and not by choice.** `scripts/sync-codex-tooling.py`
enforces MCP-*name* parity between `.codex/config.toml` and `.mcp.json` (it compares
`set(claude_mcp) != set(codex_mcp)`, then `command`/`args` for shared names). Dropping
SonarQube from `.mcp.json` therefore requires dropping it here in the same commit, or
the `check-codex-mirrors` pre-commit hook fails. Restore the capability at user scope
with `sonar integrate codex --global`, which uses the same Keychain credential as the
Claude Code path.

### Why separate configs?

Claude Code reads `.mcp.json`; Codex reads `.codex/config.toml`. Their project servers use each host's environment-variable conventions. The sync check keeps server names, commands and arguments aligned. SonarQube remains registered at user scope for both hosts.

---

## 4. Vetting Checklist (for new servers)

Before approving a new MCP server or re-vetting an existing one, complete each item:

- [ ] **Data access:** What data does the server access? (filesystem, network APIs, databases, credentials)
- [ ] **Credential requirements:** What credentials does it require? What is the minimum scope sufficient for its function?
- [ ] **Source availability:** Is the server open source and auditable? Provide a link to the source repository.
- [ ] **Maintainer:** Who maintains it? Is it actively maintained with recent releases and security patches?
- [ ] **Network destinations:** Does it make network requests? To what endpoints? Can destinations be restricted via configuration?
- [ ] **Command execution:** Can it execute arbitrary commands on the host? If so, what sandboxing is in place?
- [ ] **Prompt injection:** Has the server been reviewed for prompt injection vulnerabilities? Can a malicious response from its upstream API influence tool behavior?
- [ ] **Data classification:** Does it comply with the data classification gates defined in `ai-generated-code-policy.md`? Specifically, does it handle or transmit any data classified as Confidential or Restricted?
- [ ] **Telemetry:** Does the server collect telemetry? What data is included? Can it be disabled?
- [ ] **Known vulnerabilities:** Are there open security advisories or CVEs? (Check the server's GitHub security tab and NVD.)
- [ ] **Host classification:** Which host runtime is this server intended for? Options:
      - `.mcp.json` only (Claude Code App + CLI) — server fits Claude Code's npm/Docker process model and consumes credentials from process env
      - `.codex/config.toml` only (Codex App/CLI project MCP) — server fits Codex's npm/Docker process model and consumes credentials from process env
      - **Multiple hosts** — only if the server is genuinely host-agnostic (same package and version usable across those runtimes; credentials map cleanly to each host's idiom) AND its presence on each surface is documented in §3

      Additionally: any host-warranted deviation (different transport, different version, different credential mechanism per host) must be documented in §3 with rationale, regardless of which option above was chosen.

---

## 5. Credential Scoping Requirements

All MCP server credential assignments must follow these rules:

1. **Minimum privilege.** MCP servers must receive the minimum credentials required for their function. Use restricted or scoped tokens rather than full-access keys.
2. **No production secrets.** No MCP server may hold production credentials, API keys, or database connection strings. Development credentials only.
3. **No staging credentials.** Staging environments mirror production data. MCP servers must not connect to staging.
4. **Short-lived tokens.** Use scoped, short-lived tokens where the upstream service supports them (e.g., fine-grained GitHub PATs with expiration, restricted Stripe API keys).
5. **Secure storage.** Credentials must be stored in environment variables or a secure credential store. Never commit credentials to MCP configuration files, `.json` configs, or version control.
6. **Rotation.** Rotate MCP-related credentials on the same schedule as other development credentials, or immediately if a server is removed from the allowlist.

---

## 6. Re-vetting Schedule

- **Quarterly review:** All approved and pending MCP servers are re-vetted every quarter using the checklist in Section 4. Use the `quarterly-ai-review` issue template to track this.
- **Major update trigger:** If an MCP server releases a major version (semver major bump or breaking change), re-vet before updating.
- **Security advisory trigger:** If a CVE or security advisory is published against an approved server, immediately assess impact and suspend use if necessary pending re-vetting.
- **Removal:** Servers that fail re-vetting or are no longer in use must be removed from IDE/CLI configurations and this inventory.

---

## 7. Related Documents

- [ai-generated-code-policy.md](ai-generated-code-policy.md) -- Data classification gates and AI code review requirements
- [audit-log-retention.md](audit-log-retention.md) -- Retention policies for audit trails
- [SECURITY.md](../../.github/SECURITY.md) -- Vulnerability reporting and security practices
- [[internal]sonarqube-false-positive-register.md](../ci/sonarqube-false-positive-register.md) -- Canonical register for SonarQube FP decisions

---

## 8. SonarQube Integration Notes & Credential-Surface Taxonomy

The supported local SonarQube MCP integration uses `sonarqube-cli` (`sonar run mcp`) with the SonarSource-published image `docker.io/sonarsource/sonarqube-mcp`. Claude Code and Codex register it at user scope and use the CLI's macOS Keychain-backed authentication. It exposes issue, coverage and quality-gate tools; it does not replace the CI scanner. [SonarSource's image migration notice](https://community.sonarsource.com/t/sonarqube-mcp-server-1-20-long-lived-branch-support-improved-snippet-analysis-more/183982) identifies this image as the replacement for the retired Docker MCP listing. See §3 for the project inventories.

### User-scope setup (`sonarqube-cli`)

```bash
brew install --cask sonarqube-cli
sonar auth login -o concord-voice
sonar integrate claude --global  # Claude Code
sonar integrate codex --global   # Codex
```

Run the registration command for each host you use. This policy does not attest the version running on another developer's machine; verify the actual image and bundled dependencies when triaging an advisory.

`sonar auth status` verifies the connection. Two caveats learned on 2026-08-19:

- **`--global` still writes a project-local entry.** Despite the flag, `integrate` auto-discovers the
  nearest project and adds a *local*-scope `sonarqube` server to `~/.claude.json` pointing at
  `https://api.sonarcloud.io/mcp` with a **plaintext `Authorization: Bearer` header**. That entry
  outranks both project and user scope. Remove it — `claude mcp remove sonarqube -s local` — so the
  Keychain-backed user-scope entry is the one that binds.
- **`claude mcp get sonarqube` prints that bearer token in cleartext.** SonarCloud tokens can be bare
  40-char hex with no `sq*_` prefix, so prefix-based redaction does not catch them. Redact on length
  or entropy before pasting MCP config anywhere, and rotate any token that has been echoed.

### Credential mechanism per surface

The supported hosts use the following credential mechanisms. User-scope SonarQube registration is separate from the project servers that require environment variables.

| Surface | Credential mechanism | Secret persistence scope | Recommended? |
| ------- | -------------------- | ------------------------ | ------------ |
| **Claude Code CLI** (terminal `claude`) | Shell `export VAR=...` in `~/.zshrc` or `~/.bashrc` | Shell profile (per-user file readable by other terminal processes the user runs) | Default for CLI use; no alternative mechanism in current project tooling |
| **Codex App/CLI** (`.codex/config.toml`) | Codex process env for servers listed with `env_vars` | Same exposure profile as the process that launches Codex | Acceptable; use minimum-scope dev credentials only |
| **Claude Code App** (standalone `.app`) | `launchctl setenv VAR ...` (one-time) or LaunchAgent plist (persistent) | launchd env (process-wide — visible to **every GUI app the user launches** for the session, not just Claude Code) | Acceptable, but the launchd-env exposure is the textbook OWASP A02 misconfiguration — see "Rule of thumb" below |
| **`sonarqube-cli`** (`sonar run mcp`, user-scope MCP — SonarQube only) | `sonar auth login` → macOS Keychain | macOS Keychain (per-user, OS-protected); the CLI supplies credentials at launch | **Preferred for SonarQube** — the only surface with no env-var dependency, so it works identically in every repo and worktree |

**Rule of thumb:** use the CLI's secure credential store for SonarQube. The other project servers still require the host environment; on macOS, `launchctl setenv` supplies those values to Claude Code App.

### One-time `launchctl` setup (for Claude Code App)

Two `.mcp.json` servers still take credentials from the process env — GitHub (`ADMIN_PAT`) and Stripe (`STRIPE_SECRET_KEY`). For Claude Code App on macOS, these must be set in launchd, since GUI apps don't inherit terminal shell env:

```bash
launchctl setenv ADMIN_PAT '<your fine-grained github pat>'
launchctl setenv STRIPE_SECRET_KEY '<your rk_test_... restricted key>'
```

Persistent across reboots: create a LaunchAgent plist at `~/Library/LaunchAgents/com.concord.mcp-env.plist` that runs `launchctl setenv` at login. Shell env (`export ADMIN_PAT=...` in `~/.zshrc`) does NOT work for GUI-launched apps — terminal env doesn't propagate to launchd-managed GUI processes.

**SonarQube no longer needs any of this.** `SONARQUBE_TOKEN` was the original reason this section existed; the CLI keychain path replaced it. A stale `launchctl setenv SONARQUBE_TOKEN` does no harm but has no effect — and note that its absence is exactly why a `.mcp.json` SonarQube entry failed to connect from worktrees.

### SonarCloud state machine: two-step FP workflow

`mcp__sonarqube__change_sonar_issue_status` is a thin wrapper around SonarCloud's `/api/issues/do_transition` endpoint. SonarCloud's issue state machine does NOT allow direct transitions between terminal states. To mark an issue currently in `ACCEPTED` (resolution=ACCEPTED) as `FALSE_POSITIVE`, you MUST make two sequential calls:

1. `change_sonar_issue_status({ key, status: ["reopen"] })` — transitions `RESOLVED` → `REOPENED`
2. `change_sonar_issue_status({ key, status: ["falsepositive"] })` — transitions `REOPENED` → `RESOLVED` with `resolution=FALSE-POSITIVE`

**Never abort between the two calls for the same issue.** If the reopen succeeds but the falsepositive call fails (network, rate limit, auth expiry), the issue will be left in `REOPENED`/`OPEN` state — counts as a new open finding in SonarCloud's new-code accounting and can fail the Quality Gate on the next scan. If a failure occurs between step 1 and step 2, immediately retry step 2; if retries fail, restore the original state with `change_sonar_issue_status({ key, status: ["accept"] })` to move it back to `RESOLVED/ACCEPTED` before handling the incident.

The MCP tool's description does not document this constraint. See issue #431's implementation for the first encounter and resolution pattern.

### AI-Code Assurance policy

Per [[internal]](../..[internal]) "AI CODE GENERATION SECURITY CONSTRAINTS" → "SonarQube AI-Code Assurance", rule-level suppression and tuning are **prohibited** on AI-authored code. All false positives must be handled at the instance level via the two-step workflow above, and each marking must be recorded in `[internal]sonarqube-false-positive-register.md`.
