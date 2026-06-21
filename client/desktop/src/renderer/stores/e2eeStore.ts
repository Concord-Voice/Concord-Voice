/**
 * E2EE Store (#270, Task 21b)
 *
 * Reactive flags that mirror the lifecycle of `e2eeService` so the post-auth
 * gate (App.tsx) can render `SSOEagerUnlock` exactly when an SSO user has an
 * access token but no unwrapped private key on this device.
 *
 * `ready` tracks whether `e2eeService.initialize`/`initializeFromStoredKeys`
 * has run successfully — i.e. the user can decrypt DMs and channel keys. The
 * service itself is the source of truth (it owns the CryptoKey objects); this
 * store is a downstream subscription so React components can re-render when
 * the flag flips. `e2eeService.initialize` calls `setReady(true)` on success;
 * `e2eeService.clearKeys` calls `setReady(false)`. Tests that exercise
 * `e2eeService` directly should reset the store via `useE2EEStore.getState().reset()`
 * in `beforeEach` to match the test-isolation contract from `[internal]rules/tests.md`.
 *
 * `needsSSOUnlock` is set by `useSSOFlow` when an SSO callback returns
 * `logged_in` but no E2EE keys have been initialized yet — the gate uses this
 * flag to decide whether to mount `SSOEagerUnlock`. Distinct from `ready=false`
 * because session-restore on launch also produces `ready=false` while it
 * decrypts keys from safeStorage; we only want the eager-unlock prompt when
 * the user has just SSO'd from a fresh device, not on every cold start.
 *
 * No `persist` middleware: both flags must reset on app restart. `ready` is
 * recomputed from the e2eeService restore path; `needsSSOUnlock` is a
 * one-shot signal that should never survive a reload.
 */

import { createStore } from '../utils/createStore';

interface E2EEStore {
  /** True when user's private key is unwrapped and ready to decrypt. */
  ready: boolean;
  /**
   * True when an SSO callback completed with `logged_in` but the renderer
   * needs to gate through `SSOEagerUnlock` before the main app. Distinct from
   * `ready` because password-login users initialize E2EE inline before they
   * reach the post-auth gate (so they never need this flag set).
   */
  needsSSOUnlock: boolean;
  setReady: (ready: boolean) => void;
  setNeedsSSOUnlock: (needs: boolean) => void;
  reset: () => void;
}

export const useE2EEStore = createStore<E2EEStore>()((set) => ({
  ready: false,
  needsSSOUnlock: false,
  setReady: (ready) => set({ ready }),
  setNeedsSSOUnlock: (needs) => set({ needsSSOUnlock: needs }),
  reset: () => set({ ready: false, needsSSOUnlock: false }),
}));
