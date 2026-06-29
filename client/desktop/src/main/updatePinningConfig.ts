import type { PinConfig } from './updatePinning';

// ⚠ SECURITY-CRITICAL — TLS pin configuration for the update feed. ⚠
//
// This file is the sole source of truth for which TLS certs are trusted to
// serve the Concord Voice electron-updater feed on api.concordvoice.chat.
// Every change requires:
//   1. PR description linking to [internal]
//   2. Security reviewer sign-off (enforced via CODEOWNERS)
//   3. The runbook's operational checks (SPKI computation, CF upload)
//
// Computing a new SPKI value:
//   node scripts/compute-spki.mjs <path-to-cert.pem>
//
// Semgrep rule `concord-cert-pinning-config-import-scope` restricts imports of
// this file to updatePinning.ts + main.ts + tests.

export const PIN_CONFIG: PinConfig = {
  pinnedHosts: ['api.concordvoice.chat'] as const,
  primaryPins: [
    // Emergency 2026-06-29: Cloudflare edge cert renewed with a new keypair.
    // Primary avoids fallback-warning spam on every API request. The previous
    // production pin below is rollback-only and test-guarded to expire by 2026-07-29.
    '6f7894c8ade945ca564b5c26cd0bae8f2994bd417da5d25e4c29e9b4564a5ac2', // pragma: allowlist secret
    // Previous production leaf SPKI SHA-256 (Let's Encrypt E7, via CloudFlare edge).
    // Measured 2026-05-08 — replaces pre-cutover pin (53257bb4...) after the
    // cert was renewed during the #885 webroot-certbot switch. The renewal
    // generated a new keypair instead of following the rotation runbook's
    // backup-keypair swap procedure (#899); a future PR (V1.7) will regenerate
    // the backup keypair and engage the proper rotation invariant. This is a
    // public-key fingerprint, not a secret — reproducible by anyone connecting
    // to the host:
    //   echo | openssl s_client -connect api.concordvoice.chat:443 \
    //     -servername api.concordvoice.chat 2>/dev/null \
    //     | openssl x509 -pubkey -noout | openssl pkey -pubin -outform der \
    //     | openssl dgst -sha256 -binary | xxd -p | tr -d '\n'
    '0a4ccc0dfc2c60c67e4b814292467bbf7e525d6b75d38e32ea646153fc7c49f2', // pragma: allowlist secret
  ] as const,
  fallbackPins: [
    // Pre-staged backup keypair SPKI SHA-256. The corresponding RSA-4096
    // private key was generated offline during the Appendix A key ceremony
    // (2026-04-20) and is stored exclusively in the ops vault — it is never
    // committed to this repo, never on any VM, and never in CI secrets.
    //
    // This pin only comes into play during a planned key-rotation cycle: when
    // the primary cert's keypair is rotated, the backup key is retrieved from
    // the vault, a new LE cert is issued against it via the runbook procedure,
    // and the roles swap (see [internal] §4).
    //
    // If you change this value, the rotation runbook MUST be updated in the
    // same PR so the swap procedure references the correct vault identifier.
    'b03ab5df6d5ff66d9ab54ea0ef83e032a14c533e973edb70f8d24214e70eeb37', // pragma: allowlist secret
  ] as const,
} as const;
