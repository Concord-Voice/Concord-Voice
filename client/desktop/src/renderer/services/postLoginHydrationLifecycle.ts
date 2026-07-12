import { useAuthStore } from '../stores/authStore';

interface PostLoginHydrationRun {
  generation: number;
  controller: AbortController;
}

export interface HydrationLifecycleGuard {
  signal: AbortSignal;
  isCurrent: () => boolean;
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
