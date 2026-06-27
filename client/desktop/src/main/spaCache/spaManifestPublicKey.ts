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
export const SPA_MANIFEST_PUBLIC_KEY_PEM = `
-----BEGIN PUBLIC KEY-----
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAhed4jelYZcSwE6SFPkEG
GgqCrxrOh4bjeU1xwCoBV/6eSucz+lJeSpdglGXxFgHgOP5/2dvAtbvhib+HHsNH
ofIbODN2UYANMHDD2hYRQBFQJ8yfL1IhYSmRnOXw4JEdTSdFUQVX4Vs5f27VlH0n
Fj13240pEMCPD49CX1bOvA2FvobLti2HJrDIwnH8q5rK/1PQjvh0xUqz2D3Bg5tS
yaZEPNzelsdpc1ibiYciGTlbFHxNTMWU1mGFOdLyEWxV92nGgqwBdh3nqrnJ3gO6
liofHz6apbhU/r1vDe1/n5f8QXH1eOIuVAV60Z8nWXRHtf2uDDJiIW20EosDnxDw
dUZCV6cJLamaoIC7i3IIknZco41p2bnv9lLKGw9KmT0+ibczLbmNNblsRfcwip+S
GDgAYQcatcZcD/sdAvYrvRU8Zk0dMasMEbD9mUeZRlANNlSi1a+1rr+7JhVWDFtZ
vCwyNjJyEWC21Lt7vMQv8kyLL110tv0WIteoHNaYXqLmFFp1TY7dwRodXFPq7bFa
cj7MUFmgLyE8Tma2dN1dar+cja2MLDWgSGBWnddtTnNwu7iuEUw/7ADQqkF2AtZw
SNKLa1s7XpnQNQ6GYlffptBBiOARneZMhQO9uc7scHrxqHmQaLPFcvnDmX/oONad
n0KoRpM0ehtGnDHzJtSt1DMCAwEAAQ==
-----END PUBLIC KEY-----
`;

/** True only when a non-placeholder public key is configured. */
export function isSpaManifestKeyConfigured(): boolean {
  return SPA_MANIFEST_PUBLIC_KEY_PEM.trim().length > 0;
}
