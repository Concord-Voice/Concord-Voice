/**
 * Linux update-artifact signing PUBLIC key (#653).
 *
 * Public half of the Ed25519 keypair whose private half signs every Linux
 * release artifact (.AppImage / .deb / .rpm) in `.github/workflows/build-desktop.yml`
 * (CI secret `LINUX_UPDATE_SIGNING_KEY`, repo scope — like the Apple/Azure
 * code-signing secrets). Public keys are safe to commit and bake into the binary.
 *
 * This is the SOLE trust anchor for Linux auto-update authenticity:
 * `verifyLinuxArtifact()` checks the downloaded AppImage's detached signature
 * against this key inside `safeQuitAndInstall()` before install. electron-updater
 * has no Linux signature hook (its `verifyUpdateCodeSignature` is Windows-only),
 * so this out-of-band check is what enforces update authenticity on Linux.
 *
 * ── NO PLACEHOLDER / FAIL-LOUD ─────────────────────────────────────────────
 * Unlike `spaCache/spaManifestPublicKey.ts` (which ships an empty placeholder and
 * fail-SAFE disables its cache), an update-security gate must NOT have a
 * skip-when-empty path: an empty or wrong key makes every Linux install fail
 * CLOSED (refuse), never fail open. The CI self-verify step re-checks each
 * signature against this same public key, so a key mismatch fails the release
 * build before any user is affected.
 *
 * Keypair created: 2026-06-27 (operator ceremony, #653).
 * Rotation: [internal]refresh-linux-update-key.md.
 * Mirror copy (CI self-verify input): infrastructure/signing/linux-update.pub.
 */
export const LINUX_UPDATE_PUBLIC_KEY_PEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAPT7VQyNKko24Tl7EhD9yUMkmK1N+/RJXp3ob1sP/51s=
-----END PUBLIC KEY-----
`;
