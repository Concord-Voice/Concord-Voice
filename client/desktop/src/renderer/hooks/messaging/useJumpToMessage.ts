import { useCallback, useEffect, useReducer, useRef, useState, type RefObject } from 'react';
import type { MessageListHandle } from '../../components/Chat/MessageList';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
  type AuthLifecycleSnapshot,
} from '../../services/system/postLoginHydrationLifecycle';
import type { LoadUntilResult, LoadUntilStatus } from './useMessageFetch';

/** HTTP requests one jump may start; `loadUntil` counts reconcile retries and joined pages (#3894). */
export const JUMP_REQUEST_BUDGET = 10;

export type JumpOutcome = 'reached' | 'too-far' | 'missing' | 'failed' | 'cancelled';
export type JumpFailure = 'too-far' | 'missing' | 'failed';

/** Shown on the search card and in the chat notice. Names no channel and quotes no message. */
export const JUMP_FAILURE_COPY: Record<JumpFailure, string> = {
  'too-far':
    "Couldn't reach that message. It's too far back to load. Scroll up in the chat, then try again.",
  missing: "Couldn't reach that message. It may have been deleted or expired.",
  failed: "Couldn't reach that message. Loading older messages failed. Try again in a moment.",
};

export interface JumpOptions {
  /** Ends the jump as `cancelled` when aborted. */
  signal?: AbortSignal;
  /** The caller shows its own failure line, so a same-channel failure sets no notice. */
  inline?: boolean;
}

export interface UseJumpToMessageParams {
  channelId: string | null;
  isHistoryReady: boolean;
  /** Owners pass `error !== null && messages.length === 0`, so a failed scroll never blocks a jump. */
  hasInitialHistoryError: boolean;
  loadUntil: (
    messageId: string,
    maxRequests: number,
    signal?: AbortSignal
  ) => Promise<LoadUntilResult>;
  listRef: RefObject<MessageListHandle | null>;
  /** Moves the chat to another channel; false means it refused. Absent: the surface cannot cross. */
  switchChannel?: (channelId: string) => boolean;
}

export interface UseJumpToMessageResult {
  jump: (channelId: string, messageId: string, options?: JumpOptions) => Promise<JumpOutcome>;
  /** The failure text for the current channel, or null. */
  notice: string | null;
}

interface PendingJump {
  generation: number;
  channelId: string;
  messageId: string;
  /** Requests still allowed; every `loadUntil` result spends from it. */
  remaining: number;
  auth: AuthLifecycleSnapshot;
  /** The jump switched channels, so the target channel may not have committed yet. */
  crossed: boolean;
  /** `loadUntil` reported the row in the store. */
  found: boolean;
  inline: boolean;
  resolve: (outcome: JumpOutcome) => void;
  /** Removes the caller's abort listener once the jump ends. */
  detach: () => void;
}

interface NoticeState {
  channelId: string | null;
  text: string;
}

interface GateInput {
  channelId: string | null;
  seen: boolean;
  isHistoryReady: boolean;
  hasInitialHistoryError: boolean;
}

function nextVersion(version: number): number {
  return version + 1;
}

export function isJumpFailure(outcome: JumpOutcome): outcome is JumpFailure {
  return outcome !== 'reached' && outcome !== 'cancelled';
}

/** How a `loadUntil` status ends a jump. `found` and `interrupted` are absent: they keep it going. */
const STATUS_OUTCOME: Partial<Record<LoadUntilStatus, JumpOutcome>> = {
  exhausted: 'missing',
  'too-far': 'too-far',
  failed: 'failed',
  cancelled: 'cancelled',
};

/**
 * The failure notice, tagged with the channel it belongs to. It is dropped
 * during render once the chat shows another channel: setting state while
 * rendering the same component re-renders before commit, so no frame shows a
 * stale notice and no effect is needed.
 */
function useChannelNotice(channelId: string | null) {
  const [state, setState] = useState<NoticeState | null>(null);
  if (state !== null && state.channelId !== channelId) setState(null);
  return [state?.text ?? null, setState] as const;
}

