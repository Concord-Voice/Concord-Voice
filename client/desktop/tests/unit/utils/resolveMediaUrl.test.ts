import { describe, it, expect } from 'vitest';
import { resolveMediaUrl } from '@/renderer/utils/resolveMediaUrl';
import { API_BASE } from '@/renderer/config';

describe('resolveMediaUrl', () => {
  it('prefixes API_BASE for relative /api/v1/media paths', () => {
    expect(resolveMediaUrl('/api/v1/media/avatars/abc')).toBe(
      `${API_BASE}/api/v1/media/avatars/abc`
    );
  });

  it('prefixes API_BASE for any leading-slash path', () => {
    expect(resolveMediaUrl('/api/v1/media/server-icons/s1')).toBe(
      `${API_BASE}/api/v1/media/server-icons/s1`
    );
  });

  it('passes through data: URLs unchanged', () => {
    expect(resolveMediaUrl('data:image/png;base64,AAAA')).toBe('data:image/png;base64,AAAA');
  });

  it('passes through blob: URLs unchanged (local crop preview)', () => {
    expect(resolveMediaUrl('blob:app://concord/abc')).toBe('blob:app://concord/abc');
  });

  it('passes through absolute http(s) URLs unchanged', () => {
    expect(resolveMediaUrl('https://cdn.example/x.png')).toBe('https://cdn.example/x.png');
    expect(resolveMediaUrl('http://localhost:8080/api/v1/media/avatars/x')).toBe(
      'http://localhost:8080/api/v1/media/avatars/x'
    );
  });

  it('returns undefined for nullish/empty', () => {
    expect(resolveMediaUrl(undefined)).toBeUndefined();
    expect(resolveMediaUrl(null)).toBeUndefined();
    expect(resolveMediaUrl('')).toBeUndefined();
  });

  it('returns undefined for non-allowlisted schemes (allowlist preserved)', () => {
    expect(resolveMediaUrl('javascript:alert(1)')).toBeUndefined();
    expect(resolveMediaUrl('ftp://x/y')).toBeUndefined();
    expect(resolveMediaUrl('relative/no/leading/slash')).toBeUndefined();
  });
});
