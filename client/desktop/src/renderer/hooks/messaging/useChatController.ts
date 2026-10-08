/**
 * useChatController — Unified chat operations hook (#492)
 *
 * Routes send/edit/delete/reply/pin/typing to the correct transport
 * (WebSocket method, REST endpoint) and permission model based on context type.
 *
 * Security boundaries enforced:
 * - Channel/voice: sendMessage(), /api/v1/messages/, RBAC permissions
 * - DM: sendDMMessage(), /api/v1/dm/conversations/{id}/messages/, ownership-based
 * - Typing/subscription methods never cross context boundaries
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useChatStore } from '../../stores/chat/chatStore';
import { useUserStore } from '../../stores/auth/userStore';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import { PIN_MESSAGES } from '../../utils/policy/permissions';
import { useMessaging } from './useMessaging';
import { pinMessage, unpinMessage } from '../../services/messaging/pinService';
import { getWebSocketService, ConnectionState } from '../../services/messaging/websocketService';
import { apiFetch, safeJson } from '../../services/system/apiClient';
import {
  apiFetchInContext,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { indexMessage, removeMessage } from '../../services/messaging/searchService';
import { wrapContentWithGifSlug } from '../../services/messaging/dmMessageSender';
import {
  mintRefusalView,
  toDeleteRefusalView,
  toDeleteSubmitOutcome,
  type DeleteRefusalView,
} from '../../services/messaging/deleteRefusal';
import {
  mintPasswordStepUpToken,
  passwordStepUpRefusalMessage,
  type PasswordStepUpMint,
} from '../../services/system/stepUpToken';
import type { PasswordStepUpPurpose } from '../../components/Auth/stepUpPurpose';
import type { StepUpSubmitOutcome } from '../auth/useStepUpFactor';
import type {
  ChatContext,
  ChatContextType,
  MessageWithStatus,
  AttachmentSummary,
} from '../../types/chat';

/**
 * A step-up factor the refusal modal collected for a delete retry. Exactly one
 * is populated. The password never reaches the delete route (#3509): it is
 * exchanged for a single-use token first (see `mintedRetryBody`).
 */
export interface DeleteStepUp {
  mfaCode?: string;
  currentPassword?: string;
}

/**
 * What a retry sends, or what stopped it before the route: a refusal, with the
 * factor hook's reading of it, or an exchange that never left (D7). A retry
 * carries the request context it belongs to, so the delete is admitted against
 * that same account and server.
 */
type DeleteRetry =
  | { body?: Record<string, string>; context?: ApiRequestContext }
  | { refusal: DeleteRefusalView; outcome: StepUpSubmitOutcome }
  | { notSent: true };

/** Where a delete goes, and the purpose a password minted for it is bound to. */
function deleteTarget(
  isDM: boolean,
  chatId: string,
  messageId: string
): { url: string; purpose: PasswordStepUpPurpose } {
  return isDM
    ? {
        url: `/api/v1/dm/conversations/${chatId}/messages/${messageId}`,
        purpose: 'dm.message_delete',
      }
    : { url: `/api/v1/messages/${messageId}`, purpose: 'messages.delete' };
}

/** A retry carrying a password and no code: the only one that needs a mint. */
function needsMint(
  step: DeleteStepUp | undefined
): step is DeleteStepUp & { currentPassword: string } {
  return !step?.mfaCode && !!step?.currentPassword;
}

/** The retry body when no password exchange is needed: a code, or nothing. */
function directRetryBody(
  step: DeleteStepUp | undefined,
  context: ApiRequestContext | undefined
): DeleteRetry {
  return step?.mfaCode ? { body: { mfa_code: step.mfaCode }, context } : { context };
}

/**
 * The retry body for a password. The password goes only to the mint endpoint,
 * which answers a token bound to this route's purpose; the route then gets
 * `{ step_up_token }`, and a refused exchange fills the slot instead (#3509).
 *
 * The exchange and the delete are one operation, so they run against one
 * request context, the caller's capture or one taken here, returned with the
 * body: the delete then refuses to dispatch if another account or server took
 * over after the exchange, and a token is never spent as another account
 * (#3509 review).
 */
