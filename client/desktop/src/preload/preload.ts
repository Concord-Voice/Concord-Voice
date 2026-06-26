import { contextBridge, ipcRenderer } from 'electron';

import type { AppleSignInResult } from '../shared/appleSso';
import type { SSOSignInResult } from '../shared/sso';

// Expose protected methods that allow the renderer process to use
// the ipcRenderer without exposing the entire object
// Desktop source info for screen share picker
export interface DesktopSource {
  id: string;
  name: string;
  thumbnail: string; // data URL
  appIcon: string | null; // data URL
}

export type PermissionStatus =
  | 'granted'
  | 'denied'
  | 'not-determined'
  | 'restricted'
  | 'unavailable';

contextBridge.exposeInMainWorld('electron', {
  // App info
  getVersion: () => ipcRenderer.invoke('app:getVersion'),
  getPlatform: () => ipcRenderer.invoke('app:getPlatform'),
  // Reserved for Tier 3 remote SPA — the remote renderer calls this to verify
  // compatibility with the shell's IPC contract version before using newer APIs.
  getIpcContract: () => ipcRenderer.invoke('app:getIpcContract') as Promise<number>,
  // Forensic build-tag observability (#920 §5.13, #939). Read-only: returns
  // the CI build tag baked into the packaged app or 'unknown' for dev builds.
  // Threat-model: knowing the tag does not unlock any capability — exposure
  // here is acceptable because the read-only surface is bounded.
  getBuildTag: () => ipcRenderer.invoke('app:getBuildTag') as Promise<string>,

  // SPA self-heal (#753, ADR-0001): renderer signals chunk-load failure or
  // unhandled dynamic-import rejection. Main process refetches client config,
  // re-validates via resolveSpaSource(), and reloads the BrowserWindow once
  // before falling back to the bundled renderer (R2 retry policy).
  spa: {
    /**
     * Signal a chunk-load failure to the main process for self-heal recovery.
     *
     * @param payload.reason  Detection source — `'chunk-load'` (window.error
     *                        on script/link asset) or `'chunk-import-rejected'`
     *                        (unhandledrejection on dynamic import()).
     * @param payload.url     **Diagnostic-only.** The main-process recovery
     *                        primitive ignores this field; it never reaches
     *                        `webContents.loadURL`. Recovery refetches
     *                        `/api/v1/client/config` from scratch and
     *                        re-validates through `resolveSpaSource()`.
     *                        Logged for breadcrumb-style diagnostics only.
     */
    requestSelfHeal: (payload: { reason: 'chunk-load' | 'chunk-import-rejected'; url?: string }) =>
      ipcRenderer.invoke('spa:requestSelfHeal', payload) as Promise<void>,
  },

  // Media: screen capture sources for voice/video screen share
  getDesktopSources: () =>
    ipcRenderer.invoke('media:getDesktopSources') as Promise<DesktopSource[]>,

  // Clipboard (navigator.clipboard.writeText is blocked in Electron renderers)
  writeClipboard: (text: string) => ipcRenderer.invoke('clipboard:writeText', text),

  // GPU, display info & hardware acceleration
  getGPUInfo: () =>
    ipcRenderer.invoke('gpu:getInfo') as Promise<{
      vendor: string;
      device: string;
      encodeProfiles: string[];
    } | null>,
  getDisplayInfo: () =>
    ipcRenderer.invoke('screen:getDisplayInfo') as Promise<
      {
        width: number;
        height: number;
        refreshRate: number;
        scaleFactor: number;
        isPrimary: boolean;
        colorDepth: number;
        colorSpace: string;
      }[]
    >,
  getHardwareAcceleration: () =>
    ipcRenderer.invoke('app:getHardwareAcceleration') as Promise<boolean>,
  setHardwareAcceleration: (enabled: boolean) =>
    ipcRenderer.invoke('app:setHardwareAcceleration', enabled),
  relaunchApp: () => ipcRenderer.invoke('app:relaunch'),
  quitApp: () => ipcRenderer.invoke('app:quit'),

  // PiP (Picture-in-Picture) windows
  openPipWindow: (opts: { id: string; width?: number; height?: number; title?: string }) =>
    ipcRenderer.invoke('pip:open', opts),
  closePipWindow: (id: string) => ipcRenderer.invoke('pip:close', { id }),
  setPipAlwaysOnTop: (id: string, flag: boolean) =>
    ipcRenderer.invoke('pip:setAlwaysOnTop', { id, flag }),
  onPipClosed: (callback: (id: string) => void) => {
    const handler = (_event: Electron.IpcRendererEvent, data: { id: string }) => callback(data.id);
    ipcRenderer.on('pip:closed', handler);
    return () => {
      ipcRenderer.removeListener('pip:closed', handler);
    };
  },

  // Secure auth token management (safeStorage via main process)
  storeRefreshToken: (data: {
    refreshToken: string;
    rememberMe: boolean;
    apiBase: string;
    accessToken?: string;
  }) => ipcRenderer.invoke('auth:storeRefreshToken', data),
  restoreSession: () =>
    ipcRenderer.invoke('auth:restoreSession') as Promise<{
      status: string;
      accessToken?: string;
      sessionId?: string;
      e2eeKeys?: {
        wrappingKeyBase64: string;
        preferencesKeyBase64: string;
        wrappedPrivateKeyBase64: string;
      } | null;
      rememberMe?: boolean;
    }>,
  refreshToken: () =>
    ipcRenderer.invoke('auth:refreshToken') as Promise<{
      status: string;
      accessToken?: string;
      sessionId?: string;
      mfaChallengeToken?: string;
      mfaMethods?: string[];
      mfaRecoveryOnlyMethods?: string[];
    }>,
  clearTokens: () => ipcRenderer.invoke('auth:clearTokens'),
  logout: (data?: { accessToken?: string }) => ipcRenderer.invoke('auth:logout', data),
  getAuthCapabilities: () =>
    ipcRenderer.invoke('auth:getCapabilities') as Promise<{ persistAvailable: boolean }>,

  // E2EE key persistence (safeStorage via main process)
  storeE2EEKeys: (data: {
    wrappingKeyBase64: string;
    preferencesKeyBase64: string;
    wrappedPrivateKeyBase64: string;
  }) => ipcRenderer.invoke('auth:storeE2EEKeys', data),

  // Proactive token refresh event (#254): main process pushes new access token
  // when its timer or sleep/wake handler refreshes proactively.
  onTokenRefreshed: (callback: (data: { accessToken: string; sessionId?: string }) => void) => {
    const handler = (
      _event: Electron.IpcRendererEvent,
      data: { accessToken: string; sessionId?: string }
    ) => callback(data);
    ipcRenderer.on('auth:token-refreshed', handler);
    return () => {
      ipcRenderer.removeListener('auth:token-refreshed', handler);
    };
  },

  // Bundled-SPA fallback diagnostic event (#830/#831): main process emits
  // when spaLoader falls back to bundled for an unexpected reason.
  // Renderer subscribes via SpaFallbackOverlay component.
  onConfigFetchFailed: (callback: (data: { reason: string }) => void) => {
    const handler = (_event: Electron.IpcRendererEvent, data: { reason: string }) => callback(data);
    ipcRenderer.on('app:configFetchFailed', handler);
    return () => {
      ipcRenderer.removeListener('app:configFetchFailed', handler);
    };
  },
  onInviteReceived: (callback: (data: { code: string }) => void) => {
    const handler = (_event: Electron.IpcRendererEvent, data: { code: string }) => callback(data);
    ipcRenderer.on('invite:received', handler);
    return () => {
      ipcRenderer.removeListener('invite:received', handler);
    };
  },
  inviteRendererReady: () => ipcRenderer.send('invite:renderer-ready'),

  // Machine ID for token theft detection (#89)
  getMachineId: () => ipcRenderer.invoke('auth:getMachineId') as Promise<string>,

  // System info for About page (#155 Tier 4)
  getSystemInfo: () =>
    ipcRenderer.invoke('app:getSystemInfo') as Promise<{
      platform: string;
      arch: string;
      electronVersion: string;
      chromiumVersion: string;
      nodeVersion: string;
    }>,

  // Developer Mode (REMOVE BEFORE BETA)
  getDeveloperMode: () => ipcRenderer.invoke('app:getDeveloperMode') as Promise<boolean>,
  setDeveloperMode: (enabled: boolean) =>
    ipcRenderer.invoke('app:setDeveloperMode', enabled) as Promise<void>,

  // Auto-update (#155)
  checkForUpdates: () => ipcRenderer.invoke('update:check'),
  downloadUpdate: () => ipcRenderer.invoke('update:download'),
  installUpdate: () => ipcRenderer.invoke('update:install'),
  getAllowPrerelease: () => ipcRenderer.invoke('update:getAllowPrerelease') as Promise<boolean>,
  setAllowPrerelease: (enabled: boolean) =>
    ipcRenderer.invoke('update:setAllowPrerelease', enabled),
  onUpdateAvailable: (
    callback: (info: { version: string; releaseNotes?: string; releaseDate?: string }) => void
  ) => {
    const handler = (
      _event: Electron.IpcRendererEvent,
      data: { version: string; releaseNotes?: string; releaseDate?: string }
    ) => callback(data);
    ipcRenderer.on('update:available', handler);
    return () => {
      ipcRenderer.removeListener('update:available', handler);
    };
  },
  onUpdateNotAvailable: (callback: (info: { version: string }) => void) => {
    const handler = (_event: Electron.IpcRendererEvent, data: { version: string }) =>
      callback(data);
    ipcRenderer.on('update:not-available', handler);
    return () => {
      ipcRenderer.removeListener('update:not-available', handler);
    };
  },
  onUpdateDownloadProgress: (
    callback: (progress: {
      percent: number;
      transferred: number;
      total: number;
      bytesPerSecond: number;
    }) => void
  ) => {
    const handler = (
      _event: Electron.IpcRendererEvent,
      data: { percent: number; transferred: number; total: number; bytesPerSecond: number }
    ) => callback(data);
    ipcRenderer.on('update:download-progress', handler);
    return () => {
      ipcRenderer.removeListener('update:download-progress', handler);
    };
  },
  onUpdateDownloaded: (callback: (info: { version: string }) => void) => {
    const handler = (_event: Electron.IpcRendererEvent, data: { version: string }) =>
      callback(data);
    ipcRenderer.on('update:downloaded', handler);
    return () => {
      ipcRenderer.removeListener('update:downloaded', handler);
    };
  },
  // Extended payload (#658): `securityEvent` + `subtype` discriminate cert-pin
  // vs. publisher-signature failures so the renderer can render the correct
  // banner (UpdateSecurityBanner). Optional to remain backward-compatible with
  // existing listeners that only read `message`.
  onUpdateError: (
    callback: (error: {
      message: string;
      securityEvent?: boolean;
      subtype?: 'cert-pin-failure' | 'publisher-failure';
    }) => void
  ) => {
    const handler = (
      _event: Electron.IpcRendererEvent,
      data: {
        message: string;
        securityEvent?: boolean;
        subtype?: 'cert-pin-failure' | 'publisher-failure';
      }
    ) => callback(data);
    ipcRenderer.on('update:error', handler);
    return () => {
      ipcRenderer.removeListener('update:error', handler);
    };
  },
  onUpdateRollback: (
    callback: (data: { fromVersion: string; toVersion: string; message: string }) => void
  ) => {
    const handler = (
      _event: Electron.IpcRendererEvent,
      data: { fromVersion: string; toVersion: string; message: string }
    ) => callback(data);
    ipcRenderer.on('update:rollback', handler);
    return () => {
      ipcRenderer.removeListener('update:rollback', handler);
    };
  },
  getUpdateLogPath: () => ipcRenderer.invoke('update:getLogPath') as Promise<string | null>,

  // OS permission management (#197)
  checkAllPermissions: () =>
    ipcRenderer.invoke('permission:checkAll') as Promise<Record<string, PermissionStatus>>,
  checkPermission: (type: string) =>
    ipcRenderer.invoke('permission:check', type) as Promise<PermissionStatus>,
  requestPermission: (type: string) =>
    ipcRenderer.invoke('permission:request', type) as Promise<PermissionStatus>,
  openPermissionSettings: (type: string) =>
    ipcRenderer.invoke('permission:openSettings', type) as Promise<void>,
  onPermissionChanged: (callback: (data: { type: string; status: PermissionStatus }) => void) => {
    const handler = (
      _event: Electron.IpcRendererEvent,
      data: {
        type: string;
        status: PermissionStatus;
      }
    ) => callback(data);
    ipcRenderer.on('permission:changed', handler);
    return () => {
      ipcRenderer.removeListener('permission:changed', handler);
    };
  },

  // Desktop notification helpers (#175)
  setBadgeCount: (count: number) => ipcRenderer.invoke('app:setBadgeCount', count),
  flashFrame: (flash: boolean) => ipcRenderer.invoke('app:flashFrame', flash),
  focusWindow: () => ipcRenderer.invoke('app:focusWindow'),

  // Open a URL in the user's default browser via the main process. The main
  // process re-validates the protocol (http/https/mailto only) before calling
  // shell.openExternal — never trust the renderer. Returns `{ ok }` so the
  // renderer can log or surface failures without needing to reason about
  // exceptions. SafeLink.tsx is the canonical caller.
  openExternal: (url: string) =>
    ipcRenderer.invoke('open-external', url) as Promise<{
      ok: boolean;
      reason?: 'untrusted-sender' | 'invalid-protocol' | 'invalid-url';
    }>,

  // Save a decrypted image attachment to disk via a native Save-As dialog
  // (#1729). An E2EE attachment is decrypted only in the renderer (a blob URL),
  // so the bytes are passed to the main process, which enforces sender-frame
  // validation, a size cap, filename sanitisation, and a user-chosen path.
  saveImageAs: (bytes: ArrayBuffer, suggestedName: string) =>
    ipcRenderer.invoke('image:saveAs', bytes, suggestedName) as Promise<{
      ok: boolean;
      canceled?: boolean;
      reason?: 'untrusted-sender' | 'invalid-args' | 'too-large' | 'write-failed';
    }>,

  // SSO loopback HTTP server bridge (#270, extended for Apple in #271): main
  // process owns a 127.0.0.1 ephemeral-port server that captures the OAuth
  // callback. Renderer drives the provider redirect via the system browser
  // and awaits code+state. Apple's response_mode=form_post additionally
  // delivers the first-auth `user` JSON; the loopback surfaces it as the
  // optional appleUserData field, undefined for Google or Apple subsequent
  // auths.
  // #974: appleSignIn runs the whole Apple flow main-side; appleCancel tears it down.
  sso: {
    startLoopback: (): Promise<{ port: number; redirectURI: string }> =>
      ipcRenderer.invoke('sso:startLoopback'),
    awaitCallback: (
      port: number
    ): Promise<{ code: string; state: string; appleUserData?: string }> =>
      ipcRenderer.invoke('sso:awaitCallback', port),
    cancelLoopback: (port: number): void => ipcRenderer.send('sso:cancelLoopback', port),
    appleSignIn: (): Promise<AppleSignInResult> => ipcRenderer.invoke('sso:appleSignIn'),
    appleCancel: (): void => ipcRenderer.send('sso:appleCancel'),
    googleSignIn: (): Promise<SSOSignInResult> => ipcRenderer.invoke('sso:googleSignIn'),
    googleCancel: (): void => ipcRenderer.send('sso:googleCancel'),
  },

  // Window chrome control surface (#806): renderer pushes Client Behavior
  // preferences so the main-process [X]/[-] intercept can route correctly,
  // and the theme-color sync keeps the per-platform titleBarOverlay matching
  // the active theme.
  window: {
    setClientBehavior: (cb: { toTray: string; toToolbar: string }): Promise<void> =>
      ipcRenderer.invoke('window:setClientBehavior', cb),
    quit: (): Promise<void> => ipcRenderer.invoke('window:quit'),
    setTitleBarOverlayColor: (options: { color: string; symbolColor: string }): Promise<void> =>
      ipcRenderer.invoke('window:setTitleBarOverlayColor', options),
  },

  // Version surface (#806): packaged app version + active SPA hash for the
  // Titlebar version label. onChange fires when an SPA hot-update lands.
  version: {
    get: (): Promise<{ appVersion: string; spaHash: string | null }> =>
      ipcRenderer.invoke('window:getVersionString'),
    onChange: (callback: (data: { spaHash: string | null }) => void): (() => void) => {
      const listener = (
        _event: Electron.IpcRendererEvent,
        data: { spaHash: string | null }
      ): void => callback(data);
      ipcRenderer.on('spa:versionChanged', listener);
      return (): void => {
        ipcRenderer.removeListener('spa:versionChanged', listener);
      };
    },
  },

  // Attestation token bridge (#677): renderer reads the main-process-cached
  // attestation token to attach as X-Attestation-Token, and clears it after
  // a 403 to force re-attest.
  attestation: {
    getToken: (): Promise<string | null> => ipcRenderer.invoke('attestation:get-token'),
    clearToken: (): Promise<void> => ipcRenderer.invoke('attestation:clear-token'),
  },

  // Force an immediate update check (#677): the attestation 403-retry path
  // calls this to pull a newer signed build when the server rejects the
  // current client. Always uses the pinned feed (#719).
  updater: {
    forceCheckForUpdates: (reason: 'attestation_required' | 'user_triggered'): Promise<void> =>
      ipcRenderer.invoke('updater:force-check', reason),
  },
  // SPA (UI) update axis — distinct from the electron-updater desktop-binary
  // axis above. Lets the user check for / load the latest remote UI without an
  // app restart (escape the bundled-fallback cold-start trap). NO URL crosses
  // the bridge: the main process derives the load URL from the authenticated
  // config fetch, so a compromised renderer cannot choose the origin.
  spaUpdate: {
    checkForUpdate: (): Promise<{
      currentMode: 'remote' | 'bundled';
      remoteAvailable: boolean;
      newerBytesAvailable: boolean | null;
      reason: string;
    }> => ipcRenderer.invoke('spa:checkForUpdate'),
    reloadLatest: (): Promise<{
      mode: 'remote' | 'bundled';
      changed: boolean;
      rejected?: boolean;
    }> => ipcRenderer.invoke('spa:reloadLatest'),
  },
});

