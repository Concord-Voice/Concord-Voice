import { describe, expect, it } from 'vitest';
import { compactSpaHash, formatClientVersion } from '@/renderer/utils/clientVersion';

const FULL_HASH = 'a'.repeat(40);

describe('client version formatting', () => {
  it('shortens full SHA hashes to the 7-character display form', () => {
    expect(compactSpaHash(FULL_HASH)).toBe('aaaaaaa');
  });

  it('keeps already-short hashes unchanged', () => {
    expect(compactSpaHash('abc1234')).toBe('abc1234');
  });

  it('shortens captured HTML hashes to the 7-character display form', () => {
    expect(compactSpaHash(`sha256:${'b'.repeat(64)}`)).toBe('bbbbbbb');
  });

  it('treats empty and sentinel SPA versions as no hash', () => {
    expect(compactSpaHash(null)).toBeNull();
    expect(compactSpaHash(undefined)).toBeNull();
    expect(compactSpaHash('')).toBeNull();
    expect(compactSpaHash('bundled')).toBeNull();
    expect(compactSpaHash('remote')).toBeNull();
  });

  it('composes app version with a compact hash when both are available', () => {
    expect(formatClientVersion('0.2.18', FULL_HASH)).toBe('v0.2.18-aaaaaaa');
  });

  it('composes just the app version when no usable hash exists', () => {
    expect(formatClientVersion('0.2.18', 'bundled')).toBe('v0.2.18');
    expect(formatClientVersion('0.2.18', null)).toBe('v0.2.18');
  });

  it('renders no version text when the app version is missing', () => {
    expect(formatClientVersion('', FULL_HASH)).toBe('');
    expect(formatClientVersion(null, FULL_HASH)).toBe('');
  });
});
