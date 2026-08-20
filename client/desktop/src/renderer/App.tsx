import { Suspense, lazy, useEffect, useRef, useState } from 'react';
import { Routes, Route, Navigate, Outlet, useNavigate } from 'react-router';
import { Titlebar } from './components/Titlebar/Titlebar';
import AuthFlow from './components/Auth/AuthFlow';
import { isE2EEUnlockPending } from './utils/authAdmissionGate';
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
import { AudioOutputs } from './components/Voice/ParticipantGrid';
import { useUpdateErrorListener } from './hooks/useUpdateErrorListener';
import MFAChallengeModal from './components/Auth/MFAChallengeModal';
import AttestationFailedModalHost from './components/AttestationFailedModal';
import ChangelogModalHost from './components/ChangelogModal/ChangelogModal';
import JoinServerModal from './components/Servers/JoinServerModal';
import AddFriendModal from './components/DirectMessages/AddFriendModal';
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
import { E2EEInitTeardownError } from './services/e2eeErrors';
import { hydratePostLogin } from './services/postLoginHydration';
import { usePrivacyStore } from './stores/privacyStore';
import { klipyClient } from './services/gifProvider/klipyClient';
import { clientConfigService } from './services/clientConfigService';
import { detectCodecCapabilities, prewarmWebRTC } from './services/mediaCapabilities';
import { useNotificationNavigationStore } from './stores/notificationNavigationStore';
import { useServerStore } from './stores/serverStore';
import { useChannelStore } from './stores/channelStore';
import { useDMStore } from './stores/dmStore';
import { useVoiceStore } from './stores/voiceStore';
import { desktopNotificationService } from './services/desktopNotificationService';
import { usePendingRegistrationStore } from './stores/pendingRegistrationStore';
// resetService is eagerly registered by main.tsx; local dynamic imports resolve
// from that loaded module while avoiding direct feature-module cycles.

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

/**
 * Log an E2EE session-restore failure, distinguishing a benign superseded/
 * torn-down restore from a genuine key failure (Gitar quality note, PR #2337).
 * Module-level (not inlined in the restore catch) so its branch does not add
 * to App()'s cognitive complexity at the deep useEffect nesting depth (S3776).
 */
function logE2EERestoreError(err: unknown): void {
  if (err instanceof E2EEInitTeardownError) {
    // Superseded or torn-down restore — expected when a fresh login raced the
    // restore (the newer attempt owns the E2EE singleton) or a teardown landed
    // mid-restore. Benign; not a key failure.
    console.debug('E2EE restore superseded/torn down (benign):', errorMessage(err));
    return;
  }
  console.warn(
    'Failed to restore E2EE keys — E2EE features will require re-login:',
    errorMessage(err)
  );
}

/**
 * Clear a restored credential that arrived without a valid owner id, then reset
 * to a clean logged-out state. Extracted verbatim from App()'s session-restore
 * effect so the ownerless-credential rejection does not add to its cognitive
 * complexity (S3776) at the deep useEffect nesting depth. `electron` is the
 * caller's already-narrowed non-null handle, so the receiver is unchanged.
 */
async function clearOwnerlessRestoredCredential(
  electron: NonNullable<typeof globalThis.electron>
): Promise<void> {
  console.warn('[App] Session restore rejected: missing credential owner');
  await (electron.clearTokens?.() ?? Promise.resolve()).catch((err) => {
    console.warn('Failed to clear ownerless restored credential:', errorMessage(err));
  });
  await runRecoveryModule(
    () => import('./services/resetService'),
    (m) => m.gracefulReset(),
    'gracefulReset'
  );
}

/**
 * Restore the E2EE service from owner-bound stored session keys, returning
 * whether the credential must still enter the eager-unlock gate
 * (`pendingE2EEUnlock`). Extracted verbatim from App()'s session-restore effect
 * to keep its cognitive complexity under the S3776 threshold. This does NOT
 * touch beginAuthLifecycle / needsSSOUnlock ordering — the caller still
 * sequences those after this resolves.
 */
