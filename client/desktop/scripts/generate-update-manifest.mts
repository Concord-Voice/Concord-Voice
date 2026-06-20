#!/usr/bin/env -S npx tsx
// Generator for electron-updater manifests (latest.yml / latest-mac.yml /
// latest-linux.yml), validated against schemas/update-manifest.json at
// write time.
//
// Replaces the prior heredoc-based manifest assembly in build-desktop.yml's
// "Generate update manifests" step (#920 §5.7). The schema is the single
// source of truth shared with scripts/verify-update-manifest.mjs.
//
// CLI usage:
//   tsx scripts/generate-update-manifest.mts <version> <output> <file> [<file> ...]
//
// Example:
//   tsx scripts/generate-update-manifest.mts 0.1.36 artifacts/latest.yml \
//     artifacts/concord-voice-windows-x64-v0.1.36/ConcordVoice-0.1.36-windows-x64-Setup.exe
//
// The script:
//   1. Loads schemas/update-manifest.json
//   2. Computes sha512_b64 + size for each file via Node crypto/fs
//   3. Assembles the manifest object
//   4. Validates against the schema via ajv (Draft 2020-12)
//   5. Writes YAML to the output path
//
// Exit codes:
//   0 — manifest written and schema-valid
//   1 — schema violation OR I/O error
//   2 — CLI usage error

import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import { readFileSync, writeFileSync, statSync, existsSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { dirname, basename, resolve as pathResolve } from 'node:path';
import { stringify as yamlStringify } from 'yaml';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SCHEMA_PATH = pathResolve(__dirname, '../schemas/update-manifest.json');

export interface ManifestEntry {
  url: string;
  sha512: string;
  size: number;
  // #1292: byte size of the sibling <url>.blockmap (macOS differential-update
  // block manifest). Present only for entries that have a .blockmap on disk.
  blockMapSize?: number;
}

export interface Manifest {
  version: string;
  files: ManifestEntry[];
  releaseDate: string;
}

export class ManifestSchemaError extends Error {
  constructor(public errors: unknown) {
    super(`update-manifest schema violation: ${JSON.stringify(errors)}`);
    this.name = 'ManifestSchemaError';
  }
}

function loadSchema(): object {
  return JSON.parse(readFileSync(SCHEMA_PATH, 'utf-8'));
}

const ajv = new Ajv2020({ allErrors: true, strict: false });
addFormats(ajv);
const validateFn = ajv.compile(loadSchema());

export function validateManifest(manifest: unknown): asserts manifest is Manifest {
  if (!validateFn(manifest)) {
    throw new ManifestSchemaError(validateFn.errors);
  }
}

export function sha512Base64(filePath: string): string {
  return createHash('sha512').update(readFileSync(filePath)).digest('base64');
}

export function buildManifest(opts: {
  version: string;
  files: string[];
  releaseDate?: string;
}): Manifest {
  const releaseDate = opts.releaseDate ?? new Date().toISOString();
  const manifest: Manifest = {
    version: opts.version,
    files: opts.files.map((f) => {
      const entry: ManifestEntry = {
        url: basename(f),
        sha512: sha512Base64(f),
        size: statSync(f).size,
      };
      // #1292: the release-job app-builder step writes <file>.blockmap next to
      // each macOS .zip. Advertise its byte size as blockMapSize so
      // electron-updater can perform differential downloads. Platforms without
      // a blockmap (Windows/Linux) leave the field absent.
      const blockMapPath = `${f}.blockmap`;
      if (existsSync(blockMapPath)) {
        entry.blockMapSize = statSync(blockMapPath).size;
      }
      return entry;
    }),
    releaseDate,
  };
  validateManifest(manifest);
  return manifest;
}

export function renderManifestYaml(manifest: Manifest): string {
  return yamlStringify(manifest);
}

/* istanbul ignore next -- CLI usage helper, only invoked from the entry shim below */
function usage(): never {
  process.stderr.write(
    'Usage: generate-update-manifest.mts <version> <output-yaml> <file> [<file> ...]\n',
  );
  process.exit(2);
}

/* istanbul ignore next -- thin CLI entry shim (argv parsing + exit codes); library
   functions buildManifest/renderManifestYaml/validateManifest are covered by tests */
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const [, , version, output, ...files] = process.argv;
  if (!version || !output || files.length === 0) usage();

  try {
    const manifest = buildManifest({ version, files });
    const yaml = renderManifestYaml(manifest);
    writeFileSync(output, yaml);
    process.stdout.write(`Wrote ${output} (${manifest.files.length} file${manifest.files.length === 1 ? '' : 's'})\n`);
  } catch (err) {
    if (err instanceof ManifestSchemaError) {
      process.stderr.write(`::error::${err.message}\n`);
      process.exit(1);
    }
    const msg = err instanceof Error ? err.message : String(err);
    process.stderr.write(`::error::generate-update-manifest failed: ${msg}\n`);
    process.exit(1);
  }
}
