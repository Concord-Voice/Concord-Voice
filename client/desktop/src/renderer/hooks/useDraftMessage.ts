import { useEffect, useRef, useCallback, useState } from 'react';
import { useDraftMessageStore, type DraftContent } from '../stores/draftMessageStore';
import type { MessageWithStatus } from '../types/chat';

interface PendingDebounce {
  timer: ReturnType<typeof setTimeout>;
  targetId: string;
  text: string;
  replyTo: MessageWithStatus | null | undefined;
}

function buildDraftContent(text: string, replyTo?: MessageWithStatus | null): DraftContent {
  return {
    text,
    replyToId: replyTo?.id,
    replyToUserId: replyTo?.user_id,
    replyToUsername: replyTo?.username || replyTo?.display_name || undefined,
    updatedAt: Date.now(),
  };
}

function flushPending(pending: PendingDebounce | null): void {
  if (!pending) return;
  clearTimeout(pending.timer);
  const store = useDraftMessageStore.getState();
  if (!pending.text && !pending.replyTo) {
    store.clearDraft(pending.targetId);
  } else {
    store.setDraft(pending.targetId, buildDraftContent(pending.text, pending.replyTo));
  }
}

export function useDraftMessage(targetId: string | undefined) {
  const pendingRef = useRef<PendingDebounce | null>(null);
  const prevTargetIdRef = useRef<string | undefined>(targetId);

  const [initialDraft, setInitialDraft] = useState<DraftContent | undefined>(() => {
    if (!targetId) return undefined;
    return useDraftMessageStore.getState().getDraft(targetId) ?? undefined;
  });

  // Handle targetId changes: flush old, load new
  useEffect(() => {
    const prevTargetId = prevTargetIdRef.current;

    if (prevTargetId !== targetId) {
      // Flush any pending debounce for the old targetId
      if (pendingRef.current && pendingRef.current.targetId === prevTargetId) {
        flushPending(pendingRef.current);
        pendingRef.current = null;
      }

      // Load draft for the new targetId
      if (targetId) {
        const draft = useDraftMessageStore.getState().getDraft(targetId);
        setInitialDraft(draft ?? undefined);
      } else {
        setInitialDraft(undefined);
      }

      prevTargetIdRef.current = targetId;
    }
  }, [targetId]);

  // Cleanup on unmount: flush any pending debounce
  useEffect(() => {
    return () => {
      if (pendingRef.current) {
        flushPending(pendingRef.current);
        pendingRef.current = null;
      }
    };
  }, []);

  const saveDraft = useCallback(
    (text: string, replyTo?: MessageWithStatus | null) => {
      if (!targetId) return;

      // Cancel any existing pending debounce
      if (pendingRef.current) {
        clearTimeout(pendingRef.current.timer);
      }

      const currentTargetId = targetId;

      const timer = setTimeout(() => {
        const store = useDraftMessageStore.getState();
        if (!text && !replyTo) {
          store.clearDraft(currentTargetId);
        } else {
          store.setDraft(currentTargetId, buildDraftContent(text, replyTo));
        }
        pendingRef.current = null;
      }, 500);

      pendingRef.current = { timer, targetId: currentTargetId, text, replyTo };
    },
    [targetId]
  );

  const clearDraft = useCallback(() => {
    if (!targetId) return;

    // Cancel any pending debounce
    if (pendingRef.current) {
      clearTimeout(pendingRef.current.timer);
      pendingRef.current = null;
    }

    useDraftMessageStore.getState().clearDraft(targetId);
  }, [targetId]);

  return { initialDraft, saveDraft, clearDraft };
}