// Type definitions for the exposed API
export interface ElectronAPI {
  getVersion: () => Promise<string>;
  getPlatform: () => Promise<string>;
  /** Tier 3 remote SPA: query the shell's IPC contract version for compatibility. */
  getIpcContract: () => Promise<number>;
  /**
   * Forensic build-tag observability (#920 §5.13, #939). Returns the CI build
   * tag baked into the packaged app (via forge `extraResource` `buildtag.json`)
   * or `'unknown'` for local dev builds. Read-only, information-only — knowing
   * the tag does not unlock any capability.
   */
  getBuildTag: () => Promise<string>;

  /**
   * SPA self-heal (#753, ADR-0001): renderer requests the main process refetch
   * client config and reload the BrowserWindow once after a chunk-load failure.
   */
  spa: {
    requestSelfHeal: (payload: {
      reason: 'chunk-load' | 'chunk-import-rejected';
      url?: string;
    }) => Promise<void>;
  };

  getDesktopSources: () => Promise<DesktopSource[]>;
  writeClipboard: (text: string) => Promise<void>;
  getGPUInfo: () => Promise<{ vendor: string; device: string; encodeProfiles: string[] } | null>;
  getDisplayInfo: () => Promise<
    {
      width: number;
      height: number;
      refreshRate: number;
      scaleFactor: number;
      isPrimary: boolean;
      colorDepth: number;
      colorSpace: string;
    }[]
  >;
  getHardwareAcceleration: () => Promise<boolean>;
  setHardwareAcceleration: (enabled: boolean) => Promise<void>;
  relaunchApp: () => Promise<void>;
  quitApp: () => Promise<void>;
  openPipWindow: (opts: {
    id: string;
    width?: number;
    height?: number;
    title?: string;
  }) => Promise<void>;
  closePipWindow: (id: string) => Promise<void>;
  setPipAlwaysOnTop: (id: string, flag: boolean) => Promise<void>;
  onPipClosed: (callback: (id: string) => void) => () => void;

