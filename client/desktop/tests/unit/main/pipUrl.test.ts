import { describe, it, expect } from 'vitest';

import { buildRemotePipUrl, isValidPipOpenSender } from '@/main/pipUrl';

describe('buildRemotePipUrl', () => {
  it('preserves the full SPA URL path component (regression for #802)', () => {
    // `remoteSpaBaseUrl` (origin only) used to be passed here, which stripped
    // the `/spa/<sha>/index.html` path. Without the path nginx falls through
    // to its catch-all `location /` and redirects to the marketing site.
    const spaUrl = 'https://api.concordvoice.chat/spa/abc1234/index.html';
    const result = buildRemotePipUrl(spaUrl, 'window-1');
    expect(result.startsWith('https://api.concordvoice.chat/spa/abc1234/')).toBe(true);
  });

  it('appends the hash route in `#/pip/<id>` form', () => {
    const result = buildRemotePipUrl(
      'https://api.concordvoice.chat/spa/abc1234/index.html',
      'window-2'
    );
    expect(result.endsWith('#/pip/window-2')).toBe(true);
  });

  it('preserves a query string in the SPA URL', () => {
    // Defensive: the server contract today is `/spa/<sha>/index.html` with no
    // query string, but if that ever evolves the assembly should still be valid.
    const spaUrl = 'https://example.com/spa/abc/index.html?v=123';
    const result = buildRemotePipUrl(spaUrl, 'window-3');
    expect(result).toBe('https://example.com/spa/abc/index.html?v=123#/pip/window-3');
  });

  it('contains a hash-bearing pipId within the fragment, not the path', () => {
    // RFC 3986 §3.5: the first `#` terminates the resource portion of the
    // URL; everything after is fragment. A pipId containing `#` cannot
    // escape upward into the path or authority.
    const result = buildRemotePipUrl(
      'https://api.concordvoice.chat/spa/abc/index.html',
      'win#evil.com'
    );
    const firstHashIndex = result.indexOf('#');
    expect(result.substring(0, firstHashIndex)).toBe(
      'https://api.concordvoice.chat/spa/abc/index.html'
    );
  });

  it('contains a CR/LF-bearing pipId within the fragment', () => {
    // Newlines in pipId stay inside the fragment — the URL path is fixed
    // by the SPA URL prefix.
    const result = buildRemotePipUrl(
      'https://api.concordvoice.chat/spa/abc/index.html',
      'win\nmalicious'
    );
    expect(result.startsWith('https://api.concordvoice.chat/spa/abc/index.html#/pip/win')).toBe(
      true
    );
  });
});

