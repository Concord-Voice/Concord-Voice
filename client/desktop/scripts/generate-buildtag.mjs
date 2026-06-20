#!/usr/bin/env node
// Generator for client/desktop/buildtag.json (#920 §5.13).
//
// Reads CONCORD_BUILD_TAG from process.env (or from a sibling .env file in
// the `client/desktop/` working directory if the env var is unset) and writes
// the build tag into `buildtag.json` so the main process can expose it via
// IPC for incident-response forensics.
//
// Naming: deliberately NOT `VITE_*`-prefixed. The §5.13 threat model requires
// the tag to be main-process-only — a `VITE_*` name would be processed by
// Vite's dotenv loader and exposed via `import.meta.env` to renderer source
// (and inlined into the bundle if any renderer source references it). Using
// `CONCORD_BUILD_TAG` keeps Vite hands-off; only `generate-buildtag.mjs`
// reads it, and only the main-process IPC path consumes the resulting
// `buildtag.json`. Renderer source MUST NOT reference either name —
// asserted by the grep-audit unit tests in tests/unit/main/buildInfo.test.ts.
//
// Invoked from `.github/workflows/build-desktop.yml`'s dedicated
// "Generate buildtag.json for forensic observability" step (between the
// Apple API key setup and the forge make step), so the file is picked up
// by forge's `extraResource` and ends up at
// `process.resourcesPath/buildtag.json` in the packaged app.
//
// Exposes a pure `resolveBuildTag()` helper for unit testing; the CLI shim
// at the bottom runs only when invoked directly.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Resolve the build tag from explicit env, then `.env` fallback, then 'unknown'.
 * Pure: side-effect-free, parameterized for tests.
 *
 * Both sources are trimmed for consistency — `CONCORD_BUILD_TAG` is often
 * copied from CI logs or shell history with leading/trailing whitespace.
 */
export function resolveBuildTag(env, envFileContent) {
  if (env && typeof env.CONCORD_BUILD_TAG === 'string') {
    const trimmed = env.CONCORD_BUILD_TAG.trim();
    if (trimmed.length > 0) return trimmed;
  }
  if (envFileContent) {
    const match = envFileContent.match(/^CONCORD_BUILD_TAG=(.+)$/m);
    if (match) {
      const trimmed = match[1].trim();
      if (trimmed.length > 0) return trimmed;
    }
  }
  return 'unknown';
}

/**
 * Format the buildtag.json payload. Kept separate for unit tests.
 */
export function formatBuildtagJson(tag) {
  return JSON.stringify({ tag }, null, 2) + '\n';
}

/* istanbul ignore next -- thin CLI entry shim; helpers above are covered by tests */
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const cwd = process.cwd();
  const envPath = path.resolve(cwd, '.env');
  const envFileContent = fs.existsSync(envPath) ? fs.readFileSync(envPath, 'utf-8') : null;
  const tag = resolveBuildTag(process.env, envFileContent);
  // Fail-loud in CI: a packaged build with tag='unknown' silently strips
  // forensic identification from production artifacts. The workflow's
  // Configure-build-environment step is responsible for setting
  // CONCORD_BUILD_TAG; if it hasn't, exit non-zero so the build aborts.
  // (#920 §5.13 silent-failure-hunter F2.)
  if (tag === 'unknown' && process.env.CI === 'true') {
    console.error(
      'generate-buildtag: CONCORD_BUILD_TAG is unset in CI — refusing to emit a buildtag.json with tag=unknown. ' +
        'Set CONCORD_BUILD_TAG in the workflow before this step runs.',
    );
    process.exit(1);
  }
  const outputPath = path.resolve(cwd, 'buildtag.json');
  fs.writeFileSync(outputPath, formatBuildtagJson(tag));
  console.log(`Wrote ${outputPath} with tag=${tag}`);
}