async function mintedRetryBody(
  password: string,
  purpose: PasswordStepUpPurpose,
  context: ApiRequestContext | undefined
): Promise<DeleteRetry> {
  const operation = context ?? captureApiRequestContext();
  const minted = await mintPasswordStepUpToken(password, purpose, operation);
  if (minted.kind === 'minted') {
    return { body: { step_up_token: minted.token }, context: operation };
  }
  // D7: the exchange never left, so there is nothing to say about the password.
  if (minted.unsent) return { notSent: true };
  return mintRefusalRetry(minted);
}

/**
 * The slot's view of a refused exchange, with the factor hook's reading of it.
 * The fields can answer only the methods it named or a wrong password. Any
 * other refusal (a lockout, a server without the endpoint, a failed lookup, an
 * MFA requirement naming no method) is no verdict on the password, so typing
 * again answers nothing: the challenge ends Close-only with the exchange's own
 * words, as the purge soft-lock's does (`endExchangeRefusal`).
 */
function mintRefusalRetry(minted: Extract<PasswordStepUpMint, { kind: 'refused' }>): DeleteRetry {
  const view = mintRefusalView(minted);
  if (view.view === 'confirm') {
    return {
      refusal: view,
      outcome: { kind: 'refusal', refusal: { kind: 'mfaRequired', methods: view.methods } },
    };
  }
  if (minted.reason === 'invalidPassword') {
    return { refusal: view, outcome: { kind: 'refusal', refusal: { kind: 'invalidPassword' } } };
  }
  return {
    refusal: { view: 'failed', message: passwordStepUpRefusalMessage(minted) },
    outcome: { kind: 'answered' },
  };
}

/**
 * True when the row should go: a success, or a 404 on a RETRY, which counts as
 * gone because the message was deleted by the time the confirmation
 * round-tripped (another moderator, another device).
 */
function deletedOrGone(res: Response, isRetry: boolean): boolean {
  return res.ok || (isRetry && res.status === 404);
}

/** A body only when a factor is present (X10): a fresh delete carries none. */
function deleteInit(body: Record<string, string> | undefined): RequestInit {
  if (!body) return { method: 'DELETE' };
  return {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  };
}

/** The one refusal slot `DeleteRefusalModal` reads. */
export interface DeleteRefusalState {
  messageId: string;
  view: DeleteRefusalView;
  openedAt: number;
}

export interface SendOpts {
  mentionMeta?: string;
  replyToId?: string;
  attachmentIds?: string[];
  attachments?: AttachmentSummary[];
  gifSlug?: string;
}

interface EncryptedEditResult {
  keyVersion: number;
  message: {
    content: string;
    edited_at: string;
    updated_at?: string;
  };
}

/** `apiFetch` throws a DOMException named AbortError; jsdom's is not an Error subclass. */
function isAbortError(err: unknown): boolean {
  return (
    typeof err === 'object' && err !== null && (err as { name?: unknown }).name === 'AbortError'
  );
}

async function patchEncryptedMessage(
  channelId: string,
  url: string,
  plaintext: string
): Promise<EncryptedEditResult> {
  for (let attempt = 0; attempt < 2; attempt += 1) {
    const { ciphertext, keyVersion } = await e2eeService.encryptForChannelWithVersion(
      channelId,
      plaintext
    );
    const res = await apiFetch(url, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content: ciphertext, key_version: keyVersion }),
    });

    if (!res.ok) {
      const errorData = await safeJson<{ error?: string; code?: string }>(res);
      if (attempt === 0 && res.status === 409 && errorData.code === 'epoch_revoked') {
        e2eeService.invalidateChannelKey(channelId);
        continue;
      }
      throw new Error(errorData.error || 'Failed to edit message');
    }

    const data = await safeJson<{ message: EncryptedEditResult['message'] }>(res);
    return { keyVersion, message: data.message };
  }

  throw new Error('Failed to edit message');
}

