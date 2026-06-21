// Forensic build-tag observability for incident response (#920 §5.13).
//
// Reads the build tag from `buildtag.json` (packaged via forge's
// `extraResource` mechanism, see `forge.config.ts`). In packaged builds the
// file lives at `process.resourcesPath/buildtag.json`; in dev/test it falls
// back to the working directory. Missing-file or malformed-JSON paths fall
// back to 'unknown' so this surface is always callable.
//
// Threat-model rationale (per #920 spec §5.13): the build tag is exposed via
// MAIN-PROCESS IPC only — never via `window.__BUILD_TAG__` or via
// `import.meta.env.VITE_BUILD_TAG` in renderer code. A compromised renderer
// dependency cannot exfiltrate the tag without an explicit, auditable IPC
// call. Knowing the build tag does not unlock any capability — this is
// observability, not secrecy.

import fs from 'node:fs';
import path from 'node:path';

let cachedTag: string | null = null;

/**
 * Structural type predicate for the `buildtag.json` payload shape.
 * Extracted from inline narrowing for readability and reuse if future
 * fields are added (e.g., a `version` field).
 */
function isBuildtagPayload(value: unknown): value is { tag: string } {
  return (
    typeof value === 'object' && value !== null && 'tag' in value && typeof value.tag === 'string'
  );
}

/**
 * Resolve the on-disk path to `buildtag.json`.
 *
 * `process.resourcesPath` is set by Electron in packaged builds to the
 * Resources directory. The Electron type augmentation declares it as
 * `string` globally — in Vitest / `tsc` / direct-node contexts the value
 * is undefined at runtime despite the type, so guard with a truthy check
 * and fall back to the current working directory (matching where
 * `generate-buildtag.mjs` writes the file).
 *
 * **In packaged builds** the cwd fallback is unreachable because Electron
 * always sets `process.resourcesPath`. The fallback exists solely for
 * Vitest / dev / direct-node contexts.
 *
 * Parameterized with default args so unit tests can exercise both branches
 * without mocking process globals.
 */
export function resolveBuildtagPath(
  resourcesPath: string | undefined = process.resourcesPath,
  cwd: string = process.cwd()
): string {
  return resourcesPath
    ? path.join(resourcesPath, 'buildtag.json')
    : path.resolve(cwd, 'buildtag.json');
}

/**
 * Returns the build tag baked into the packaged app at build time, or
 * 'unknown' for local dev builds where the file is absent or malformed.
 *
 * Memoized after the first call — buildtag.json never changes during a
 * single process lifetime, so repeated reads are wasted I/O. The cache
 * pins on first call regardless of outcome (string or 'unknown'), which
 * means a transient `EBUSY` or other recoverable error at startup will
 * permanently degrade the IPC contract to 'unknown' for the process
 * lifetime. This is acceptable for forensic observability — "unknown is
 * better than crashed" is the right contract for incident-response
 * tooling — but logged below so genuine failures are diagnosable.
 */
export function getBuildTag(): string {
  if (cachedTag !== null) return cachedTag;

  try {
    const buildtagPath = resolveBuildtagPath();
    if (!fs.existsSync(buildtagPath)) {
      cachedTag = 'unknown';
      return cachedTag;
    }
    const content = fs.readFileSync(buildtagPath, 'utf-8');
    const parsed = JSON.parse(content) as unknown;
    if (isBuildtagPayload(parsed)) {
      cachedTag = parsed.tag;
    } else {
      cachedTag = 'unknown';
    }
  } catch (err) {
    // Non-empty catch so genuine failures (EACCES, EIO, malformed
    // buildtag.json in a production build) are diagnosable rather than
    // silently degrading to 'unknown'. Per [internal]rules/observability.md,
    // log only the error message — never the raw err object (Error.cause
    // can carry secret material upward).
    const message = err instanceof Error ? err.message : 'buildtag_read_failed';
    // String-concatenate the message into the format string so the rule
    // forbidding bare identifiers as the final console.warn argument is
    // satisfied (see [internal]rules/observability.md "Console error logging").
    console.warn(`buildInfo: buildtag.json read failed, defaulting to unknown: ${message}`);
    cachedTag = 'unknown';
  }
  return cachedTag;
}

