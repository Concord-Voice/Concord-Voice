# Security Policy

Concord Voice builds privacy- and security-critical software. We take vulnerabilities seriously and genuinely appreciate responsible disclosure.

## Supported Versions

| Version | Supported |
| ------- | --------- |
| 0.x.x   | Yes       |

**Note:** Concord Voice is in active development toward v0.2.0-Beta, with v1.0 targeted for January 2027. Security updates are provided for the current development release.

## Reporting a Vulnerability

**Please do not open public issues, pull requests, or discussions for security vulnerabilities.**

Report privately to **[security@concordvoice.com](mailto:security@concordvoice.com)**. A PGP key is available on request.

Where possible, please include:

- A description of the vulnerability and its potential impact
- Steps to reproduce, or a proof-of-concept
- Affected component(s) and version(s)
- Any suggested remediation
- How you would like to be credited

### What to Expect

| Stage | Target |
| :-- | :-- |
| Acknowledgement of your report | within **48 hours** |
| Triage and severity assessment | within **7 days** |
| Status updates | every **7 days** until resolved |
| Coordinated fix and disclosure | we will keep you updated throughout |

We will credit you for the discovery once a fix ships, unless you prefer to remain anonymous. Please give us a reasonable window to remediate before any public disclosure.

### Severity and Response Targets

| Severity | Response | Examples |
| :-- | :-- | :-- |
| **Critical** | 24-48 hours | Remote code execution; authentication bypass; data exposure affecting all users; privilege escalation to admin |
| **High** | 3-7 days | SQL injection; XSS; CSRF; unauthorized data access |
| **Medium** | 7-14 days | Denial of service; information disclosure; session management issues |
| **Low** | 14-30 days | Missing security headers; weak cryptography; minor information leaks |

### Disclosure Timeline

1. **Day 0** — vulnerability reported
2. **Day 1-2** — acknowledgement sent
3. **Day 1-7** — initial assessment and severity classification
4. **Day 7-30** — patch development and testing
5. **Day 30** — patch released (critical and high severity)
6. **Day 30-60** — public disclosure, after patch deployment

## Scope

