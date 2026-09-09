import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { NATIVE_ADDON_ENV, resolveNativeAddonPath } from '../../../src/main/nativeAddonPath';

const DEV_PATH = path.join(
  '/repo',
  'native',
  'concord-audiocap',
  'build',
  'Release',
  'concord_audiocap.node'
);

describe('resolveNativeAddonPath (#3194)', () => {
  it('resolves into Resources when packaged', () => {
    expect(resolveNativeAddonPath('darwin', true, '/App/Contents/Resources', '/repo')).toBe(
      path.join('/App/Contents/Resources', 'concord_audiocap.node')
    );
  });

  it('resolves into the repo build dir when not packaged', () => {
    expect(resolveNativeAddonPath('darwin', false, undefined, '/repo')).toBe(DEV_PATH);
  });

  // Electron types resourcesPath as always-present; at runtime it is undefined in
  // dev and under vitest. resolveTrayIconPath guards it for the same reason
  // (src/main/tray.ts:83). Without the guard this becomes path.join(undefined, …),
  // which throws only under packaging — the worst place to find out.
  it('falls back to the dev path when packaged but resourcesPath is undefined', () => {
    expect(resolveNativeAddonPath('win32', true, undefined, '/repo')).toBe(DEV_PATH);
  });

  it('resolves for win32 when packaged', () => {
    expect(resolveNativeAddonPath('win32', true, 'C:\\App\\Resources', '/repo')).toBe(
      path.join('C:\\App\\Resources', 'concord_audiocap.node')
    );
  });

  // ADR-0043 puts Linux/PipeWire out of scope. null is a legitimate outcome here,
  // and callers must read it as "no per-process audio on this machine" — never as
  // a reason to widen capture to a system mix (#2161's defect).
  it.each(['linux', 'freebsd', 'aix'] as const)(
    'returns null on unsupported platform %s',
    (platform) => {
      expect(resolveNativeAddonPath(platform, true, '/App/Contents/Resources', '/repo')).toBeNull();
    }
  );

  it('exports the env var name the loader and the fork agree on', () => {
    expect(NATIVE_ADDON_ENV).toBe('CONCORD_AUDIOCAP_PATH');
  });
});
