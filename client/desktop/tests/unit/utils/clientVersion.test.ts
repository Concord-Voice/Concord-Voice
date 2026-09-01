import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  _resetClientVersionCache,
  compactSpaHash,
  formatClientVersion,
  getDesktopClientDisplayVersion,
  getDesktopClientVersion,
} from '@/renderer/utils/runtime/clientVersion';

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

describe('desktop client version lookup', () => {
  beforeEach(() => {
    _resetClientVersionCache();
  });

  afterEach(() => {
    (globalThis as unknown as { electron?: unknown }).electron = undefined;
  });

  it('caches the resolved desktop version across repeated access', async () => {
    const getVersion = vi.fn().mockResolvedValue('0.2.18');
    (globalThis as unknown as { electron?: unknown }).electron = { getVersion };

    await expect(getDesktopClientVersion()).resolves.toBe('0.2.18');
    await expect(getDesktopClientVersion()).resolves.toBe('0.2.18');

    expect(getVersion).toHaveBeenCalledTimes(1);
  });

  it('keeps a raw prerelease version for display while strict lookup rejects it', async () => {
    const getVersion = vi.fn().mockResolvedValue('0.2.18-beta.1');
    (globalThis as unknown as { electron?: unknown }).electron = { getVersion };

    await expect(getDesktopClientDisplayVersion()).resolves.toBe('0.2.18-beta.1');
    await expect(getDesktopClientVersion()).resolves.toBeNull();

    expect(getVersion).toHaveBeenCalledTimes(1);
  });

  it.each(['18446744073709551615.0.0', '1234567890.1234567890.1234567890'])(
    'accepts the Go parser boundary version %j',
    async (version) => {
      (globalThis as unknown as { electron?: unknown }).electron = {
        getVersion: vi.fn().mockResolvedValue(version),
      };

      await expect(getDesktopClientVersion()).resolves.toBe(version);
    }
  );

  it.each([
    'v0.2.18',
    '0.2.18-beta.1',
    '0.2.18+build.1',
    ' 0.2.18',
    '0.2.18\n',
    '0.02.18',
    '0.2.018',
    '０.２.１８',
    '18446744073709551616.0.0',
    '12345678901.1234567890.1234567890',
  ])('omits non-exact stable version %j', async (version) => {
    (globalThis as unknown as { electron?: unknown }).electron = {
      getVersion: vi.fn().mockResolvedValue(version),
    };

    await expect(getDesktopClientVersion()).resolves.toBeNull();
  });

  it('omits the declaration when the bridge is absent or rejects', async () => {
    await expect(getDesktopClientVersion()).resolves.toBeNull();

    _resetClientVersionCache();
    (globalThis as unknown as { electron?: unknown }).electron = {
      getVersion: vi.fn().mockRejectedValue(new Error('bridge unavailable')),
    };

    await expect(getDesktopClientVersion()).resolves.toBeNull();
  });

  it('caches a missing version after a bridge rejection', async () => {
    const getVersion = vi
      .fn()
      .mockRejectedValueOnce(new Error('bridge unavailable'))
      .mockResolvedValueOnce('0.2.18');
    (globalThis as unknown as { electron?: unknown }).electron = { getVersion };

    await expect(getDesktopClientVersion()).resolves.toBeNull();
    await expect(getDesktopClientVersion()).resolves.toBeNull();

    expect(getVersion).toHaveBeenCalledTimes(1);
  });

  it('caches a missing version after an invalid bridge result', async () => {
    const getVersion = vi.fn().mockResolvedValueOnce('invalid').mockResolvedValueOnce('0.2.18');
    (globalThis as unknown as { electron?: unknown }).electron = { getVersion };

    await expect(getDesktopClientVersion()).resolves.toBeNull();
    await expect(getDesktopClientVersion()).resolves.toBeNull();

    expect(getVersion).toHaveBeenCalledTimes(1);
  });
});