This policy applies to all repositories under the [Concord Voice organization](https://github.com/Concord-Voice) and the services hosted at `concordvoice.com` and `concordvoice.chat`.

## Bug Bounty Program

**Status:** targeted for v1.0.0. A formal program has not launched, and we are not currently paying vulnerability rewards. In the interim, reports are welcome through the channels above and researchers are credited in the Acknowledgements section below.

Anticipated in scope:

- Authentication bypass
- Data exposure
- Privilege escalation
- Remote code execution
- End-to-end encryption implementation flaws

Anticipated out of scope:

- Social engineering
- Physical attacks
- Denial of service
- Self-XSS

## Safe Harbor

If you follow this policy in good faith, we treat your research as authorized. We will not pursue legal action against you for it, and if a third party does, we will state plainly that you were acting within this policy.

Good-faith research means: use your own accounts, or your own self-hosted instance; do not access, modify, or retain anyone else's data; do not run denial-of-service, load, or stress tests against our hosted services; and do not social-engineer our team, our users, or our vendors. If a proof-of-concept would require crossing one of those lines, describe it rather than running it — we would rather read the write-up.

If you are unsure whether something is in scope, ask first at [security@concordvoice.com](mailto:security@concordvoice.com). Asking will never count against you.

## Our Security Posture

Concord Voice is engineered for defense in depth.

### Implemented

- **End-to-end encryption:** AES-256-GCM with RSA-OAEP 4096-bit key wrapping. Always on — there is no setting to disable it.
- **Epoch-based key rotation:** automated rotation of channel and conversation keys, with rotation fingerprints binding successor epochs.
- **Password hashing:** Argon2id, OWASP-recommended parameters.
- **Credential protection:** OS-keychain-backed storage on desktop.
- **Strong authentication:** TOTP multi-factor with backup codes, and WebAuthn/FIDO2 security keys and passkeys.
- **Recovery circles:** social recovery for account access.
- **Token theft detection:** device binding with machine-ID verification.
- **Session security:** short-lived JWT access tokens (15 minutes) and HttpOnly refresh cookies (30 days, rolling).
- **Access control:** role-based and scope-based permissions.
- **Rate limiting:** Redis-backed, per-IP and per-user.
- **Injection defenses:** parameterized queries throughout; validation at every request boundary.
- **Transport security:** TLS 1.3 required in production, with perfect forward secrecy.
- **Static and dynamic analysis in CI:** CodeQL, Semgrep, ESLint, OSV-Scanner, govulncheck, OpenSSF Scorecard, and an API DAST pass, with results published to GitHub Code Scanning. A subset are merge-gated by branch rulesets.
- **Secret detection:** detect-secrets, TruffleHog, and gitleaks in pre-commit, plus a CI secret-scanning gate.
- **Dependency scanning:** Dependabot alerts, `npm audit`, and Go vulnerability database checks.
- **Quality gate:** SonarQube analysis with AI-Code Assurance enabled, enforced on every pull request.

### Planned

- Comprehensive security event audit logging
- Server-level IP allowlisting

## Cryptography

**Key exchange**

- RSA-OAEP 4096-bit keys
- Key derivation: PBKDF2, 600,000 iterations

**Message encryption**

- AES-GCM 256-bit
- Unique IV per message
- Authenticated encryption

**Transport**

- TLS 1.3 required in production
- Perfect forward secrecy

**Password storage**

- Algorithm: Argon2id
- Time cost 3, memory cost 64 MB, parallelism 4
- Unique 16-byte salt per user, 32-byte key length
- Minimum 12 characters, maximum 128, requiring at least three of: uppercase, lowercase, number, symbol

## Data Handling

**Stored**

- User credentials, hashed
- Message metadata: sender, timestamp, and channel or conversation membership
- Server and channel membership
- Session information

**Not stored**

- Message content in any form we can read. Messages are persisted only as ciphertext for which we hold no key.
- Private encryption keys
- Plaintext passwords

## Security Best Practices

### For Users

1. **Keep updated** — always run the latest version.
2. **Use a strong password** — minimum 12 characters, mixed case, numbers, symbols.
3. **Verify identity** — verify server identity when connecting to self-hosted instances.
4. **Secure your keys** — back up your encryption keys somewhere safe. We cannot recover them for you.

### For Contributors

1. **Never commit `.env` files** or any credential material.
2. **Keep dependencies updated.**
3. **Do not bypass pre-commit hooks** — they exist to catch secrets before they leave your machine.
4. **All code requires review before merge.**
5. **Validate all user input** at the trust boundary.
6. **Use parameterized queries** — never string-formatted SQL.
7. **HTTPS only** for all production traffic.

### For Self-Hosters

1. **TLS certificates** — use valid certificates; Let's Encrypt is recommended.
2. **Firewall** — restrict access to the services that need to be reachable.
3. **Database security** — strong passwords, restricted network access.
4. **Regular backups** — automate them, and test a restore.
5. **Monitoring** — set up monitoring and alerting.
6. **Updates** — subscribe to security announcements.

## AI-Introduced Vulnerability Response

Concord Voice uses AI code generation tools. AI-generated code is held to the same security standards as human-authored code, but it introduces distinct risk patterns. This playbook applies when a vulnerability is traced to AI-generated code, in addition to the standard response above.

**Identification.** Run `git blame` on the affected file to find the introducing commit, and check for a `Co-Authored-By` trailer to confirm AI involvement and identify the model.

**Scope assessment.** Search for the same pattern across other AI-generated commits — the same model given a similar prompt tends to produce the same flaw, so an isolated-looking bug may be systematic:

```bash
git log --grep="Co-Authored-By.*Claude" --all -- "<affected-file-pattern>"
```

**Remediation.**

1. Fix the vulnerability on the standard severity timeline.
2. Search for the same flaw elsewhere in AI-generated code.
3. Add the pattern as a negative example in the AI instruction files.
4. Add a Semgrep rule so the pattern is caught automatically in future.
5. Update any prompt template covering the affected area.

**Reporting.** Document the incident in the next quarterly AI security review, including originating tool and model, vulnerability class, root cause, and preventive measures taken.

## Security Audits

**Last audit:** internal security audit, 2026-03-27 — covering multi-factor authentication and WebAuthn, role-based access control, CORS hardening, credential extraction defenses, token theft detection, device binding, and recovery circles.

**Next audit:** a formal third-party audit is planned ahead of the v1.0 release.

## Compliance

- **GDPR** — partial compliance: data minimization and user rights implemented.
- **CCPA** — partial compliance.
- **SOC 2** — not audited. Planned for the enterprise offering.

We would rather state this plainly than imply a certification we do not hold.

## Security Updates

Subscribe to security announcements through GitHub Security Advisories on this organization, or by email at [security@concordvoice.com](mailto:security@concordvoice.com) for critical updates.

Current vulnerability status for any repository is published on that repository's Security tab.

## Internal Security Documentation

The following documents describe the technical implementation of Concord Voice's security properties. They live in the working repository and are intended for contributors and security reviewers — for vulnerability reports, use the process above.

- Update trust model — per-platform auto-update trust model, CI integrity gate, and known gaps
- AI-generated code policy — governance and constraints on AI-authored code
- Agentic AI controls — execution safety controls for agentic tooling
- Key-compromise incident response — playbook for cryptographic key and credential compromise
- Signing-certificate compromise and rotation — Apple Developer ID and Windows signing certificates

## Contact

- **Security issues:** [security@concordvoice.com](mailto:security@concordvoice.com)
- **Data privacy:** [privacy@concordvoice.com](mailto:privacy@concordvoice.com)
- **General inquiries:** [contact-us@concordvoice.com](mailto:contact-us@concordvoice.com)

## Acknowledgements

We thank the following researchers for responsibly disclosing vulnerabilities:

- None yet — the project is in active development.

Thank you for helping keep Concord Voice and its users safe.

---

**Last Updated:** 2026-08-20