  // Secure auth token management
  storeRefreshToken: (data: {
    refreshToken: string;
    rememberMe: boolean;
    apiBase: string;
    accessToken?: string;
  }) => Promise<void>;
  restoreSession: () => Promise<{
    status: string;
    accessToken?: string;
    sessionId?: string;
    e2eeKeys?: {
      wrappingKeyBase64: string;
      preferencesKeyBase64: string;
      wrappedPrivateKeyBase64: string;
    } | null;
    rememberMe?: boolean;
  }>;
  refreshToken: () => Promise<{
    status: string;
    accessToken?: string;
    sessionId?: string;
    mfaChallengeToken?: string;
    mfaMethods?: string[];
    mfaRecoveryOnlyMethods?: string[];
  }>;
  clearTokens: () => Promise<void>;
  logout: (data?: { accessToken?: string }) => Promise<void>;
  getAuthCapabilities: () => Promise<{ persistAvailable: boolean }>;

  // E2EE key persistence
  storeE2EEKeys: (data: {
    wrappingKeyBase64: string;
    preferencesKeyBase64: string;
    wrappedPrivateKeyBase64: string;
  }) => Promise<void>;

  // Proactive token refresh event (#254)
  onTokenRefreshed: (
    callback: (data: { accessToken: string; sessionId?: string }) => void
  ) => () => void;

