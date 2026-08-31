// @vitest-environment jsdom
//
// persistE2EESessionKeys — the shared login/registration/SSO E2EE-key
// persistence helper (#1288). Verifies the warn-on-failure / never-clear
// contract across all branches.

import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

import { persistE2EESessionKeys } from '@/renderer/utils/crypto/persistE2EESessionKeys';

const keys = {
  wrappingKeyBase64: 'wk',
  preferencesKeyBase64: 'pk',
  wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
};

describe('persistE2EESessionKeys (#1288)', () => {
  let warnSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    (globalThis as { electron?: unknown }).electron = undefined;
  });

  afterEach(() => {
    vi.restoreAllMocks();
    // `electron` is a non-configurable global (declared `var`), so it can't be
    // deleted — reset by assignment, matching beforeEach.
    (globalThis as { electron?: unknown }).electron = undefined;
  });

  it('no-ops (no bridge call, no warn) when sessionKeys is null', async () => {
    const storeE2EEKeys = vi.fn();
    (globalThis as { electron?: unknown }).electron = { storeE2EEKeys };

    await persistE2EESessionKeys(null);

    expect(storeE2EEKeys).not.toHaveBeenCalled();
    expect(warnSpy).not.toHaveBeenCalled();
  });

  it('no-ops (no warn) when the Electron bridge is unavailable', async () => {
    (globalThis as { electron?: unknown }).electron = undefined;

    await expect(persistE2EESessionKeys(keys)).resolves.toBe(true);
    expect(warnSpy).not.toHaveBeenCalled();
  });

  it('does not warn when persistence succeeds (resolves true)', async () => {
    const storeE2EEKeys = vi.fn().mockResolvedValue(true);
    (globalThis as { electron?: unknown }).electron = { storeE2EEKeys };

    await persistE2EESessionKeys(keys);

    expect(storeE2EEKeys).toHaveBeenCalledWith(keys);
    expect(warnSpy).not.toHaveBeenCalled();
  });

  it('warns on a genuine keychain write failure (resolves false)', async () => {
    const storeE2EEKeys = vi.fn().mockResolvedValue(false);
    (globalThis as { electron?: unknown }).electron = { storeE2EEKeys };

    await persistE2EESessionKeys(keys);

    expect(warnSpy).toHaveBeenCalledOnce();
    expect(String(warnSpy.mock.calls[0][0])).toContain('did not persist');
  });

  it('warns on an IPC-transport failure (invoke rejects)', async () => {
    const storeE2EEKeys = vi.fn().mockRejectedValue(new Error('ipc gone'));
    (globalThis as { electron?: unknown }).electron = { storeE2EEKeys };

    await persistE2EESessionKeys(keys);

    expect(warnSpy).toHaveBeenCalledOnce();
    expect(String(warnSpy.mock.calls[0][0])).toContain('Failed to invoke');
  });

  it('uses only the owner-scoped writer when a credential owner is supplied', async () => {
    const storeE2EEKeys = vi.fn().mockResolvedValue(true);
    const storeE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
    (globalThis as { electron?: unknown }).electron = {
      storeE2EEKeys,
      storeE2EEKeysIfOwner,
    };

    await expect(persistE2EESessionKeys(keys, 41)).resolves.toBe(true);

    expect(storeE2EEKeysIfOwner).toHaveBeenCalledWith(keys, 41);
    expect(storeE2EEKeys).not.toHaveBeenCalled();
  });

  it('never falls back to the generic writer when owner-scoped persistence is unavailable', async () => {
    const storeE2EEKeys = vi.fn().mockResolvedValue(true);
    (globalThis as { electron?: unknown }).electron = { storeE2EEKeys };

    await expect(persistE2EESessionKeys(keys, 41)).resolves.toBe(false);

    expect(storeE2EEKeys).not.toHaveBeenCalled();
    expect(warnSpy).toHaveBeenCalledOnce();
  });
});