/**
 * Passes 1-4 of the state machine: an outcome ends the jump, `wait` holds it
 * until a dependency changes, null lets it go on.
 */
function gate(cur: PendingJump, input: GateInput): JumpOutcome | 'wait' | null {
  if (input.channelId !== cur.channelId) {
    // A crossed jump waits for the switch to commit; once it has been in the
    // target channel, or if it never left one, a mismatch means the user left.
    return input.seen || !cur.crossed ? 'cancelled' : 'wait';
  }
  if (!isSameAuthLifecycle(cur.auth)) return 'cancelled'; // sign-out or account switch
  // A refetch pauses the jump rather than ending it. Paging before the channel
  // settles can also read the previous channel's `hasMore` and end `exhausted`.
  if (!input.isHistoryReady) return 'wait';
  return input.hasInitialHistoryError ? 'failed' : null;
}

/** Pass 6: the row was loaded but never mounted, or the budget is spent. */
function stuckOutcome(cur: PendingJump): JumpOutcome | null {
  if (cur.found) return 'missing';
  return cur.remaining <= 0 ? 'too-far' : null;
}

/** What a crossing jump does about moving the chat: null once it moved, else how the jump ends. */
function crossTo(
  target: string,
  switchTo: ((channelId: string) => boolean) | undefined
): JumpOutcome | null {
  if (!switchTo) return 'cancelled';
  return switchTo(target) ? null : 'missing';
}

/**
 * Drives a jump to a message the chat view may not have loaded: switches
 * channel when the message lives elsewhere, waits for the list to be ready,
 * pages older history through `loadUntil`, and ends in one `JumpOutcome`.
 *
 * It is an effect-driven state machine because a cross-channel jump has to
 * wait for a list that remounts under a new key. The one pending record lives
 * in a ref; a counter re-runs the effect when the record changes. The main
 * effect cancels nothing from its cleanup: StrictMode replays effects and every
 * dependency change would end a live jump. Only the owner going away ends a
 * pending jump, from a separate effect that waits a microtask, so the replay
 * (cleanup then setup in one task) does not.
 *
 * The hook writes no store except through `switchChannel`, and logs nothing:
 * message and channel ids never reach a log.
 */
