#!/usr/bin/env node
import { existsSync, statSync, readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Assert that a directory TREE contains at least one file whose basename
 * matches every required glob pattern. Sibling to assert-manifest-set.mjs,
 * but matches globs against the RECURSIVE basename set (binary artifacts are
 * nested under per-leg bundle dirs and carry version/arch in the name),
 * whereas assert-manifest-set matches exact flat filenames. (#643)
 *
 * @param {{ dir: string, require: string[] }} args
 * @returns {{ ok: boolean, errors: string[] }}
 */
export function assertBinaryAssetSet({ dir, require }) {
  if (!existsSync(dir)) {
    return { ok: false, errors: [`Target directory ${dir} does not exist`] };
  }
  if (!statSync(dir).isDirectory()) {
    return { ok: false, errors: [`Target ${dir} exists but is not a directory`] };
  }

  const basenames = [];
  const walk = (d) => {
    for (const entry of readdirSync(d, { withFileTypes: true })) {
      const full = path.join(d, entry.name);
      if (entry.isDirectory()) walk(full);
      else basenames.push(entry.name);
    }
  };
  walk(dir);

  const errors = [];
  for (const pattern of require) {
    if (!basenames.some((b) => globMatch(pattern, b))) {
      errors.push(`No artifact matches required pattern ${pattern}`);
    }
  }
  return { ok: errors.length === 0, errors };
}

/**
 * Match a string against a minimal glob whose ONLY metacharacter is `*`.
 * Splits on `*` and verifies the literal segments appear in order — a pure
 * linear string scan (startsWith / indexOf / endsWith). Deliberately NOT
 * regex-based: there is no backtracking surface, so it is structurally free
 * of ReDoS (CWE-1333) even though our patterns are CI-authored, not user
 * input. Case-insensitive (artifact names vary in case across tools).
 *
 * @param {string} glob e.g. "*arm64*.zip"
 * @param {string} str  candidate basename
 * @returns {boolean}
 */
export function globMatch(glob, str) {
  const hay = str.toLowerCase();
  const parts = glob.toLowerCase().split('*');
  if (parts.length === 1) return hay === parts[0]; // no '*' → exact match
  const first = parts[0];
  const last = parts[parts.length - 1];
  if (!hay.startsWith(first)) return false;
  if (hay.length < first.length + last.length) return false; // prefix+suffix would overlap
  if (!hay.endsWith(last)) return false;
  const suffixStart = hay.length - last.length;
  let cursor = first.length;
  for (let i = 1; i < parts.length - 1; i++) {
    const seg = parts[i];
    if (seg === '') continue;
    const found = hay.indexOf(seg, cursor);
    if (found === -1 || found + seg.length > suffixStart) return false;
    cursor = found + seg.length;
  }
  return true;
}

// CLI entry. Mirrors assert-manifest-set.mjs's guard pattern + exit codes.
/* istanbul ignore next -- thin CLI entry shim; assertBinaryAssetSet itself is covered by tests */
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
    console.error('Usage: assert-binary-asset-set.mjs --dir=<path> --require=<glob1,glob2,...>');
    process.exit(2);
  }
  const result = assertBinaryAssetSet({ dir, require });
  if (result.ok) {
    console.log(`OK: all ${require.length} required binary pattern(s) matched in ${dir}`);
    process.exit(0);
  }
  for (const e of result.errors) {
    console.error(`::error::${e}`);
  }
  process.exit(1);
}
