import { createStore } from '../../utils/runtime/createStore';

/**
 * When this client last had a TOTP code accepted, per account, for the
 * recently-spent hint (S2a): a code is accepted once per 30-second step, so a
 * second step-up in the same period needs the next code, and the picker says
 * so (design 2026-09-26-mfa-factor-picker §4.1, §4.2).
 *
 * In memory only, deliberately: it is never persisted, logged or sent (§6,
 * E6). `gracefulReset` clears it, which covers logout and an account switch.
 */
interface TotpAcceptedState {
  /** Account id → the `Date.now()` milliseconds of its last accepted TOTP code. */
  acceptedAt: Record<string, number>;
  noteTotpAccepted: (accountId: string, at: number) => void;
  clearAccount: (accountId: string) => void;
  reset: () => void;
}

export const useTotpAcceptedStore = createStore<TotpAcceptedState>()((set) => ({
  acceptedAt: {},

  noteTotpAccepted: (accountId, at) =>
    set((state) => ({ acceptedAt: { ...state.acceptedAt, [accountId]: at } })),

  clearAccount: (accountId) =>
    set((state) => {
      if (!Object.hasOwn(state.acceptedAt, accountId)) return {};
      const { [accountId]: _, ...rest } = state.acceptedAt;
      return { acceptedAt: rest };
    }),

  reset: () => set({ acceptedAt: {} }),
}));
