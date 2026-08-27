/**
 * SPA manifest verification PUBLIC keys (#1870), during rotation (#2958).
 *
 * These are the public halves of the RSA-4096 keypairs whose private halves
 * sign `spa-manifest.json` in the deploy pipeline. Public keys are safe to
 * commit and bake into the binary.
 *
 * ── DUAL TRUST (rotation window, closes 2026-11-27) ─────────────────────────
 * The client currently trusts TWO keys so the signing key can be rotated
 * without stranding already-shipped binaries:
 *
 *   V1  OUTGOING  private half lives in the GitHub `production` environment
 *                 secret `SPA_MANIFEST_SIGNING_KEY`. This is what the deploy
 *                 signs with TODAY.
 *   V2  INCOMING  private half lives in Infisical at
 *                 `prod:/sync/production/SPA_MANIFEST_SIGNING_KEY_V2`. Nothing
 *                 signs with it yet — it is pre-trusted so that when the deploy
 *                 switches over, clients already in the field accept it.
 *
 * A binary trusts the keys it was BUILT with, so the incoming key has to ship
 * and reach users BEFORE anything signs with it. That ordering is the whole
 * reason this list has two entries; retiring V1 on day one would brick every
 * client that had not yet updated.
 *
 * Retirement (#2958, on/after 2026-11-27, in this order):
 *   1. Point the deploy at V2 (`SPA_MANIFEST_SIGNING_KEY` <- the V2 private half).
 *   2. Confirm a real deploy verifies in the field.
 *   3. THEN delete `SPA_MANIFEST_PUBLIC_KEY_V1_PEM` and its array entry, and
 *      delete the GitHub secret. Doing 3 before 2 fails closed but takes the
 *      SPA cache down until the next client release.
 *
 * ── FAIL-CLOSED ────────────────────────────────────────────────────────────
 * With no non-blank key configured, `isSpaManifestKeyConfigured()` returns
 * false and the verifier refuses ALL manifests — the cache stays disabled and
 * the client falls back to remote->bundled exactly as before this feature
 * shipped. A placeholder key can NEVER falsely verify a manifest, and adding a
 * key to this list can only ever ADD an accepted signer, never bypass the
 * signature check itself.
 */

/** OUTGOING signer (#1870). Retire per #2958 once the deploy signs with V2. */
export const SPA_MANIFEST_PUBLIC_KEY_V1_PEM = `
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

/** INCOMING signer (#2958). Pre-trusted; nothing signs with it yet. */
export const SPA_MANIFEST_PUBLIC_KEY_V2_PEM = `
-----BEGIN PUBLIC KEY-----
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAkTKfO0U9umDgpHV03nv3
EJfwuGo4wLdUKDfMFqa8ThkmaEGiTVA/FqOKQB+H2nqfDBn9xOAj6/U2JHX2AHk7
y2Li6NIqzoOo7a2paBOglfDUSaNLmSi1ejS/qxhd39ynsyVMK46HhxOW7yF5C8Pl
oBdPCZ16GTqj1AkVNurOia85LKVJqclwdqA88YNFzQnQx/UMyf6/I7Z3R4gQK9UD
Qo/5ECowifO2/aThqONIYlLiyh5unZSIwNvrL/wzgDRraX/Wn9KrLsYbs9ngvER0
rnTqsIEklVzmfBLvPj8iJNadPw8kf67lINlV3lINC0mdxHobdpPhHBcniAgt6LKg
PjvnCkZ/Qpz6u8fwx+P8iEe+RrkNV4bX9Q9rebhQdqbE/HozmhDWJtWfCbMHA3c6
mxJfc83yuv1YR06/Kfnvq3LsK5Eaa4DYuStW3YqaWpdVLiJF9GKS+Z7pltTvo1Uj
QOkCwWh9OP2uf0utZCrOjMhg0nDtGh513rWJOQkna1isiakN9w06L1IXx6Hbi7eS
MN1ZVJ65hYR5QHN1Yi9ptVF3+AY8nz63WSCWvYbD7NQWOp+814Ua7H0J+fLYeZzi
1TlOksZ4wEuGniyTbpDDfwAfOptRHv8uzbW+xAMmy0VYLdgi/WKdCZzakveqQQCa
0v/GlAfhkC//60LHTkHxYPUCAwEAAQ==
-----END PUBLIC KEY-----
`;

/**
 * Every public key the client accepts as a manifest signer, most-current first.
 * A manifest is trusted when its signature verifies under ANY entry.
 */
export const SPA_MANIFEST_PUBLIC_KEYS_PEM: readonly string[] = [
  SPA_MANIFEST_PUBLIC_KEY_V2_PEM,
  SPA_MANIFEST_PUBLIC_KEY_V1_PEM,
];

/**
 * True only when at least one non-placeholder public key is configured.
 *
 * Filters blanks BEFORE testing emptiness: a list of only placeholders must
 * report "dormant", not "configured with a key that never verifies".
 */
export function isSpaManifestKeyConfigured(): boolean {
  return SPA_MANIFEST_PUBLIC_KEYS_PEM.some((pem) => pem.trim().length > 0);
}
