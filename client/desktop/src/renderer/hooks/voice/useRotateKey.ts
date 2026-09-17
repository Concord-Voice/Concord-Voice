import { useState, useCallback } from 'react';
import { DMRotationError } from '../../services/e2ee/e2eeErrors';
import { apiFetch } from '../../services/system/apiClient';
import { formatRetryAfter } from '../../utils/runtime/formatRetryAfter';

type RotateStatus = 'idle' | 'success' | 'error';

interface UseRotateKeyResult {
  rotateStatus: RotateStatus;
  rotateMessage: string;
  handleRotate: () => Promise<void>;
}

/**
 * Hook for triggering E2EE key rotation via API.
 * Handles success, 429 rate limiting (with human-readable retry delta), and errors.
 */
/**
 * @param request — performs the rotation instead of a bare POST to `endpoint`.
 * A DM rotation must carry the successor wraps for every participant (see
 * e2eeService.rotateDMKey); a server channel's rotation is still the bare
 * POST, its successor established by the rotation coordinator.
 */
export function useRotateKey(
  endpoint: string,
  onSuccess: () => void,
  request?: () => Promise<Response>
): UseRotateKeyResult {
  const [rotateStatus, setRotateStatus] = useState<RotateStatus>('idle');
  const [rotateMessage, setRotateMessage] = useState('');

  const handleRotate = useCallback(async () => {
    try {
      const res = request ? await request() : await apiFetch(endpoint, { method: 'POST' });
      if (res.ok) {
        setRotateStatus('success');
        onSuccess();
      } else if (res.status === 429) {
        const data = await res.json();
        setRotateStatus('error');
        setRotateMessage(`Try again in ${formatRetryAfter(data.retry_after)}`);
      } else {
        const data = await res.json().catch(() => ({ error: 'Rotation failed' }));
        setRotateStatus('error');
        setRotateMessage(data.error || 'Rotation failed');
      }
    } catch (error) {
      if (
        typeof error === 'object' &&
        error !== null &&
        'name' in error &&
        error.name === 'AbortError'
      ) {
        return;
      }
      setRotateStatus('error');
      setRotateMessage(error instanceof DMRotationError ? error.message : 'Network error');
    }
  }, [endpoint, onSuccess, request]);

  return { rotateStatus, rotateMessage, handleRotate };
}
