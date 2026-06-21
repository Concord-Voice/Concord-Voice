/**
 * useOsPermission — Convenience hook for components that need to gate on a
 * specific OS permission. Wraps the osPermissionStore for a single type.
 */

import { useState, useCallback } from 'react';
import {
  useOsPermissionStore,
  type OsPermissionType,
  type OsPermissionStatus,
} from '../stores/osPermissionStore';

interface UseOsPermissionReturn {
  status: OsPermissionStatus;
  isGranted: boolean;
  isDenied: boolean;
  isChecking: boolean;
  check: () => Promise<OsPermissionStatus>;
  request: () => Promise<OsPermissionStatus>;
  openSettings: () => Promise<void>;
}

export function useOsPermission(type: OsPermissionType): UseOsPermissionReturn {
  const status = useOsPermissionStore((s) => s[type]);
  const checkOne = useOsPermissionStore((s) => s.checkOne);
  const requestOne = useOsPermissionStore((s) => s.requestOne);
  const openSettingsAction = useOsPermissionStore((s) => s.openSettings);

  const [isChecking, setIsChecking] = useState(false);

  const check = useCallback(async (): Promise<OsPermissionStatus> => {
    setIsChecking(true);
    try {
      return await checkOne(type);
    } finally {
      setIsChecking(false);
    }
  }, [checkOne, type]);

  const request = useCallback(async (): Promise<OsPermissionStatus> => {
    setIsChecking(true);
    try {
      return await requestOne(type);
    } finally {
      setIsChecking(false);
    }
  }, [requestOne, type]);

  const openSettings = useCallback(async () => {
    await openSettingsAction(type);
  }, [openSettingsAction, type]);

  return {
    status,
    isGranted: status === 'granted',
    isDenied: status === 'denied' || status === 'restricted',
    isChecking,
    check,
    request,
    openSettings,
  };
}
