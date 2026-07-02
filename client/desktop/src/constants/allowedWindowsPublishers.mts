// Windows publisher allow-list, single source of truth.
//
// Consumed at build time by `scripts/generate-app-update.mts` (via tsx, which
// needs this .mts ESM entry) and at runtime by `src/main/updater.ts` via the
// sibling `../shared/allowedWindowsPublishers` module. Keep this list
// app-controlled: it is what arms electron-updater's Windows install-time
// Authenticode gate (#2020).

export { ALLOWED_WINDOWS_PUBLISHERS } from '../shared/allowedWindowsPublishers';
