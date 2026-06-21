import { describe, it, expect } from 'vitest';
import { SPA_CHUNK_URL_PATTERN } from '@/shared/spaUrlPattern';

describe('SPA_CHUNK_URL_PATTERN', () => {
  it('matches legacy per-SHA chunk URLs (pre-#976)', () => {
    expect(
      SPA_CHUNK_URL_PATTERN.test('https://api.example.com/spa/abc1234/assets/Settings-Xyz.js')
    ).toBe(true);
  });

  it('matches flat Cloudflare Pages chunk URLs (post-#976)', () => {
    // The #976 cutover (ADR-0015) serves the SPA flat at spa.example.com
    // with no /spa/<sha>/ prefix. The stale pattern silently stopped matching
    // these, disabling self-heal on the production host — the founding bug.
    expect(
      SPA_CHUNK_URL_PATTERN.test('https://spa.example.com/assets/recoveryService-40XchBlv.js')
    ).toBe(true);
  });

  it('does NOT match non-asset paths (frames, API, index.html)', () => {
    expect(SPA_CHUNK_URL_PATTERN.test('https://spa.example.com/index.html')).toBe(false);
    expect(SPA_CHUNK_URL_PATTERN.test('https://spa.example.com/api/v1/messages')).toBe(false);
    expect(SPA_CHUNK_URL_PATTERN.test('https://spa.example.com/')).toBe(false);
  });
});
