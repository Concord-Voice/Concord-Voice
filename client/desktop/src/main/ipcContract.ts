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
 *        AMENDED by #2401 — see v20 below for the `kind` classification the
 *        payload gained. The "spaIpcContract is NOT bumped" note above is
 *        about the SERVER's required minimum and still holds; it is NOT a
 *        precedent against bumping this shell's own IPC_CONTRACT_VERSION,
 *        which #2401 does bump because the payload signature changed.
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
 * - v17: Self-hosted server discovery (#1618): selfHosted:probeServer handler
 *        validates a user-entered origin in the main process and probes
 *        /api/v1/client/config plus /api/v1/server/capabilities before the
 *        renderer may route auth storage to that origin.
 * - v18: Refresh ownership + session lineage (#2374): auth:refreshToken results
 *        and the auth:token-refreshed main-to-renderer event expose
 *        previousSessionId. auth:storeRefreshToken returns an opaque
 *        CredentialOwner; auth:clearTokensIfOwner and
 *        auth:storeE2EEKeysIfOwner provide main-process CAS operations for
 *        aborting stale login continuations without touching a successor.
 *        sso:appleSignIn and sso:googleSignIn accept an approved API origin;
 *        main stores the returned refresh credential and exposes only the
 *        access token, session ID, and opaque CredentialOwner to renderer.
 *        sso:completeRegistration and sso:completeLink keep the same custody
 *        boundary for the final first-user/link exchanges. Session restore
 *        exposes the owner-scoped pending-E2EE-unlock state so reload cannot
 *        enter the authenticated app before matching keys are available.
 * - v19: SSO MFA custody (#2424): the mfa_challenge sign-in result now carries
 *        the reserved CredentialOwner, and sso:completeMFA submits the MFA proof
 *        (TOTP code or WebAuthn assertion) in main, storing the resulting refresh
 *        credential under that owner and returning only the access token, session
 *        ID, and owner — extending the SSO refresh-token-never-in-renderer custody
 *        boundary to the MFA path.
 * - v20: SPA fallback diagnostic classification (#2401): the app:configFetchFailed
 *        payload gains an OPTIONAL `kind` ('unreachable' | 'rejected' | 'contract',
 *        SpaFallbackDiagnostic in shared/spaIpcTypes.ts) so the renderer can tell
 *        which of the six isUnexpectedBundled conditions fired. Only 'unreachable'
 *        is falsified by proof of reachability; 'rejected' (a refused spaUrl, incl.
 *        the #750 poisoned sentinel) and 'contract' (shell older than the deployed
 *        SPA) fire against a REACHABLE server, so retracting them on connectivity
 *        would silence a fail-loud sentinel.
 *
 *        The SERVER's spaIpcContract stays 19 — the field is additive and a
 *        renderer that receives no `kind` fails closed to non-retractable, i.e.
 *        degrades exactly to v19 behavior. Bumping the SHELL version is still
 *        correct per [internal]rules/electron.md ("bump when channels are added,
 *        removed, or change signature"): the payload signature changed. The two
 *        numbers are independent — IPC_CONTRACT_VERSION is what this shell
 *        implements, SPA_IPC_CONTRACT (spa.env) is the minimum the deployed SPA
 *        demands, and a 20-shell still satisfies a 19-minimum.
 * - v21: Orphaned SSO reservation release (#2394): the zero-argument
 *        sso:abandonReservation channel releases a credential reservation that
 *        has no live main-process continuation, reopening the pre-credential
 *        auth:storeE2EEKeys staging lane for password registration. Before it,
 *        an abandoned SSO attempt left the reservation resident forever and a
 *        later password registration silently lost restart-survival of its
 *        E2EE keys.
 *
 *        It takes NO renderer-supplied authority — main resolves the target
 *        from reservedCredentialOwner / pendingCompletion / pendingMFA, the
 *        same zero-arg posture as spa:reloadLatest. It requires the slot to be
 *        RESERVED and UNFILLED, so it is a structural no-op once a credential
 *        is published and cannot be used as a logout. Deliberately NOT routed
 *        through clearTokensIfOwner, which CAS-checks the generation only while
 *        publishRefreshToken preserves the generation across a rotation — that
 *        path could pass the CAS and wipe a live credential.
 *
 *        The SERVER's spaIpcContract stays 19, for the same reason as v20: the
 *        channel is additive, and a shell without it degrades to the pre-#2394
 *        bug (E2EE keys session-only, recoverable by unlock or re-login) —
 *        never to a weaker or unowned credential writer.
 * - v22: Friend-code deep links (#945): deeplink:friend-code (main -> renderer,
 *        { code }) and deeplink:renderer-ready (renderer -> main). A
 *        concord://friend/CODE deep link lands on this pair; invite:received
 *        keeps emitting a bare { code } forever.
 *
 *        A SECOND channel rather than a widened invite:received payload,
 *        because spaLoader's version gate is one-directional: it refuses an SPA
 *        NEWER than the shell but loads an older one indefinitely, and
 *        SPA_IPC_CONTRACT is an operator-set, hot-reloadable env var, so the
 *        window is unbounded by deploy. A { kind, code } payload on
 *        invite:received would reach a pre-22 SPA that ignores the unknown
 *        field and opens the SERVER-join modal with a friend code. On its own
 *        channel that SPA simply never subscribes and the deep link no-ops.
 *        Main keeps ONE {kind, code} queue with TWO independent readiness
 *        flags for the same reason — a shared flag would let an old SPA's
 *        invite subscription vouch for a friend subscription it never made.
 *
 *        The SERVER's spaIpcContract stays 19 — the channel is purely additive
 *        (v9/v10/v20/v21 precedent), and a shell without it simply never
 *        delivers the friend deep link.
 * - v23: Content protection (#2468): app:getContentProtection and
 *        app:setContentProtection expose the main-owned exact-boolean
 *        preference. The setter persists before applying it to live windows
 *        and rejects invalid or untrusted calls. The SERVER's spaIpcContract
 *        stays 19 because these default-off channels are additive.
 * - v24: `auth:clearTokens` signature change (#2363): accepts an optional
 *        `{ keepDeepLinks?: boolean }`. A SIGNATURE change, so it bumps —
 *        the first draft added the argument without bumping, which this file's
 *        own rule forbids.
 *
 *        The DEFAULT FORGETS, and login-side callers opt out. That direction is
 *        the whole point. An older SPA on this shell calls `clearTokens()` with
 *        no argument, and it must land on the SAFE side of the question: with a
 *        keep-by-default it would leave user A's delivered invite alive through
 *        a forced logout (`nuclearReset`, on a refresh failure with
 *        `rememberMe === false`) and replay it into user B's session. Forgetting
 *        by default costs an old SPA an invite at SSO start — the pre-#2363
 *        behaviour, recoverable by clicking the link again — and costs a current
 *        SPA nothing, because it sends the flag.
 *
 *        The SERVER's spaIpcContract stays 19: nothing here is required of the
 *        SPA. An old SPA that never sends the flag is safe by construction,
 *        which is precisely why the default was inverted rather than versioned
 *        and left permissive.
 * - v25: `deeplink:forget` (#2363): a renderer->main channel that forgets every
 *        deliverable deep link WITHOUT touching credentials. Every other edge
 *        that forgets also destroys them, and the one teardown that must not is a
 *        `rememberMe` refresh failure — the session has ended, but the disk
 *        tokens stay so the next launch can retry. Without this, main kept
 *        `gate.lastCode` alive across that teardown and a source swap inside the
 *        carry window replayed user A's invite into user B's renderer.
 *
 *        Sender-fenced like the auth channels beside it. The SERVER's
 *        spaIpcContract stays 19: an older SPA simply never calls it, and the
 *        pre-existing renderer-side fence still applies there.
 */
export const IPC_CONTRACT_VERSION = 25;

/**
 * Opaque main-process identity for one stored credential lifecycle.
 *
 * Renderers may retain and return this value for conditional operations, but
 * must not derive ordering or user identity from it.
 */
export type CredentialOwner = number;

/**
 * Result shape returned by performRefresh() in the main process and
 * relayed to the renderer via the auth:refreshToken IPC channel.
 * Single source of truth — used by tokenManager, preload, and renderer.
 */
export interface RefreshResult {
  status: string;
  accessToken?: string;
  sessionId?: string;
  previousSessionId?: string;
  mfaChallengeToken?: string;
  mfaMethods?: string[];
  mfaRecoveryOnlyMethods?: string[];
}

export type SelfHostedProbeResult =
  | {
      status: 'ok';
      apiBase: string;
      clientConfig: unknown;
      capabilities: unknown;
    }
  | {
      status: 'error';
      code: string;
      message: string;
    };
