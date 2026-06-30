// Single source of truth for the production electron-updater feed URL.
//
// Consumed at build time by `scripts/generate-app-update.mts` (which writes
// `app-update.yml` into the packaged Resources dir) and at runtime by
// `src/main/updater.ts`. Keep this URL static/app-controlled: the API server
// must not be able to steer the desktop updater to an arbitrary feed.
//
// GitHub serves release assets through /releases/latest/download/<asset>, which
// electron-updater's generic provider consumes by appending latest*.yml and the
// artifact paths advertised by those manifests.

export { UPDATE_ENDPOINT_URL } from '../shared/updateEndpoint';
