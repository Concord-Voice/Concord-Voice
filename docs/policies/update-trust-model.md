# Update Trust Model

> **Status:** Living document. Last updated 2026-06-30 (#654).
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
- **CI verification** (`build-desktop.yml` — "Verify signature and notarization" step): every build runs `codesign --verify --deep --strict`, `spctl --assess --type execute` (Gatekeeper simulation), and `xcrun stapler validate`. Any failure fails the workflow.
- **Install-time:** Gatekeeper re-verifies the signature and notarization before executing the updated app. Unsigned or tampered artifacts are rejected by the OS.

**What the user is trusting:** Apple's Developer ID program + Apple's notary service.

### Windows

**Shipped in #404; runtime hardening added in #644.**

- **Build-time:** artifacts are signed by Microsoft Azure Trusted Signing using OIDC federation (no long-lived keys in CI). Leaf certs are 72-hour short-lived, auto-renewed daily. All signatures are RFC 3161 timestamped, so they remain valid past leaf expiry.
- **CI verification** (`build-desktop.yml` — "Verify Windows signature" step): every build verifies Authenticode status, requires leaf CN `Concord Voice LLC`, requires RFC 3161 timestamp, and requires the issuer CN to match `^Microsoft ID Verified CS `.
- **Install-time (electron-updater built-in):** verifies Authenticode signature validity and compares the leaf CN against the `publisherName` list in the packaged `app-update.yml`, generated from the shared `ALLOWED_WINDOWS_PUBLISHERS` constant ([`client/desktop/src/shared/allowedWindowsPublishers.ts`](../../client/desktop/src/shared/allowedWindowsPublishers.ts), currently `['Concord Voice LLC']`) by `scripts/generate-app-update.mts`. electron-updater reads the allow-list ONLY from this on-disk file — the key was absent before #2020, leaving both install-time checks dormant.
- **Install-time (custom hook, added by #644, armed by #2020):** `verifyUpdateCodeSignature` extracts the full cert chain and rejects the update unless the leaf was issued by a cert matching `^Microsoft ID Verified CS `. This defeats the CN-collision attack where a publicly-trusted cert with matching leaf CN is obtained from a different CA. See [`client/desktop/src/main/verifyWindowsSignature.ts`](../../client/desktop/src/main/verifyWindowsSignature.ts).

**What the user is trusting:** Microsoft Trusted Signing (which performs D-U-N-S-backed corporate verification before issuing to the allowed CN) + the Microsoft root of trust on Windows.

**Why leaf-thumbprint pinning is not used:** Trusted Signing leaves are 72-hour short-lived and auto-renewed daily. Pinning a leaf thumbprint would require shipping a new client build every 3 days. Issuer pinning at the Trusted Signing intermediate provides equivalent protection against the realistic attack (CN collision from a different CA) without requiring a rotation pipeline.

### Linux

**Shipped in #653.** See [ADR-0026](../adr/0026-linux-update-signing.md) for the tool-choice rationale.

`electron-updater` has no Linux signature hook (`verifyUpdateCodeSignature` is wired only into the Windows `NsisUpdater`), so Linux verification is **out-of-band**, at a choke point Concord owns inside its own `safeQuitAndInstall()`.

- **Build-time:** each Linux artifact (`.AppImage` / `.deb` / `.rpm`, both arches) is signed in CI (`.github/workflows/build-desktop.yml` release path) with a **raw Ed25519 detached signature** — `openssl pkeyutl -sign -rawin` — producing a 64-byte `<artifact>.sig` next to each artifact. The private key is the `desktop-release` environment secret `LINUX_UPDATE_SIGNING_KEY`, reachable only by the trusted exact-main release jobs.
- **CI verification:** the signing step self-verifies each `.sig` against the committed public key (a signing-key/bundled-key mismatch tripwire), and the existing `required_assets` asset-set gate hard-fails the release if any of the six Linux `.sig` files are missing — so an unsigned Linux Release cannot publish.
- **Install-time:** inside `safeQuitAndInstall()` on Linux, the client fetches `<artifact>.sig` from the static public recovery feed (`https://github.com/Concord-Voice/Concord-Voice/releases/latest/download`) and verifies it over the downloaded artifact bytes with one native `crypto.verify(null, …)` call against the bundled public key (`client/desktop/src/main/linuxUpdatePublicKey.ts`, via `client/desktop/src/main/verifyLinuxSignature.ts`). The verify is **fail-closed**: the only path to install is `verify → true`; a missing artifact, a non-2xx `.sig` fetch, a signature not exactly 64 bytes, a `verify → false`, or any thrown exception refuses the install (identical install boundary in every case). The *message* distinguishes the cause: only a cryptographic `verify → false` — the one outcome that is genuine evidence of an altered artifact — surfaces the `signature-failure` security banner; availability failures (network/IO error, a missing or malformed `.sig`) refuse with a retryable non-security "couldn't verify right now" message instead, so a transient blip does not cry wolf and erode the banner's credibility. An attacker who strips/blocks the `.sig` lands in the availability path and still cannot install. electron-updater still cross-checks the `latest-linux.yml` SHA-512 at download (#644); the Ed25519 signature is the layer that defends a manifest the attacker controls.

**What the user is trusting on Linux:** the bundled Ed25519 public key. **This is the SOLE trust anchor** — it holds even with no API-host TLS pinning, because an attacker who controls the public recovery feed controls both the artifact and its `.sig` but cannot forge a signature without the private key. TLS-layer API pinning (#658) was defense-in-depth for the API host and was never load-bearing for this guarantee; it has since been removed (see below).

**What this defends:** a forged or tampered Linux update artifact served from a compromised feed (`/opt/concord/releases/`), a compromised GitHub Release, the public mirror, or an on-path attacker.

**What this does NOT defend (by design):**

- **Update freeze** — a feed-controlling attacker answering "you're up to date" (`update-not-available`) can pin a victim indefinitely to a specific signed-but-known-vulnerable build. This product rides the prerelease channel fleet-wide (`allowPrerelease` default true), so the consequence is concrete: indefinite pinning to a known-CVE *signed* build. Artifact signing provides zero mitigation. #654 adds public distribution provenance and Alpha/public byte binding, but freeze defense still needs a separate manifest-freshness control.
- **The verify→install TOCTOU residual (accepted).** The client verifies the bytes at the downloaded path, then `quitAndInstall()` independently re-reads the **same** cached path with no install-time checksum. A local process with write access to the updater cache (under user-writable `userData`) could swap the file in the gap — a wider window than the Windows model, whose verify runs inside electron-updater's install flow. Accepted because it requires a local attacker who already has `userData` write (who can compromise the app more cheaply) and is parity with the unavoidable disk-read-at-install on every platform. Mitigated by calling `quitAndInstall()` immediately after a successful verify with no intervening `await`.

**Key rotation** requires shipping a new client build (the trust anchor is bundled, not fetched). See [`[internal]refresh-linux-update-key.md`](../runbooks/refresh-linux-update-key.md).

### API-host TLS trust (#658 removed)

`api.concordvoice.chat` is **not** certificate-pinned. HTTPS to the API uses Chromium's default validation — publicly-trusted CA chain plus Certificate Transparency — the same anchor the browser SPA and the update feed rely on.

The pin was removed after five fleet-wide outages between 2026-04 and 2026-08. It matched the leaf SPKI against values baked into the binary, on the premise that Concord uploaded its own origin keypair to Cloudflare via the Custom SSL Certificates API. That endpoint is Business/Enterprise-only and this account is on Pro, so the upload never ran: Cloudflare served a *managed* edge certificate it rotated on its own schedule with a new keypair, and every rotation locked the entire installed base out with `net::ERR_FAILED` until a new binary shipped. No server-side remedy exists, because the rejection happens client-side before the request leaves the machine.

**This does not weaken the update trust anchors.** They never depended on it — see the Linux note above: the bundled Ed25519 key "holds even with no API-host TLS pinning". The Windows anchor is artifact-level Authenticode verification (#2020). Both are unchanged.

**Rogue-cert MITM** is currently unmitigated for the API host. The intended control is out-of-band Certificate Transparency monitoring, which alerts an operator rather than bricking every client — it is **not yet implemented** (see the linked trust model). Re-pinning is safe only against a keypair Concord controls end to end; the conditions are enumerated in [`api-tls-trust-model.md`](api-tls-trust-model.md).

## API-host trust boundary

The packaged binary update feed is `https://github.com/Concord-Voice/Concord-Voice/releases/latest/download`. The API-hosted release directory remains an operational surface, but it is not the only packaged-client recovery path. API-host transport protections are:

- **HTTPS only:** transport encryption + standard Web PKI cert validation on the server host.
- **TLS-layer API pinning (#658):** in addition to standard Web PKI validation, clients pin the SPKI SHA-256 of the CloudFlare edge certificate for `api.concordvoice.chat`. See the dedicated subsection under §Per-platform trust model above.

**What HTTPS + API pinning DOES guarantee:**

- An attacker with a rogue cert for the API host (e.g., via a compromised intermediate CA) CANNOT MITM API-host traffic, because the rogue cert's SPKI will not match either pin.

**What HTTPS + API pinning does NOT guarantee:**

- That the server host itself has not been compromised (an attacker with root on the origin could serve hostile API responses or alter any API-hosted release directory — this is outside pinning's scope; the artifact-level signing layer from #644 is what defends update artifacts).
- Self-hosted deployments at a different `apiBase`: pinning is SaaS-only; self-hosted deployments use system-CA trust.

Those gaps are why the per-platform artifact trust anchors above are load-bearing. The public recovery feed is transport-diverse from the API host, but artifact signing remains the update-authenticity boundary.

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
6. Mirror the signed release to the public repo via `.github/workflows/publish-public-mirror.yml`; `scripts/public-mirror/mirror-release.sh` fails closed unless the source and public release contain exactly the expected `latest.yml`, `latest-mac.yml`, `latest-linux.yml`, platform installers, blockmaps, and Linux `.sig` files needed by the public recovery feed, then checks every public release asset digest against the Alpha source asset, passes the Alpha-generated digest manifest to the public `public-release-attestation.yml` workflow, verifies every public release asset attestation, and only then promotes `/releases/latest` before the mirror is considered successful

Step 4 catches the concrete threat of a tampered manifest between sign and publish. Step 3 catches broken signing and corrupted artifacts. Step 6 adds public distribution provenance for the bytes hosted in `Concord-Voice/Concord-Voice`; it does not prove original Alpha build-runner provenance.

## Known gaps

- **Public release attestations are distribution provenance, not Alpha build provenance.** Public desktop release assets mirrored to `Concord-Voice/Concord-Voice` are attested by the public repo workflow after #654 and can be verified with `gh attestation verify --repo Concord-Voice/Concord-Voice --signer-workflow github.com/Concord-Voice/Concord-Voice/.github/workflows/public-release-attestation.yml`. Alpha-private Gate B remains disabled until Alpha is public or on GitHub Enterprise Cloud, so original private build-runner provenance remains a known gap.
- **No certificate transparency monitoring.** We do not monitor public CT logs for the issuance of certs matching our leaf CN from CAs outside the pinned Microsoft chain. Future work.
- **No binary transparency / Rekor publication.** Our release hashes are not published to a public transparency log. Future work.
- **Linux update freeze + verify→install TOCTOU.** Linux update *artifact* signing shipped in #653 (see the Linux section above) — but a feed-controlling attacker can still pin a victim to a known-vulnerable *signed* build via update-freeze, and a local `userData`-write attacker has a narrow verify→install swap window. Both are accepted residuals; freeze defense still needs a separate manifest-freshness/attestation control.
- **Chain validity is asserted at the leaf, not walked end-to-end by our hook.** `verifyWindowsSignature.ts` requires `Get-AuthenticodeSignature` to return `Status = Valid` on the downloaded installer — that status already covers signature integrity, root-of-trust chaining, expiry (including timestamp validity), and revocation via the Windows certificate store. On top of that, our hook inspects chain _structure_ to enforce the leaf CN allow-list and issuer-prefix pin. We do not separately walk or re-validate each intermediate; we trust the OS-level chain build that produced the `Valid` status. If that upstream check is ever bypassed, our structural checks alone would not detect a revoked or untrusted intermediate.

## Related documents

- [`[internal]specs/2026-04-15-644-update-trust-hardening-design.md`](../superpowers/specs/2026-04-15-644-update-trust-hardening-design.md) — design spec
- [`[internal]specs/2026-04-14-403-macos-code-signing-design.md`](../superpowers/specs/2026-04-14-403-macos-code-signing-design.md) — macOS as-shipped design
- [`SECURITY.md`](../../.github/SECURITY.md) — vulnerability reporting policy
