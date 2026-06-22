// Single source of truth for the production electron-updater feed URL.
//
// Consumed at build time by `scripts/generate-app-update.mts` (which writes
// `app-update.yml` into the packaged Resources dir). Runtime code in
// `src/main/updater.ts` derives its own feed URL from the user's connected
// server (`apiBase`), so it does NOT import this constant — the only string
// `https://api.concordvoice.chat/api/v1/updates` lives here, in the workflow
// generator output, and in the packaged `app-update.yml` artifact.
//
// The grep-audit guarantee is:
//   git grep -F 'https://api.concordvoice.chat/api/v1/updates' \
//     client/desktop/src client/desktop/scripts .github/workflows
//   → returns exactly one source location: this file.
//
// If you change this URL, verify the host change is also reflected in
// `src/main/updatePinningConfig.ts` (TLS-pinned host list) — the pinning
// runbook documents the coordinated rotation procedure.

export const UPDATE_ENDPOINT_URL = 'https://api.concordvoice.chat/api/v1/updates' as const;