export function useJumpToMessage({
  channelId,
  isHistoryReady,
  hasInitialHistoryError,
  loadUntil,
  listRef,
  switchChannel,
}: UseJumpToMessageParams): UseJumpToMessageResult {
  const [notice, setNotice] = useChannelNotice(channelId);
  const [version, bump] = useReducer(nextVersion, 0);
  const pendingRef = useRef<PendingJump | null>(null);
  const generationRef = useRef(0);
  // Generation whose `loadUntil` is in flight, and that run's controller.
  const runningRef = useRef<number | null>(null);
  const runAbortRef = useRef<AbortController | null>(null);
  // Generation that has seen its target channel mounted as the current one.
  const seenRef = useRef(0);
  // Latest params, so `jump` keeps one identity for effect dependency lists.
  const paramsRef = useRef({ channelId, switchChannel });
  paramsRef.current = { channelId, switchChannel };

  const finish = useCallback(
    (generation: number, outcome: JumpOutcome) => {
      const cur = pendingRef.current;
      if (cur?.generation !== generation) return;
      pendingRef.current = null;
      cur.detach();
      // Stop paging for a jump that ended without its page: replaced, aborted
      // or reached from the store. A page that ended the jump is already clear.
      if (runningRef.current === generation) runAbortRef.current?.abort();
      cur.resolve(outcome);
      const quiet = cur.inline && !cur.crossed; // the card shows its own failure line
      if (isJumpFailure(outcome) && !quiet) {
        setNotice({ channelId: cur.channelId, text: JUMP_FAILURE_COPY[outcome] });
      }
    },
    [setNotice]
  );

  const cancelPending = useCallback(() => {
    const earlier = pendingRef.current;
    if (earlier) finish(earlier.generation, 'cancelled');
  }, [finish]);

  const jump = useCallback(
    (targetChannelId: string, messageId: string, options?: JumpOptions) =>
      new Promise<JumpOutcome>((resolve) => {
        cancelPending();
        setNotice(null);
        const generation = ++generationRef.current;
        const signal = options?.signal;
        if (signal?.aborted) {
          resolve('cancelled');
          return;
        }
        const { channelId: current, switchChannel: switchTo } = paramsRef.current;
        const crossed = targetChannelId !== current;
        if (crossed) {
          // Before the record is stored and in this same handler, so React
          // commits one render that already shows the target channel.
          const refused = crossTo(targetChannelId, switchTo);
          if (refused) {
            // The user stays where they are, so the notice belongs to that channel.
            if (refused === 'missing') {
              setNotice({ channelId: current, text: JUMP_FAILURE_COPY.missing });
            }
            resolve(refused);
            return;
          }
        }
        const onAbort = () => finish(generation, 'cancelled');
        signal?.addEventListener('abort', onAbort, { once: true });
        pendingRef.current = {
          generation,
          channelId: targetChannelId,
          messageId,
          remaining: JUMP_REQUEST_BUDGET,
          auth: captureAuthLifecycle(),
          crossed,
          found: false,
          inline: options?.inline === true,
          resolve,
          detach: () => signal?.removeEventListener('abort', onAbort),
        };
        bump();
      }),
    [cancelPending, finish, setNotice]
  );

  // Pass 7: one `loadUntil` for this generation, with a controller the hook
  // owns so finishing the jump stops its paging.
  const startPage = useCallback(
    (cur: PendingJump) => {
      const generation = cur.generation;
      const run = new AbortController();
      runningRef.current = generation;
      runAbortRef.current = run;
      const settle = (result: LoadUntilResult) => {
        if (runningRef.current === generation) runningRef.current = null;
        if (runAbortRef.current === run) runAbortRef.current = null;
        if (pendingRef.current !== cur) return; // replaced or finished while paging
        // A page cut short spent its requests; one refused for a running history fetch
        // sends none, but `loadUntil` waits that fetch out before it tries again.
        cur.remaining -= result.requests;
        const outcome = STATUS_OUTCOME[result.status];
        if (outcome) {
          finish(generation, outcome);
          return;
        }
        if (result.status === 'found') cur.found = true;
        bump(); // `found` mounts the row; `interrupted` waits for readiness in pass 3
      };
      // A rejection is not a result `loadUntil` promises, but a jump left
      // waiting on it would hold its card busy for good. The error is not logged.
      void loadUntil(cur.messageId, cur.remaining, run.signal).then(settle, () =>
        settle({ status: 'failed', requests: 0 })
      );
    },
    [loadUntil, finish]
  );

  // The owner leaving ends a pending jump: its promise settles, the caller's
  // abort listener comes off and a running page stops spending the list route's
  // budget. Waiting on readiness, nothing else would ever end it.
  const mountedRef = useRef(false);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      queueMicrotask(() => {
        if (!mountedRef.current) cancelPending();
      });
    };
  }, [cancelPending]);

  useEffect(() => {
    const cur = pendingRef.current;
    if (!cur) return;
    const generation = cur.generation;
    if (channelId === cur.channelId) seenRef.current = generation;
    const verdict = gate(cur, {
      channelId,
      seen: seenRef.current === generation,
      isHistoryReady,
      hasInitialHistoryError,
    });
    if (verdict === 'wait') return;
    if (verdict) {
      finish(generation, verdict);
      return;
    }
    if (listRef.current?.scrollToMessage(cur.messageId)) {
      finish(generation, 'reached');
      return;
    }
    const stuck = stuckOutcome(cur);
    if (stuck) {
      finish(generation, stuck);
      return;
    }
    if (runningRef.current !== generation) startPage(cur);
  }, [version, channelId, isHistoryReady, hasInitialHistoryError, listRef, finish, startPage]);

  return { jump, notice };
}