describe('isValidPipOpenSender', () => {
  describe('dev mode', () => {
    it('accepts http://localhost', () => {
      expect(isValidPipOpenSender('http://localhost:3001/', false, null)).toBe(true);
    });

    it('accepts http://localhost on any port', () => {
      expect(isValidPipOpenSender('http://localhost:5173/foo', false, null)).toBe(true);
    });

    it('rejects non-localhost hosts in dev', () => {
      expect(isValidPipOpenSender('https://evil.example.com/', false, null)).toBe(false);
    });

    it('rejects 127.0.0.1 (only the literal hostname `localhost` matches)', () => {
      // Tightening: `localhost` and `127.0.0.1` resolve identically at the
      // network layer but the dev server only ever serves on `localhost`.
      expect(isValidPipOpenSender('http://127.0.0.1:3001/', false, null)).toBe(false);
    });
  });

  describe('packaged + remote SPA active', () => {
    const spaUrl = 'https://api.concordvoice.chat/spa/abc1234/index.html';

    it('accepts sender from the SPA origin', () => {
      expect(
        isValidPipOpenSender('https://api.concordvoice.chat/spa/abc1234/index.html', true, spaUrl)
      ).toBe(true);
    });

    it('accepts sender from the same origin with a different path', () => {
      expect(isValidPipOpenSender('https://api.concordvoice.chat/other', true, spaUrl)).toBe(true);
    });

    it('rejects sender from a different origin', () => {
      expect(isValidPipOpenSender('https://evil.example.com/', true, spaUrl)).toBe(false);
    });

    it('rejects file:// sender when remote SPA is active', () => {
      expect(isValidPipOpenSender('file:///app/renderer/index.html', true, spaUrl)).toBe(false);
    });

    it('rejects malformed remoteSpaUrl (fail-closed)', () => {
      expect(isValidPipOpenSender('https://api.concordvoice.chat/', true, 'not a url')).toBe(false);
    });
  });

  describe('packaged + bundled fallback (remoteSpaUrl null)', () => {
    it('accepts app://concord sender', () => {
      // #830: bundled-mode renderer now loads via the custom app:// scheme
      // registered in main.ts (Task 4 of #830). The legacy file:// origin
      // is rejected — see the dedicated #830 describe block below.
      expect(isValidPipOpenSender('app://concord/index.html', true, null)).toBe(true);
    });

    it('accepts spa-cache://concord sender (#1870 signed LKG cache)', () => {
      // The signed last-known-good cache serves from spa-cache://concord — a
      // first-party, protocol-gated, bundled-equivalent fallback trusted here
      // identically to app://concord.
      expect(isValidPipOpenSender('spa-cache://concord/index.html', true, null)).toBe(true);
    });

    it('rejects a spa-cache look-alike host (spa-cache://concord.evil)', () => {
      expect(isValidPipOpenSender('spa-cache://concord.evil/index.html', true, null)).toBe(false);
    });

    it('rejects https sender when bundled', () => {
      expect(isValidPipOpenSender('https://api.concordvoice.chat/', true, null)).toBe(false);
    });

    it('rejects localhost sender when bundled (packaged build)', () => {
      expect(isValidPipOpenSender('http://localhost:3001/', true, null)).toBe(false);
    });
  });

  describe('fail-closed cases', () => {
    it('rejects empty sender URL', () => {
      expect(isValidPipOpenSender('', false, null)).toBe(false);
      expect(isValidPipOpenSender('', true, 'https://x/spa/y/index.html')).toBe(false);
      expect(isValidPipOpenSender('', true, null)).toBe(false);
    });

    it('rejects malformed sender URL', () => {
      expect(isValidPipOpenSender('not a url', false, null)).toBe(false);
      expect(isValidPipOpenSender('not a url', true, 'https://x/spa/y/index.html')).toBe(false);
      expect(isValidPipOpenSender('not a url', true, null)).toBe(false);
    });
  });

  describe('isValidPipOpenSender — #830 app:// scheme support', () => {
    it('accepts app://concord/index.html in packaged + bundled mode', () => {
      const result = isValidPipOpenSender('app://concord/index.html', true, null);
      expect(result).toBe(true);
    });

    it('accepts app://concord/index.html#/pip/abc in packaged + bundled mode', () => {
      const result = isValidPipOpenSender('app://concord/index.html#/pip/abc', true, null);
      expect(result).toBe(true);
    });

    it('rejects app://other/index.html (wrong host)', () => {
      const result = isValidPipOpenSender('app://other/index.html', true, null);
      expect(result).toBe(false);
    });

    it('rejects file:// in packaged + bundled mode (post-#830, file:// is gone)', () => {
      const result = isValidPipOpenSender(
        'file:///Applications/Concord%20Voice.app/Contents/Resources/app.asar/dist/renderer/index.html',
        true,
        null
      );
      expect(result).toBe(false);
    });

    it('still rejects app://concord when remoteSpaUrl is active (only HTTPS SPA origin valid in remote mode)', () => {
      const result = isValidPipOpenSender(
        'app://concord/index.html',
        true,
        'https://api.concordvoice.chat/spa/abc1234/index.html'
      );
      expect(result).toBe(false);
    });
  });
});
