import { useState, useCallback, useEffect } from 'react';
import { useFriendStore } from '../stores/friendStore';
import { useUserStore } from '../stores/userStore';
import {
  fetchEligibility,
  peekEligibility,
  type EligibilityVerdict,
} from '../services/friendEligibility';

/**
 * Lifecycle of an in-flight "Send Friend Request" action. `sent` is a transient
 * confirmation state — the underlying friendship/pending state (which drives
 * `label` / `canSend`) updates independently via the friendStore's optimistic
 * write + WebSocket sync, so the button naturally settles into "Request
 * Pending" after a successful send.
 */
export type FriendRequestSendStatus = 'idle' | 'sending' | 'sent' | 'error';

export interface FriendRequestState {
  isFriend: boolean;
  hasPendingRequest: boolean;
  /**
   * Whether the friend-request affordance should render at all.
   *
   * False for self, for an authoritative `{"eligible": false}`, and — on inline
   * surfaces — while the verdict is in flight. Every OTHER failure degrades
   * OPEN: the server is the only authority, so a shown button that 403s is a
   * recoverable annoyance while a wrongly hidden one is an undiagnosable dead
   * end for the user and for support.
   *
   * The two hidden states (in flight, not eligible) are deliberately
   * indistinguishable to the viewer — a cue that separated them would leak the
   * verdict the boolean exists to hide.
   */
  visible: boolean;
  /** Actionable right now: not self, not already friends, no pending, not mid-send. */
  canSend: boolean;
  status: FriendRequestSendStatus;
  errorMessage: string | null;
  /** Discord-style relationship label: Friends / Request Pending / Send Friend Request. */
  label: string;
  send: () => Promise<void>;
}

/**
 * Shared state + action for the "Send Friend Request" affordance. Consumed by
 * every surface that offers it (member context menu, member profile card, chat
 * username/avatar profile card) so the relationship-state logic and labels
 * never drift between them.
 */
/** Verdict state, keyed to the subject it was captured for. */
interface CapturedVerdict {
  forUserId: string | undefined;
  frozen: EligibilityVerdict | 'pending';
  verdict: EligibilityVerdict | 'pending';
}

export interface UseFriendRequestStateOptions {
  /**
   * Context-menu mode. Freezes the verdict at mount so the menu's item set
   * never mutates after paint, and treats an unresolved verdict as shown. A row
   * appearing in an open menu is a layout shift, a focus-order mutation
   * (WCAG 2.4.3), and invisible to a screen reader that already announced the
   * item count (4.1.3).
   */
  freezeAtOpen?: boolean;
}

export function useFriendRequestState(
  userId: string | undefined,
  options?: UseFriendRequestStateOptions
): FriendRequestState {
  const currentUserId = useUserStore((s) => s.user?.id);
  const isFriend = useFriendStore((s) =>
    userId ? s.friends.some((f) => f.userId === userId) : false
  );
  const hasPendingRequest = useFriendStore((s) =>
    userId ? s.pendingRequests.some((r) => r.fromUserId === userId || r.toUserId === userId) : false
  );

  const [status, setStatus] = useState<FriendRequestSendStatus>('idle');
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  const isSelf = !!userId && userId === currentUserId;

  // None of these need the server to answer, and in FriendsList the viewer is
  // already a friend — probing there is pure waste.
  const shortCircuit = !userId || isSelf || isFriend || hasPendingRequest;

  const freeze = options?.freezeAtOpen === true;

  // Both verdicts are seeded from the synchronous cache so a WARM answer renders
  // on the first frame — without it the common double-mount (MemberProfileCard
  // renders SendFriendRequestButton for the same user) would flash hidden for a
  // frame on a verdict already known.
  const seed = (): CapturedVerdict => {
    const peeked = userId && !shortCircuit ? peekEligibility(userId) : 'pending';
    // `frozen` is captured once PER SUBJECT so a resolve cannot move it while a
    // menu is open; `verdict` tracks resolves for the inline surfaces.
    return { forUserId: userId, frozen: freeze ? peeked : 'pending', verdict: peeked };
  };

  const [captured, setCaptured] = useState<CapturedVerdict>(seed);

  // Both pieces of state are `useState` seeds, which React runs once per
  // INSTANCE — not once per subject. A parent that swaps `userId` without
  // remounting would otherwise render user B against user A's answer, and in the
  // frozen case for the whole life of that instance. This is reachable: the
  // context menu's close is deferred 150 ms, so right-clicking one member row
  // then another moves the menu A -> B without passing through null, and the
  // component reconciles rather than unmounts. Re-seeding during render is
  // React's documented way to adjust state when a prop changes — it re-renders
  // immediately, before paint, so no wrong-subject frame is ever shown.
  if (captured.forUserId !== userId) {
    setCaptured(seed());
  }

  const frozen = captured.frozen;
  const verdict = captured.verdict;

  useEffect(() => {
    if (!userId || shortCircuit) return;
    let active = true;
    void fetchEligibility(userId).then((v) => {
      // The fetch itself is still wanted when frozen — it warms the cache for
      // the NEXT open — but `frozen` is what this instance renders, so writing
      // `verdict` would re-render an open menu for a value it cannot consume.
      if (!active || freeze) return;
      // Guarded on the subject as well as on `active`: the cleanup flag stops a
      // stale resolve for a REPLACED subject, but this instance may since have
      // been re-seeded for a different user.
      setCaptured((prev) => (prev.forUserId === userId ? { ...prev, verdict: v } : prev));
    });
    return () => {
      active = false;
    };
  }, [userId, shortCircuit, freeze]);

  const effective = freeze ? frozen : verdict;

  const visible = (() => {
    if (!userId || isSelf) return false;
    if (isFriend || hasPendingRequest) return true;
    if (effective === 'ineligible') return false;
    // A frozen menu shows on an unresolved verdict; an inline surface hides, so
    // "in flight" and "not eligible" render identically (absent).
    if (effective === 'pending') return freeze;
    return true;
  })();

  const canSend = visible && !isFriend && !hasPendingRequest && status !== 'sending';

  const label = (() => {
    if (isFriend) return 'Friends';
    if (hasPendingRequest) return 'Request Pending';
    return 'Send Friend Request';
  })();

  const send = useCallback(async () => {
    if (!userId) return;
    setStatus('sending');
    setErrorMessage(null);
    try {
      await useFriendStore.getState().sendRequest(userId);
      setStatus('sent');
    } catch (error) {
      setStatus('error');
      setErrorMessage(error instanceof Error ? error.message : 'Failed to send friend request');
    }
  }, [userId]);

  return {
    isFriend,
    hasPendingRequest,
    visible,
    canSend,
    status,
    errorMessage,
    label,
    send,
  };
}
