/**
 * SSO Store (#270)
 *
 * Ephemeral state for an in-flight SSO authentication. The `state` field is a
 * discriminated union on `phase`, so consumer components render exactly the
 * right UI for the current step (e.g. spinner for 'authenticating', a
 * username/passphrase form for 'register_required').
 *
 * No `persist` middleware: SSO state is short-lived (seconds) and must NEVER
 * survive a process restart — sso_token and mfa_challenge_token are sensitive
 * authorization grants that should only live in memory.
 */

import { createStore } from '../utils/createStore';
import type { SSOProvider } from '../services/ssoService';

export type SSOState =
  | { phase: 'idle' }
  | { phase: 'authenticating'; provider: SSOProvider }
  | {
      phase: 'register_required';
      provider: SSOProvider;
      ssoToken: string;
      email: string;
      name?: string;
    }
  | {
      phase: 'link_required';
      provider: SSOProvider;
      ssoToken: string;
      maskedEmail: string;
    }
  | { phase: 'mfa_required'; mfaChallengeToken: string }
  | { phase: 'error'; message: string };

interface SSOStore {
  state: SSOState;
  setState: (state: SSOState) => void;
  reset: () => void;
}

export const useSSOStore = createStore<SSOStore>()((set) => ({
  state: { phase: 'idle' },
  setState: (state) => set({ state }),
  reset: () => set({ state: { phase: 'idle' } }),
}));
