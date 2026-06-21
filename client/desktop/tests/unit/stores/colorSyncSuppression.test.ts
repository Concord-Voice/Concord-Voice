import { setSyncSuppressed, isSyncSuppressed } from '@/renderer/stores/colorSyncSuppression';

// The color-sync suppression flag was extracted from settingsStore into this
// dependency-free leaf module so userStore can flip it via a static import
// (avoiding the unawaited dynamic import('./settingsStore') that raced vitest
// worker teardown). These tests cover the leaf's get/set round-trip.
describe('colorSyncSuppression', () => {
  // Module state is process-global; reset to the documented default after each
  // test so ordering can't leak the flag between cases.
  afterEach(() => setSyncSuppressed(false));

  it('defaults to not suppressed', () => {
    expect(isSyncSuppressed()).toBe(false);
  });

  it('reflects the value set via setSyncSuppressed', () => {
    setSyncSuppressed(true);
    expect(isSyncSuppressed()).toBe(true);
    setSyncSuppressed(false);
    expect(isSyncSuppressed()).toBe(false);
  });

  it('exposes setSyncSuppressed and isSyncSuppressed as functions', () => {
    expect(typeof setSyncSuppressed).toBe('function');
    expect(typeof isSyncSuppressed).toBe('function');
  });

  it('can be toggled repeatedly without throwing', () => {
    expect(() => {
      setSyncSuppressed(true);
      setSyncSuppressed(false);
      setSyncSuppressed(true);
    }).not.toThrow();
    expect(isSyncSuppressed()).toBe(true);
  });
});
