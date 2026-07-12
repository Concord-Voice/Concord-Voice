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
import { fetchEncryptedBlob } from '@/renderer/services/e2eeBlobTransport';

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
