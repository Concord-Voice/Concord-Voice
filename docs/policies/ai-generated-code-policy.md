# AI-Generated Code Policy

| Field          | Value                          |
|----------------|--------------------------------|
| Effective Date | 2026-04-03                     |
| Version        | 1.0                            |
| Review Cadence | Quarterly                      |
| Owner          | Concord Voice LLC              |
| Parent Issue   | #453 (AI Governance Framework) |
| Tracking Issue | #454                           |

---

## 1. Purpose

AI code generation tools accelerate development but introduce distinct security risks that traditional secure coding practices do not address. Industry data shows AI-generated code carries an 86% XSS vulnerability rate (CWE-80)[^1] and repositories using Copilot leak secrets at a 40% higher rate than the baseline.[^2] This policy establishes governance controls to capture the productivity benefits of AI tooling while mitigating those risks across the Concord Voice codebase.

[^1]: Veracode, *GenAI Code Security Report* (2025). Tested 100+ LLMs across Java, JavaScript, Python, and C#. 86% of AI-generated code failed XSS (CWE-80) security tests; 88% contained log injection (CWE-117). [veracode.com/resources/analyst-reports/2025-genai-code-security-report](https://www.veracode.com/resources/analyst-reports/2025-genai-code-security-report/)
[^2]: GitGuardian, *State of Secrets Sprawl 2025*. In a sample of ~20,000 repositories where Copilot is active, 6.4% leaked at least one secret vs. 4.6% across all public repositories (40% higher incidence). [blog.gitguardian.com/yes-github-copilot-can-leak-secrets](https://blog.gitguardian.com/yes-github-copilot-can-leak-secrets/)

## 2. Scope

This policy applies to **all AI code generation tools** used in Concord Voice development, including CLI agents, IDE extensions, code completion engines, and automated PR reviewers. It covers code authored, modified, refactored, or reviewed with AI assistance that is committed to any Concord repository.

---

## 3. Authorized AI Tools

| Tool | Role | Usage |
|------|------|-------|
| **Claude Code** (primary) | Anthropic Claude Opus 4.6 | CLI agent, IDE extensions |
| **GitHub Copilot** (secondary) | Code completion, PR review | IDE integration |
| **OpenAI Codex** (tertiary) | Code generation tasks | As needed |

All other AI code generation tools require **explicit written approval** from the project owner before use. Approval requests should reference this policy and describe the tool, its data handling practices, and the intended use case.

---

## 4. Data Classification Gates

| Classification | AI Input Permitted | Controls Required |
|----------------|-------------------|-------------------|
| Public code | Yes | Standard development controls |
| Internal code | Yes | Standard controls + code review |
| Confidential (auth, crypto, PII flows) | Yes | Mandatory security review gate |
| Restricted (production secrets, signing keys, customer data) | **Never** | N/A -- prohibited |

**Restricted data must never be provided as input to any AI tool**, regardless of the tool's data retention or privacy policies. This includes production database credentials, signing keys, API secrets, and customer data.

---

## 5. Prohibited Actions

AI tools must **not** be used to:

1. Generate or modify production secrets, API keys, or credentials.
2. Implement cryptographic algorithms (key generation, encryption, decryption) without expert review.
3. Modify authentication or authorization logic without mandatory security review.
4. Generate code handling PII, financial data, or compliance-regulated content without review.
5. Access production or staging environments.
6. Bypass pre-commit hooks or CI quality gates (e.g., `--no-verify`, skipping linters).

---

## 6. Ownership and Traceability

### 6.1 Attribution

All AI-assisted commits must include a `Co-Authored-By` trailer identifying the AI tool used:

```
Co-Authored-By: Claude Opus 4.6 (1M context) <noreply@anthropic.com>
Co-Authored-By: GitHub Copilot <noreply@github.com>
Co-Authored-By: OpenAI Codex <noreply@openai.com>
```

### 6.2 Accountability

The developer who prompted, reviewed, and committed AI-generated code **owns** that code. The developer is fully accountable for its correctness, security, and compliance with project standards. AI tools do not reduce the developer's review obligations.

### 6.3 Traceability Query

To audit AI-assisted commits across the repository:

```bash
git log --grep="Co-Authored-By.*Claude\|Co-Authored-By.*Copilot\|Co-Authored-By.*Codex"
```

---

## 7. Mandatory Review Gates

### 7.1 Security-Sensitive Paths

The following paths require team review on all PRs, whether AI-assisted or not. When AI-generated code touches these paths, the review gate is mandatory:

```
crypto/*        auth/*          mfa/*
rbac/*          middleware/security*
preload/*       ipc*            tokenManager*
password*       e2ee*
```

These paths are defined in `CODEOWNERS` and enforced via GitHub branch protection rules.

### 7.2 Scope Creep Rule

If AI-generated code introduces **new usage** of crypto, secrets, or auth APIs that was not in the original task scope, the developer must flag it for mandatory security review before merging, even if the changed files are outside the paths listed above.

### 7.3 Activation

Review gate enforcement via CODEOWNERS is deferred to post-org-migration (#448). The CODEOWNERS framework is in place and the paths above are documented; enforcement will activate once the org migration completes.

---

## 8. Standards References

This policy is informed by the following frameworks and guidance:

- [OWASP Top 10 for LLMs (2025)](https://owasp.org/www-project-top-10-for-large-language-model-applications/)
- [OWASP Top 10 for Agentic AI (Dec 2025)](https://owasp.org/www-project-agentic-ai-threats/)
- [NIST AI Risk Management Framework (AI RMF)](https://www.nist.gov/artificial-intelligence/ai-risk-management-framework)
- [NIST Cyber AI Profile IR 8596 (Dec 2025 draft)](https://csrc.nist.gov/publications/detail/ir/8596/draft)
- [OpenSSF Security-Focused Guide for AI Code Assistant Instructions](https://openssf.org/blog/2025/04/17/introducing-the-security-focused-guide-for-ai-code-assistant-instructions/)
- [CSA AI Controls Matrix (AICM, July 2025)](https://cloudsecurityalliance.org/research/artifacts/ai-controls-matrix)
- [NSA/CISA AI Data Security CSI (May 2025)](https://www.nsa.gov/Press-Room/Cybersecurity-Advisories-Guidance/)

---

## 9. Review Cadence

| What | Frequency | Mechanism |
|------|-----------|-----------|
| This policy | Quarterly | `quarterly-ai-review` issue template |
| AI instruction files (`[internal]`, `copilot-instructions.md`) | Quarterly | Same review cycle |
| MCP server inventory | Quarterly | Re-vet per `mcp-server-policy.md` |

Reviews should confirm that authorized tools, data classification gates, and prohibited actions remain current and aligned with the project's threat model.

---

## 10. Enforcement

- **Technical controls**: Pre-commit hooks and CI quality gates automatically enforce Conventional Commit formatting, linting, test coverage, and secret scanning. Attribution (Co-Authored-By trailers) is a policy requirement verified during code review; automated enforcement is planned for Phase 4 (#458). These controls and review requirements must not be bypassed.
- **Policy violations**: Treated as security findings per `SECURITY.md` severity levels (Critical, High, Medium, Low).
- **Remediation**: Violations, including missing required attribution identified during review, must be documented and remediated before the PR is merged. Repeat violations should trigger a review of developer onboarding and tooling configuration.

---

## 11. Related Documents

| Document | Purpose |
|----------|---------|
| [`docs/policies/mcp-server-policy.md`](mcp-server-policy.md) | MCP server allowlist and vetting procedures |
| [`[internal]`](audit-log-retention.md) | AI session log retention requirements |
| [`SECURITY.md`](../../.github/SECURITY.md) | Vulnerability response, including AI-introduced CVE playbook |
| [`CODEOWNERS`](../../CODEOWNERS) | Security-sensitive path review assignments |
| [`[internal]`](../..[internal]) | AI instruction ground truth for Claude Code |
| [`.github/copilot-instructions.md`](../../.github/copilot-instructions.md) | Copilot review guidance |
