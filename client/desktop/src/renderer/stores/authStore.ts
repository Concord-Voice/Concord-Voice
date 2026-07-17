import { createStore } from '../utils/createStore';

interface AuthState {
  accessToken: string | null;
  sessionId: string | null;
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
  rememberMe: true,
  emailVerified: true, // Default true for backward compat (existing sessions)
  loginNotice: null,
  setAccessToken: (accessToken) => set({ accessToken }),
  setSessionId: (sessionId) => set({ sessionId }),
  setRememberMe: (rememberMe) => set({ rememberMe }),
  setEmailVerified: (emailVerified) => set({ emailVerified }),
  setLoginNotice: (loginNotice) => set({ loginNotice }),
  clearAccessToken: () =>
    set({
      accessToken: null,
      sessionId: null,
      emailVerified: true,
    }),
}));
