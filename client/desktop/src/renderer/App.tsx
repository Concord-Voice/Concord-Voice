import { Suspense, lazy, useEffect, useState } from 'react';
import { Routes, Route, Navigate, Outlet, useNavigate } from 'react-router-dom';
import { Titlebar } from './components/Titlebar/Titlebar';
import AuthFlow from './components/Auth/AuthFlow';
import MainView from './components/MainView/MainView';
import DirectMessagesView from './components/DirectMessages/DirectMessagesView';
import ContextMenuProvider from './components/ui/ContextMenuProvider';
import ConnectionLostOverlay from './components/ui/ConnectionLostOverlay';
import SettingsOverlayHost from './components/Settings/SettingsOverlayHost';
import { SpaFallbackOverlay } from './components/SpaFallbackOverlay/SpaFallbackOverlay';
import { ErrorBoundary } from './components/ErrorBoundary';
const PipWindow = lazy(() => import('./components/Voice/PipWindow'));
import ForceUpdateOverlay from './components/ui/ForceUpdateOverlay';
import UpdateBanner from './components/ui/UpdateBanner';
import { UpdateSecurityBanner } from './components/Updates/UpdateSecurityBanner';
import { IncomingCallBanner } from './components/Voice/IncomingCallBanner';
import { OutgoingCallModal } from './components/Voice/OutgoingCallModal';
import { useUpdateErrorListener } from './hooks/useUpdateErrorListener';
import MFAChallengeModal from './components/Auth/MFAChallengeModal';
import AttestationFailedModalHost from './components/AttestationFailedModal';
import JoinServerModal from './components/Servers/JoinServerModal';
import SSOEagerUnlock from './components/Auth/SSOEagerUnlock';
import { useAuthStore } from './stores/authStore';
import { useE2EEStore } from './stores/e2eeStore';
import { useUserStore } from './stores/userStore';
import { errorMessage } from './utils/redactError';
import { runRecoveryModule } from './utils/runRecoveryModule';
import { useVideoSettingsStore } from './stores/videoSettingsStore';
import { useOsPermissionStore, type OsPermissionType } from './stores/osPermissionStore';
import { useWebSocket } from './hooks/useWebSocket';
import { useLaunchReset } from './hooks/useLaunchReset';
import SubscriptionResetModal from './components/Settings/SubscriptionResetModal';
import { e2eeService } from './services/e2eeService';
import { hydratePostLogin } from './services/postLoginHydration';
import { usePrivacyStore } from './stores/privacyStore';
import { klipyClient } from './services/gifProvider/klipyClient';
import { clientConfigService } from './services/clientConfigService';
import { detectCodecCapabilities, prewarmWebRTC } from './services/mediaCapabilities';
import { useNotificationNavigationStore } from './stores/notificationNavigationStore';
import { useServerStore } from './stores/serverStore';
import { useChannelStore } from './stores/channelStore';
import { useDMStore } from './stores/dmStore';
import { desktopNotificationService } from './services/desktopNotificationService';
import { usePendingRegistrationStore } from './stores/pendingRegistrationStore';
// resetService is loaded on-demand via dynamic import() to allow code splitting

// ─── Error Boundary Fallbacks ─────────────────────────────────────────
// Static text only — no user data, no display names, no avatars.

export function AppRootFallback() {
  return (
    <div className="error-boundary-fallback error-boundary-fallback--fatal">
      <h2>Something went wrong</h2>
      <p>Please restart the application.</p>
      <button onClick={() => globalThis.location.reload()}>Reload</button>
    </div>
  );
}

export function AuthenticatedViewFallback() {
  return (
    <div className="error-boundary-fallback">
      <h2>This view failed to load</h2>
      <p>Try selecting a different view.</p>
    </div>
  );
}

export function SettingsFallback() {
  return (
    <div className="error-boundary-fallback">
      <h2>Settings failed to load</h2>
      <p>Close and reopen settings to try again.</p>
    </div>
  );
}

export function PipFallback() {
  return (
    <div className="error-boundary-fallback">
      <h2>Voice UI crashed</h2>
      <p>Your call is still active. Close this window and rejoin from the main app.</p>
    </div>
  );
}

