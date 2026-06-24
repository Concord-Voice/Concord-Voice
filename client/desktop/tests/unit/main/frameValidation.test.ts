import { describe, it, expect } from 'vitest';

import { isPermittedFrameUrl } from '@/main/ipc/frameValidation';

/**
 * Frame-origin allowlist used by the privileged IPC handlers (attestation,
 * sso, openExternal). The key regression this suite locks: the post-#830
 * bundled renderer loads from `app://concord` and MUST be a permitted frame —
 * otherwise every `attestation:get-token` call from the bundled build throws
 * "untrusted sender frame" and bricks all API connectivity.
 *
 * Founding incident: bundled-build clients could not connect — the attestation
 * IPC (invoked on every apiFetch) rejected the `app://concord` sender frame.
 */
describe('isPermittedFrameUrl', () => {
  describe('app://concord bundled renderer (post-#830 regression guard)', () => {
    it('accepts app://concord/index.html when no remote SPA is active', () => {
      // Bundled mode: getRemoteSpaBaseUrl() === null. The bundled renderer is
      // a first-party, protocol-gated origin and must be trusted here.
      expect(isPermittedFrameUrl('app://concord/index.html', null)).toBe(true);
    });

    it('accepts app://concord with a hash route', () => {
      expect(isPermittedFrameUrl('app://concord/index.html#/channels/1', null)).toBe(true);
    });

    it('accepts app://concord even when a remote SPA origin is also configured', () => {
      // PiP / self-heal can leave a remote origin set while a bundled frame
      // exists; the bundled origin is independently trusted.
      expect(isPermittedFrameUrl('app://concord/index.html', 'https://spa.concordvoice.chat')).toBe(
        true
      );
    });

    it('rejects app://other (wrong host)', () => {
      expect(isPermittedFrameUrl('app://other/index.html', null)).toBe(false);
    });

    it('rejects a look-alike host app://concord.evil.com', () => {
      expect(isPermittedFrameUrl('app://concord.evil.com/index.html', null)).toBe(false);
    });

    it('rejects a host-suffix look-alike app://concordX', () => {
      expect(isPermittedFrameUrl('app://concordX/index.html', null)).toBe(false);
    });
  });

  describe('spa-cache://concord cache renderer (#1870, Finding C)', () => {
    // The signed last-known-good cache serves from spa-cache://concord and must
    // be a permitted frame — otherwise every privileged IPC (attestation token,
    // spa:reloadLatest, SSO, openExternal) rejects the cache renderer exactly as
    // the bundled build was rejected before #830. The cache origin is granted
    // the SAME trust as app://concord.
    //
    // NOTE: depends on the orchestrator applying the (hook-blocked) source edit
    // to src/main/ipc/frameValidation.ts that accepts spa-cache://concord.
    it('accepts spa-cache://concord when no remote SPA is active', () => {
      expect(isPermittedFrameUrl('spa-cache://concord', null)).toBe(true);
    });

    it('accepts spa-cache://concord/index.html', () => {
      expect(isPermittedFrameUrl('spa-cache://concord/index.html', null)).toBe(true);
    });

    it('accepts spa-cache://concord with a hash route', () => {
      expect(isPermittedFrameUrl('spa-cache://concord/index.html#/channels/1', null)).toBe(true);
    });

    it('accepts spa-cache://concord even when a remote SPA origin is configured', () => {
      expect(
        isPermittedFrameUrl('spa-cache://concord/index.html', 'https://spa.concordvoice.chat')
      ).toBe(true);
    });

    it('rejects a look-alike host spa-cache://concord.evil', () => {
      expect(isPermittedFrameUrl('spa-cache://concord.evil/index.html', null)).toBe(false);
    });

    it('rejects a host-suffix look-alike spa-cache://evil', () => {
      expect(isPermittedFrameUrl('spa-cache://evil/index.html', null)).toBe(false);
    });

    it('rejects a scheme look-alike spa-cacheX://concord', () => {
      expect(isPermittedFrameUrl('spa-cacheX://concord/index.html', null)).toBe(false);
    });
  });

  describe('legacy file:// bundled renderer (pre-#830)', () => {
    it('accepts file:// with any path', () => {
      expect(
        isPermittedFrameUrl('file:///Applications/Concord.app/Contents/index.html', null)
      ).toBe(true);
    });
  });

  describe('dev server', () => {
    it('accepts http://localhost on any port', () => {
      expect(isPermittedFrameUrl('http://localhost:3001/', null)).toBe(true);
      expect(isPermittedFrameUrl('http://localhost:5173/index.html', null)).toBe(true);
    });

    it('rejects localhost scheme-injection bypass attempts', () => {
      expect(isPermittedFrameUrl('http://localhost:3000@evil.com/', null)).toBe(false);
      expect(isPermittedFrameUrl('http://localhost:3000.evil.com/', null)).toBe(false);
    });
  });

  describe('remote SPA origin', () => {
    const origin = 'https://spa.concordvoice.chat';

    it('accepts a URL under the configured remote origin', () => {
      expect(isPermittedFrameUrl(`${origin}/index.html`, origin)).toBe(true);
    });

    it('rejects a different https origin', () => {
      expect(isPermittedFrameUrl('https://evil.example.com/', origin)).toBe(false);
    });

    it('rejects an origin-prefix look-alike (no trailing slash boundary)', () => {
      // `${origin}/` boundary prevents https://spa.concordvoice.chat.evil.com/.
      expect(isPermittedFrameUrl('https://spa.concordvoice.chat.evil.com/', origin)).toBe(false);
    });
  });

  describe('fail-closed cases', () => {
    it('rejects empty string', () => {
      expect(isPermittedFrameUrl('', null)).toBe(false);
    });

    it('rejects a malformed URL', () => {
      expect(isPermittedFrameUrl('not a url', null)).toBe(false);
    });

    it('rejects arbitrary https when no remote origin is configured', () => {
      expect(isPermittedFrameUrl('https://evil.example.com/', null)).toBe(false);
    });
  });
});
