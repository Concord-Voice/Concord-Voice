/**
 * Unit tests for the production-build CSP stripper.
 *
 * The companion `csp-policy.test.ts` exercises the dev-mode CSP source in
 * `index.html`. This file exercises the production-build transformation —
 * `stripLoopbackCspEntries` is what `vite build` applies to the renderer
 * HTML before bundling. Together the two test files cover the full
 * dev-vs-prod CSP shape.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { stripLoopbackCspEntries } from '../../scripts/csp-prod-strip';

describe('stripLoopbackCspEntries', () => {
  it('removes http://localhost:* from the CSP', () => {
    const input = `img-src 'self' http://localhost:* https://example.com;`;
    const result = stripLoopbackCspEntries(input);
    expect(result).not.toContain('http://localhost:*');
    expect(result).toContain('https://example.com');
  });

  it('removes http://127.0.0.1:* from the CSP', () => {
    const input = `img-src 'self' http://127.0.0.1:* https://example.com;`;
    const result = stripLoopbackCspEntries(input);
    expect(result).not.toContain('http://127.0.0.1:*');
    expect(result).toContain('https://example.com');
  });

  it('removes all WebSocket loopback variants from connect-src', () => {
    // Intentional cleartext WS / loopback HTTPS in test input — these are the
    // exact strings the stripper targets in dev-mode CSP source.
    const input = [
      'ws',
      '://localhost:* wss',
      '://localhost:* https',
      '://localhost:* ws',
      '://127.0.0.1:*',
    ].join('');
    const cspLine = `connect-src 'self' ${input} https://example.com;`;
    const result = stripLoopbackCspEntries(cspLine);
    expect(result).not.toContain('localhost');
    expect(result).not.toContain('127.0.0.1');
    expect(result).toContain('https://example.com');
  });

  it('is idempotent — re-running on stripped output is a no-op', () => {
    const input = `img-src 'self' http://localhost:* https://example.com;`;
    const once = stripLoopbackCspEntries(input);
    const twice = stripLoopbackCspEntries(once);
    expect(twice).toBe(once);
  });

  it('leaves non-loopback CSP entries unchanged', () => {
    const input = `default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; img-src 'self' data: blob: https://cdn.jsdelivr.net https://*.example.com;`;
    expect(stripLoopbackCspEntries(input)).toBe(input);
  });

  it('preserves CSP structure (semicolons + directive boundaries) when stripping', () => {
    const input = `connect-src 'self' http://localhost:* https://example.com; img-src 'self' http://localhost:*;`;
    const result = stripLoopbackCspEntries(input);
    // Both directives still present, both still terminated with a semicolon.
    expect(result).toMatch(/connect-src\s+'self'\s+https:\/\/concordvoice\.chat\s*;/);
    expect(result).toMatch(/img-src\s+'self'\s*;/);
  });

  // Integration check: load the actual dev-mode index.html, run the stripper,
  // verify the production-mode output is loopback-free but otherwise intact.
  describe('against real index.html', () => {
    const html = readFileSync(resolve(__dirname, '../../index.html'), 'utf-8');
    const stripped = stripLoopbackCspEntries(html);

    it('removes all loopback origins from the production-mode CSP', () => {
      expect(stripped).not.toContain('http://localhost:*');
      expect(stripped).not.toContain('http://127.0.0.1:*');
    });

    it('preserves the public production origins', () => {
      expect(stripped).toContain('https://example.com');
      expect(stripped).toContain('https://*.example.com');
    });

    it('preserves the renderer scaffold (root div, main.tsx entry, etc.)', () => {
      expect(stripped).toContain('<div id="root">');
      expect(stripped).toContain('src/renderer/main.tsx');
    });
  });
});