export function AuthFallback() {
  return (
    <div className="error-boundary-fallback">
      <h2>Authentication UI failed to load</h2>
      <p>Please reload the application.</p>
      <button onClick={() => globalThis.location.reload()}>Reload</button>
    </div>
  );
}

/** Soft-restart on fatal render crash — preserves session, avoids nuclear reset. */
export function handleAppRootError() {
  import('./services/recoveryService')
    .then((m) => m.markRendererCrashed())
    .catch((err) => {
      console.error('[App] Failed to mark renderer crashed:', errorMessage(err));
    });
  import('./services/resetService')
    .then((m) => m.softRestart())
    .catch((err) => {
      console.error('[App] Failed to soft-restart, forcing reload:', errorMessage(err));
      globalThis.location.reload();
    });
}

// ─── Launch-reset host (#1301) ─────────────────────────────────────────
// Runs the once-per-session free-tier settings clamp after entitlements are
// known (hydrated by hydratePostLogin) and surfaces the one-time explainer.
// Mounted inside the authenticated tree so it fires only for a logged-in user.
function LaunchResetHost() {
  const { showResetModal, acknowledge } = useLaunchReset();
  return <SubscriptionResetModal open={showResetModal} onAcknowledge={acknowledge} />;
}

// ─── Authenticated Layout ──────────────────────────────────────────────

