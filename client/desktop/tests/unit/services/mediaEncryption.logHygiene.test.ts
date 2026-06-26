// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/**
 * #1895 spec §10.7 / OQ-7: lock the no-key-material-in-logs rule across the
 * media frame crypto modules. No console.* call may pass an identifier matching
 * iv / ciphertext / plaintext / key (except the literal indices keyId/keyVersion)
 * in a log-value position. A lightweight source scan (mirrors the Go AST test in
 * services/control-plane/internal/auth/log_emissions_test.go and the frontend
 * ESLint AST rule for no-raw-err-to-console — same discipline, different scope).
 */
const CRYPTO_SOURCES = [
  'src/renderer/services/mediaEncryption.ts',
  'src/renderer/services/mediaFrameMiniHeader.ts',
  'src/renderer/services/av1ObuParser.ts',
];

// Forbidden value-name patterns inside any console.* / log() call argument list.
const FORBIDDEN = /\b(iv|ciphertext|plaintext|cskBytes|keyBytes|rawKey)\b/;

describe('media crypto log hygiene (#1895 §10.7)', () => {
  for (const rel of CRYPTO_SOURCES) {
    it(`${rel} emits no IV/ciphertext/plaintext/key bytes to a log sink`, () => {
      const src = readFileSync(resolve(__dirname, '../../../', rel), 'utf8');
      // Find console.* and log(...) call argument spans and assert none reference
      // a forbidden identifier. Keyword keyId/keyVersion are explicitly allowed.
      const logCalls =
        src.match(/(console\.(log|debug|info|warn|error)|[^.\w]log)\s*\([^;]*?\)/gs) ?? [];
      for (const call of logCalls) {
        const withoutAllowed = call.replace(/\bkey(Id|Version)\b/g, '');
        expect(FORBIDDEN.test(withoutAllowed), `forbidden token in: ${call}`).toBe(false);
      }
    });
  }
});
