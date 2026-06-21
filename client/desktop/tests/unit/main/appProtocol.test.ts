import { describe, expect, it } from 'vitest';
import path from 'node:path';
import { resolveAppProtocolPath } from '../../../src/main/appProtocol';

const BUNDLE_ROOT = path.resolve('/app/Contents/Resources/app.asar/dist/renderer');

describe('resolveAppProtocolPath', () => {
  it('returns 404 for malformed URL', () => {
    const result = resolveAppProtocolPath('not a url', BUNDLE_ROOT);
    expect(result.ok).toBe(false);
    expect(result.status).toBe(404);
  });

  it('returns 404 for wrong host', () => {
    const result = resolveAppProtocolPath('app://other/index.html', BUNDLE_ROOT);
    expect(result.ok).toBe(false);
    expect(result.status).toBe(404);
  });

  it('resolves app://concord/index.html to bundle root + /index.html', () => {
    const result = resolveAppProtocolPath('app://concord/index.html', BUNDLE_ROOT);
    expect(result.ok).toBe(true);
    expect(result.absolutePath).toBe(path.join(BUNDLE_ROOT, 'index.html'));
  });

  it('defaults empty path to /index.html', () => {
    const result = resolveAppProtocolPath('app://concord/', BUNDLE_ROOT);
    expect(result.ok).toBe(true);
    expect(result.absolutePath).toBe(path.join(BUNDLE_ROOT, 'index.html'));
  });

  it('resolves app://concord/assets/foo.js to bundle/assets/foo.js', () => {
    const result = resolveAppProtocolPath('app://concord/assets/foo.js', BUNDLE_ROOT);
    expect(result.ok).toBe(true);
    expect(result.absolutePath).toBe(path.join(BUNDLE_ROOT, 'assets', 'foo.js'));
  });

  it('rejects path traversal: app://concord/../../../etc/passwd → 403', () => {
    const result = resolveAppProtocolPath('app://concord/../../../etc/passwd', BUNDLE_ROOT);
    expect(result.ok).toBe(false);
    expect(result.status).toBe(403);
  });

  it('rejects path traversal: app://concord/foo/../../../bar → 403', () => {
    const result = resolveAppProtocolPath('app://concord/foo/../../../bar', BUNDLE_ROOT);
    expect(result.ok).toBe(false);
    expect(result.status).toBe(403);
  });

  it('rejects encoded path traversal: app://concord/%2E%2E/foo → 403', () => {
    const result = resolveAppProtocolPath('app://concord/%2E%2E/foo', BUNDLE_ROOT);
    expect(result.ok).toBe(false);
    expect(result.status).toBe(403);
  });

  it('rejects mixed-case scheme with traversal: APP://concord/../../etc → 403 (security review hardening)', () => {
    // Without the case-insensitive slice fix, the WHATWG URL parser would
    // strip the `..` segments during construction (yielding a benign pathname
    // inside bundleRoot), so this would silently return ok:true. The
    // case-insensitive slice ensures the raw-URL `..` scan triggers
    // regardless of input case, returning 403 as the "fail loud" contract
    // requires.
    const result = resolveAppProtocolPath('APP://concord/../../etc/passwd', BUNDLE_ROOT);
    expect(result.ok).toBe(false);
    expect(result.status).toBe(403);
  });
});
