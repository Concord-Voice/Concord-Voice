/**
 * SPA manifest verification PUBLIC key (#1870).
 *
 * This is the public half of the RSA-4096 keypair whose private half signs
 * `spa-manifest.json` in the deploy pipeline (env `SPA_MANIFEST_SIGNING_KEY`,
 * consumed only by the `deploy-spa` CI job). Public keys are safe to commit and
 * bake into the binary.
 *
 * ── ACTIVATION (operator, one-time key ceremony) ───────────────────────────
 * The cache is DORMANT until a real keypair exists. To activate:
 *   1. Run `scripts/gen-spa-signing-key.sh` (generates RSA-4096).
 *   2. `gh secret set SPA_MANIFEST_SIGNING_KEY < spa-signing-key.pem` (production env).
 *   3. Replace `SPA_MANIFEST_PUBLIC_KEY_PEM` below with the printed public key.
 *   4. Rebuild + ship the desktop client; redeploy the SPA.
 * See [internal]spa-manifest-signing.md.
 *
 * ── FAIL-CLOSED ────────────────────────────────────────────────────────────
 * While the placeholder (empty) value is in place, `isSpaManifestKeyConfigured()`
 * returns false and the verifier refuses ALL manifests — the cache stays
 * disabled and the client falls back to remote→bundled exactly as before this
 * feature shipped. A placeholder key can NEVER falsely verify a manifest.
 */

/**
 * PEM-encoded RSA-4096 SPKI public key, or '' (placeholder) before the operator
 * runs the key ceremony. MUST begin with the SPKI header once configured.
 */
export const SPA_MANIFEST_PUBLIC_KEY_PEM = '';

/** True only when a non-placeholder public key is configured. */
export function isSpaManifestKeyConfigured(): boolean {
  return SPA_MANIFEST_PUBLIC_KEY_PEM.trim().length > 0;
}
