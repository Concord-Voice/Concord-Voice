# Self-host public acceptance checklist template

This is an **uncompleted template**, not an evidence ledger or release authorization. Do not enter completed observations here: this file may be published. Keep actual reviewed manual and ingress records exclusively in the private release-evidence directory. Never include credentials, certificate/key bytes, raw logs, raw Compose environment, host addresses, user data or identifying customer details.

Publication remains **BLOCKED** until exact-public-bundle local acceptance, every required manual observation and the reviewed ingress contract are established. Earlier source-checkout CI and synthetic fixtures do not authorize a public bundle.

**Current record:** Local fresh-host acceptance is **UNVERIFIED** pending an explicit exact-candidate acceptance run and its sanitized evidence. Public acceptance is **UNVERIFIED**. Full #2505 closure and public publication are **BLOCKED** while internal development reference omitted remains open and required public evidence is absent. No operator run or evidence reference is recorded yet.

## Candidate identity template

Record these in the private evidence record: canonical version and immutable tag, full source SHA, full public snapshot SHA, archive SHA-256, trusted private staging run and Git-bundle digest, public-main parent SHA, exact acceptance run/job and schema-2 safe artifact, manual revision/attester/date, and ingress contract SHA/proof reference. All lanes must identify the same verified public archive. A PR head, PR merge ref and later squash commit are separate identities.

Release authorization also requires the candidate's path, mode and byte inventory to match an independent assembly of the pinned source data using current approved tooling, manifest and public/release overlays. Historical source commits supply data, never their own executable assembly policy. This template does not establish that reconstruction or any acceptance run succeeded.

## Lane 1: Local fresh-host acceptance

Routine PR CI no longer runs the full fresh-host acceptance job (removed 2026-10-06). Installer, wrapper, certificate and acceptance-validator contract tests remain; their results do not supply fresh-host evidence. Publication still requires the explicit protected-main exact-public-bundle lane and its schema-2 identity checks. Removing routine acceptance neither runs that lane nor establishes its evidence.

The explicit lane uses a disposable Blacksmith Ubuntu 24.04 runner with four vCPUs, a 50-minute job budget and a 44-minute acceptance-step limit. The harness admits its closed GitHub-hosted or Blacksmith CI compatibility profile; arbitrary self-hosted runners are refused. These admission and sizing contracts do not establish runner provenance or change the supported deployment-host requirements.

Historical schema-1 artifacts identify their exact candidate SHA, run, runner and tools. Each bounded check contains only its stage, assertion, status and fixed code; these fields point to the recovery guidance below. Additional sanitized operator observations belong in the private evidence record, outside the artifact. Link only evidence from the actual candidate and acceptance run. All template entries remain unverified.

| Required assertion | Template status | Next action |
|---|---|---|
| Fresh Ubuntu admission, prerequisite floors, capacity, exact selected Compose model and installer | UNVERIFIED | Review actual exact-bundle fresh-host run and safe artifact. |
| Required public security-log helper and guarded refusal/preparation | UNVERIFIED | Confirm the shipped helper, not a managed provisioner, was exercised. |
| Certificate delivery metadata | UNVERIFIED | Inspect the separate delivery check. |
| Selected coturn container's served leaf and local TLS handshake | UNVERIFIED | Inspect actual served-leaf check separately from delivery. |
| Local service health, nginx routes, rendered/live bindings | UNVERIFIED | Review the actual checks; do not infer external reachability. |
| Wrapper stop/up and down/up retention of the synthetic database row | UNVERIFIED | Establish only the tested named-volume retention scope. |

A schema-1 PR/source artifact retains its development purpose. Publication needs a protected-main release-candidate run and schema-2 artifact that verifies both full SHAs and the exact public bundle digest before running the same fresh-host checks. Job success alone is insufficient. This lane is local-only: its loopback address and test FQDN establish no public DNS, NAT, provider firewall behavior, external TURN or client use. Local TLS is not system-CA client trust; the disposable PostgreSQL-row readback establishes only the tested named-volume retention behavior, not backup or recovery.

Fresh-host CI admits the tested nginx service only when it is loaded, inactive or failed, and reports a zero main PID; other states are refused before configuration changes. It then gives that service a dedicated configuration and PID and validates the shipped vhost through syntax, activation, reload, TLS and route checks. It does not stop or kill an existing service. These local checks do not replace manual public-host observations or the reviewed ingress contract.

## Manual public-host lane

For every private observation, record exact candidate identities, date, attester, safe host label, named procedure/result and sanitized proof reference. Review actual cited proof; a self-declared status cannot prove itself.