function AuthenticatedLayout() {
  const accessToken = useAuthStore((state) => state.accessToken);
  const emailVerified = useAuthStore((state) => state.emailVerified);
  const user = useUserStore((state) => state.user);
  const fetchUser = useUserStore((state) => state.fetchUser);
  // SSO eager-unlock gate (#270 Task 21b). Selectively subscribed so the
  // layout re-renders the moment e2eeService.initialize flips `ready=true`,
  // letting us fall through to <Outlet /> on the next render.
  const e2eeReady = useE2EEStore((s) => s.ready);
  const needsSSOUnlock = useE2EEStore((s) => s.needsSSOUnlock);
  const navigate = useNavigate();

  // Single WebSocket connection that persists across all authenticated routes
  useWebSocket();

  // Handle notification click navigation (#175) — must live here (not MainView)
  // so it works regardless of which authenticated route is active.
  useEffect(() => {
    const unsub = useNotificationNavigationStore.subscribe((state) => {
      const nav = state.pendingNavigation;
      if (!nav) return;

      if (nav.type === 'channel' && nav.serverId) {
        const serverStore = useServerStore.getState();
        if (serverStore.activeServerId !== nav.serverId) {
          serverStore.setActiveServer(nav.serverId);
        }
        useChannelStore.getState().setActiveChannel(nav.targetId);
        navigate('/app');
      } else if (nav.type === 'dm') {
        useDMStore.getState().setActiveConversation(nav.targetId);
        navigate('/app/dms');
      }

      desktopNotificationService.clearBadge();
      useNotificationNavigationStore.getState().clearPendingNavigation();
    });

    return unsub;
  }, [navigate]);

  // Eagerly fetch user profile when authenticated (before child routes mount)
  useEffect(() => {
    if (accessToken && !user) {
      fetchUser();
    }
  }, [accessToken, user, fetchUser]);

  // Pre-warm WebRTC engine + voice chunk so first voice join is fast (~200ms vs ~2s)
  useEffect(() => {
    prewarmWebRTC();
  }, []);

  // Load privacy settings + wire KLIPY personalization preference.
  // KLIPY traffic always routes through the control-plane proxy now, so we
  // only need to forward the personalization (customer_id) preference.
  useEffect(() => {
    if (!accessToken) return;
    const applyKlipyMode = (s: ReturnType<typeof usePrivacyStore.getState>['settings']) => {
      klipyClient.setPersonalizationEnabled(s.sharePersonalizationWithGifProvider);
    };
    usePrivacyStore
      .getState()
      .fetchPrivacy()
      .then(() => {
        applyKlipyMode(usePrivacyStore.getState().settings);
      });
    const unsub = usePrivacyStore.subscribe((state) => applyKlipyMode(state.settings));
    return () => unsub();
  }, [accessToken]);

  // Pre-cache codec capabilities, GPU info, and detect system HDR
  useEffect(() => {
    Promise.all([
      detectCodecCapabilities(),
      globalThis.electron?.getGPUInfo?.() ?? Promise.resolve(null),
      globalThis.electron?.getHardwareAcceleration?.() ?? Promise.resolve(null),
      globalThis.electron?.getDisplayInfo?.() ?? Promise.resolve(null),
    ]).then(([caps, gpu, hwAccel, displays]) => {
      // Detect HDR: any display with colorDepth > 24 or wide gamut color space
      const systemHdr =
        (displays as { colorDepth?: number; colorSpace?: string }[] | null)?.some(
          (d) => (d.colorDepth ?? 0) > 24 || (d.colorSpace && d.colorSpace !== 'srgb')
        ) ?? false;

      const current = useVideoSettingsStore.getState();
      useVideoSettingsStore.setState({
        codecCapabilities: caps,
        gpuInfo: gpu ?? current.gpuInfo,
        systemHdr,
        ...(hwAccel == null ? {} : { hardwareAcceleration: hwAccel }),
      });

      // Auto-enable HDR encoding on first detection if user hasn't explicitly toggled it
      if (systemHdr && !current.hdrEncoding) {
        useVideoSettingsStore.getState().setHdrEncoding(true);
      }
    });
  }, []);

  if (!accessToken) {
    return <Navigate to="/" replace />;
  }

  // Redirect unverified users back to auth flow (which will show email verification)
  if (!emailVerified) {
    return <Navigate to="/" replace />;
  }

  // SSO eager-unlock gate (#270 Task 21b): when an SSO callback returned
  // `logged_in` but no E2EE keys have been initialized on this device yet,
  // SSOEagerUnlock prompts for the user's passphrase and calls
  // e2eeService.initialize, which flips `ready=true` in useE2EEStore. Once
  // ready, the gate falls through on the next render. Password-login users
  // never set `needsSSOUnlock` (they initialize E2EE inline before
  // navigating here), so they bypass this gate entirely.
  if (needsSSOUnlock && !e2eeReady) {
    const handleUnlock = () => {
      // Clear the one-shot flag — `e2eeService.initialize` already flipped
      // `ready=true` via the store sync, so the next render falls through.
      useE2EEStore.getState().setNeedsSSOUnlock(false);
    };
    const handleSocialRecovery = () => {
      // Drop the access token + reset E2EE state to return the user to the
      // auth flow, where they can choose "Forgot password?" from Login.
      // We don't have a direct route from inside the post-auth gate to
      // AuthFlow's `forgot-password` step (it's an internal AuthStep), and
      // the IR playbook for first-device recovery passes through password
      // recovery anyway, so this redirect is the canonical entry.
      useE2EEStore.getState().reset();
      useAuthStore.getState().clearAccessToken();
    };
    return (
      <ErrorBoundary fallback={<AuthenticatedViewFallback />}>
        <SSOEagerUnlock onUnlock={handleUnlock} onSocialRecovery={handleSocialRecovery} />
      </ErrorBoundary>
    );
  }

  return (
    <>
      <ErrorBoundary fallback={<AuthenticatedViewFallback />}>
        <Outlet />
      </ErrorBoundary>
      <ErrorBoundary fallback={<SettingsFallback />}>
        <SettingsOverlayHost />
      </ErrorBoundary>
      <ConnectionLostOverlay />
      <LaunchResetHost />
    </>
  );
}

// ─── App Root ──────────────────────────────────────────────────────────

// Module-level guard: React Strict Mode (dev) double-mounts the component,
// firing useEffect twice. Without this flag, two restoreSession IPC calls
// can trigger two sequential token rotations.
let restoreSessionCalled = false;

export function __resetRestoreSessionCalledForTesting(): void {
  restoreSessionCalled = false;
}

