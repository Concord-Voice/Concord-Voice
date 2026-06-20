# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.x.x   | :white_check_mark: |

<!-- audit-exempt: historical reference (v0.1.0-Alpha is a shipped versioned identifier) -->

**Note:** Concord Voice is currently in active development. Phase 1 is live (v0.1.0-Alpha); Phase 2A is near-complete and Phase 2B is in flight, with active development toward v0.2.0-Beta. Security updates are provided for the latest development version.

## Reporting a Vulnerability

**DO NOT** open public GitHub issues for security vulnerabilities.

### How to Report

Send security reports to: **security@concordvoice.com**

Include:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)
- Your contact information

### What to Expect

1. **Acknowledgment**: Within 48 hours
2. **Initial Assessment**: Within 7 days
3. **Status Updates**: Every 7 days until resolved
4. **Resolution**: Depends on severity

### Severity Levels

#### Critical (24-48 hour response)

- Remote code execution
- Authentication bypass
- Data exposure of all users
- Privilege escalation to admin

#### High (3-7 day response)

- SQL injection
- XSS vulnerabilities
- CSRF vulnerabilities
- Unauthorized data access

#### Medium (7-14 day response)

- Denial of service
- Information disclosure
- Session management issues

#### Low (14-30 day response)

- Missing security headers
- Weak cryptography
- Minor information leaks

## Security Best Practices

### For Users

1. **Keep Updated**: Always use the latest version
2. **Strong Passwords**: Minimum 12 characters, mixed case, numbers, symbols
3. **End-to-End Encryption**: All conversations are end-to-end encrypted by default — there is no setting to disable it (see [ADR-0003](../[internal]0003-e2ee-always.md))
4. **Verify Identity**: Verify server identity when connecting to self-hosted instances
5. **Secure Your Keys**: Back up encryption keys securely

### For Developers

1. **Environment Variables**: Never commit `.env` files
2. **Dependencies**: Keep dependencies updated
3. **Secret Detection**: Pre-commit hooks prevent secret commits
4. **Code Review**: All code requires review before merge
5. **Input Validation**: Validate all user input
6. **Parameterized Queries**: Use parameterized queries to prevent SQL injection
7. **HTTPS Only**: All production traffic must use HTTPS/TLS

### For Self-Hosters