export function useChatController(ctx: ChatContext) {
  const messaging = useMessaging();
  const user = useUserStore((s) => s.user);
  const updateMessage = useChatStore((s) => s.updateMessage);
  const storeDeleteMessage = useChatStore((s) => s.deleteMessage);
  const replyingTo = useChatStore((s) => s.replyingTo.get(ctx.id) ?? null);
  const setReplyingTo = useChatStore((s) => s.setReplyingTo);
  const hasServerPermission = usePermissionStore((s) => s.hasServerPermission);

  // Runtime sanity: DM context should never have serverId
  useEffect(() => {
    if (ctx.type === 'dm' && ctx.serverId) {
      console.warn(
        '[useChatController] DM context should not have serverId — possible misconfiguration'
      );
    }
  }, [ctx.type, ctx.serverId]);

  // --- Context derivation ---
  const chatContext: ChatContextType = ctx.type;
  const isDM = ctx.type === 'dm';

  // --- Send ---
  const sendMessage = useCallback(
    (content: string, opts?: SendOpts) => {
      if (!ctx.id || !user) return;
      const username = user.username || 'You';
      const sendOpts = {
        avatarUrl: user.avatar_url,
        displayName: user.display_name,
        mentionMeta: opts?.mentionMeta,
        replyToId: opts?.replyToId,
        attachmentIds: opts?.attachmentIds,
        attachments: opts?.attachments,
        gifSlug: opts?.gifSlug,
      };

      if (isDM) {
        messaging.sendDMMessage(ctx.id, content, username, sendOpts);
      } else {
        messaging.sendMessage(ctx.id, content, username, sendOpts);
      }

      // Clear reply state after send
      setReplyingTo(ctx.id, null);
    },
    [ctx.id, isDM, user, messaging, setReplyingTo]
  );

  // --- Edit ---
  const editMessage = useCallback(
    async (messageId: string, newContent: string) => {
      if (!ctx.id || !e2eeService.isInitialized) return;

      try {
        const existingMessage = useChatStore
          .getState()
          .messagesByChannel.get(ctx.id)
          ?.find((message) => message.id === messageId);
        const gifSlug = existingMessage?.gif_slug;
        const plaintext = wrapContentWithGifSlug(newContent, gifSlug);
        const url = isDM
          ? `/api/v1/dm/conversations/${ctx.id}/messages/${messageId}`
          : `/api/v1/messages/${messageId}`;

        const { keyVersion, message } = await patchEncryptedMessage(ctx.id, url, plaintext);

        // A delete event may win while the edit request is awaiting. Keep
        // both the store and plaintext search index deleted in that case.
        const messageStillExists = useChatStore
          .getState()
          .messagesByChannel.get(ctx.id)
          ?.some((candidate) => candidate.id === messageId);
        // #1959: resolve true only when there is nothing to give back — the edit was saved, or
        // the message is gone. Any other outcome (no context, e2ee not ready, a failed PATCH)
        // means the user's text never reached the server, and the row restores it.
        if (!messageStillExists) return true;

        updateMessage(ctx.id, messageId, {
          content: newContent,
          key_version: keyVersion,
          decryptFailed: false,
          pendingKeys: false,
          ...(gifSlug !== undefined && { gif_slug: gifSlug }),
          edited_at: message.edited_at,
          ...(message.updated_at && { updated_at: message.updated_at }),
        });
        if (newContent) {
          indexMessage(messageId, newContent, ctx.id);
        } else {
          removeMessage(messageId);
        }
        return true;
      } catch (err) {
        if (isDM) {
          console.error('Failed to edit DM message:', (err as Error).message);
        } else {
          console.error('Failed to edit message:', (err as Error).message);
        }
        return false;
      }
    },
    [ctx.id, isDM, updateMessage]
  );

  // --- Delete ---
  // One refusal slot (#3455): the first refusal occupies it, and a refusal for
  // any OTHER message — a delete already in flight when the slot filled — is
  // discarded. Hook-local state only; no store field carries this (§2.10).
  const [deleteRefusal, setDeleteRefusal] = useState<DeleteRefusalState | null>(null);
  // Dedupe, not a queue: a duplicate delete for an id already in flight is a
  // no-op. Without this a double shift-click would draw a spurious 404 that
  // the fixed error path now surfaces instead of swallowing.
  const inFlightDeleteIdsRef = useRef<Set<string>>(new Set());
  const isMountedRef = useRef(true);
  useEffect(() => {
    isMountedRef.current = true;
    return () => {
      isMountedRef.current = false;
    };
  }, []);

  const refusalMessageId = deleteRefusal?.messageId ?? null;
  // Selective subscription: only whether the refused message is still present
  // in ITS channel/DM, not the whole map — a full-map subscription would
  // re-run this hook on every unrelated message arriving anywhere.
  const refusalMessageStillExists = useChatStore((s) =>
    refusalMessageId
      ? (s.messagesByChannel.get(ctx.id)?.some((m) => m.id === refusalMessageId) ?? false)
      : true
  );

  useEffect(() => {
    if (refusalMessageId && !refusalMessageStillExists) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- the refused message was deleted elsewhere (a moderator, another device); the slot must not outlive it
      setDeleteRefusal(null);
    }
  }, [refusalMessageId, refusalMessageStillExists]);

  // The chat on screen, for a delete whose response outlives the chat it was
  // sent from. Clearing the slot on a switch is not enough on its own: a
  // response still in flight would fill it again afterwards, showing the old
  // message's refusal here, and confirming it would retry that message
  // against this chat.
  const currentCtxIdRef = useRef(ctx.id);
  useEffect(() => {
    currentCtxIdRef.current = ctx.id;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- refusal state is scoped to this chat context; switching channel/DM must not leak a stale confirmation into the next one
    setDeleteRefusal(null);
  }, [ctx.id]);

  // Fills the one refusal slot for messageId. A refusal for any OTHER message
  // keeps the slot as it is. Each fill is its own countdown anchor:
  // `Retry-After` is relative to the response that carried it.
  const fillRefusalSlot = useCallback(
    (messageId: string, viewFor: (prior?: DeleteRefusalView) => DeleteRefusalView) => {
      const openedAt = Date.now();
      setDeleteRefusal((cur) => {
        if (cur && cur.messageId !== messageId) return cur;
        return { messageId, view: viewFor(cur?.view), openedAt };
      });
    },
    []
  );

  const sendDelete = useCallback(
    async (
      messageId: string,
      step?: DeleteStepUp,
      context?: ApiRequestContext
    ): Promise<StepUpSubmitOutcome> => {
      // Nothing is sent: no chat, or a delete for this id is already in flight.
      if (!ctx.id || inFlightDeleteIdsRef.current.has(messageId)) return { kind: 'aborted' };
      inFlightDeleteIdsRef.current.add(messageId);
      const isRetry = step !== undefined;
      const sentFrom = ctx.id;
      // A success still removes the row from the chat it was sent from; only
      // the refusal slot belongs to the chat on screen. The mint round trip
      // (#3509) answers to the same rule: a mint refusal that lands after the
      // chat changed is discarded with everything else.
      const reportIfShowing = (viewFor: (prior?: DeleteRefusalView) => DeleteRefusalView) => {
        if (isMountedRef.current && currentCtxIdRef.current === sentFrom) {
          fillRefusalSlot(messageId, viewFor);
        }
      };

      try {
        const { url, purpose } = deleteTarget(isDM, ctx.id, messageId);

        // Only a password costs a round trip before the route; a fresh delete
        // or a code goes out in this same tick, as before.
        const retry = needsMint(step)
          ? await mintedRetryBody(step.currentPassword, purpose, context)
          : directRetryBody(step, context);
        // An exchange apiFetch refused to dispatch is the AbortError below.
        if ('notSent' in retry) return { kind: 'aborted' };
        if ('refusal' in retry) {
          reportIfShowing(() => retry.refusal);
          return retry.outcome;
        }

        const res = await apiFetchInContext(url, deleteInit(retry.body), retry.context);

        if (deletedOrGone(res, isRetry)) {
          // The row is stale either way, so a retry's 404 is removed the same
          // as a success. The vanished-message effect above closes the slot.
          storeDeleteMessage(ctx.id, messageId);
          removeMessage(messageId);
          return { kind: 'success' };
        }

        // safeJson throws on a non-JSON body (a proxy's HTML error page). That
        // must not become the dialog's text, so an unreadable body is an empty
        // one and the mapper falls back to its own copy.
        const body: unknown = await safeJson(res).catch((): unknown => ({}));
        const retryAfter = res.headers.get('Retry-After');
        reportIfShowing((prior) => toDeleteRefusalView(res.status, body, retryAfter, prior));
        return toDeleteSubmitOutcome(res.status, body);
      } catch (err) {
        // Transport failure — reported, not swallowed (the fix this issue
        // makes to the pre-existing behaviour below the soft-lock threshold).
        // apiFetch refuses to dispatch once the account or server changed
        // under it: nothing was sent, so there is nothing to report. The error
        // text is never shown — it is transport detail, not user copy.
        if (isAbortError(err)) return { kind: 'aborted' };
        reportIfShowing(() => ({ view: 'failed' }));
        return { kind: 'transport' };
      } finally {
        inFlightDeleteIdsRef.current.delete(messageId);
      }
    },
    [ctx.id, isDM, storeDeleteMessage, fillRefusalSlot]
  );

  // A fresh delete reports through the slot alone; its outcome is the hook's.
  const deleteMessage = useCallback(
    async (messageId: string): Promise<void> => {
      await sendDelete(messageId);
    },
    [sendDelete]
  );

  // Resolves to the factor hook's reading of the retry (D6), so `run` can
  // await it. `context` is the run's capture: the retry, and any password
  // exchange before it, are admitted against it.
  const confirmDelete = useCallback(
    (step: DeleteStepUp, context?: ApiRequestContext): Promise<StepUpSubmitOutcome> => {
      if (!deleteRefusal) return Promise.resolve({ kind: 'aborted' });
      return sendDelete(deleteRefusal.messageId, step, context);
    },
    [deleteRefusal, sendDelete]
  );

  const dismissDeleteRefusal = useCallback(() => {
    setDeleteRefusal(null);
  }, []);

  // --- Reply ---
  const handleReply = useCallback(
    (msg: MessageWithStatus) => {
      setReplyingTo(ctx.id, msg);
    },
    [ctx.id, setReplyingTo]
  );

  const cancelReply = useCallback(() => {
    setReplyingTo(ctx.id, null);
  }, [ctx.id, setReplyingTo]);

  // --- Pin ---
  const canPin = useMemo(() => {
    if (isDM) return true; // DMs: ownership-based, no RBAC
    if (!ctx.serverId) return false;
    return hasServerPermission(ctx.serverId, PIN_MESSAGES);
  }, [isDM, ctx.serverId, hasServerPermission]);

  const handlePinToggle = useCallback(async (msg: MessageWithStatus) => {
    try {
      if (msg.pinned_at) {
        await unpinMessage(msg.id);
      } else {
        await pinMessage(msg.id);
      }
    } catch (err) {
      console.error('Failed to toggle pin:', (err as Error).message);
    }
  }, []);

  // --- Typing ---
  const sendTyping = useCallback(
    (isTyping: boolean) => {
      const ws = getWebSocketService();
      if (ws?.getState() !== ConnectionState.CONNECTED) return;

      if (isDM) {
        ws.sendDMTypingIndicator(ctx.id, isTyping);
      } else {
        ws.sendTypingIndicator(ctx.id, isTyping);
      }
    },
    [isDM, ctx.id]
  );

  return {
    // Message operations
    sendMessage,
    editMessage,
    deleteMessage,
    // Delete-rate soft-lock refusal (#3455)
    deleteRefusal,
    confirmDelete,
    dismissDeleteRefusal,
    // Reply
    replyingTo,
    handleReply,
    cancelReply,
    // Pin
    canPin,
    handlePinToggle,
    // Typing
    sendTyping,
    // Context
    chatContext,
  };
}
