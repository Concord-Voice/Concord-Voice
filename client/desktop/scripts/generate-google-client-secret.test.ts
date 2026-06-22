import { describe, it, expect } from 'vitest';
import { resolveClientSecret, formatClientSecretJson } from './generate-google-client-secret.mjs';

describe('resolveClientSecret', () => {
  it('prefers env var, trimmed', () => {
    expect(resolveClientSecret({ GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP: '  sek  ' }, null)).toBe('sek');
  });
  it('falls back to .env content', () => {
    expect(resolveClientSecret({}, 'X=1\nGOOGLE_OAUTH_CLIENT_SECRET_DESKTOP=fromfile\n')).toBe('fromfile');
  });
  it('returns empty string when unset', () => {
    expect(resolveClientSecret({}, null)).toBe('');
  });
});

describe('formatClientSecretJson', () => {
  it('emits {clientSecret} JSON with trailing newline', () => {
    expect(formatClientSecretJson('s')).toBe('{\n  "clientSecret": "s"\n}\n');
  });
});
