---
name: Quarterly AI Security Review
about: Scheduled quarterly review of AI tools, policies, and security controls
title: '[AI-REVIEW] Q_ 20__ — Quarterly AI Security Review'
labels:
  - security
  - 'theme: code-quality'
  - 'type: chore'
assignees: ''
---

## Review Period

- **Quarter:** Q_ 20__
- **Review Date:** YYYY-MM-DD
- **Reviewer:**

## AI Tool Inventory Review

- [ ] Verify authorized tool list is current (Claude Code, GitHub Copilot, OpenAI Codex)
- [ ] Check for new AI tool versions — update pinned versions if applicable
- [ ] Review any new AI tools requested for authorization
- [ ] Verify Co-Authored-By trailer conventions are being followed

## MCP Server Re-vetting

Reference: `docs/policies/mcp-server-policy.md`

- [ ] Review each approved MCP server for new versions or security advisories
- [ ] Re-assess pending MCP servers for approval/rejection
- [ ] Verify credential scoping — no production secrets in MCP server configs
- [ ] Check for new MCP servers added since last review
- [ ] Remove or flag any MCP servers no longer maintained

## AI Instruction File Review

- [ ] Review `[internal]` security constraints — still accurate and complete?
- [ ] Review `.github/copilot-instructions.md` — synced with [internal]?
- [ ] Review `.github/instructions/codex-instructions.md` — still relevant?
- [ ] Review domain-specific instructions (go-backend, react-frontend, media-plane, database-migrations)
- [ ] Check for documentation drift between instruction files and actual codebase

## CVE Watchlist Update

Reference: `docs/policies/cve-watchlist.md` (planned -- Phase 4, #458)

- [ ] Check for new CVEs in Claude Code
- [ ] Check for new CVEs in GitHub Copilot
- [ ] Check for new CVEs in OpenAI Codex
- [ ] Check for new CVEs in MCP protocol implementations
- [ ] Verify existing mitigations still address known attack patterns

## Security-Sensitive Commit Audit

- [ ] Query AI-assisted commits touching security-sensitive paths since last review:

  ```bash
  git log --since="<last-review-date>" --grep="Co-Authored-By" -- \
    "**/crypto*" "**/auth/**" "**/mfa/**" "**/rbac/**" \
    "**/e2ee*" "**/preload/**" "**/middleware/security*"
  ```

- [ ] Review flagged commits for security concerns
- [ ] Verify all flagged commits had appropriate human review

## Policy Document Review

- [ ] `docs/policies/ai-generated-code-policy.md` — still current?
- [ ] `docs/policies/mcp-server-policy.md` — inventory accurate?
- [ ] `[internal]` — retention requirements met?
- [ ] `docs/policies/agentic-ai-controls.md` — risk tiers still appropriate? (planned -- Phase 4, #458)
- [ ] `CODEOWNERS` — paths still cover all security-sensitive areas?

## Quality Gate Review

- [ ] SonarQube quality gate thresholds still appropriate?
- [ ] Custom Semgrep rules — any false positives to fix or coverage gaps to close?
- [ ] Pre-commit hook configuration — all hooks still relevant and passing?

## Findings & Actions

<!-- Document any findings, decisions, or action items from this review -->

| Finding | Severity | Action Required | Issue # |
|---------|----------|-----------------|---------|
| | | | |

## Next Review

- **Scheduled:** Q_ 20__ (approximately YYYY-MM-DD)
