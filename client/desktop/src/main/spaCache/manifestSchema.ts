/**
 * SPA last-known-good (LKG) cache — shared manifest contract (#1870).
 *
 * This module is the SINGLE SOURCE OF TRUTH for the signed SPA manifest shape,
 * the signature algorithm parameters, the cache scheme/paths, and the safety
 * bounds. It is consumed by BOTH sides of the trust boundary:
 *
 *   - Producer: `client/desktop/scripts/sign-spa-manifest.mjs` (CI/deploy) reads
 *     the field shape + algorithm to build and sign `spa-manifest.json`.
 *   - Consumer: `src/main/spaCache/*` (Electron main) reads the zod schema +
 *     algorithm to verify and validate a fetched manifest.
 *
 * Signature design — DETACHED signature over RAW manifest bytes (NOT a
 * canonicalized re-serialization). The deploy publishes `spa-manifest.json`
 * and a sibling `spa-manifest.json.sig` (base64 of the RSA-PSS signature over
 * the exact bytes of `spa-manifest.json`). The verifier checks the signature
 * over the bytes it fetched VERBATIM and only then parses the JSON — so there
 * is no cross-context canonicalization to drift. See the #1870 design spec
 * "Signed LKG Cache (Full-Vertical Implementation)".
 */

import { z } from 'zod';

/** Manifest schema version. Bump only on a breaking field-shape change. */
export const SPA_MANIFEST_SCHEMA_VERSION = 1;

/** Published artifact filenames (served next to the SPA on Cloudflare Pages). */
export const SPA_MANIFEST_FILENAME = 'spa-manifest.json';
export const SPA_MANIFEST_SIG_FILENAME = 'spa-manifest.json.sig';

/**
 * Privileged scheme + host for serving the VERIFIED cache. Distinct from
 * `app://concord` (bundled) and never generic `file://` — per the #1870
 * trust-limits ("Serve from a distinct verified cache scheme/path").
 */
export const SPA_CACHE_SCHEME = 'spa-cache';
export const SPA_CACHE_HOST = 'concord';

/**
 * RSA-PSS / SHA-256 / saltLength 32 — matches the age-claim signing scheme
 * ([internal]rules/e2ee.md #1624) and the project's "RSA-4096" minimum.
 * Shared by signer (`crypto.sign`) and verifier (`crypto.verify`).
 */
export const SPA_MANIFEST_SIGN_ALGORITHM = 'sha256' as const;
export const SPA_MANIFEST_SIGN_SALT_LENGTH = 32 as const;
/** node:crypto constants.RSA_PKCS1_PSS_PADDING — re-declared to avoid importing
 *  electron/node constants into the shared contract surface. */
export const SPA_MANIFEST_RSA_PSS_PADDING = 6 as const; // crypto.constants.RSA_PKCS1_PSS_PADDING

// ── Safety bounds (reject anything beyond these) ───────────────────────────
/** Max bytes of the manifest JSON itself (DoS guard on the verifier). */
export const SPA_MANIFEST_MAX_BYTES = 1_000_000; // 1 MB
/** Per-file (entry or asset) byte cap. */
export const SPA_CACHE_MAX_FILE_BYTES = 25_000_000; // 25 MB
/** Total cache byte cap (sum of entry + assets). */
export const SPA_CACHE_MAX_TOTAL_BYTES = 100_000_000; // 100 MB
/** Max number of asset entries. */
export const SPA_CACHE_MAX_ASSETS = 2_000;
/** Bounded staleness: reject a manifest older than this (do not run ancient bytes). */
export const SPA_CACHE_MAX_STALENESS_MS = 30 * 24 * 60 * 60 * 1000; // 30 days

const SHA256_HEX = /^[a-f0-9]{64}$/;

/**
 * A manifest path is a RELATIVE, forward-slash POSIX path inside the SPA dist.
 * Reject absolute paths, drive letters, backslashes, `..` traversal segments,
 * leading slashes, and empty segments. The cache protocol handler re-checks
 * path safety at serve time (defense in depth), but rejecting here keeps a
 * malformed manifest from ever being promoted.
 */
const ManifestPath = z
  .string()
  .min(1)
  .max(1024)
  .refine((p) => !p.startsWith('/') && !p.includes('\\') && !/^[a-zA-Z]:/.test(p), {
    message: 'path must be relative (no leading slash, drive letter, or backslash)',
  })
  .refine((p) => !p.split('/').some((seg) => seg === '..' || seg === '' || seg === '.'), {
    message: 'path must not contain traversal, empty, or dot segments',
  });

const ManifestFile = z.object({
  path: ManifestPath,
  sha256: z.string().regex(SHA256_HEX, 'sha256 must be 64 lowercase hex chars'),
  size: z.number().int().nonnegative().max(SPA_CACHE_MAX_FILE_BYTES),
});

export const SpaManifestSchema = z
  .object({
    schemaVersion: z.literal(SPA_MANIFEST_SCHEMA_VERSION),
    buildId: z
      .string()
      .min(1)
      .max(64)
      .regex(/^[a-zA-Z0-9._-]+$/, 'buildId must be a safe identifier'),
    spaIpcContract: z.number().int().positive(),
    generatedAt: z.string().datetime({ message: 'generatedAt must be ISO-8601 UTC' }),
    entry: ManifestFile,
    assets: z.array(ManifestFile).max(SPA_CACHE_MAX_ASSETS),
    totalSize: z.number().int().nonnegative().max(SPA_CACHE_MAX_TOTAL_BYTES),
  })
  .strict();

export type SpaManifest = z.infer<typeof SpaManifestSchema>;
export type SpaManifestFile = z.infer<typeof ManifestFile>;
