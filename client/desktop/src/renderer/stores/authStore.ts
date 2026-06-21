import { createStore } from '../utils/createStore';

interface AuthState {
  accessToken: string | null;
  sessionId: string | null;
  rememberMe: boolean;
  emailVerified: boolean;
  setAccessToken: (accessToken: string) => void;
  setSessionId: (sessionId: string | null) => void;
  setRememberMe: (rememberMe: boolean) => void;
  setEmailVerified: (verified: boolean) => void;
  clearAccessToken: () => void;
}

export const useAuthStore = createStore<AuthState>()((set) => ({
  accessToken: null,
  sessionId: null,
  rememberMe: true,
  emailVerified: true, // Default true for backward compat (existing sessions)
  setAccessToken: (accessToken) => set({ accessToken }),
  setSessionId: (sessionId) => set({ sessionId }),
  setRememberMe: (rememberMe) => set({ rememberMe }),
  setEmailVerified: (emailVerified) => set({ emailVerified }),
  clearAccessToken: () =>
    set({
      accessToken: null,
      sessionId: null,
      emailVerified: true,
    }),
}));
