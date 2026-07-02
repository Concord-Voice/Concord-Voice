#!/usr/bin/env -S npx tsx
// Generator for client/desktop/app-update.yml.
//
// Reads UPDATE_ENDPOINT_URL from src/constants/updateEndpoint.mts and emits
// the electron-updater app-update.yml YAML to stdout. Invoked from
// .github/workflows/build-desktop.yml (Generate app-update.yml step) via
// `npx tsx scripts/generate-app-update.mts > app-update.yml`.
//
// Exposes a pure `renderAppUpdateYaml()` for unit testing; the CLI shim at
// the bottom runs the renderer only when invoked directly.

import { UPDATE_ENDPOINT_URL } from '../src/constants/updateEndpoint.mts';
import { ALLOWED_WINDOWS_PUBLISHERS } from '../src/constants/allowedWindowsPublishers.mts';
import { stringify } from 'yaml';
import { fileURLToPath } from 'node:url';

export function renderAppUpdateYaml(url: string = UPDATE_ENDPOINT_URL): string {
  // updaterCacheDirName pins electron-updater's cache subdir to a stable,
  // spaceless name, independent of app.getName()/productName. Without it the
  // updater derived the cache dir from the (mutable) product name and churned it
  // across the v0.1.38 rename. See ADR-0020 D1 + [internal]rules/electron.md.
  //
  // publisherName arms NsisUpdater.verifySignature: electron-updater reads the
  // Windows publisher allow-list ONLY from this on-disk file (never from the
  // runtime setFeedURL call), so omitting it silently disables install-time
  // Authenticode verification and the #644 issuer-pin hook (#2020).
  return stringify({
    provider: 'generic',
    url,
    updaterCacheDirName: 'ConcordVoice',
    publisherName: [...ALLOWED_WINDOWS_PUBLISHERS],
  });
}

/* istanbul ignore next -- thin CLI entry shim; renderAppUpdateYaml is covered by tests */
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.stdout.write(renderAppUpdateYaml());
}
