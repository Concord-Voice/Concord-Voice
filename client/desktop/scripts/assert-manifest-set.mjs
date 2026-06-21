#!/usr/bin/env node
import { existsSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Assert that a target directory contains every required manifest filename.
 * Sibling script to verify-update-manifest.mjs. This script does NOT verify
 * content or sha512 — it only asserts presence. Content/structure validation
 * is the verifier's responsibility (#920 §5.7).
 *
 * #1009 L2: fail-loud guard for the per-platform expected-set contract.
 *
 * @param {{ dir: string, require: string[] }} args
 * @returns {{ ok: boolean, errors: string[] }}
 */
export function assertManifestSet({ dir, require }) {
  const errors = [];

  if (!existsSync(dir)) {
    return { ok: false, errors: [`Target directory ${dir} does not exist`] };
  }
  if (!statSync(dir).isDirectory()) {
    return { ok: false, errors: [`Target ${dir} exists but is not a directory`] };
  }

  for (const name of require) {
    const full = path.join(dir, name);
    if (!existsSync(full)) {
      errors.push(`Required manifest ${name} is missing`);
    }
  }

  return { ok: errors.length === 0, errors };
}

// CLI entry. Mirrors verify-update-manifest.mjs's guard pattern.
/* istanbul ignore next -- thin CLI entry shim; assertManifestSet itself is covered by tests */
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const args = process.argv.slice(2);
  let dir;
  let require;
  for (const arg of args) {
    if (arg.startsWith('--dir=')) {
      dir = arg.slice('--dir='.length);
    } else if (arg.startsWith('--require=')) {
      require = arg
        .slice('--require='.length)
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);
    }
  }
  if (!dir || !require || require.length === 0) {
    console.error('Usage: assert-manifest-set.mjs --dir=<path> --require=<a.yml,b.yml,...>');
    process.exit(2);
  }
  const result = assertManifestSet({ dir, require });
  if (result.ok) {
    console.log(`OK: all ${require.length} required manifest(s) present in ${dir}`);
    process.exit(0);
  }
  for (const e of result.errors) {
    console.error(`::error::${e}`);
  }
  process.exit(1);
}
