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
  beginAuthLifecycle: (accessToken: string, sessionId: string | null) => number;
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
  clearAccessToken: () => void;
}

export const useAuthStore = createStore<AuthState>()((set) => ({
  accessToken: null,
  sessionId: null,
  authGeneration: 0,
  rememberMe: true,
  emailVerified: true, // Default true for backward compat (existing sessions)
  loginNotice: null,
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
  clearAccessToken: () =>
    set((state) => ({
      accessToken: null,
      sessionId: null,
      authGeneration: state.authGeneration + 1,
      emailVerified: true,
    })),
}));