function App() {
  // PiP windows don't need auth — they communicate via BroadcastChannel.
  // Skip session restore entirely to avoid unnecessary token rotations.
  const isPipWindow = globalThis.location.hash.startsWith('#/pip/');
  const [isRestoring, setIsRestoring] = useState(!isPipWindow);
  const [deepLinkInviteCode, setDeepLinkInviteCode] = useState<string | null>(null);
  const [isDeepLinkInviteOpen, setIsDeepLinkInviteOpen] = useState(false);
  const accessToken = useAuthStore((state) => state.accessToken);
  const emailVerified = useAuthStore((state) => state.emailVerified);
  const navigate = useNavigate();

  // Route cert-pin / publisher-signature failures into useUpdateStatusStore
  // so UpdateSecurityBanner can surface them. #658
  useUpdateErrorListener();

  // Clear any expired pending registration on startup so stale sessionStorage
  // state doesn't route the user to an unreachable verification screen.
  useEffect(() => {
    const pending = usePendingRegistrationStore.getState();
    if (pending.pendingId && pending.isExpired()) {
      pending.clearPending();
    }
  }, []);

  useEffect(() => {
    if (isPipWindow) return undefined;
    const subscribe = globalThis.electron?.onInviteReceived;
    if (typeof subscribe !== 'function') return undefined;
    const unsubscribe = subscribe(({ code }) => {
      setDeepLinkInviteCode(code);
      if (useAuthStore.getState().accessToken && useAuthStore.getState().emailVerified) {
        setIsDeepLinkInviteOpen(true);
      }
    });
    globalThis.electron?.inviteRendererReady?.();
    return unsubscribe;
  }, [isPipWindow]);

  useEffect(() => {
    if (deepLinkInviteCode && accessToken && emailVerified) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: opens queued invite modal after auth state becomes eligible; not a render loop
      setIsDeepLinkInviteOpen(true);
    }
  }, [deepLinkInviteCode, accessToken, emailVerified]);

  // Restore session on startup: ask main process to decrypt the
  // safeStorage-encrypted refresh token and exchange it for a fresh
  // access token, so users with "Remember Me" don't see a login flash.
  // Note: the IPC handler is also deduplicated in main.ts, but we guard
  // here as well to avoid even making a redundant IPC call.
  useEffect(() => {
    if (isPipWindow) return; // PiP windows skip session restore
    // Already authenticated (login succeeded, or HMR re-mounted the component)
    // — no need to hit restoreSession again.
    if (useAuthStore.getState().accessToken) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears restoring flag when session already exists; not a render loop
      setIsRestoring(false);
      return;
    }
    if (restoreSessionCalled) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears restoring flag when restore already in progress; not a render loop
      setIsRestoring(false);
      return;
    }
    restoreSessionCalled = true;

    const restore = async () => {
      if (!globalThis.electron?.restoreSession) {
        setIsRestoring(false);
        return;
      }

      const result = await globalThis.electron.restoreSession();
      if (result.status === 'restored' && result.accessToken) {
        useAuthStore.getState().setAccessToken(result.accessToken);
        if (result.sessionId) useAuthStore.getState().setSessionId(result.sessionId);
        if (typeof result.rememberMe === 'boolean') {
          useAuthStore.getState().setRememberMe(result.rememberMe);
        }

        // Restore E2EE service from the restored key material (disk for
        // rememberMe sessions; main-process memory for session-only soft
        // reloads — see tokenManager.restoreE2EEKeys). Skip only when there is
        // genuinely no key material to initialize from.
        if (result.e2eeKeys) {
          try {
            await e2eeService.initializeFromStoredKeys(result.e2eeKeys);
            console.debug('E2EE service restored from stored session keys');
          } catch (err) {
            console.warn(
              'Failed to restore E2EE keys — E2EE features will require re-login:',
              errorMessage(err)
            );
          }
        }

        // Hydrate post-login user state on EVERY successful restore (#1297, #1870)
        // — not only when e2eeKeys are present — so a session-only (rememberMe=false)
        // soft reload, which restores auth + E2EE from main-process memory, also
        // reloads servers/profile/preferences instead of landing authenticated but
        // empty. Runs after E2EE init so decrypted content has its keys. Wrapped so
        // a hydration throw cannot strand the UI in the restoring state.
        try {
          await hydratePostLogin();
        } catch (err) {
          console.warn('Post-login hydration failed during session restore:', errorMessage(err));
        }
      } else {
        // Session cannot be restored — clear content stores but keep disk tokens.
        // The refresh may have failed due to a transient error (server not ready,
        // network issue). Preserving disk tokens allows the next launch to retry.
        // If the user logs in fresh, new tokens overwrite the old files anyway.
        console.warn('[App] Session restore failed:', result.status);
        await runRecoveryModule(
          () => import('./services/resetService'),
          (m) => m.gracefulReset(),
          'gracefulReset'
        );
      }
      setIsRestoring(false);
    };
    restore();
  }, [isPipWindow]);

  useEffect(() => {
    // Initialize OS permission state (non-blocking) and subscribe to changes (#197)
    let unsubPermission: (() => void) | undefined;
    if (globalThis.electron) {
      useOsPermissionStore.getState().fetchAll();
      // Guard for IPC contract mismatch — older shells may lack v3 permission channels
      if (typeof globalThis.electron.onPermissionChanged === 'function') {
        unsubPermission = globalThis.electron.onPermissionChanged((data) => {
          useOsPermissionStore.getState().updateStatus(data.type as OsPermissionType, data.status);
        });
      }
    }
    return () => {
      unsubPermission?.();
    };
  }, []);

  // Poll server for client config (feature flags, min version, TURN/media-plane URLs).
  // Runs at App root so minVersion enforcement works regardless of auth state.
  useEffect(() => {
    clientConfigService.start();
    return () => clientConfigService.stop();
  }, []);

  if (isRestoring) {
    return (
      <ContextMenuProvider>
        <div className="app">{!isPipWindow && <Titlebar />}</div>
      </ContextMenuProvider>
    );
  }

  return (
    <ErrorBoundary fallback={<AppRootFallback />} onError={handleAppRootError}>
      <ContextMenuProvider>
        <SpaFallbackOverlay />
        <div className="app">
          {!isPipWindow && <Titlebar />}
          <UpdateSecurityBanner />
          <UpdateBanner />
          {/* DM voice call ring UI (#1209). IncomingCallBanner: corner banner
              for callee. OutgoingCallModal: centered modal for caller. Both
              are render-nothing when voiceStore.callState.kind is idle. */}
          <IncomingCallBanner />
          <OutgoingCallModal />

          <ForceUpdateOverlay />
          <MFAChallengeModal />
          <AttestationFailedModalHost />
          <JoinServerModal
            isOpen={!!deepLinkInviteCode && isDeepLinkInviteOpen && !!accessToken && emailVerified}
            initialCode={deepLinkInviteCode}
            onClose={() => {
              setIsDeepLinkInviteOpen(false);
              setDeepLinkInviteCode(null);
            }}
            onSuccess={(server) => {
              useServerStore.getState().setActiveServer(server.id);
              setIsDeepLinkInviteOpen(false);
              setDeepLinkInviteCode(null);
              navigate('/app');
            }}
          />
          <Suspense fallback={null}>
            <Routes>
              <Route
                path="/"
                element={
                  accessToken && emailVerified ? (
                    <Navigate to="/app/dms" replace />
                  ) : (
                    <ErrorBoundary fallback={<AuthFallback />}>
                      <AuthFlow />
                    </ErrorBoundary>
                  )
                }
              />
              <Route element={<AuthenticatedLayout />}>
                <Route path="/app" element={<MainView />} />
                <Route path="/app/dms" element={<DirectMessagesView />} />
              </Route>
              {/* PiP windows — separate Electron BrowserWindows, state synced via BroadcastChannel */}
              <Route
                path="/pip/:pipId"
                element={
                  <ErrorBoundary fallback={<PipFallback />}>
                    <PipWindow />
                  </ErrorBoundary>
                }
              />
            </Routes>
          </Suspense>
        </div>
      </ContextMenuProvider>
    </ErrorBoundary>
  );
}

export default App;