  // Bundled-SPA fallback diagnostic event (#830/#831)
  onConfigFetchFailed: (callback: (data: { reason: string }) => void) => () => void;
  onInviteReceived: (callback: (data: { code: string }) => void) => () => void;
  inviteRendererReady: () => void;

  // Machine ID for token theft detection
  getMachineId: () => Promise<string>;

  // System info for About page (#155 Tier 4)
  getSystemInfo: () => Promise<{
    platform: string;
    arch: string;
    electronVersion: string;
    chromiumVersion: string;
    nodeVersion: string;
  }>;

  // Developer Mode (REMOVE BEFORE BETA)
  getDeveloperMode: () => Promise<boolean>;
  setDeveloperMode: (enabled: boolean) => Promise<void>;

  // Auto-update (#155)
  checkForUpdates: () => Promise<void>;
  downloadUpdate: () => Promise<string[]>;
  installUpdate: () => Promise<void>;
  getAllowPrerelease: () => Promise<boolean>;
  setAllowPrerelease: (enabled: boolean) => Promise<void>;
  onUpdateAvailable: (
    callback: (info: { version: string; releaseNotes?: string; releaseDate?: string }) => void
  ) => () => void;
  onUpdateNotAvailable: (callback: (info: { version: string }) => void) => () => void;
  onUpdateDownloadProgress: (
    callback: (progress: {
      percent: number;
      transferred: number;
      total: number;
      bytesPerSecond: number;
    }) => void
  ) => () => void;
  onUpdateDownloaded: (callback: (info: { version: string }) => void) => () => void;
  onUpdateError: (
    callback: (error: {
      message: string;
      securityEvent?: boolean;
      subtype?: 'cert-pin-failure' | 'publisher-failure';
    }) => void
  ) => () => void;
  onUpdateRollback: (
    callback: (data: { fromVersion: string; toVersion: string; message: string }) => void
  ) => () => void;
  getUpdateLogPath: () => Promise<string | null>;

