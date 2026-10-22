import { useEffect, useState, useSyncExternalStore } from 'react';
import { apiFetch } from '../../services/system/apiClient';
import {
  captureRuntimeServerSelection,
  getApiBase,
  onRuntimeServerSelectionChange,
} from '../../services/system/runtimeServerBase';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
} from '../../services/system/requestContext';
import { useAuthStore } from '../../stores/auth/authStore';
import { resolveMediaUrl } from '../../utils/ui/resolveMediaUrl';

const protectedMediaPath =
  /^\/api\/v1\/media\/(?:avatars|banners|server-icons|server-banners|dm-icons)\/[0-9a-fA-F-]{36}$/;
const imageTypes = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);

interface MediaRequest {
  controller: AbortController;
  exhausted: boolean;
  listeners: Set<(url: string) => void>;
  refs: number;
  retryTimer: ReturnType<typeof setTimeout> | null;
  url: string | null;
}

const mediaRequests = new Map<string, MediaRequest>();
const mediaRevisions = new Map<string, number>();
const revisionListeners = new Set<() => void>();
const retryAttempts = new Map<string, number>();

const subscribeRevisions = (listener: () => void) => {
  revisionListeners.add(listener);
  return () => revisionListeners.delete(listener);
};

function authenticatedPath(source: string | null | undefined): string | undefined {
  if (!source) return undefined;
  if (protectedMediaPath.test(source)) return source;
  if (!/^https?:\/\//i.test(source)) return undefined;
  try {
    const url = new URL(source);
    if (
      url.origin === getApiBase() &&
      !url.search &&
      !url.hash &&
      protectedMediaPath.test(url.pathname)
    ) {
      return url.pathname;
    }
  } catch {
    return undefined;
  }
  return undefined;
}

const readServerEpoch = () => captureRuntimeServerSelection().epoch;

export function invalidateAuthenticatedMediaUrl(source: string): void {
  const path = authenticatedPath(source);
  if (!path) return;
  mediaRevisions.set(path, (mediaRevisions.get(path) ?? 0) + 1);
  revisionListeners.forEach((listener) => listener());
}

/** Fetch identity media with the account session; only the resulting Blob URL reaches img.src. */
export function useAuthenticatedMediaUrl(source?: string | null): string | undefined {
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const hasSession = useAuthStore((state) => Boolean(state.accessToken));
  const serverEpoch = useSyncExternalStore(onRuntimeServerSelectionChange, readServerEpoch);
  const path = authenticatedPath(source);
  const revision = useSyncExternalStore(
    subscribeRevisions,
    () => mediaRevisions.get(path ?? '') ?? 0
  );
  const key = path ? `${authGeneration}:${serverEpoch}:${revision}:${path}` : undefined;
  const [loaded, setLoaded] = useState<{ key: string; url: string } | null>(null);

  useEffect(() => {
    if (!path || !key || !hasSession) return;
    const retryKey = `${authGeneration}:${serverEpoch}:${path}`;
    let request = mediaRequests.get(key);
    if (!request) {
      request = {
        controller: new AbortController(),
        exhausted: false,
        listeners: new Set(),
        refs: 0,
        retryTimer: null,
        url: null,
      };
      mediaRequests.set(key, request);
      const shared = request;
      const context = captureApiRequestContext();
      const retry = () => {
        if (shared.controller.signal.aborted || !apiRequestContextIsCurrent(context)) return;
        const attempts = retryAttempts.get(retryKey) ?? 0;
        if (attempts >= 2) {
          retryAttempts.delete(retryKey);
          shared.exhausted = true;
          return;
        }
        retryAttempts.set(retryKey, attempts + 1);
        // eslint-disable-next-line @eslint-react/web-api-no-leaked-timeout -- The final subscriber clears this shared timer in effect cleanup.
        shared.retryTimer = setTimeout(
          () => {
            shared.retryTimer = null;
            if (shared.refs > 0 && mediaRequests.get(key) === shared)
              invalidateAuthenticatedMediaUrl(path);
          },
          250 * (attempts + 1)
        );
      };
      void (async () => {
        try {
          const response = await apiFetch(
            path,
            { signal: shared.controller.signal, cache: 'no-store' },
            { context, authoritative: false }
          );
          if (!response.ok) {
            if (response.status === 429 || response.status >= 500) retry();
            return;
          }
          if (shared.controller.signal.aborted || !apiRequestContextIsCurrent(context)) return;
          const contentType = response.headers
            .get('Content-Type')
            ?.split(';')[0]
            .trim()
            .toLowerCase();
          if (!contentType || !imageTypes.has(contentType)) return;
          const blob = await response.blob();
          if (
            shared.controller.signal.aborted ||
            !apiRequestContextIsCurrent(context) ||
            blob.size === 0 ||
            !imageTypes.has(blob.type.toLowerCase())
          )
            return;
          const url = URL.createObjectURL(blob);
          if (shared.controller.signal.aborted || !apiRequestContextIsCurrent(context)) {
            URL.revokeObjectURL(url);
            return;
          }
          shared.url = url;
          retryAttempts.delete(retryKey);
          shared.listeners.forEach((listener) => listener(url));
        } catch {
          retry();
        }
      })();
    }
    request.refs++;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- a new image must read a blob already loaded by another mount
    const onLoaded = (url: string) => setLoaded({ key, url });
    request.listeners.add(onLoaded);
    if (request.url) onLoaded(request.url);
    if (request.exhausted) invalidateAuthenticatedMediaUrl(path);
    return () => {
      request.listeners.delete(onLoaded);
      if (--request.refs === 0) {
        request.controller.abort();
        if (request.retryTimer) clearTimeout(request.retryTimer);
        if (mediaRevisions.get(path) === revision) retryAttempts.delete(retryKey);
        if (request.url) URL.revokeObjectURL(request.url);
        if (mediaRequests.get(key) === request) mediaRequests.delete(key);
      }
    };
  }, [authGeneration, hasSession, key, path, revision, serverEpoch]);

  if (path) return hasSession && loaded && loaded.key === key ? loaded.url : undefined;
  if (
    source?.startsWith('data:image/') ||
    source?.startsWith('blob:') ||
    (source?.startsWith('/') && !source.startsWith('/api/v1/media/'))
  ) {
    return resolveMediaUrl(source);
  }
  return undefined;
}