/**
 * Derive the 7-character commit hash (sha7) from a build tag.
 *
 * The build tag format from `.github/workflows/build-desktop.yml` is:
 *   - Release/push : `release-<sha8>`   → sha7 = first 7 chars of sha8
 *   - PR-mode      : `pr-smoke-<run-id>` → sha7 = ''   (not a commit-derived tag)
 *
 * The server's SPA attestation registry is keyed by sha7 (main-cd.yml publishes
 * with `GITHUB_SHA:0:7`), so bundled-mode attestation MUST send the sha7 form
 * rather than the build tag. Returns '' for tags that are not commit-derived;
 * the attestation middleware will surface `ATTESTATION_UNKNOWN_RELEASE` (a
 * terminal code) which is the correct posture for non-release builds.
 *
 * Pure function — exported only for direct callers that already have a tag.
 * Callers that want the runtime sha7 should use `getBuildSha7()` instead.
 */
export function deriveSha7FromBuildTag(tag: string): string {
  // `release-<sha8>` → return first 7 chars of the sha8 portion.
  const RELEASE_PREFIX = 'release-';
  if (tag.startsWith(RELEASE_PREFIX)) {
    const sha = tag.slice(RELEASE_PREFIX.length);
    // Defensive: require ≥7 lowercase-hex chars before treating as a commit hash.
    // A future workflow change that emits `release-foo` would otherwise yield
    // a 3-char "sha7" that the server cannot resolve. The hex predicate keeps
    // the failure mode loud (empty string → ATTESTATION_UNKNOWN_RELEASE) rather
    // than silent-corrupt.
    if (/^[a-f0-9]{7,}$/i.test(sha)) {
      return sha.slice(0, 7).toLowerCase();
    }
  }
  return '';
}

/**
 * Returns the 7-character commit hash (sha7) of the build, derived from the
 * baked `buildtag.json` tag. Returns '' for non-release builds (PR-smoke, dev,
 * malformed tag). Used by the SPA loader's bundled-mode path to populate the
 * `spa_version` attestation signal — the server's SPA registry is keyed by
 * sha7, not by build tag.
 */
export function getBuildSha7(): string {
  return deriveSha7FromBuildTag(getBuildTag());
}

let cachedCertHash: string | null = null;

/**
 * Structural type predicate for the `cert-hash.json` payload shape.
 * Mirrors `isBuildtagPayload` above.
 */
function isCertHashPayload(value: unknown): value is { cert_hash: string } {
  return (
    typeof value === 'object' &&
    value !== null &&
    'cert_hash' in value &&
    typeof (value as Record<string, unknown>).cert_hash === 'string'
  );
}

/**
 * Returns the SHA-256 code-signing-cert fingerprint baked into the packaged
 * app at build time by `.github/workflows/build-desktop.yml` (see Task 23),
 * read from `<resourcesPath>/cert-hash.json`. Dev builds legitimately lack
 * this file, so a read/parse failure returns '' (silent — unlike getBuildTag,
 * cert-hash absence is expected in dev and not diagnostic). Memoized for the
 * process lifetime (pins on first call regardless of outcome, matching
 * getBuildTag).
 *
 * Parameterized with a default arg (mirroring resolveBuildtagPath) so unit
 * tests can inject a resources path without mutating process globals.
 */
export function getBuildCertHash(
  resourcesPath: string | undefined = process.resourcesPath
): string {
  if (cachedCertHash !== null) return cachedCertHash;
  try {
    const filePath = path.join(resourcesPath ?? process.cwd(), 'cert-hash.json');
    const parsed = JSON.parse(fs.readFileSync(filePath, 'utf-8')) as unknown;
    cachedCertHash = isCertHashPayload(parsed) ? parsed.cert_hash : '';
  } catch {
    cachedCertHash = '';
  }
  return cachedCertHash;
}
