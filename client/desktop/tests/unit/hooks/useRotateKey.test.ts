import { act, renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/system/apiClient';
import { useRotateKey } from '@/renderer/hooks/voice/useRotateKey';
import { DMRotationError } from '@/renderer/services/e2ee/e2eeErrors';
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

  // A DM rotation must carry the successor wraps, so the caller supplies the
  // request; the bare POST would revoke the only epoch anyone holds.
  it('performs the supplied request instead of a bare POST', async () => {
    const request = vi.fn().mockResolvedValue({ ok: true, status: 200 } as Response);
    const onSuccess = vi.fn();
    const { result } = renderHook(() => useRotateKey('/rotate', onSuccess, request));

    await act(async () => {
      await result.current.handleRotate();
    });

    expect(request).toHaveBeenCalledTimes(1);
    expect(apiFetchMock).not.toHaveBeenCalled();
    expect(result.current.rotateStatus).toBe('success');
    expect(onSuccess).toHaveBeenCalledTimes(1);
  });

  it('shows a DMRotationError message verbatim', async () => {
    const request = vi
      .fn()
      .mockRejectedValue(new DMRotationError('A participant has no encryption key yet'));
    const { result } = renderHook(() => useRotateKey('/rotate', vi.fn(), request));

    await act(async () => {
      await result.current.handleRotate();
    });

    expect(result.current.rotateStatus).toBe('error');
    expect(result.current.rotateMessage).toBe('A participant has no encryption key yet');
  });

  it('keeps the generic message for other request failures', async () => {
    const request = vi.fn().mockRejectedValue(new Error('offline'));
    const { result } = renderHook(() => useRotateKey('/rotate', vi.fn(), request));

    await act(async () => {
      await result.current.handleRotate();
    });

    expect(result.current.rotateMessage).toBe('Network error');
  });
});
