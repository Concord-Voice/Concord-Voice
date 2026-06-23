/**
 * IPC Contract Version — the compatibility boundary between shell and renderer.
 *
 * This integer ONLY bumps when the IPC surface changes:
 * - New ipcMain.handle() channels added
 * - Existing channel signatures change
 * - Channels removed
 *
 * It does NOT bump for renderer-only changes (React components, CSS, services).
 *
 * The server's client config returns `spaIpcContract` — the minimum contract
 * version required by the remote SPA. If the shell's contract version is below
 * that, the shell falls back to its bundled SPA and triggers auto-update.
 *
 * History:
 * - v1: Initial contract (auth, clipboard, GPU, display, PiP, HW accel)
 * - v2: Auto-update channels (update:check/download/install/getAllowPrerelease/
 *        setAllowPrerelease), update events (available/not-available/progress/
 *        downloaded/error), app:getSystemInfo, app:getIpcContract
 * - v3: OS permission management channels (permission:checkAll, permission:check,
 *        permission:request, permission:openSettings), permission:changed event
 * - v4: Update safety (#383, #384): update:getLogPath handler,
 *        update:rollback event
 * - v5: Desktop notification support (#175): app:setBadgeCount,
 *        app:flashFrame, app:focusWindow handlers
 * - v6: Developer Mode toggle (TEMPORARY — remove before BETA):
 *        app:getDeveloperMode, app:setDeveloperMode handlers
 * - v7: Telemetry consent (#618): consent:getState, consent:setState
 *        handlers, consent:changed event. REMOVED in #757 along with the
 *        broader telemetry strip (sub-epic G #756) — these channels had zero
 *        renderer consumers after the consent surface was removed.
 * - v8: SPA self-heal (#753): spa:requestSelfHeal handler. Renderer signals
 *        chunk-load failures so main process can refetch /api/v1/client/config
 *        and reload via the existing resolveSpaSource() validators. See
 *        [internal]0001-spa-deploy-contract.md.
 * - v9: Bundled-SPA fallback observability (#830, deferred to #831):
 *        app:configFetchFailed event (main → renderer), emitted when
 *        spaLoader's resolveSpaSource() falls back to bundled for an
 *        unexpected reason (config fetch failed, network issue, spaUrl
 *        rejected, IPC contract mismatch). Renderer subscribes via
 *        SpaFallbackOverlay component and surfaces a dismissable banner.
 *        Server's spaIpcContract is NOT bumped to 9 — older v8 shells
 *        continue to work without the overlay, since webContents.send
 *        to a non-listening renderer is a harmless no-op.
 * - v10: Forensic build-tag observability (#920 §5.13, #939):
 *        app:getBuildTag handler, returns the CI build tag baked into
 *        the packaged app at build time (via forge extraResource
 *        buildtag.json) or 'unknown' for local dev builds. Read-only;
 *        knowing the tag does not unlock any capability. Server's
 *        spaIpcContract is NOT bumped — the channel is additive, and no
 *        production renderer code currently consumes it. The surface
 *        is reserved for incident-response forensics where a deployed
 *        install needs runtime identification without filesystem access
 *        to the bundle source.
 * - v11: Client attestation token bridge (#677): attestation:get-token
 *        and attestation:clear-token handlers. Renderer reads the
 *        main-process-cached attestation token to attach as the
 *        X-Attestation-Token header on gated requests, and clears it
 *        after a 403 to force re-attestation. Server's spaIpcContract is
 *        NOT bumped — channels are additive and gated behind
 *        REQUIRE_CLIENT_ATTESTATION (default false).
 * - v12: updater:force-check handler (#677): renderer-triggered immediate
 *        update check, used by the attestation 403-retry path to pull a
 *        newer signed build when the server rejects the current client.
 *        Always uses the pinned generic feed (#719) — never honors a
 *        server-supplied URL. Server's spaIpcContract is NOT bumped —
 *        the channel is additive and gated behind REQUIRE_CLIENT_ATTESTATION
 *        (default false); older shells without it simply never call it.
 * - v13: Client-driven Apple SSO (#974): sso:appleSignIn handler (runs the
 *        full main-process Apple flow — PKCE, loopback, broker secret,
 *        Apple /auth/token, jose verification, /session POST) and the
 *        sso:appleCancel teardown channel. Server's spaIpcContract is NOT
 *        bumped — the channels are additive, and an older shell without
 *        them yields a clean renderer-side error (the legacy renderer-
 *        driven apple path no longer exists server-side; Callback 410s).
 * - v14: Client-driven Google SSO (#975): sso:googleSignIn handler (runs the
 *        full main-process Google flow — PKCE, loopback, Google /token with an
 *        embedded non-confidential client_secret, jose verification, /session
 *        POST) and the sso:googleCancel teardown channel. Server's spaIpcContract
 *        is NOT bumped — the channels are additive; the legacy renderer-driven
 *        google path no longer exists server-side (Callback route removed).
 * - v15: Window behavior controls (#806/#1099): window:setClientBehavior,
 *        window:quit, and window:setTitleBarOverlayColor.
 * - v16: Invite deep links (#1355): preload exposes onInviteReceived and main
 *        emits invite:received with a validated 8-character invite code only.
 */
export const IPC_CONTRACT_VERSION = 16;

/**
 * Result shape returned by performRefresh() in the main process and
 * relayed to the renderer via the auth:refreshToken IPC channel.
 * Single source of truth — used by tokenManager, preload, and renderer.
 */
export interface RefreshResult {
  status: string;
  accessToken?: string;
  sessionId?: string;
  mfaChallengeToken?: string;
  mfaMethods?: string[];
  mfaRecoveryOnlyMethods?: string[];
}
