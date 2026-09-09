import path from 'node:path';

// Where the concord-audiocap native addon lives, in dev and in a packaged build
// (ADR-0043 PR 5, #3194).
//
// A packaged build ships the .node OUTSIDE app.asar via forge `extraResource`,
// because dlopen/LoadLibrary need a real path on disk and everything inside the
// archive is virtual. asar's `unpack`/`unpackDir` are not an alternative and never
// will be — see the asar comment block in forge.config.ts.
//
// This module is deliberately NOT named `ipc*`: /ipc-channel-audit globs
// src/main/ipc*, and a false-fire on a packaging change is noise that trains
// reviewers to ignore the signal.

/**
 * Env var carrying the resolved addon path from main to the utilityProcess child.
 *
 * Main is the only process that authoritatively knows `app.isPackaged` — a
 * utilityProcess child has no `app` module — so main resolves and the child reads.
 * The loader (native/concord-audiocap/index.js) hardcodes this same string; the
 * unit tests assert the two agree so they cannot silently drift apart.
 */
export const NATIVE_ADDON_ENV = 'CONCORD_AUDIOCAP_PATH';

const BINARY = 'concord_audiocap.node';

/**
 * ADR-0043 § Consequences puts Linux/PipeWire out of scope, so there is no addon
 * to find there. That makes `null` a legitimate outcome rather than an error.
 */
const SUPPORTED: readonly NodeJS.Platform[] = ['darwin', 'win32'];

/**
 * Resolve the native addon's path, mirroring `resolveTrayIconPath` in tray.ts.
 *
 * Pure and fully parameterised so it is unit-testable without a packaged app —
 * the same shape, and for the same reason, as the tray resolver it copies.
 *
 * The truthy guard on `resourcesPath` is load-bearing, not defensive noise:
 * Electron's type says it is always present, but at runtime it is `undefined` in
 * dev and under vitest. A mirror that trusts the type produces
 * `path.join(undefined, …)`, which throws only under packaging.
 *
 * @returns the absolute path to the .node, or `null` on an unsupported platform.
 *   Callers must read `null` as "no per-process audio on this machine" and never
 *   as a reason to widen capture to a system mix — that is #2161's defect.
 */
export function resolveNativeAddonPath(
  platform: NodeJS.Platform,
  isPackaged: boolean,
  resourcesPath: string | undefined,
  cwd: string
): string | null {
  if (!SUPPORTED.includes(platform)) return null;
  if (isPackaged && resourcesPath) {
    return path.join(resourcesPath, BINARY);
  }
  return path.join(cwd, 'native', 'concord-audiocap', 'build', 'Release', BINARY);
}