async function restoreStoredE2EEKeys(result: {
  pendingE2EEUnlock?: boolean;
  e2eeKeys?: {
    wrappingKeyBase64: string;
    preferencesKeyBase64: string;
    wrappedPrivateKeyBase64: string;
  } | null;
}): Promise<boolean> {
  let pendingE2EEUnlock = result.pendingE2EEUnlock === true || !result.e2eeKeys;
  if (!pendingE2EEUnlock && result.e2eeKeys) {
    try {
      await e2eeService.initializeFromStoredKeys(result.e2eeKeys);
      console.debug('E2EE service restored from stored session keys');
    } catch (err) {
      logE2EERestoreError(err);
      pendingE2EEUnlock = true;
    }
  }
  return pendingE2EEUnlock;
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
  const needsSSOUnlock = useE2EEStore((s) => s.needsSSOUnlock);
  const voiceActiveChannelId = useVoiceStore((s) => s.activeChannelId);
  const voiceConnectionState = useVoiceStore((s) => s.connectionState);
  const isInVoice =
    Boolean(voiceActiveChannelId) &&
    (voiceConnectionState === 'connected' || voiceConnectionState === 'reconnecting');
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
  // e2eeService.initialize. Gate on the one-shot SSO signal even if `ready`
  // is already true: that flag may describe a superseded account's keyset,
  // and must never let a fresh SSO lifecycle bypass its own unlock. Password-
  // login users never set `needsSSOUnlock`, so they bypass this gate entirely.
  if (needsSSOUnlock) {
    const handleUnlock = () => {
      // Clear the one-shot flag — `e2eeService.initialize` already flipped
      // `ready=true` via the store sync, so the next render falls through.
      useE2EEStore.getState().setNeedsSSOUnlock(false);
      // SSO callbacks deliberately defer encrypted hydration until passphrase
      // unlock. Run it now that E2EE is ready; its lifecycle guard prevents
      // late work from crossing an account switch.
      hydratePostLogin().catch((err) => {
        console.warn('Post-login hydration failed after SSO unlock:', errorMessage(err));
      });
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
      {isInVoice && (
        <ErrorBoundary fallback={null}>
          <AudioOutputs />
        </ErrorBoundary>
      )}
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
  const [deepLinkFriendCode, setDeepLinkFriendCode] = useState<string | null>(null);
  const [isDeepLinkFriendOpen, setIsDeepLinkFriendOpen] = useState(false);
  const accessToken = useAuthStore((state) => state.accessToken);
  const emailVerified = useAuthStore((state) => state.emailVerified);
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const pendingE2EEUnlockGeneration = useAuthStore((state) => state.pendingE2EEUnlockGeneration);
  // #2346: while a just-authenticated password login is still completing its
  // inline E2EE unlock (or awaiting consented key recovery), hold "/" at
  // AuthFlow rather than navigating into the app — otherwise Login unmounts
  // before the key-recovery prompt (Login-local state) can render, stranding the
  // user authenticated-but-undecryptable. Generation-bound (see authStore's
  // pendingE2EEUnlockGeneration): a superseded/aborted flow's stale value never
  // gates a successor, since a successor login or a clear advances authGeneration
  // past it (a same-session refresh via rotateAuthCredentials preserves it).
  const e2eeUnlockPending = isE2EEUnlockPending(pendingE2EEUnlockGeneration, authGeneration);
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

  // Friend-code deep links ride their own channel rather than a widened invite
  // payload (#945), so an older SPA that knows only 'invite:received' can never
  // route a friend code into JoinServerModal. A code that arrives while logged
  // out is held silently and replayed after auth — deliberately with NO
  // login-screen notice, since announcing whom the machine is about to friend
  // is a disclosure on a possibly-shared logged-out desktop.
  useEffect(() => {
    if (isPipWindow) return undefined;
    const subscribe = globalThis.electron?.onFriendCodeReceived;
    if (typeof subscribe !== 'function') return undefined;
    const unsubscribe = subscribe(({ code }) => {
      setDeepLinkFriendCode(code);
      if (useAuthStore.getState().accessToken && useAuthStore.getState().emailVerified) {
        setIsDeepLinkFriendOpen(true);
      }
    });
    // Optional call: a bundled SPA served to an older shell has no friend
    // readiness channel, and must not throw on the missing method.
    globalThis.electron?.friendRendererReady?.();
    return unsubscribe;
  }, [isPipWindow]);

  useEffect(() => {
    if (deepLinkFriendCode && accessToken && emailVerified) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: opens queued friend modal after auth state becomes eligible; not a render loop
      setIsDeepLinkFriendOpen(true);
    }
  }, [deepLinkFriendCode, accessToken, emailVerified]);

  // A held deep-link code must not outlive the session that was current when it
  // arrived (#945, M4). Nothing cleared these but the modals' own onClose, so a
  // code held on this machine replayed into whichever account logged in next:
  // the two effects above fire on [code, accessToken, emailVerified], so a
  // different user's session opened the modal prefilled and fetched a preview
  // under THEIR token. That is precisely the disclosure the "NO login-screen
  // notice" decision above exists to avoid. Both arms are cleared — the invite
  // arm has the identical shape and predates #945.
  //
  // The edge is accessToken → null (logout), NOT an authGeneration change:
  // beginAuthLifecycle advances the generation on LOGIN as well, so keying on it
  // would clear the code at the exact moment the hold-and-replay feature is
  // supposed to deliver it. rotateAuthCredentials deliberately preserves both,
  // so an ordinary token refresh never disturbs a held code.
  const hadAccessTokenRef = useRef(accessToken !== null);
  useEffect(() => {
    const hasAccessToken = accessToken !== null;
    const loggedOut = hadAccessTokenRef.current && !hasAccessToken;
    hadAccessTokenRef.current = hasAccessToken;
    if (!loggedOut) return;
    /* eslint-disable @eslint-react/set-state-in-effect -- intentional: this is a
       reaction to an external store transition (the auth store clearing its
       token), not derived render state, and it runs only on the logout edge —
       `loggedOut` is false on every other pass, so there is no render loop. The
       four clears share one rationale; disabling per line would repeat it four
       times. */
    setDeepLinkInviteCode(null);
    setIsDeepLinkInviteOpen(false);
    setDeepLinkFriendCode(null);
    setIsDeepLinkFriendOpen(false);
    /* eslint-enable @eslint-react/set-state-in-effect -- end of the logout-edge clear */
  }, [accessToken]);

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
        const credentialOwner = result.credentialOwner;
        if (
          typeof credentialOwner !== 'number' ||
          !Number.isSafeInteger(credentialOwner) ||
          credentialOwner <= 0
        ) {
          await clearOwnerlessRestoredCredential(globalThis.electron);
          setIsRestoring(false);
          return;
        }

        if (typeof result.rememberMe === 'boolean') {
          useAuthStore.getState().setRememberMe(result.rememberMe);
        }

        // Restore keys before publishing auth. A credential whose owner-bound
        // E2EE blob is pending, missing, malformed, or from a predecessor must
        // enter the eager-unlock gate and must not hydrate encrypted content.
        const pendingE2EEUnlock = await restoreStoredE2EEKeys(result);

        useE2EEStore
          .getState()
          .setNeedsSSOUnlock(pendingE2EEUnlock, pendingE2EEUnlock ? credentialOwner : undefined);
        useAuthStore.getState().beginAuthLifecycle(result.accessToken, result.sessionId ?? null);

        // Hydrate only after this credential owner's E2EE keys are ready. A
        // pending owner remains behind SSOEagerUnlock; that gate hydrates after
        // passphrase unlock. Session-only soft reloads still hydrate here once
        // their owner-matched in-memory keys are restored (#1297, #1870).
        if (!pendingE2EEUnlock) {
          try {
            await hydratePostLogin();
          } catch (err) {
            console.warn('Post-login hydration failed during session restore:', errorMessage(err));
          }
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
              for callee. OutgoingCallModal: bottom-right non-modal prompt for
              caller. Both render nothing when voiceStore.callState.kind is idle. */}
          <IncomingCallBanner />
          <OutgoingCallModal />

          <ForceUpdateOverlay />
          <MFAChallengeModal />
          <AttestationFailedModalHost />
          <ChangelogModalHost />
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
          <AddFriendModal
            isOpen={!!deepLinkFriendCode && isDeepLinkFriendOpen && !!accessToken && emailVerified}
            initialCode={deepLinkFriendCode}
            onClose={() => {
              setIsDeepLinkFriendOpen(false);
              // Clearing the code as well as the open flag is load-bearing: the
              // modal's prefill effect keys on [isOpen, initialCode], so a
              // retained code would not re-prefill a field the user had edited.
              setDeepLinkFriendCode(null);
            }}
          />
          <Suspense fallback={null}>
            <Routes>
              <Route
                path="/"
                element={
                  accessToken && emailVerified && !e2eeUnlockPending ? (
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
