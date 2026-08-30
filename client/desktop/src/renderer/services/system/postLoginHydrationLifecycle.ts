import { useAuthStore } from '../../stores/auth/authStore';

interface PostLoginHydrationRun {
  generation: number;
  controller: AbortController;
}

export interface HydrationLifecycleGuard {
  signal: AbortSignal;
  isCurrent: () => boolean;
}

/** The authenticated identity an in-flight continuation belongs to. */
export interface AuthLifecycleSnapshot {
  authGeneration: number;
}

/**
 * Capture the identity a request is being made ON BEHALF OF, so its
 * continuation can refuse to write into a DIFFERENT account's stores.
 *
 * `gracefulReset()` clears stores; it cannot cancel a `fetch` that is already in
 * flight. A request started by user A that resolves after A logged out and B
 * logged in will happily commit A's data into B's session unless its own
 * continuation checks. Guard every store write that follows an `await` on a
 * request whose result is account-scoped.
 */
export function captureAuthLifecycle(): AuthLifecycleSnapshot {
  return { authGeneration: useAuthStore.getState().authGeneration };
}

/**
 * True only while the same ACCOUNT is signed in — not the same credentials.
 *
 * `authGeneration` is the only field that means this. It advances on login
 * (`beginAuthLifecycle`), on logout and on `clearTokens`, and
 * `rotateAuthCredentials` DELIBERATELY preserves it while replacing both the
 * access token AND the session id. Comparing either credential therefore reads
 * an ordinary proactive refresh as an account switch, and a join that happened
 * to be in flight across one would be discarded after the server had already
 * committed the membership — the sidebar left empty, the caller told it failed
 * (#2363, Codex). Tokens rotate on a schedule, so that is a routine collision,
 * not an exotic one.
 */
export function isSameAuthLifecycle(snapshot: AuthLifecycleSnapshot): boolean {
  return useAuthStore.getState().authGeneration === snapshot.authGeneration;
}

let generation = 0;
let activeController: AbortController | null = null;
let activeAuthUnsubscribe: (() => void) | null = null;

/** Start one auth-bound hydration chain and supersede any older continuation. */
export function beginPostLoginHydration(): PostLoginHydrationRun {
  resetPostLoginHydrationLifecycle();
  const { accessToken, sessionId } = useAuthStore.getState();
  const controller = new AbortController();
  activeController = controller;
  // Deliberately NOT captureAuthLifecycle() below. This asks "are these the same
  // CREDENTIALS", which a refresh rotation legitimately answers no to — hydration
  // is short-lived and re-running it is cheap. An in-flight WRITE asks a different
  // question ("is this the same ACCOUNT") and must survive a rotation, so it uses
  // the generation instead. Unifying them looked right and was wrong (#2363, Codex).
  activeAuthUnsubscribe = useAuthStore.subscribe((auth) => {
    const sessionChanged =
      sessionId === null ? auth.accessToken !== accessToken : auth.sessionId !== sessionId;
    if (auth.accessToken === null || sessionChanged) {
      resetPostLoginHydrationLifecycle();
    }
  });
  return { generation, controller };
}

/** True only while this run still belongs to the active authenticated lifecycle. */
export function isPostLoginHydrationCurrent(run: PostLoginHydrationRun): boolean {
  return (
    run.generation === generation &&
    run.controller === activeController &&
    !run.controller.signal.aborted
  );
}

export function guardPostLoginHydration(run: PostLoginHydrationRun): HydrationLifecycleGuard {
  return {
    signal: run.controller.signal,
    isCurrent: () => isPostLoginHydrationCurrent(run),
  };
}

/** Start and return one auth-bound guard for a multi-step hydration workflow. */
export function beginPostLoginHydrationGuard(): HydrationLifecycleGuard {
  return guardPostLoginHydration(beginPostLoginHydration());
}

export function isHydrationLifecycleCurrent(guard?: HydrationLifecycleGuard): boolean {
  return guard === undefined || (!guard.signal.aborted && guard.isCurrent());
}

/** Invalidate all continuations owned by the prior authenticated lifecycle. */
export function resetPostLoginHydrationLifecycle(): void {
  generation += 1;
  activeController?.abort();
  activeController = null;
  activeAuthUnsubscribe?.();
  activeAuthUnsubscribe = null;
}
