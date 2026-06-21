import { useState, useCallback } from 'react';
import { apiFetch } from '../services/apiClient';
import { formatRetryAfter } from '../utils/formatRetryAfter';

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
export function useRotateKey(endpoint: string, onSuccess: () => void): UseRotateKeyResult {
  const [rotateStatus, setRotateStatus] = useState<RotateStatus>('idle');
  const [rotateMessage, setRotateMessage] = useState('');

  const handleRotate = useCallback(async () => {
    try {
      const res = await apiFetch(endpoint, { method: 'POST' });
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
    } catch {
      setRotateStatus('error');
      setRotateMessage('Network error');
    }
  }, [endpoint, onSuccess]);

  return { rotateStatus, rotateMessage, handleRotate };
}
