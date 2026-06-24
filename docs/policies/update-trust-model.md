# Update Trust Model

> **Status:** Living document. Last updated 2026-04-15 (#644).
> **Audience:** Concord Voice contributors, security reviewers, and users who want to understand what they trust when accepting an auto-update.

## What users implicitly trust when accepting an auto-update

By accepting an auto-update, a user trusts that:

1. The binary installed originated from Concord Voice LLC's release pipeline
2. The binary was not modified in transit between the release server and the client
3. No attacker in a network-privileged position can substitute a malicious artifact

This document describes which of those properties each platform verifies, and how.

## Per-platform trust model

### macOS

**Shipped in PR #641.**

- **Build-time:** artifacts are signed with an Apple Developer ID certificate and notarized by Apple's notary service. The stapled notarization ticket is embedded in the `.app` bundle.
- **CI verification** (`.github/workflows/build-desktop.yml:243-275`): every build runs `codesign --verify --deep --strict`, `spctl --assess --type execute` (Gatekeeper simulation), and `xcrun stapler validate`. Any failure fails the workflow.
- **Install-time:** Gatekeeper re-verifies the signature and notarization before executing the updated app. Unsigned or tampered artifacts are rejected by the OS.

**What the user is trusting:** Apple's Developer ID program + Apple's notary service.

### Windows

**Shipped in #404; runtime hardening added in #644.**

- **Build-time:** artifacts are signed by Microsoft Azure Trusted Signing using OIDC federation (no long-lived keys in CI). Leaf certs are 72-hour short-lived, auto-renewed daily. All signatures are RFC 3161 timestamped, so they remain valid past leaf expiry.
- **CI verification** (`.github/workflows/build-desktop.yml:302-339`): every build verifies Authenticode status, requires leaf CN `Concord Voice LLC`, requires RFC 3161 timestamp, and requires the issuer CN to match `^Microsoft ID Verified CS `.
- **Install-time (electron-updater built-in):** verifies Authenticode signature validity and compares leaf CN against `ALLOWED_WINDOWS_PUBLISHERS` in [`client/desktop/src/main/updater.ts`](../../client/desktop/src/main/updater.ts) (currently `['Concord Voice LLC']`).
- **Install-time (custom hook, added by #644):** `verifyUpdateCodeSignature` extracts the full cert chain and rejects the update unless the leaf was issued by a cert matching `^Microsoft ID Verified CS `. This defeats the CN-collision attack where a publicly-trusted cert with matching leaf CN is obtained from a different CA. See [`client/desktop/src/main/verifyWindowsSignature.ts`](../../client/desktop/src/main/verifyWindowsSignature.ts).

**What the user is trusting:** Microsoft Trusted Signing (which performs D-U-N-S-backed corporate verification before issuing to the allowed CN) + the Microsoft root of trust on Windows.

**Why leaf-thumbprint pinning is not used:** Trusted Signing leaves are 72-hour short-lived and auto-renewed daily. Pinning a leaf thumbprint would require shipping a new client build every 3 days. Issuer pinning at the Trusted Signing intermediate provides equivalent protection against the realistic attack (CN collision from a different CA) without requiring a rotation pipeline.

### Linux

**Not yet signature-verified. Tracked in #653.**

- **Build-time:** AppImage / deb / rpm artifacts are NOT signed.
- **CI verification:** only the manifest SHA-512 cross-check (#644) runs — no signature verification tool.
- **Install-time:** electron-updater verifies the SHA-512 hash declared in `latest-linux.yml`, but the manifest itself is trusted to be authentic purely on the basis of TLS transport security.

**What the user is trusting on Linux today:** HTTPS + the server-side update feed. A compromise of the update server or a successful MITM with a rogue certificate would allow forged updates to install.

**Tracked follow-up:** #653 will add a detached-signature scheme (minisign / cosign / GPG — TBD) with the public key baked into the client bundle.

### TLS-layer feed pinning (#658)

On **all platforms**, the HTTPS connection to `https://api.concordvoice.chat/api/v1/updates` is additionally pinned at the TLS layer via SPKI SHA-256 of the server's leaf certificate. This closes the rogue-cert MITM gap described in §Server-side trust boundary below.

**How it works:**

- `client/desktop/src/main/main.ts` installs a `Session.setCertificateVerifyProc` handler inside `app.whenReady()`. The handler is a pure function in `client/desktop/src/main/updatePinning.ts` configured from `updatePinningConfig.ts`.
- For the pinned hostname (`api.concordvoice.chat`), the handler computes SPKI SHA-256 of the leaf cert's `SubjectPublicKeyInfo` and compares against a primary+fallback pin pair. Dual-pin enables non-emergency rotation.
- For non-pinned hostnames (self-hosted deployments, staging, localhost), the handler returns `callback(-3)` — Chromium's default cert validation applies, system CA trust works normally.
- When both pins miss, the handler returns `callback(-2)` (explicit reject) and the renderer shows `UpdateSecurityBanner` directing the user to reinstall from GitHub Releases.

**Why this matters:** A rogue cert obtained from a different public CA for the same CN would pass Chromium's default validation (it chains to a trusted root). It would NOT pass the SPKI pin because the attacker doesn't have Concord's private key. The pin is the only anchor that binds TLS trust to Concord's own keypair.

**Operational contract:**

- Concord obtains the TLS cert via Let's Encrypt on the origin (certbot with `--reuse-key`) and uploads it to CloudFlare via `[internal]upload-cert-to-cloudflare.sh` (a certbot renewal hook). CloudFlare serves the uploaded cert at its edge, preserving the SPKI that clients pin.
- `--reuse-key` is load-bearing: without it, every 90-day renewal would generate a new keypair, the SPKI would change, and every pinned client would hard-fail on next update check.
- Rotation (deliberate keypair rotation for compromise or cadence) follows the dual-pin runbook procedure (`[internal]`).
- Observability: the current verify proc is wired to `console`, and `updatePinning.ts` emits plain string log lines for pinning events. Operators should expect plain-text console output for fallback-pin usage and pin mismatches; there are currently no structured `category` / `outcome` fields, no `fingerprint` array, and no built-in deduplication.

**Implementation references:**

- Module: [`client/desktop/src/main/updatePinning.ts`](../../client/desktop/src/main/updatePinning.ts)
- Config: [`client/desktop/src/main/updatePinningConfig.ts`](../../client/desktop/src/main/updatePinningConfig.ts)
- Wiring: [`client/desktop/src/main/main.ts`](../../client/desktop/src/main/main.ts) (inside `app.whenReady()`)
- Banner: [`client/desktop/src/renderer/components/Updates/UpdateSecurityBanner.tsx`](../../client/desktop/src/renderer/components/Updates/UpdateSecurityBanner.tsx)
- Design spec: [`[internal]specs/2026-04-20-658-updater-feed-cert-pin-design.md`](../superpowers/specs/2026-04-20-658-updater-feed-cert-pin-design.md)
- Rotation runbook: [`[internal]`](update-cert-pinning-runbook.md)

## Server-side trust boundary

The update feed is served at `https://api.concordvoice.chat/api/v1/updates`. Transport protections:

- **Non-HTTPS refused:** `client/desktop/src/main/updater.ts` explicitly refuses to configure the feed with a non-HTTPS `apiBase`.
- **HTTPS only:** transport encryption + standard Web PKI cert validation on the server host.
- **TLS-layer feed pinning (#658):** in addition to standard Web PKI validation, clients pin the SPKI SHA-256 of the CloudFlare edge certificate for `api.concordvoice.chat`. See the dedicated subsection under §Per-platform trust model above.

**What HTTPS + pinning DOES guarantee:**

- An attacker with a rogue cert for the host (e.g., via a compromised intermediate CA) CANNOT MITM the update feed, because the rogue cert's SPKI will not match either pin.

**What HTTPS + pinning does NOT guarantee:**

- That the server host itself has not been compromised (an attacker with root on the origin could serve real malicious updates with a real signed keypair — this is outside pinning's scope; the artifact-level signing layer from #644 is what defends that case).
- Self-hosted deployments at a different `apiBase`: pinning is SaaS-only; self-hosted deployments use system-CA trust.

Those gaps are why the per-platform trust anchors above are load-bearing — they provide defense in depth.

## SPA UI Update Trust Boundary

The SPA update axis is separate from binary auto-update. A SPA soft reload is a top-frame navigation selected by the Electron main process after `resolveSpaSource()` validates the server-advertised HTTPS URL and IPC contract. The renderer can request a check or reload, but it never supplies the target URL.

Current Phase 1 behavior:

- remote SPA config and entry-byte fetches use targeted no-cache `net.fetch` options;
- remote SPA navigations use `BrowserWindow.loadURL()` with no-cache headers;
- session-only users keep their in-memory refresh token across renderer reloads, without writing it to disk;
- session-only users likewise keep their E2EE key material in main-process memory only (never disk), wiped on logout/`clearTokens`, so a soft reload restores decryptable content; post-login state (servers/profile/preferences) is re-hydrated on every successful restore, not only when E2EE keys are present;
- auto-apply is deferred during active voice, screen share, and DM call states.

Persistent cached SPA execution is not enabled. A future last-known-good SPA cache must verify a signed manifest with a public key shipped in the desktop binary before any cached remote bytes can execute. The cache must verify the complete Vite asset graph, reject traversal/symlink/partial/stale/incompatible entries, and fall back to the signed bundled `app://concord` renderer when verification fails.

## CI integrity gate

The `build-desktop.yml` workflow runs, in order:

1. Build the artifact (`electron-forge make`)
2. Sign it (per-platform)
3. **Re-verify the signature** (per-platform, see workflow line references above)
4. **Verify update manifest SHA-512** (added by #644): `client/desktop/scripts/verify-update-manifest.mjs` reads `latest*.yml` and cross-checks every listed artifact hash against the actual file bytes on disk
5. Upload artifact and publish release

Step 4 catches the concrete threat of a tampered manifest between sign and publish. Step 3 catches broken signing and corrupted artifacts.

## Known gaps

- **No SLSA-style attestation yet.** Artifacts are not accompanied by in-toto provenance signed via Sigstore OIDC. Tracked in #654 (phase-3).
- **No certificate transparency monitoring.** We do not monitor public CT logs for the issuance of certs matching our leaf CN from CAs outside the pinned Microsoft chain. Future work.
- **No binary transparency / Rekor publication.** Our release hashes are not published to a public transparency log. Future work, likely bundled with #654.
- **Linux update signing deferred.** Tracked in #653.
- **Chain validity is asserted at the leaf, not walked end-to-end by our hook.** `verifyWindowsSignature.ts` requires `Get-AuthenticodeSignature` to return `Status = Valid` on the downloaded installer — that status already covers signature integrity, root-of-trust chaining, expiry (including timestamp validity), and revocation via the Windows certificate store. On top of that, our hook inspects chain _structure_ to enforce the leaf CN allow-list and issuer-prefix pin. We do not separately walk or re-validate each intermediate; we trust the OS-level chain build that produced the `Valid` status. If that upstream check is ever bypassed, our structural checks alone would not detect a revoked or untrusted intermediate.

## Related documents

- [`[internal]specs/2026-04-15-644-update-trust-hardening-design.md`](../superpowers/specs/2026-04-15-644-update-trust-hardening-design.md) — design spec
- [`[internal]specs/2026-04-14-403-macos-code-signing-design.md`](../superpowers/specs/2026-04-14-403-macos-code-signing-design.md) — macOS as-shipped design
- [`SECURITY.md`](../../.github/SECURITY.md) — vulnerability reporting policy
