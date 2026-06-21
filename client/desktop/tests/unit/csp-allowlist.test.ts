// client/desktop/tests/unit/csp-allowlist.test.ts
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, it, expect } from 'vitest';

describe('CSP allowlist (issue #817 regression guard)', () => {
  const indexHtml = readFileSync(resolve(__dirname, '../../index.html'), 'utf-8');
  // Scope the parse to the CSP <meta> tag's content attribute, NOT the first
  // "connect-src" anywhere in the file. A doc comment mentioning "connect-src"
  // (added for #976) previously poisoned this parse — the regex matched the
  // comment instead of the real directive. Extract the double-quoted content
  // attribute value first ([^"]* so single-quoted CSP keywords like 'self' and
  // 'none' inside are preserved), then locate connect-src within it.
  const cspContent =
    indexHtml.match(/http-equiv=["']Content-Security-Policy["'][\s\S]*?content="([^"]*)"/i)?.[1] ??
    '';
  const match = cspContent.match(/connect-src([^;]+);/);
  const directive = (match?.[1] ?? '').trim();

  // Parse the connect-src directive into individual tokens (host patterns
  // and keywords like 'self'). Splits on whitespace and removes quotes.
  const tokens = directive
    .split(/\s+/)
    .filter((t) => t.length > 0)
    .map((t) => t.replace(/^'|'$/g, ''));

  /**
   * The closed allowlist of acceptable connect-src tokens.
   * Adding a token here requires a security review — see spec §3.
   * Anything not in this set is considered a regression.
   */
  const ALLOWED_TOKENS = new Set([
    'self',
    // Localhost variants for dev (Vite dev server, local backend)
    'ws://localhost:*',
    'http://localhost:*',
    'wss://localhost:*',
    'https://localhost:*',
    'ws://127.0.0.1:*',
    'http://127.0.0.1:*',
    // Bundled-app origin (app:// scheme — packaged builds)
    // app://concord (registered in main process; see [internal]rules/electron.md)
    // No connect-src entry needed because app:// pages have origin null
    // from a CSP standpoint and CSP enforcement is bypassed by Electron
    // for same-origin asar reads. Listed here for documentation only.
    // First-party example.com surfaces (production + staging)
    'https://example.com',
    'wss://example.com',
    'https://*.example.com',
    'wss://*.example.com',
  ]);

  it('parses a connect-src directive from index.html', () => {
    expect(match).not.toBeNull();
    expect(directive.length).toBeGreaterThan(0);
    expect(tokens.length).toBeGreaterThan(0);
  });

  it('connect-src contains ONLY tokens in the closed allowlist', () => {
    // Positive allow-list: every directive token must be explicitly approved.
    // This catches ANY third-party host addition (Google, Apple, captive-portal
    // probes, CDN telemetry, etc.) without needing to enumerate the deny set.
    const unexpected = tokens.filter((t) => !ALLOWED_TOKENS.has(t));
    expect(unexpected).toEqual([]);
  });

  it('connect-src does not allow common third-party probe hosts', () => {
    // Belt-and-suspenders: explicitly assert known-bad hosts are absent
    // even if the allow-list above is later relaxed.
    const banned = [
      'clients3.google.com',
      'clients.google.com',
      'google.com',
      'captive.apple.com',
      'connectivitycheck',
      'msftconnecttest',
      'detectportal.firefox.com',
      'nmcheck.gnome.org',
      'cp.cloudflare.com',
    ];
    for (const host of banned) {
      expect(directive).not.toContain(host);
    }
  });
});
