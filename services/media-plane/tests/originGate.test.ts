import { describe, it, expect, vi } from 'vitest';
import { createOriginGate } from '../src/lib/originGate.js';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Creates a Vitest spy that can be inspected after calling the origin gate. */
function makeCallback() {
  return vi.fn<(err: Error | null, allow?: boolean) => void>();
}

const ALLOWLIST = ['app://concord', 'http://localhost:3001'];

// ---------------------------------------------------------------------------
// createOriginGate
// ---------------------------------------------------------------------------

describe('createOriginGate', () => {
  const gate = createOriginGate(ALLOWLIST);

  it('origin-gate-allows-no-origin: allows undefined origin (curl / internal harness)', () => {
    const callback = makeCallback();
    gate(undefined, callback);
    expect(callback).toHaveBeenCalledWith(null, true);
  });

  it('origin-gate-rejects-null: rejects the literal string "null" (sandboxed iframes, data: URLs)', () => {
    const callback = makeCallback();
    gate('null', callback);
    const [err] = callback.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
    expect(err?.message).toContain('null');
  });

  it('origin-gate-rejects-file-protocol: rejects "file://" origin (legacy Electron pre-#830)', () => {
    const callback = makeCallback();
    gate('file://', callback);
    const [err] = callback.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
    expect(err?.message).toContain('file://');
  });

  it('origin-gate-allows-allowlisted-origin: allows "app://concord" when it is in the allowlist', () => {
    const callback = makeCallback();
    gate('app://concord', callback);
    expect(callback).toHaveBeenCalledWith(null, true);
  });

  it('origin-gate-rejects-unknown-origin: rejects an origin not in the allowlist', () => {
    const callback = makeCallback();
    gate('https://evil.example', callback);
    const [err] = callback.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
    expect(err?.message).toContain('https://evil.example');
  });

  it('allows wildcard "*" in allowlist to pass any origin through', () => {
    const wildcardGate = createOriginGate(['*']);
    const callback = makeCallback();
    wildcardGate('https://any.example.com', callback);
    expect(callback).toHaveBeenCalledWith(null, true);
  });

  it('allows an additional allowlisted origin (localhost:3001)', () => {
    const callback = makeCallback();
    gate('http://localhost:3001', callback);
    expect(callback).toHaveBeenCalledWith(null, true);
  });

  // ── Edge cases added during /reconcile-copilot for PR #1083 ──────────────
  // (security-reviewer L1 + pr-test-analyzer gap closure)

  it('origin-gate-allows-empty-string-as-no-origin: empty string falls through !origin check', () => {
    const callback = makeCallback();
    gate('', callback);
    // Empty string is falsy → hits the !origin allow branch. Pins behavior; if
    // a future refactor changes !origin to `origin === undefined`, this test
    // flips and the contract change becomes visible.
    expect(callback).toHaveBeenCalledWith(null, true);
  });

  it('origin-gate-rejects-uppercase-NULL: case-insensitive normalization catches NULL', () => {
    const callback = makeCallback();
    gate('NULL', callback);
    const [err] = callback.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
    // Error preserves the original casing for log fidelity, but the reject
    // decision is normalized — this defends against non-browser clients
    // crafting case variants to bypass the gate.
    expect(err?.message).toContain('NULL');
  });

  it('origin-gate-rejects-mixed-case-File-protocol: case-insensitive normalization catches File://', () => {
    const callback = makeCallback();
    gate('File://', callback);
    const [err] = callback.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
    expect(err?.message).toContain('File://');
  });

  it('origin-gate-rejects-whitespace-padded-null: trim normalizes leading/trailing whitespace', () => {
    const callback = makeCallback();
    gate(' null ', callback);
    const [err] = callback.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
  });

  it('origin-gate-rejects-all-when-allowlist-empty: empty allowlist defaults to deny', () => {
    const emptyGate = createOriginGate([]);
    const callback = makeCallback();
    emptyGate('https://example.com', callback);
    const [err] = callback.mock.calls[0];
    expect(err).toBeInstanceOf(Error);
    // Secure-by-default: empty config means no origin is allowlisted, not "allow all".
  });
});
