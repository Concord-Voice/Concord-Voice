import { useCallback, useEffect, useRef } from 'react';
import { errorMessage } from '../../utils/runtime/redactError';

const DEFAULT_DELAY_MS = 3000;

type Post = () => Promise<unknown> | void;

function logFailure(err: unknown): void {
  console.error('[useReadMarker] Failed to post read marker:', errorMessage(err));
}

/**
 * Trailing-debounced "mark as read" caller. `markSeen()` may be called as
 * often as the caller likes (every message arrival, every scroll-to-bottom);
 * each call restarts a `delayMs` timer, and `post` fires once the calls go
 * quiet — coalescing a burst of arrivals into a single request instead of
 * one per message. `delayMs` should stay comfortably above whatever the
 * server's per-minute limit for `post`'s endpoint implies for one caller
 * (e.g. a 30/min limit needs at least a 2s floor; the 3s default leaves
 * headroom).
 *
 * `key` names the thread `post` marks (its id). A pending post is flushed
 * immediately when the key changes or the owner unmounts, rather than
 * dropped — leaving a thread right after reading its latest message must not
 * lose the marker to the debounce window — and it is the `post` captured
 * when `markSeen` was called that fires, not whatever the parent renders by
 * then, so a switch cannot mark the NEW thread read with the old thread's
 * intent. Flushing is keyed on `key`, not on `post`'s identity, so an inline
 * `post` cannot silently turn the debounce into a post per render.
 *
 * `flush()` posts a pending marker now. The server stamps `/read` with its
 * own clock when the request lands, so a timer still running would also
 * cover anything that arrives before it fires — a message the user never
 * saw. The owner therefore flushes the instant the user stops following the
 * latest message (`MessageList`'s `onLatestLeft`, also fired when the list
 * unmounts under a still-mounted owner), and this hook flushes on its own
 * when the document becomes hidden or the window loses focus. None adds a
 * request — they
 * move an already-owed one earlier — so a visit costs at most the open-time
 * read plus one marker against the route's 30/min limit; a 429 is logged
 * (callers reject on a non-2xx) and the next post covers it. What is still
 * lost: a read followed by an app quit inside the window — no cleanup runs
 * and the marker stays one message low, which lands the next open a row
 * early rather than past anything unread.
 *
 * `post`'s failure is swallowed: the marker is best-effort, and a failed
 * post here must not surface as a user-facing error or crash the unmount
 * path.
 */
export function useReadMarker(
  post: Post,
  key: string | null | undefined,
  delayMs = DEFAULT_DELAY_MS
) {
  const postRef = useRef(post);
  postRef.current = post;
  const delayMsRef = useRef(delayMs);
  delayMsRef.current = delayMs;
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // The post captured by the last markSeen call — the thread that was read.
  const pendingRef = useRef<Post | null>(null);

  const flush = useCallback(() => {
    if (timerRef.current) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    const pending = pendingRef.current;
    pendingRef.current = null;
    if (!pending) return;
    let result: Promise<unknown> | void;
    try {
      result = pending();
    } catch (err: unknown) {
      logFailure(err);
      return;
    }
    Promise.resolve(result).catch(logFailure);
  }, []);

  const markSeen = useCallback(() => {
    pendingRef.current = postRef.current;
    if (timerRef.current) clearTimeout(timerRef.current);
    timerRef.current = setTimeout(flush, delayMsRef.current);
  }, [flush]);

  // Key change = thread switch: settle what the previous thread still owes
  // before anything is marked for the new one. Unmount flushes the same way.
  useEffect(() => flush, [key, flush]);

  // Hidden document or unfocused window: whatever was seen was seen before
  // now. Blur matters on its own — an alt-tabbed window stays "visible", and
  // a message arriving into it is not seen, so the stamp must land first.
  useEffect(() => {
    const onVisibility = () => {
      if (document.visibilityState === 'hidden') flush();
    };
    document.addEventListener('visibilitychange', onVisibility);
    window.addEventListener('blur', flush);
    return () => {
      document.removeEventListener('visibilitychange', onVisibility);
      window.removeEventListener('blur', flush);
    };
  }, [flush]);

  return { markSeen, flush };
}
