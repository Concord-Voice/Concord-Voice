import { createStore } from '../utils/createStore';

interface AuthState {
  accessToken: string | null;
  sessionId: string | null;
  /**
   * Client-owned identity for the currently admitted auth lifecycle.
   *
   * Access and session IDs are server credentials and may both rotate during
   * refresh. This generation changes only when a new login/restore succeeds or
   * auth is cleared, so async continuations can use it as a stable CAS owner.
   */
  authGeneration: number;
  rememberMe: boolean;
  emailVerified: boolean;
  /**
   * One-shot login-screen notice staged by a flow that ends after its Login
   * instance has already unmounted (e.g. a mid-login E2EE teardown abort,
   * PR #2337): the early access-token set navigates "/" into the app, so the
   * aborting catch's setErrors is a no-op there. The next Login mount seeds
   * its error banner from this field and clears it. Deliberately NOT cleared
   * by clearAccessToken — a concurrent second 401 teardown must not wipe a
   * just-staged notice before the login screen renders it.
   */
  loginNotice: string | null;
  /**
   * The `authGeneration` of a just-authenticated session whose inline E2EE
   * unlock is still pending, or null when none is. Password login publishes the
   * access token BEFORE unwrapping keys (the PR #2337 ownership/abort machinery
   * and the consented key-reset PUT both key on the early token), so App.tsx's
   * passive "/" route must hold at AuthFlow while this equals the current
   * `authGeneration` — otherwise the token set navigates "/" to /app/dms and
   * unmounts Login before an inline unwrap failure can surface the consented
   * key-recovery prompt, stranding the user authenticated-but-undecryptable
   * (#2346). Generation-bound so a superseded or aborted flow's stale value can
   * never gate a successor session (SSO / session-restore navigate via the same
   * route): a successor login or a clear advances `authGeneration` past it
   * (`beginAuthLifecycle` / `beginAuthLifecycleIfCurrent` / `setAccessToken` /
   * `clearAccessToken`), while an ordinary same-session refresh
   * (`rotateAuthCredentials`) intentionally preserves it — correctly keeping the
   * hold for the still-pending session. Released via the
   * compare-and-clear `clearPendingE2EEUnlockGenerationIfCurrent` once this login's
   * E2EE is ready — a plain unconditional clear would let a stale predecessor
   * clobber a successor login's hold (successor-race strand, CodeRabbit review on
   * PR #2435). Aligns with the owner/generation-bound admission invariant tracked
   * for the SSO paths in #2424.
   */
  pendingE2EEUnlockGeneration: number | null;
  beginAuthLifecycle: (accessToken: string, sessionId: string | null) => number;
  /**
   * Start a new auth lifecycle iff `expectedGeneration` is still current, and
   * return the new generation (or null when a successor won the race).
   *
   * This is ALSO the #2415 continuation-adoption primitive, deliberately rather
   * than a second near-identical action: the continuation pair is a brand-new
   * server session, which is exactly what "begin a lifecycle" means. It bumps
   * `authGeneration` honestly instead of laundering itself as a refresh rotation
   * through `rotateAuthCredentials` (which PRESERVES the generation because a
   * refresh is the same session getting a new token). The bump is the point: it
   * makes in-flight requests issued under the dead credential epoch non-current,
   * so their 401s cannot tear down the session just adopted — and it keeps
   * `applyRefreshedCredentials`' `previous_session_id` lineage proof off the
   * adoption path entirely.
   */
  beginAuthLifecycleIfCurrent: (
    expectedGeneration: number,
    accessToken: string,
    sessionId: string | null
  ) => number | null;
  rotateAuthCredentials: (
    expectedGeneration: number,
    accessToken: string,
    sessionId: string | null
  ) => boolean;
  setAccessToken: (accessToken: string) => void;
  setSessionId: (sessionId: string | null) => void;
  setRememberMe: (rememberMe: boolean) => void;
  setEmailVerified: (verified: boolean) => void;
  setLoginNotice: (notice: string | null) => void;
  setPendingE2EEUnlockGeneration: (generation: number | null) => void;
  clearPendingE2EEUnlockGenerationIfCurrent: (generation: number) => void;
  clearAccessToken: () => void;
}

export const useAuthStore = createStore<AuthState>()((set) => ({
  accessToken: null,
  sessionId: null,
  authGeneration: 0,
  rememberMe: true,
  emailVerified: true, // Default true for backward compat (existing sessions)
  loginNotice: null,
  pendingE2EEUnlockGeneration: null,
  beginAuthLifecycle: (accessToken, sessionId) => {
    let generation = 0;
    set((state) => {
      generation = state.authGeneration + 1;
      return { accessToken, sessionId, authGeneration: generation };
    });
    return generation;
  },
  beginAuthLifecycleIfCurrent: (expectedGeneration, accessToken, sessionId) => {
    let generation: number | null = null;
    set((state) => {
      if (state.authGeneration !== expectedGeneration) return state;
      generation = state.authGeneration + 1;
      return { accessToken, sessionId, authGeneration: generation };
    });
    return generation;
  },
  rotateAuthCredentials: (expectedGeneration, accessToken, sessionId) => {
    let applied = false;
    set((state) => {
      if (state.authGeneration !== expectedGeneration || state.accessToken === null) return state;
      applied = true;
      return { accessToken, sessionId };
    });
    return applied;
  },
  // Compatibility surface for token-only auth responses. Replacing a token
  // must also clear any prior session ID, otherwise an SSO successor can form
  // a mixed-account {new access token, old session ID} pair.
  setAccessToken: (accessToken) => {
    set((state) => ({
      accessToken,
      sessionId: null,
      authGeneration: state.authGeneration + 1,
    }));
  },
  setSessionId: (sessionId) => set({ sessionId }),
  setRememberMe: (rememberMe) => set({ rememberMe }),
  setEmailVerified: (emailVerified) => set({ emailVerified }),
  setLoginNotice: (loginNotice) => set({ loginNotice }),
  setPendingE2EEUnlockGeneration: (pendingE2EEUnlockGeneration) =>
    set({ pendingE2EEUnlockGeneration }),
  // Compare-and-clear: release the E2EE-unlock hold ONLY if it still belongs to
  // the calling flow's generation. An inline-login continuation clears its own
  // hold after an await; without this guard a stale predecessor (whose flow lost
  // ownership to a successor that armed its OWN hold) would unconditionally write
  // null and release the SUCCESSOR into the app before its E2EE is ready — the
  // successor-race form of the #2346 strand (CodeRabbit review, PR #2435).
  clearPendingE2EEUnlockGenerationIfCurrent: (generation) =>
    set((state) =>
      state.authGeneration === generation && state.pendingE2EEUnlockGeneration === generation
        ? { pendingE2EEUnlockGeneration: null }
        : state
    ),
  clearAccessToken: () =>
    set((state) => ({
      accessToken: null,
      sessionId: null,
      authGeneration: state.authGeneration + 1,
      emailVerified: true,
    })),
}));