  // OS permission management (#197)
  checkAllPermissions: () => Promise<Record<string, PermissionStatus>>;
  checkPermission: (type: string) => Promise<PermissionStatus>;
  requestPermission: (type: string) => Promise<PermissionStatus>;
  openPermissionSettings: (type: string) => Promise<void>;
  onPermissionChanged: (
    callback: (data: { type: string; status: PermissionStatus }) => void
  ) => () => void;

  // Desktop notification helpers (#175)
  setBadgeCount: (count: number) => Promise<void>;
  flashFrame: (flash: boolean) => Promise<void>;
  focusWindow: () => Promise<void>;

  // Open an external URL via shell.openExternal (main process enforces
  // http/https/mailto protocol allowlist).
  openExternal: (url: string) => Promise<{
    ok: boolean;
    reason?: 'untrusted-sender' | 'invalid-protocol' | 'invalid-url';
  }>;

  // Save a decrypted image attachment to disk via a native Save-As dialog (#1729).
  saveImageAs: (
    bytes: ArrayBuffer,
    suggestedName: string
  ) => Promise<{
    ok: boolean;
    canceled?: boolean;
    reason?: 'untrusted-sender' | 'invalid-args' | 'too-large' | 'write-failed';
  }>;

  // SSO loopback HTTP server bridge (#270, extended for Apple in #271)
  sso: {
    startLoopback: () => Promise<{ port: number; redirectURI: string }>;
    awaitCallback: (
      port: number
    ) => Promise<{ code: string; state: string; appleUserData?: string }>;
    cancelLoopback: (port: number) => void;
    appleSignIn: () => Promise<AppleSignInResult>;
    appleCancel: () => void;
    googleSignIn: () => Promise<SSOSignInResult>;
    googleCancel: () => void;
  };

