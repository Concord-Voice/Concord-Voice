import { useEffect, useReducer, useRef } from 'react';
import { useInviteStore, type InviteInfoResponse } from '@/renderer/stores/inviteStore';

export type InvitePreviewState =
  | { status: 'loading' }
  | { status: 'ready'; info: InviteInfoResponse }
  | { status: 'invalid' };

// Cache only DEFINITIVE successful responses (HTTP 2xx, valid true|false), with a
// timestamp so entries revalidate on remount once stale — keeps the card
// "authoritative-over-snapshot" (renames/revocation) within reason. A `null` from
// getInviteInfo is a transport error OR a 404 and is NEVER cached, so a transient
// failure does not permanently poison the code: the next mount retries. (#1678, Gitar)
const CACHE_TTL_MS = 5 * 60_000;
interface CacheEntry {
  info: InviteInfoResponse;
  at: number;
}
const cache = new Map<string, CacheEntry>();
const inflight = new Map<string, Promise<InviteInfoResponse | null>>();

/** TEST-ONLY: reset the module cache so tests don't leak resolved previews. */
export function clearInvitePreviewCache(): void {
  cache.clear();
  inflight.clear();
}

function getCachedFresh(code: string): InviteInfoResponse | undefined {
  // Called from effects only (not during render) to avoid Date.now() purity lint.
  const entry = cache.get(code);
  if (entry && Date.now() - entry.at < CACHE_TTL_MS) return entry.info;
  return undefined;
}

function toState(info: InviteInfoResponse): InvitePreviewState {
  return info.valid ? { status: 'ready', info } : { status: 'invalid' };
}

export function useInvitePreview(code: string): InvitePreviewState {
  // resolvedRef holds the current render-stable result once a fetch settles.
  // null  = transient failure (this mount, not cached, remount retries)
  // undefined = not yet settled for this code
  // InviteInfoResponse = successful response
  const resolvedRef = useRef<InviteInfoResponse | null | undefined>(undefined);
  const [, forceRerender] = useReducer((n: number) => n + 1, 0);

  useEffect(() => {
    // Reset on code change.
    resolvedRef.current = undefined;

    // Check the cache inside the effect (Date.now() is fine here — effects run
    // outside render). If fresh, expose via ref and re-render.
    const fresh = getCachedFresh(code);
    if (fresh !== undefined) {
      resolvedRef.current = fresh;
      forceRerender();
      return;
    }

    let active = true;
    let p = inflight.get(code);
    if (!p) {
      p = useInviteStore
        .getState()
        .getInviteInfo(code)
        .then((info) => {
          if (info) cache.set(code, { info, at: Date.now() }); // cache successes only
          inflight.delete(code);
          return info;
        });
      inflight.set(code, p);
    }
    void p.then((info) => {
      if (!active) return;
      resolvedRef.current = info; // null for transient failures (not cached)
      forceRerender();
    });
    return () => {
      active = false;
    };
  }, [code]);

  // Derive state from the ref (set by the effect, never by render itself).
  if (resolvedRef.current === undefined) return { status: 'loading' };
  if (resolvedRef.current === null) return { status: 'invalid' };
  return toState(resolvedRef.current);
}
