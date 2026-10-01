/**
 * One multi-request operation's account and server (#3509 review).
 *
 * A password step-up is two requests: the exchange at the mint endpoint, then
 * the route that spends the token. Each apiFetch admits itself against the
 * account and server current when it starts, so without a shared capture the
 * second request goes out as whoever is signed in by then — a token minted for
 * account A spent by a destructive request sent as account B, whose
 * permissions might widen it (a self-scoped purge becoming an all-author one).
 *
 * Capture once, before the operation's first request, and pass the result to
 * each request of it: every one then refuses to dispatch, with the
 * pre-dispatch AbortError apiFetch already throws for a change during its own
 * setup, once the account or server has changed since the capture.
 */
import { useAuthStore } from '../../stores/auth/authStore';
import { apiFetch, type AuthLifecycleSnapshot } from './apiClient';
import { captureRuntimeServerSelection, type RuntimeServerSelection } from './runtimeServerBase';

export interface ApiRequestContext {
  readonly serverSelection: RuntimeServerSelection;
  readonly authLifecycle: AuthLifecycleSnapshot;
}

/**
 * Captures the account and server the operation about to start belongs to.
 *
 * The auth half reads the store exactly as apiFetch's own private capture
 * does. `AuthLifecycleSnapshot` keeps the two in step: a field added there is
 * a compile error here until this literal carries it too.
 */
export function captureApiRequestContext(): ApiRequestContext {
  const { accessToken, sessionId, authGeneration } = useAuthStore.getState();
  return {
    serverSelection: captureRuntimeServerSelection(),
    authLifecycle: { accessToken, sessionId, authGeneration },
  };
}

/**
 * `apiFetch` admitted against `context` when there is one, and as its own
 * operation otherwise — so a single-request flow keeps its call unchanged.
 */
export function apiFetchInContext(
  path: string,
  init: RequestInit,
  context: ApiRequestContext | undefined
): Promise<Response> {
  return context === undefined ? apiFetch(path, init) : apiFetch(path, init, { context });
}