| Required observation | Template status | Procedure |
|---|---|---|
| Public DNS | UNVERIFIED | Check apex/api/media/turn from an independent resolver and compare intended host. |
| ACME issuance | UNVERIFIED | Observe actual public issuance and served leaf following [TLS setup](selfhost-quickstart.md#tls). |
| ACME renewal | UNVERIFIED | Observe an actual successful renewal and renewed served leaf; a timer or dry run is insufficient. |
| SMTP registration/email | UNVERIFIED | Complete controlled registration, delivery and verification without retaining identifying content. |
| External TURN allocation and relay | UNVERIFIED | Observe allocation and bidirectional media relay from an external network; distinguish both results. |
| Desktop public system-CA trust | UNVERIFIED | Connect a supported desktop client to the public certificate with normal system validation. |
| Public ingress/listener/NAT/provider firewall contract | BLOCKED | Complete the reviewed ingress contract and authorized external observations of required/forbidden listeners. |
| Update | UNVERIFIED | Perform the [pinned update procedure](selfhost-quickstart.md#updating) on a disposable instance; record before/after identities and service/client result. |
| Backup | UNVERIFIED | Verify independently stored configuration, certificate, database and object-store backups. |
| Recovery | UNVERIFIED | Restore on a disposable host and verify scope/service outcome; restart is insufficient. |

No row records an actual observation. Configuration, local mocks, a certificate file, an issue closure or another row cannot complete a manual check.

## Local supervision and recovery guidance

Lifecycle `stop` and `down` commands have a separate 180-second phase bound, allowing the selected services' 45-second shutdown grace periods and bounded command overhead. A nonzero or timed-out shutdown remains a failure; retention PASS still requires successful restart and database readback. A timed-out or directly signalled stopped-health probe also fails, even if it printed the expected unhealthy diagnostic before completion.

The wrapper log sample uses `logs --no-follow --tail=20 postgres` and requires completed exit status zero with nonempty stdout. Timeout or signal termination fails this assertion; partial output alone is not successful lifecycle evidence. In `--no-follow` mode the wrapper rejects explicit follow flags, including assignment and combined short forms, before any Compose query. Option values are forwarded without being mistaken for flags.

The runner shares one 43-minute (2,580-second) monotonic deadline across child commands and waits, clipping each phase to the remaining time. Startup shares this remaining budget rather than an independent shorter cap. Timeout evidence retains only the last recognized finite controller phase, when available; raw command output remains private. The removed PR job used a separate two-minute prerequisite bound and a 50-minute outer limit; these are not current PR CI budgets. On a child timeout, the runner signals the owned foreground command group and reaps the direct child. Privileged preparation and nginx commands use a fixed isolated root launcher inside `sudo`; it recomputes the remaining allowance from the absolute monotonic deadline before executing GNU `timeout`. A ten-second reserve allows TERM, five-second KILL escalation and outer cleanup; an exhausted reserve refuses command execution. This supervision does not cancel operations already accepted by Docker or systemd daemons, so failed runs may still require host-local cleanup.

Exact-candidate integrity compares the complete tracked path set, file type, mode and streamed content digest before and after execution. Expected generated untracked files are allowed; tracked additions, removals or changes fail. Paths and digests remain private and do not enter the artifact. Local and hosted proof of these follow-up contracts remains pending until the exact candidate is tested; these descriptions do not record a successful run.

For startup `COMMAND_FAILED`, `startup_*` assertions name the last captured controller progress phase: dependency startup, control/media image build or readiness, enforcement activation, remaining services, or postchecks. Media startup distinguishes the Compose liveness wait (`startup_mp_liveness`) from the later protocol-3 readiness poll (`startup_mp_protocol`); the older `startup_mp_ready` marker covers both when no newer marker was captured. They identify where to begin host-local troubleshooting, not a confirmed root cause. `startup_timeout` means the bounded startup command timed out; `selected_stack` means no recognized progress context was captured. Every result remains failed, and child output stays outside the artifact.

After a media liveness failure, read-only diagnostics inspect only the selected project's media container and may read its last 20 log lines. Each command has an eight-second timeout; combined output over 64 KiB is discarded. Validated state yields fixed contexts for OOM, unhealthy, restarting, exited, created or running. An exact structured startup-fatal record with a recognized error code yields only a fixed `startup_mp_log_*` category for permission, read-only filesystem, missing file/module, native loading, memory or resource errors. These are reported signals, not a confirmed cause. Finite `startup_mp_diag_*` contexts distinguish collection timeout, missing container, daemon/access failure, other command failure, output limit, invalid metadata, ownership mismatch and unavailable diagnostics. Once state is validated, log failures or oversized log output retain that state category. The original failed result and exit code remain authoritative; no raw state, log text, paths or error values enter the artifact.

These pointers identify recovery paths and existing issue areas; they do not imply that #1619, #1899, #2504, or #3574 is open/closed beyond the status stated here, nor do they create a new issue admission. If evidence does not identify an owner, leave the check failed or blocked until the maintainer assigns one.

## Public-host procedure details

Use a disposable public host and the procedures linked below. Record observations only in the private evidence record, including the exact identities actually installed. Public ingress, NAT/provider firewall and externally observed relay behavior are distinct observations. Do not use a local loopback result as external proof. #2269 owns the final ingress/firewall contract; the ingress row remains blocked until that contract and its public verification are complete. Every cited manual or ingress proof must contain nonempty raw bytes and match the reviewed evidence revision, approved review head and current protected main. Presence and matching hashes do not establish that an observation happened.

| Check | Status | Exact candidate SHA / date | Attester | Safe host label | Procedure and result | Sanitized evidence reference | Next action |
|---|---|---|---|---|---|---|---|
| ACME certificate issuance | **UNVERIFIED** | Pending / pending | Pending | Pending | Follow [TLS setup](selfhost-quickstart.md#tls): configure public DNS and challenge reachability, then run the documented `provision-cert.sh letsencrypt` procedure. Record issuance and the served public leaf; redact names or addresses if they identify the host. | None recorded | Perform on the disposable public host and add sanitized proof. |
| ACME renewal | **UNVERIFIED** | Pending / pending | Pending | Pending | Observe an actual successful renewal through the installed renewal process and required certificate-copy hook described in [TLS setup](selfhost-quickstart.md#tls). A configured timer or dry run alone is not a completed renewal. | None recorded | Capture the successful renewal event and confirm the renewed leaf is served. |
| Public DNS | **UNVERIFIED** | Pending / pending | Pending | Pending | Verify the apex and `api`, `media`, and `turn` records using the operator's DNS provider and an independent resolver; compare with the intended host and record the sanitized result. | None recorded | Verify all required records from outside the host. |
| SMTP registration and email path | **UNVERIFIED** | Pending / pending | Pending | Pending | Configure the documented BYO SMTP settings and complete a controlled new-account registration plus email verification. Record delivery and verification outcome without message content, address, or credentials. | None recorded | Perform with a controlled test account and retain redacted outcome only. |
| External TURN allocation and relay | **UNVERIFIED** | Pending / pending | Pending | Pending | From an external network, use the supported Concord Voice client to establish a TURN allocation and relay media through it. Record allocation and bidirectional relay outcome separately; include the host NAT/forwarding context without exposing addresses. | None recorded | Verify allocation and actual relay from outside the host. |
| Desktop trust through public system CA | **UNVERIFIED** | Pending / pending | Pending | Pending | On a supported desktop installation, connect to the public host using its publicly trusted certificate and confirm normal system-CA validation succeeds. This does not imply self-signed desktop trust. | None recorded | Test the released desktop client against the public certificate. |
| Public ingress, listener, NAT, and provider firewall contract | **BLOCKED — #2269 open** | Pending / pending | Pending | Pending | After #2269 lands, verify its final required and forbidden listeners against provider firewall/NAT configuration and an authorized external observation. Local Compose/render checks alone do not satisfy this item. | None recorded | Complete #2269 contract and then perform the external verification. |
| Update | **UNVERIFIED** | Pending / pending | Pending | Pending | On the disposable host, update from a recorded prior candidate using the [quickstart update procedure](selfhost-quickstart.md#updating); record resulting exact SHA, health, and client-facing outcome. | None recorded | Perform an actual update and record its outcome. |
| Backup | **UNVERIFIED** | Pending / pending | Pending | Pending | Create an independently stored backup using the operator's documented data-protection procedure; record scope, date, and successful verification without including data or credentials. Named Docker volume retention is not a backup. | None recorded | Document and verify a backup for the disposable instance. |
| Recovery | **UNVERIFIED** | Pending / pending | Pending | Pending | Restore the recorded backup or follow the documented recovery procedure on a disposable host; record restored scope and service outcome. Do not infer recovery from update or `down`/`up`. | None recorded | Perform recovery and record sanitized result. |

## Current closure boundary

No row above records a successful run yet. Keep #2505 open until explicit protected-main exact-public-bundle fresh-host acceptance has passed with valid schema-2 evidence, every required public-host item has actual sanitized evidence, and #2269's ingress/firewall contract and public dependency are met. The closed #2504 issue remains a shipped prerequisite; its internal development reference omitted merged, and follow-up internal development reference omitted also merged. Neither merge records a successful public-host acceptance run. Issue #1945 is contextual to the history, outside this acceptance scope, and is not a new admission here.