1. **TLS Certificates**: Use valid TLS certificates (Let's Encrypt recommended)
2. **Firewall**: Configure firewall to restrict access
3. **Database Security**: Use strong database passwords, restrict network access
4. **Regular Backups**: Implement automated backup strategy
5. **Monitoring**: Set up monitoring and alerting
6. **Updates**: Subscribe to security announcements

## Security Features

### Current Implementation

- ✅ **End-to-End Encryption (E2EE)**: RSA-OAEP 4096-bit + AES-256-GCM
- ✅ **Password Hashing**: Argon2id (OWASP recommended)
- ✅ **JWT Authentication**: Short-lived access tokens (15 min)
- ✅ **Refresh Tokens**: HttpOnly cookies (30 days)
- ✅ **Rate Limiting**: Redis-based, per-IP and per-user
- ✅ **Input Validation**: Gin binding validation
- ✅ **SQL Injection Protection**: Parameterized queries
- ✅ **CORS**: Configurable allowed origins
- ✅ **Dependency Scanning**: GitHub Dependabot
- ✅ **Secret Detection**: TruffleHog pre-commit hooks
- ✅ **MFA/2FA**: TOTP-based multi-factor authentication with backup codes
- ✅ **Security Keys**: WebAuthn/FIDO2 support
- ✅ **Recovery Circles**: Social recovery for account access
- ✅ **Token Theft Detection**: Device binding with machine ID verification
- ✅ **CI/CD Security**: GitHub Actions (build.yml) + SonarQube Quality Gate enforcement
- ✅ **Key-Compromise Runbook**: [[internal]incident-response-key-compromise.md](../[internal]incident-response-key-compromise.md) — IR playbook covering E2EE keys, JWT signing keys, API/service tokens, and update signing certificates

### Planned Features

- 🔄 **Audit Logging**: Comprehensive security event logging
- 🔄 **IP Whitelisting**: Server-level IP restrictions
- 🔄 **Key Rotation**: Automated encryption key rotation

## Cryptography

### E2EE Implementation

**Key Exchange:**

- RSA-OAEP 4096-bit keys (upgraded from 2048 in PR #98)
- Key derivation: PBKDF2 (600,000 iterations)

**Message Encryption:**

- AES-GCM 256-bit
- Unique IV per message
- Authenticated encryption

**Transport Security:**

- TLS 1.3 required in production
- Perfect forward secrecy

### Password Security

**Hashing:**

- Algorithm: Argon2id
- Time cost: 3
- Memory cost: 64 MB
- Parallelism: 4
- Salt: Unique per user (16 bytes)
- Key length: 32 bytes

**Requirements:**

- Minimum length: 12 characters
- Must contain: at least 3 of uppercase, lowercase, number, special characters
- Maximum length: 128 characters

## Vulnerability Disclosure Timeline

1. **Day 0**: Vulnerability reported
2. **Day 1-2**: Acknowledgment sent
3. **Day 1-7**: Initial assessment and severity classification
4. **Day 7-30**: Patch development and testing
5. **Day 30**: Patch released (critical/high severity)
6. **Day 30-60**: Public disclosure (after patch deployment)

## Security Updates

Subscribe to security announcements:

- GitHub Security Advisories
- Email: security@concordvoice.com (for critical updates)

## Bug Bounty Program

**Status:** Targeted for v1.0.0 (formal program not yet launched). In the interim, vulnerability reports are welcomed via the channels in "Reporting a Vulnerability" above; researchers will be acknowledged in the section below.

Scope will include:

- Authentication bypass
- Data exposure
- Privilege escalation
- Remote code execution
- E2EE implementation flaws

Out of scope:

- Social engineering
- Physical attacks
- Denial of service
- Self-XSS

## Security Audit

**Last Audit:** Phase 2A documentation and security audit (2026-03-27). Covered MFA/WebAuthn implementation, RBAC, CORS hardening, credential extraction defenses, token theft detection, device binding, recovery circles, and CI reactivation with SonarQube Quality Gate enforcement. Previous: internal security hardening (PR #97, 2026-02).
**Next Audit:** Formal third-party audit planned for v1.0 release.

## AI-Introduced Vulnerability Response

Concord uses AI code generation tools (Claude Code, GitHub Copilot, OpenAI Codex). AI-generated code is subject to the same security standards as human-authored code, but introduces distinct risk patterns. This playbook covers incident response when a vulnerability is traced to AI-generated code.

### Identification

1. Run `git blame` on the affected file to identify the introducing commit
2. Check for `Co-Authored-By` trailer to confirm AI tool involvement and identify the model
3. If confirmed AI-generated, proceed with this playbook in addition to standard vulnerability response

### Scope Assessment

Search for the same vulnerability pattern in other AI-generated commits:

```bash
# Find all commits by the same AI tool touching similar files
git log --grep="Co-Authored-By.*Claude" --all -- "<affected-file-pattern>"
```

Assess whether the vulnerability is an isolated incident or a systematic model behavior pattern (same model + similar prompt = same flaw).

### Remediation

1. **Fix** the vulnerability following standard severity timelines (see above)
2. **Pattern search** — check if the same flaw exists elsewhere in AI-generated code
3. **Instruction update** — add the vulnerability pattern as a negative example in AI instruction files (`[internal]`, `.github/copilot-instructions.md`)
4. **Rule creation** — add a custom Semgrep rule to catch the pattern automatically in future code (Semgrep integration planned in Phase 3, #457; until then, document the pattern in AI instruction files)
5. **Template update** — if a prompt template exists for the affected code area, update it to prevent recurrence

### Reporting

Document the incident in the next quarterly AI security review (see `.github/ISSUE_TEMPLATE/quarterly-ai-review.md`). Include: originating tool/model, vulnerability type, root cause analysis, and preventive measures taken.

### Reference

- Full AI governance policy: `docs/policies/ai-generated-code-policy.md`
- MCP server controls: `docs/policies/mcp-server-policy.md`
- Audit log retention: `[internal]`

## Compliance

### Current Status

- **GDPR**: Partial compliance (data minimization, user rights)
- **CCPA**: Partial compliance
- **SOC 2**: Not audited (planned for enterprise offering)

### Data Handling

**Stored Data:**

- User credentials (hashed)
- Message metadata (encrypted if E2EE enabled)
- Server/channel membership
- Session information

**Not Stored:**

- Message content (when E2EE is enabled)
- Private encryption keys
- Plaintext passwords

## Third-Party Dependencies

Regularly audited via:

- `npm audit` (frontend)
- `go list -m all` + vulnerability databases (backend)
- GitHub Dependabot alerts
- Pre-commit hooks (TruffleHog, Semgrep)

See current vulnerability status: [Security tab](https://github.com/Concord-Voice/Concord-Voice-Alpha/security)

## Contact

- **Security Issues**: security@concordvoice.com
- **Data Privacy**: privacy@concordvoice.com
- **General Inquiries**: contact-us@concordvoice.com

## Acknowledgments

We thank the following researchers for responsibly disclosing vulnerabilities:

- (None yet - project in development)

## Technical security documents

These documents describe the technical implementation of Concord Voice's security properties. They are intended for contributors and security reviewers — for vulnerability reports, see the "Reporting a Vulnerability" section above.

- [`docs/policies/update-trust-model.md`](../docs/policies/update-trust-model.md) — per-platform auto-update trust model, CI integrity gate, and known gaps
- [`docs/policies/ai-generated-code-policy.md`](../docs/policies/ai-generated-code-policy.md) — AI-generated code governance and constraints
- [`docs/policies/agentic-ai-controls.md`](../docs/policies/agentic-ai-controls.md) — agentic-AI execution safety controls
- [`[internal]incident-response-key-compromise.md`](../[internal]incident-response-key-compromise.md) — incident response playbook for cryptographic key / credential compromise
- [`[internal]macos-cert-compromise.md`](../[internal]macos-cert-compromise.md) — Apple Developer ID signing-cert compromise + rotation (completes #645)

---

**Last Updated:** 2026-04-09
