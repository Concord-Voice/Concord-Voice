import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    decryptPreferences: vi.fn(),
    encryptPreferences: vi.fn(),
  },
}));

import { apiFetch } from '@/renderer/services/apiClient';
import { e2eeService } from '@/renderer/services/e2eeService';
import { fetchBlobRowForRotation, fetchEncryptedBlob } from '@/renderer/services/e2eeBlobTransport';

const mockApiFetch = vi.mocked(apiFetch);

describe('e2eeBlobTransport', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('bootstraps only when the server authoritatively reports no blob', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ saved_gifs: null }),
    } as Response);

    await expect(fetchEncryptedBlob('/saved-gifs', 'saved_gifs')).resolves.toEqual({
      blob: null,
      pushBootstrap: true,
    });
  });

  it('fails closed instead of bootstrapping over ciphertext that cannot decrypt', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ saved_gifs: { encrypted_data: 'old-key-ciphertext' } }),
    } as Response);
    vi.mocked(e2eeService.decryptPreferences).mockRejectedValue(new Error('wrong key'));

    await expect(fetchEncryptedBlob('/saved-gifs', 'saved_gifs')).resolves.toEqual({
      blob: null,
      pushBootstrap: false,
    });
  });

  it('fails closed when the expected encrypted response field is missing', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({}),
    } as Response);

    await expect(fetchEncryptedBlob('/saved-gifs', 'saved_gifs')).resolves.toEqual({
      blob: null,
      pushBootstrap: false,
    });
    expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
  });
});

describe('fetchBlobRowForRotation (#2200)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (e2eeService as { isInitialized: boolean }).isInitialized = true;
  });

  it('returns absent on an explicit null row', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ preferences: null }),
    } as Response);

    await expect(fetchBlobRowForRotation('/preferences', 'preferences')).resolves.toEqual({
      kind: 'absent',
    });
    expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
  });

  it('returns present with version and plaintext on a decryptable row', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ preferences: { encrypted_data: 'ciphertext', version: 4 } }),
    } as Response);
    vi.mocked(e2eeService.decryptPreferences).mockResolvedValue({ v: 1, data: { theme: 'dark' } });

    await expect(fetchBlobRowForRotation('/preferences', 'preferences')).resolves.toEqual({
      kind: 'present',
      version: 4,
      plaintext: { v: 1, data: { theme: 'dark' } },
    });
  });

  it('returns undecryptable with version when decryption throws', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ preferences: { encrypted_data: 'old-key-ciphertext', version: 7 } }),
    } as Response);
    vi.mocked(e2eeService.decryptPreferences).mockRejectedValue(new Error('wrong key'));

    await expect(fetchBlobRowForRotation('/preferences', 'preferences')).resolves.toEqual({
      kind: 'undecryptable',
      version: 7,
    });
  });

  it('returns error on a non-ok response', async () => {
    mockApiFetch.mockResolvedValue({ ok: false, status: 500 } as Response);

    await expect(fetchBlobRowForRotation('/preferences', 'preferences')).resolves.toEqual({
      kind: 'error',
    });
  });

  it('returns error on a malformed wrapper missing a valid version', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ preferences: { encrypted_data: 'ciphertext', version: 0 } }),
    } as Response);

    await expect(fetchBlobRowForRotation('/preferences', 'preferences')).resolves.toEqual({
      kind: 'error',
    });
    expect(e2eeService.decryptPreferences).not.toHaveBeenCalled();
  });

  it('returns error when the E2EE service is not initialized', async () => {
    (e2eeService as { isInitialized: boolean }).isInitialized = false;

    await expect(fetchBlobRowForRotation('/preferences', 'preferences')).resolves.toEqual({
      kind: 'error',
    });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });
});
