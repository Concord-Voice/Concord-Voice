/**
 * OS Permission Store — Renderer-side state for OS-level permission statuses.
 *
 * NOT to be confused with permissionStore.ts (RBAC/SBAC server permissions).
 * This store tracks microphone, camera, screen, secure storage, and notification
 * permissions at the operating system level. It is NOT persisted — statuses are
 * always fetched fresh from the main process via IPC.
 */

import { createStore } from '../utils/createStore';
import { errorMessage } from '../utils/redactError';

// ─── Types (mirror permissionManager.ts in main process) ──────────────

export type OsPermissionType =
  | 'microphone'
  | 'camera'
  | 'screen'
  | 'secureStorage'
  | 'notifications';

export type OsPermissionStatus =
  | 'granted'
  | 'denied'
  | 'not-determined'
  | 'restricted'
  | 'unavailable';

export type OsPermissionState = Record<OsPermissionType, OsPermissionStatus>;

const INITIAL_STATUS: OsPermissionStatus = 'not-determined';

function notificationPermissionToStatus(permission: NotificationPermission): OsPermissionStatus {
  if (permission === 'granted') return 'granted';
  if (permission === 'denied') return 'denied';
  return 'not-determined';
}

function browserNotificationStatus(): OsPermissionStatus | null {
  const notificationApi = globalThis.Notification;
  if (notificationApi === undefined) return null;
  return notificationPermissionToStatus(notificationApi.permission);
}

function reconcileNotificationStatus(status: OsPermissionStatus): OsPermissionStatus {
  if (status !== 'not-determined') return status;
  const browserStatus = browserNotificationStatus();
  return browserStatus && browserStatus !== 'not-determined' ? browserStatus : status;
}

async function requestBrowserNotificationStatus(): Promise<OsPermissionStatus | null> {
  const notificationApi = globalThis.Notification;
  if (notificationApi === undefined) return null;
  const requestPermission = notificationApi.requestPermission;
  if (requestPermission === undefined) return browserNotificationStatus();
  try {
    return notificationPermissionToStatus(await requestPermission.call(notificationApi));
  } catch {
    return browserNotificationStatus();
  }
}

interface OsPermissionStoreState {
  // Permission statuses
  microphone: OsPermissionStatus;
  camera: OsPermissionStatus;
  screen: OsPermissionStatus;
  secureStorage: OsPermissionStatus;
  notifications: OsPermissionStatus;

  /** Whether the initial fetchAll has completed */
  isLoaded: boolean;

  /** Fetch all permission statuses from the main process */
  fetchAll: () => Promise<void>;

  /** Check a single permission (always goes to main process, updates store) */
  checkOne: (type: OsPermissionType) => Promise<OsPermissionStatus>;

  /** Request a single permission (triggers OS prompt on macOS, updates store) */
  requestOne: (type: OsPermissionType) => Promise<OsPermissionStatus>;

  /** Open the OS settings panel for a specific permission */
  openSettings: (type: OsPermissionType) => Promise<void>;

  /** Update a single permission status (used by push events from main process) */
  updateStatus: (type: OsPermissionType, status: OsPermissionStatus) => void;
}

export const useOsPermissionStore = createStore<OsPermissionStoreState>()((set, get) => ({
  microphone: INITIAL_STATUS,
  camera: INITIAL_STATUS,
  screen: INITIAL_STATUS,
  secureStorage: INITIAL_STATUS,
  notifications: INITIAL_STATUS,
  isLoaded: false,

  fetchAll: async () => {
    if (!globalThis.electron?.checkAllPermissions) {
      // Non-Electron context (web) — mark as loaded with unavailable statuses
      set({
        microphone: 'unavailable',
        camera: 'unavailable',
        screen: 'unavailable',
        secureStorage: 'unavailable',
        notifications: 'unavailable',
        isLoaded: true,
      });
      return;
    }
    try {
      const state = await globalThis.electron.checkAllPermissions();
      set({
        microphone: state.microphone ?? INITIAL_STATUS,
        camera: state.camera ?? INITIAL_STATUS,
        screen: state.screen ?? INITIAL_STATUS,
        secureStorage: state.secureStorage ?? INITIAL_STATUS,
        notifications: reconcileNotificationStatus(state.notifications ?? INITIAL_STATUS),
        isLoaded: true,
      });
    } catch (err) {
      console.error('[osPermissionStore] fetchAll failed:', errorMessage(err));
      set({ isLoaded: true }); // Avoid perpetual loading state
    }
  },

  checkOne: async (type) => {
    if (!globalThis.electron?.checkPermission) return get()[type];
    try {
      const status = await globalThis.electron.checkPermission(type);
      const resolved = type === 'notifications' ? reconcileNotificationStatus(status) : status;
      set({ [type]: resolved });
      return resolved;
    } catch (err) {
      console.error('[osPermissionStore] checkOne failed:', type, errorMessage(err));
      return get()[type];
    }
  },

  requestOne: async (type) => {
    let browserStatus: OsPermissionStatus | null = null;
    if (type === 'notifications') {
      browserStatus = await requestBrowserNotificationStatus();
      if (browserStatus === 'denied') {
        set({ [type]: browserStatus });
        return browserStatus;
      }
    }
    if (!globalThis.electron?.requestPermission) {
      if (browserStatus && browserStatus !== 'not-determined') {
        set({ [type]: browserStatus });
        return browserStatus;
      }
      return get()[type];
    }
    try {
      const status = await globalThis.electron.requestPermission(type);
      const resolved = type === 'notifications' ? reconcileNotificationStatus(status) : status;
      set({ [type]: resolved });
      return resolved;
    } catch (err) {
      console.error('[osPermissionStore] requestOne failed:', type, errorMessage(err));
      return get()[type];
    }
  },

  openSettings: async (type) => {
    if (!globalThis.electron?.openPermissionSettings) return;
    try {
      await globalThis.electron.openPermissionSettings(type);
    } catch (err) {
      console.error('[osPermissionStore] openSettings failed:', type, errorMessage(err));
    }
  },

  updateStatus: (type, status) => {
    set({ [type]: status });
  },
}));

// ─── Shared JIT Helper ───────────────────────────────────────────────

/**
 * Ensure an OS-level permission is granted. Checks current status, requests
 * if not-determined, and returns the final status. Use this in any code path
 * that needs a permission before proceeding (voiceService, useMicTest, etc.)
 * to avoid duplicating the check→request→evaluate flow.
 */
export async function ensureOsPermission(type: OsPermissionType): Promise<OsPermissionStatus> {
  const store = useOsPermissionStore.getState();
  let status = await store.checkOne(type);
  if (status === 'not-determined') {
    status = await store.requestOne(type);
  }
  return status;
}
