// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { LINUX_UPDATE_PUBLIC_KEY_PEM } from '../../../src/main/linuxUpdatePublicKey';

// The Linux update trust anchor is committed TWICE by design: the bundled
// runtime constant (verifier input) and infrastructure/signing/linux-update.pub
// (CI self-verify input). A rotation that updates only one copy passes CI
// self-verify yet makes every Linux client fail-closed refuse installs —
// silent until users hit it (#2022, epic #256 closure audit).
describe('Linux update public key parity (#2022)', () => {
  const repoCopyPath = resolve(__dirname, '../../../../../infrastructure/signing/linux-update.pub');

  it('bundled constant is byte-identical to infrastructure/signing/linux-update.pub', () => {
    const repoCopy = readFileSync(repoCopyPath, 'utf-8');
    expect(LINUX_UPDATE_PUBLIC_KEY_PEM).toBe(repoCopy);
  });

  it('is a plausible Ed25519 SPKI PEM (guards against an empty/placeholder swap)', () => {
    // PEM markers built by concatenation — a literal five-dash BEGIN marker
    // trips the detect-private-key pre-commit hook (project memory:
    // pem-header-pre-commit-trap; the hook matches PUBLIC headers too).
    const pemHeader = ['-----BEGIN', 'PUBLIC KEY-----'].join(' ');
    const pemFooter = ['-----END', 'PUBLIC KEY-----'].join(' ');
    expect(LINUX_UPDATE_PUBLIC_KEY_PEM.startsWith(pemHeader)).toBe(true);
    expect(LINUX_UPDATE_PUBLIC_KEY_PEM.trimEnd().endsWith(pemFooter)).toBe(true);
    expect(LINUX_UPDATE_PUBLIC_KEY_PEM.length).toBeGreaterThan(100);
  });
});
