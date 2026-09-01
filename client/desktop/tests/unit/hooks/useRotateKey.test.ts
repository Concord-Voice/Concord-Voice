import { act, renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/system/apiClient';
import { useRotateKey } from '@/renderer/hooks/voice/useRotateKey';
import { resetAllStores } from '../../helpers/store-helpers';

const apiFetchMock = vi.mocked(apiFetch);

describe('useRotateKey', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('silently ignores an aborted request', async () => {
    const error = new Error('request cancelled');
    error.name = 'AbortError';
    apiFetchMock.mockRejectedValue(error);
    const onSuccess = vi.fn();
    const { result } = renderHook(() => useRotateKey('/rotate', onSuccess));

    await act(async () => {
      await result.current.handleRotate();
    });

    expect(result.current.rotateStatus).toBe('idle');
    expect(result.current.rotateMessage).toBe('');
    expect(onSuccess).not.toHaveBeenCalled();
  });

  it('reports genuine network errors', async () => {
    apiFetchMock.mockRejectedValue(new Error('offline'));
    const { result } = renderHook(() => useRotateKey('/rotate', vi.fn()));

    await act(async () => {
      await result.current.handleRotate();
    });

    expect(result.current.rotateStatus).toBe('error');
    expect(result.current.rotateMessage).toBe('Network error');
  });
});