  // Window chrome control surface (#806)
  window: {
    setClientBehavior: (cb: { toTray: string; toToolbar: string }) => Promise<void>;
    quit: () => Promise<void>;
    setTitleBarOverlayColor: (options: { color: string; symbolColor: string }) => Promise<void>;
  };

  // Version + SPA hash surface (#806)
  version: {
    get: () => Promise<{ appVersion: string; spaHash: string | null }>;
    onChange: (callback: (data: { spaHash: string | null }) => void) => () => void;
  };

  // Attestation token bridge (#677)
  attestation: {
    getToken: () => Promise<string | null>;
    clearToken: () => Promise<void>;
  };

  // Force-check bridge (#677)
  updater: {
    forceCheckForUpdates: (reason: 'attestation_required' | 'user_triggered') => Promise<void>;
  };

  // SPA (UI) update axis — check for / load the latest remote UI without restart.
  spaUpdate: {
    checkForUpdate: () => Promise<{
      currentMode: 'remote' | 'bundled';
      remoteAvailable: boolean;
      newerBytesAvailable: boolean | null;
      reason: string;
    }>;
    reloadLatest: () => Promise<{
      mode: 'remote' | 'bundled';
      changed: boolean;
      rejected?: boolean;
    }>;
  };
}

declare global {
  var electron: ElectronAPI;
  interface Window {
    electron: ElectronAPI;
  }
}
