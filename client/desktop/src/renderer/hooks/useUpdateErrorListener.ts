import { useEffect } from 'react';
import { useUpdateStatusStore } from '../stores/updateStatusStore';

/**
 * Subscribes to `update:error` IPC events via the preload bridge and routes
 * security-flagged failures (cert-pin-failure, publisher-failure) into
 * `useUpdateStatusStore` so `UpdateSecurityBanner` renders. Non-security
 * errors (transient network failures) are ignored — those surface via the
 * existing `UpdateBanner` flow. Issue #658.
 */
export function useUpdateErrorListener(): void {
  const setSecurityError = useUpdateStatusStore((s) => s.setSecurityError);

  useEffect(() => {
    const api = globalThis.electron;
    if (!api?.onUpdateError) {
      // Dev environment (preload bridge may be unavailable outside packaged Electron).
      return undefined;
    }
    const unsubscribe = api.onUpdateError((payload) => {
      if (payload.securityEvent && payload.subtype) {
        setSecurityError(payload.subtype, payload.message);
      }
    });
    return () => {
      unsubscribe();
    };
  }, [setSecurityError]);
}
