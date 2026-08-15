/**
 * useWebSocketMessages - All WebSocket message handler subscriptions.
 *
 * Extracted from useWebSocket to isolate message routing from connection management.
 * Registers all `wsService.on(...)` handlers in a single useEffect and cleans up on unmount.
 */

import { useEffect } from 'react';
import { useChatStore } from '../stores/chatStore';
import type { Channel, ChannelGroup } from '../types/chat';
import { useChannelStore } from '../stores/channelStore';
import { useServerStore } from '../stores/serverStore';
import { useMemberStore } from '../stores/memberStore';
import { useUserStore } from '../stores/userStore';
import { useUnreadStore } from '../stores/unreadStore';
import { getWebSocketService, ConnectionState } from '../services/websocketService';
import {
  handleCallInvited,
  handleCallCanceled,
  handleCallDeclined,
  handleCallTimedOut,
} from '../services/voiceService/callStateMachine';
import { voiceService } from '../services/voiceService';
import type { WebSocketEvent } from '../types/ws-events';
import { e2eeService } from '../services/e2eeService';
import { isPendingKeyError } from '../services/e2eeErrors';
import { preferencesSyncService } from '../services/preferencesSync';
import { savedGifsSyncService } from '../services/savedGifsSync';
import { friendOrgSyncService } from '../services/friendOrgSync';
import { presenceOverrideSyncService } from '../services/presenceOverrideSync';
import { apiFetch } from '../services/apiClient';
import { useVoiceStore, channelVoiceMemberFromApi } from '../stores/voiceStore';
import { useConnectionStore } from '../stores/connectionStore';
import { useDMStore } from '../stores/dmStore';
import { useFriendStore } from '../stores/friendStore';
import { useFriendOrgStore } from '../stores/friendOrgStore';
import { useSubscriptionStore } from '../stores/subscriptionStore';
import { useRichPresenceStore } from '../stores/richPresenceStore';
import { speak as ttsSpeak } from '../services/ttsService';
import { notificationSoundService } from '../services/notificationSoundService';
import {
  isChannelMuted,
  isDMMuted,
  hasChannelMuteOverride,
} from '../stores/notificationPrefsStore';

/**
 * True if the user has Do Not Disturb on. DND suppresses ALL notification
 * surfacing (badges, sounds, desktop notifications, server-dot marking) —
 * the WebSocket events still arrive and the chat content still updates,
 * the client just doesn't draw attention to anything.
 *
 * Per issue #84, when DND turns off the client backfills unread counts for
 * the active server (the visible one) so the user sees the right state
 * without waiting for new traffic. That refetch is wired in App.tsx via a
 * status-transition effect, not here.
 */
function isDoNotDisturb(): boolean {
  return useMemberStore.getState().selfStatus === 'dnd';
}
import { indexMessage, removeMessage, removeScope } from '../services/searchService';
import { unwrapGifEnvelope } from '../utils/gifEnvelope';
import { formatMessagePreview } from '../utils/messagePreview';
import { summarizeWsServerError } from '../utils/wsDiagnostics';
import {
  desktopNotificationService,
  type NotificationType,
} from '../services/desktopNotificationService';

type ChannelMessagePayload = Extract<WebSocketEvent, { type: 'message' }>['data'];
type DMMessagePayload = Extract<WebSocketEvent, { type: 'dm_message' }>['data'];

// Debounce timers for re-fetching voice participants after join/leave events.
// Coalesces rapid bursts (e.g. 10 users joining) into a single API call.
const voiceRefetchTimers = new Map<string, ReturnType<typeof setTimeout>>();

/** TTS helper: speak message if it's in a voice-linked text channel and not from self */
function speakIfVoiceLinked(text: string, channelId: string, senderId: string, senderName: string) {
  const selfId = useUserStore.getState().user?.id;
  if (senderId === selfId) return;
  const ch = useChannelStore.getState().channels.find((c) => c.id === channelId);
  if (!ch?.linked_voice_channel_id) return;
  const voiceActiveId = useVoiceStore.getState().activeChannelId;
  if (voiceActiveId !== ch.linked_voice_channel_id) return;
  ttsSpeak(text, senderName);
}

function notificationPreviewBody(
  content: string,
  gifSlug: string | undefined,
  data: Pick<ChannelMessagePayload | DMMessagePayload, 'attachments'>
): string {
  return formatMessagePreview({
    content,
    gifSlug,
    attachments: data.attachments,
    fallback: '',
  });
}

function shouldSurfaceChannelNotification(data: ChannelMessagePayload, channelId: string): boolean {
  const selfId = useUserStore.getState().user?.id;
  return (
    data.user_id !== selfId &&
    !isChannelMuted(channelId, data.server_id ?? null) &&
    !isDoNotDisturb()
  );
}

function shouldSurfaceDMNotification(data: DMMessagePayload, conversationId: string): boolean {
  const selfId = useUserStore.getState().user?.id;
  return data.user_id !== selfId && !isDMMuted(conversationId) && !isDoNotDisturb();
}

function shouldShowChannelDesktopNotification(
  data: ChannelMessagePayload,
  channelId: string
): boolean {
  const notifType: NotificationType = data.mentioned === true ? 'mention' : 'message';
  const activeChannelId = useChannelStore.getState().activeChannelId;

  return desktopNotificationService.shouldNotify({
    type: notifType,
    isWindowFocused: document.hasFocus(),
    isActiveChannel: channelId === activeChannelId,
  });
}

function shouldShowDMDesktopNotification(conversationId: string): boolean {
  return desktopNotificationService.shouldNotify({
    type: 'dm',
    isWindowFocused: document.hasFocus(),
    isActiveChannel: conversationId === useDMStore.getState().activeConversationId,
  });
}

function notifyChannelMessagePreview(
  channelId: string,
  data: ChannelMessagePayload,
  content: string,
  gifSlug: string | undefined,
  shouldNotifyDesktop: boolean
): void {
  if (!shouldNotifyDesktop) return;

  const channelName = useChannelStore.getState().channels.find((c) => c.id === channelId)?.name;
  desktopNotificationService.notify({
    title: `${data.username || 'Unknown'} in #${channelName || 'channel'}`,
    senderDisplayName: data.username || 'Unknown',
    body: notificationPreviewBody(content, gifSlug, data),
    targetType: 'channel',
    targetId: channelId,
    serverId: data.server_id,
    senderId: data.user_id,
  });
  desktopNotificationService.incrementBadge();
}

function notifyDMMessagePreview(
  conversationId: string,
  data: DMMessagePayload,
  content: string,
  gifSlug: string | undefined,
  shouldNotifyDesktop: boolean
): void {
  if (!shouldNotifyDesktop) return;

  desktopNotificationService.notify({
    title: `DM from ${data.display_name || data.username || 'Unknown'}`,
    senderDisplayName: data.display_name || data.username || 'Unknown',
    body: notificationPreviewBody(content, gifSlug, data),
    targetType: 'dm',
    targetId: conversationId,
    senderId: data.user_id,
  });
  desktopNotificationService.incrementBadge();
}

// ── Extracted handlers to reduce cognitive complexity ─────────────────────

/** Lookup table for voice participant state updates (avoids nested switch). */
const VOICE_PARTICIPANT_UPDATES: Record<string, Record<string, boolean>> = {
  muted: { isMuted: true },
  unmuted: { isMuted: false },
  video_on: { isVideoOn: true },
  video_off: { isVideoOn: false },
  screen_on: { isScreenSharing: true },
  screen_off: { isScreenSharing: false },
  server_muted: { serverMuted: true },
  server_unmuted: { serverMuted: false },
  server_deafened: { serverDeafened: true, serverMuted: true },
  server_undeafened: { serverDeafened: false, serverMuted: false },
};

/** Returns true if a voice-linked text channel notification should be suppressed. */
function shouldSuppressLinkedTextNotification(channelId: string): boolean {
  const channel = useChannelStore.getState().channels.find((c) => c.id === channelId);
  if (!channel?.linked_voice_channel_id) return false;
  return useVoiceStore.getState().activeChannelId !== channel.linked_voice_channel_id;
}

/**
 * Mark a server and optionally its mention state as having unreads.
 *
 * `precise` records whether this mark was mute-resolved. Pass true ONLY when a
 * concrete channel gated the mark via `isChannelMuted` in `handleUnreadNotify`
 * (channel-wins resolution ran, so it already means "has an unmuted unread").
 * A `channel_id`-less `unread_notify` has had NO mute resolution — the guard at
 * the top of `handleUnreadNotify` is skipped when `channelId` is falsy — so it
 * must stay approximate and let ServerBar's server-level `!isMuted` gate apply.
 * Marking such a notify precise would light the dot on a server the user muted
 * at the server level (regression of the #84 / epic #1029 mute contract).
 *
 * `channelWins` records that the gating channel had its own mute override, so a
 * background server's precise mark survives a later server mute (the demote
 * sweep in useServerChannelSubscriptions skips channel-wins servers).
 */
function markServerUnreadWithMention(
  serverId: string,
  isMentioned: boolean,
  precise: boolean,
  channelWins = false
): void {
  const unreadStore = useUnreadStore.getState();
  unreadStore.markServerUnread(serverId, precise, channelWins);
  if (isMentioned) unreadStore.markServerMention(serverId);
}

/** Handle unread notification from server subscription. */
function handleUnreadNotify(msg: Extract<WebSocketEvent, { type: 'unread_notify' }>): void {
  const { data } = msg;
  // Schema (UnreadNotifySchema) already narrows both to `string | undefined`;
  // no `as` cast needed. See [internal]rules/frontend.md § WebSocket payload validation.
  const channelId = data.channel_id;
  const serverId = data.server_id;

  if (channelId && shouldSuppressLinkedTextNotification(channelId)) return;

  // DND check: when Do Not Disturb is on, drop every notification side
  // effect outright. Sits BEFORE the mute check because DND is the
  // broader-stroke override — it doesn't matter what the user's per-target
  // prefs are when they've explicitly asked for quiet.
  if (isDoNotDisturb()) return;

  // Mute check: a muted channel (or its parent server, when the channel has
  // no explicit override) must NOT increment unread counters, mark the
  // server as having unread, OR play a sound. The resolution order lives in
  // isChannelMuted — channel pref wins outright, server pref is the
  // fallback. Bail at the top so every downstream side-effect is skipped.
  if (channelId && isChannelMuted(channelId, serverId ?? null)) return;

  const isMentioned = data.mentioned === true;
  const activeServerId = useServerStore.getState().activeServerId;

  if (serverId && serverId !== activeServerId) {
    // Precise only when a concrete channel gated the mark above (the mute guard
    // at line ~227 runs solely for a present `channelId`). A `channel_id`-less
    // notify has had no mute resolution, so keep it approximate and let
    // ServerBar's server-level `!isMuted` gate suppress muted servers.
    //
    // channelWins: when that concrete channel carries its own mute override, the
    // not-muted verdict came from the channel-wins branch (not the server
    // fallback), so this precise mark must outlive a later server mute; the
    // background demote sweep leaves channel-wins servers alone.
    const channelWins = channelId ? hasChannelMuteOverride(channelId) : false;
    markServerUnreadWithMention(serverId, isMentioned, Boolean(channelId), channelWins);
    return;
  }

  if (!channelId) return;
  const activeChannelId = useChannelStore.getState().activeChannelId;
  if (channelId === activeChannelId) return;

  useUnreadStore.getState().incrementUnread(channelId);
  if (isMentioned) useUnreadStore.getState().incrementMention(channelId);
  // `channelId` is guaranteed present here (guarded above), so the mute
  // resolution ran — this mark is precise.
  if (serverId) markServerUnreadWithMention(serverId, isMentioned, true);

  // Notification sound for unfocused-channel messages
  if (isMentioned) {
    notificationSoundService.play('mention');
  } else {
    notificationSoundService.play('message');
  }
}

/** Context bag for voice membership change handling — reduces parameter count. */
interface VoiceMembershipContext {
  channelId: string;
  action: string;
  userId: string | undefined;
  serverId: string | undefined;
  username: string;
  displayName: string | undefined;
  localUserId: string | undefined;
  isLocalUserInChannel: boolean;
  voiceStore: ReturnType<typeof useVoiceStore.getState>;
}

/** Play join/leave notification sound when a remote user enters or exits the local channel. */
function playVoicePresenceSound(
  action: string,
  userId: string | undefined,
  localUserId: string | undefined,
  isLocalUserInChannel: boolean
): void {
  if (!isLocalUserInChannel || !userId || userId === localUserId) return;
  notificationSoundService.play(action === 'joined' ? 'user-join' : 'user-leave');
}

/** Handle joined/left/room_empty — sidebar member list + sounds + debounced refetch. */
function handleVoiceMembershipChange(ctx: VoiceMembershipContext): void {
  const { action, channelId, userId, serverId, voiceStore, isLocalUserInChannel, localUserId } =
    ctx;

  if (action === 'joined' && userId) {
    voiceStore.addChannelVoiceMember(channelId, {
      userId,
      username: ctx.username,
      displayName: ctx.displayName,
      isMuted: false,
      isDeafened: false,
      serverMuted: false,
      serverDeafened: false,
    });
    if (serverId) voiceStore.incrementServerVoiceCount(serverId);
    playVoicePresenceSound(action, userId, localUserId, isLocalUserInChannel);
  } else if (action === 'left' && userId) {
    voiceStore.removeChannelVoiceMember(channelId, userId);
    if (serverId) voiceStore.decrementServerVoiceCount(serverId);
    playVoicePresenceSound(action, userId, localUserId, isLocalUserInChannel);
  } else if (action === 'room_empty') {
    voiceStore.setChannelVoiceMembers(channelId, []);
  }

  // Debounced re-fetch for authoritative state
  if (action === 'joined' || action === 'left') {
    scheduleVoiceRefetch(channelId, voiceStore);
  }
}

/** Enforcement actions: server_muted / server_unmuted / server_deafened / server_undeafened. */
const ENFORCEMENT_ACTIONS = new Set([
  'server_muted',
  'server_unmuted',
  'server_deafened',
  'server_undeafened',
]);

/** Toast messages for enforcement actions targeting the local user. */
const ENFORCEMENT_TOAST_MESSAGES: Record<string, string> = {
  server_muted: 'You have been server-muted by a moderator',
  server_unmuted: 'A moderator has removed your server mute',
  server_deafened: 'You have been server-deafened by a moderator',
  server_undeafened: 'A moderator has removed your server deafen',
};

/** Handle server mute/deafen enforcement — update sidebar member + notify local user. */
function handleVoiceEnforcementAction(
  action: string,
  channelId: string,
  userId: string,
  localUserId: string | undefined,
  voiceStore: ReturnType<typeof useVoiceStore.getState>
): void {
  const update = VOICE_PARTICIPANT_UPDATES[action];
  if (update) {
    voiceStore.updateChannelVoiceMember(channelId, userId, update);
  }

  // Toast notification when local user is targeted
  if (userId === localUserId) {
    const message = ENFORCEMENT_TOAST_MESSAGES[action];
    if (message) {
      console.debug('[Voice]', message);
    }
  }
}

/** Update detailed participants when the active channel matches — extracted for complexity. */
function updateActiveParticipant(
  voiceStore: ReturnType<typeof useVoiceStore.getState>,
  channelId: string,
  action: string,
  userId: string | undefined
): void {
  if (voiceStore.activeChannelId !== channelId || !userId) return;
  const update = VOICE_PARTICIPANT_UPDATES[action];
  if (update) voiceStore.updateParticipant(userId, update);
}

/** Handle voice state updates — sidebar members + active participants. */
function handleVoiceStateUpdate(
  msg: Extract<WebSocketEvent, { type: 'voice_state_update' }>
): void {
  const { data } = msg;
  // channel_id + action are schema-required (UUID + VoiceActionSchema enum);
  // user_id / server_id / username / display_name are schema-optional and
  // already narrow to `string | undefined`. All `as` casts from the pre-schema
  // era are gone — see [internal]rules/frontend.md § WebSocket payload validation.
  const channelId = data.channel_id;
  const action = data.action;

  const userId = data.user_id;
  const serverId = data.server_id;
  const voiceStore = useVoiceStore.getState();
  const localUserId = useUserStore.getState().user?.id;

  // Update channel voice members (sidebar display)
  handleVoiceMembershipChange({
    action,
    channelId,
    userId,
    serverId,
    // Use `||` (not `??`) so empty strings also fall back to 'Unknown' —
    // `VoiceStateUpdateSchema` permits `username: z.string().optional()` with
    // no `.min(1)`, so a server-emitted empty string is schema-valid and would
    // otherwise display as a blank participant name. (PR #1184 review.)
    username: data.username || 'Unknown',
    displayName: data.display_name,
    localUserId,
    isLocalUserInChannel: voiceStore.activeChannelId === channelId,
    voiceStore,
  });

  // Update detailed participants for the active channel
  updateActiveParticipant(voiceStore, channelId, action, userId);

  // Update sidebar voice members for enforcement changes
  if (ENFORCEMENT_ACTIONS.has(action) && userId) {
    handleVoiceEnforcementAction(action, channelId, userId, localUserId, voiceStore);
  }
}

/**
 * Handle a directed `voice_move` signal (#487 Scope B). The server has already
 * granted any temporary SBAC override on the destination channel BEFORE
 * sending this signal (grant-before-signal ordering), so the client is clear
 * to leave its current voice channel and join the target. The cooperative
 * leave+rejoin (D2) lets the existing AuthorizeJoin / Socket.IO / produce-
 * consume flow handle the actual media transition.
 *
 * `joinChannel` already leaves the active channel internally, but we leave
 * explicitly first so the sidebar updates promptly and the intent is clear.
 */
function handleVoiceMove(msg: Extract<WebSocketEvent, { type: 'voice_move' }>): void {
  const { to_channel_id: toChannelId } = msg.data;

  // Sequence leave → join. Both are async; we chain so the join authorizes
  // only after the leave's cleanup settles. Errors are logged, not thrown —
  // a failed move must not crash the WS dispatch loop.
  voiceService
    .leaveChannel()
    .then(() => voiceService.joinChannel(toChannelId))
    .catch((err: unknown) => {
      // Inline the message so the last console.error arg is not a bare
      // identifier (no-restricted-syntax raw-err guard, observability.md).
      console.error(
        '[WebSocket] voice_move handling failed:',
        err instanceof Error ? err.message : 'voice_move_failed'
      );
    });
}

/**
 * Handle a directed `channel_access_revoked` signal (#487 P4). A temporary
 * channel grant the user held was revoked server-side. Converge the client
 * to the post-revoke state: drop the channel from the sidebar, purge its
 * cached messages, invalidate the cached E2EE channel key (the server rotates
 * the CSK so post-visit traffic is undecryptable), and leave voice if the
 * user is currently connected to that channel.
 */
function handleChannelAccessRevoked(
  msg: Extract<WebSocketEvent, { type: 'channel_access_revoked' }>
): void {
  const { channel_id: channelId } = msg.data;

  // removeChannel also fences pending E2EE generations before purging the
  // search scope. All payload fields are schema-required here.
  useChannelStore.getState().removeChannel(channelId);

  // Purge cached messages for the now-inaccessible channel.
  useChatStore.getState().clearMessages(channelId);

  // If currently in this channel's voice, leave it (the server also
  // force-disconnects the live peer via voice.enforce.disconnect; this is the
  // client-side convergence so local state isn't stranded).
  if (useVoiceStore.getState().activeChannelId === channelId) {
    voiceService.leaveChannel().catch((err: unknown) => {
      // Inline message — see voice_move handler note re no-restricted-syntax.
      console.error(
        '[WebSocket] channel_access_revoked leave failed:',
        err instanceof Error ? err.message : 'leave_on_revoke_failed'
      );
    });
  }
}

/** Schedule a debounced re-fetch of voice participants for a channel. */
function scheduleVoiceRefetch(
  channelId: string,
  voiceStore: ReturnType<typeof useVoiceStore.getState>
): void {
  const existing = voiceRefetchTimers.get(channelId);
  if (existing) clearTimeout(existing);
  voiceRefetchTimers.set(
    channelId,
    setTimeout(async () => {
      voiceRefetchTimers.delete(channelId);
      try {
        const res = await apiFetch(`/api/v1/channels/${channelId}/voice/participants`);
        if (!res.ok) return;
        const json = await res.json();
        const members = (json.participants || []).map(channelVoiceMemberFromApi);
        voiceStore.setChannelVoiceMembers(channelId, members);
      } catch {
        /* non-critical */
      }
    }, 2000)
  );
}

interface DecryptedMessageContent {
  content: string;
  gif_slug?: string;
  key_version?: number;
  decryptFailed: boolean;
  pendingKeys: boolean;
}

interface MessageContentOperationOwner {
  active: boolean;
  nextToken: number;
  latestOperations: Map<string, number>;
}

const MESSAGE_CONTENT_OPERATION_KINDS = ['message-content', 'reply-decrypt'] as const;
type MessageContentOperationKind = (typeof MESSAGE_CONTENT_OPERATION_KINDS)[number];

interface MessageContentOperation {
  owner: MessageContentOperationOwner;
  messageKey: string;
  token: number;
}

function createMessageContentOperationOwner(): MessageContentOperationOwner {
  return { active: true, nextToken: 0, latestOperations: new Map() };
}

function messageContentOperationKey(
  channelId: string,
  messageId: string,
  kind: MessageContentOperationKind
): string {
  return `${channelId}\u0000${messageId}\u0000${kind}`;
}

function messageContentPlaceholderKey(channelId: string, messageId: string): string {
  return `${channelId}\u0000${messageId}`;
}

function beginMessageContentOperation(
  owner: MessageContentOperationOwner,
  channelId: string,
  messageId: string,
  kind: MessageContentOperationKind
): MessageContentOperation {
  const messageKey = messageContentOperationKey(channelId, messageId, kind);
  const token = ++owner.nextToken;
  owner.latestOperations.set(messageKey, token);
  return { owner, messageKey, token };
}

function invalidateMessageContentOperation(
  owner: MessageContentOperationOwner,
  channelId: string,
  messageId: string
): void {
  for (const kind of MESSAGE_CONTENT_OPERATION_KINDS) {
    owner.latestOperations.delete(messageContentOperationKey(channelId, messageId, kind));
  }
}

/**
 * Invalidate every in-flight message-content operation for a whole scope.
 *
 * A purge deletes an unbounded set of messages and the event carries no IDs, so
 * the per-message invalidator cannot be used. Keys are
 * `${channelId}\u0000${messageId}\u0000${kind}`, and the NUL separator cannot
 * appear in a UUID, so a prefix match is exact.
 *
 * This is the #1741 content-invariant lane: a live deletion must never be
 * restorable by a decrypt continuation that started before the purge landed.
 */
function invalidateMessageContentOperationsForScope(
  owner: MessageContentOperationOwner,
  scopeId: string
): void {
  const prefix = `${scopeId}\u0000`;
  for (const key of owner.latestOperations.keys()) {
    if (key.startsWith(prefix)) {
      owner.latestOperations.delete(key);
    }
  }
}

function deactivateMessageContentOperationOwner(owner: MessageContentOperationOwner): void {
  owner.active = false;
  owner.latestOperations.clear();
}

function claimMessageContentOperation(operation?: MessageContentOperation): boolean {
  if (!operation) return true;
  if (!operation.owner.active) return false;
  if (operation.owner.latestOperations.get(operation.messageKey) !== operation.token) return false;
  operation.owner.latestOperations.delete(operation.messageKey);
  return true;
}

interface DecryptAndStoreMessageContentOptions {
  channelId: string;
  messageId: string;
  ciphertext: string;
  keyVersion: number | undefined;
  storeContent: (content: DecryptedMessageContent) => boolean;
  operation?: MessageContentOperation;
  onPlaintext?: (text: string, gifSlug?: string) => void;
  onDecryptFailed?: () => void;
  onSettled?: () => void;
}

function decryptAndStoreMessageContent({
  channelId,
  messageId,
  ciphertext,
  keyVersion,
  storeContent,
  operation,
  onPlaintext,
  onDecryptFailed,
  onSettled,
}: DecryptAndStoreMessageContentOptions): void {
  if (!e2eeService.isInitialized) {
    if (!claimMessageContentOperation(operation)) return;
    onSettled?.();
    if (
      !storeContent({
        content: '',
        gif_slug: undefined,
        key_version: keyVersion,
        decryptFailed: true,
        pendingKeys: false,
      })
    ) {
      removeMessage(messageId);
      return;
    }
    removeMessage(messageId);
    onDecryptFailed?.();
    return;
  }

  const decryptPromise =
    keyVersion && keyVersion > 1
      ? e2eeService.decryptForChannelWithVersion(channelId, ciphertext, keyVersion)
      : e2eeService.decryptForChannel(channelId, ciphertext);

  decryptPromise
    .then((plaintext) => {
      if (!claimMessageContentOperation(operation)) return;
      onSettled?.();
      // E2EE GIF messages are encrypted as JSON: {"text":"...","gif_slug":"..."}
      const { text: content, gifSlug } = unwrapGifEnvelope(plaintext);
      if (
        !storeContent({
          content,
          gif_slug: gifSlug,
          key_version: keyVersion,
          decryptFailed: false,
          pendingKeys: false,
        })
      ) {
        removeMessage(messageId);
        return;
      }
      // Passively index decrypted content for search
      if (content) {
        indexMessage(messageId, content, channelId);
      } else {
        removeMessage(messageId);
      }
      onPlaintext?.(content, gifSlug);
    })
    .catch((err) => {
      if (!claimMessageContentOperation(operation)) return;
      onSettled?.();
      const isPending = isPendingKeyError(err);
      if (
        !storeContent({
          content: '',
          gif_slug: undefined,
          key_version: keyVersion,
          decryptFailed: !isPending,
          pendingKeys: isPending,
        })
      ) {
        removeMessage(messageId);
        return;
      }
      removeMessage(messageId);
      onDecryptFailed?.();
    });
}

type DMCallSoundState = { backgroundRingtoneConversationId: string | null };

interface DMCallSoundContext {
  conversationId: string;
  userId: string | undefined;
  soundState: DMCallSoundState;
  voiceState: ReturnType<typeof useVoiceStore.getState>;
  isInThisCall: boolean;
  isMatchingCall: boolean;
  ownsBackgroundRingtone: boolean;
  ownsAudibleBackgroundRingtone: boolean;
}

function handleJoinedDMCallSound(context: DMCallSoundContext): void {
  const { conversationId, soundState, voiceState, isInThisCall } = context;
  if (isInThisCall) {
    // Other party answered our call — stop ringback, play connected.
    notificationSoundService.stopAllLoops();
    notificationSoundService.play('call-connected');
    soundState.backgroundRingtoneConversationId = null;
    return;
  }
  if (voiceState.callState.kind !== 'idle') return;

  // Only ring for the initial join. This runs before applyDMVoiceState, so a
  // non-empty roster means the group call was already active.
  const alreadyActive = (voiceState.activeDMCalls[conversationId]?.participantIds.length ?? 0) > 0;
  if (alreadyActive || soundState.backgroundRingtoneConversationId !== null) return;
  soundState.backgroundRingtoneConversationId = conversationId;
  notificationSoundService.playLoop('call-ringing');
}

function handleLeftDMCallSound(context: DMCallSoundContext): void {
  const {
    conversationId,
    userId,
    soundState,
    voiceState,
    isMatchingCall,
    ownsBackgroundRingtone,
    ownsAudibleBackgroundRingtone,
  } = context;
  // This handler runs before applyDMVoiceState mutates the roster. A member
  // leaving a group call is not terminal while anyone else remains.
  const participantIds = voiceState.activeDMCalls[conversationId]?.participantIds ?? [];
  const hasRemainingParticipant =
    userId !== undefined && participantIds.some((participantId) => participantId !== userId);
  if (hasRemainingParticipant) return;

  if (
    (isMatchingCall || ownsAudibleBackgroundRingtone) &&
    notificationSoundService.isLooping('call-ringing')
  ) {
    notificationSoundService.stopLoop('call-ringing');
    notificationSoundService.play('call-declined');
  }
  if (ownsBackgroundRingtone) soundState.backgroundRingtoneConversationId = null;
}

function handleEmptyDMCallSound(context: DMCallSoundContext): void {
  const { soundState, isMatchingCall, ownsBackgroundRingtone, ownsAudibleBackgroundRingtone } =
    context;
  // A background call ending must not silence another conversation's ring.
  if (isMatchingCall) {
    notificationSoundService.stopAllLoops();
  } else if (ownsAudibleBackgroundRingtone) {
    notificationSoundService.stopLoop('call-ringing');
  }
  if (ownsBackgroundRingtone) soundState.backgroundRingtoneConversationId = null;
}

/** Handle DM call sound notifications based on voice state changes. */
function handleDMCallSounds(
  conversationId: string,
  action: string,
  userId: string | undefined,
  soundState: DMCallSoundState
): void {
  const voiceState = useVoiceStore.getState();
  if (userId === useUserStore.getState().user?.id) return;

  const isInThisCall = voiceState.isDMCall && voiceState.dmConversationId === conversationId;
  const callState = voiceState.callState;
  const isMatchingRingingConversation =
    (callState.kind === 'outgoing-ringing' ||
      callState.kind === 'incoming-ringing' ||
      callState.kind === 'joining') &&
    callState.conversationId === conversationId;
  const ownsBackgroundRingtone = soundState.backgroundRingtoneConversationId === conversationId;
  const context: DMCallSoundContext = {
    conversationId,
    userId,
    soundState,
    voiceState,
    isInThisCall,
    isMatchingCall: isInThisCall || isMatchingRingingConversation,
    ownsBackgroundRingtone,
    ownsAudibleBackgroundRingtone: ownsBackgroundRingtone && callState.kind === 'idle',
  };

  if (action === 'joined') handleJoinedDMCallSound(context);
  if (action === 'left') handleLeftDMCallSound(context);
  if (action === 'room_empty') handleEmptyDMCallSound(context);
}

function refreshDMConversationPreviews(): void {
  useDMStore
    .getState()
    .fetchConversations()
    .catch(() => console.error('[DM] Unexpected conversation-preview refresh failure'));
}

export function useWebSocketMessages(wsService: ReturnType<typeof getWebSocketService>) {
  const addMessage = useChatStore((s) => s.addMessage);
  const updateMessage = useChatStore((s) => s.updateMessage);
  const updateMessageStatus = useChatStore((s) => s.updateMessageStatus);
  const updateUserInMessages = useChatStore((s) => s.updateUserInMessages);
  const deleteMessage = useChatStore((s) => s.deleteMessage);
  const setTyping = useChatStore((s) => s.setTyping);

  useEffect(() => {
    const dmCallSoundState: { backgroundRingtoneConversationId: string | null } = {
      backgroundRingtoneConversationId: null,
    };
    const unsubscribeDMCallSoundState = useVoiceStore.subscribe((state) => {
      const owner = dmCallSoundState.backgroundRingtoneConversationId;
      const callState = state.callState;
      if (owner === null || callState.kind === 'idle') return;
      const callOwnsBackgroundRingtone =
        (callState.kind === 'outgoing-ringing' ||
          callState.kind === 'incoming-ringing' ||
          callState.kind === 'joining') &&
        callState.conversationId === owner;
      if (callOwnsBackgroundRingtone) return;

      notificationSoundService.stopLoop('call-ringing');
      dmCallSoundState.backgroundRingtoneConversationId = null;
    });
    const messageContentOperationOwner = createMessageContentOperationOwner();
    const pendingMessagePlaceholders = new Map<string, { channelId: string; messageId: string }>();
    const settleMessagePlaceholder = (channelId: string, messageId: string) =>
      pendingMessagePlaceholders.delete(messageContentPlaceholderKey(channelId, messageId));
    /**
     * Settle every placeholder for a whole scope.
     *
     * A purge carries no message IDs, so the per-message settler cannot be
     * used. Placeholder keys are `${channelId}\u0000${messageId}` — the same
     * NUL shape as the operation keys — so a prefix match is exact. Without
     * this the scope invalidator makes the decrypt continuation return before
     * `onSettled`, orphaning the row until unmount (#1741 names placeholders
     * explicitly).
     */
    const settleMessagePlaceholdersForScope = (scopeId: string) => {
      const prefix = `${scopeId}\u0000`;
      for (const key of pendingMessagePlaceholders.keys()) {
        if (key.startsWith(prefix)) {
          pendingMessagePlaceholders.delete(key);
        }
      }
    };
    const storeMessageContent = (
      channelId: string,
      messageId: string,
      content: DecryptedMessageContent
    ): boolean => {
      const messageExists = useChatStore
        .getState()
        .messagesByChannel.get(channelId)
        ?.some((message) => message.id === messageId);
      if (!messageExists) return false;
      updateMessage(channelId, messageId, content);
      return true;
    };

    // Message handler
    const unsubMessage = wsService.on('message', (msg) => {
      // msg.data is narrowed to MessagePayload (validated at the dispatch
      // boundary via zod — see services/websocketService.ts handleMessage).
      // channel_id and user_id are schema-required UUIDs, so the PR #704
      // hand-written runtime guards are now structurally guaranteed.
      const data = msg.data;
      const channelId = data.channel_id;
      const userId = data.user_id;
      if (!channelId || !userId) return;

      const keyVersion = data.key_version;
      // Wire types allow `string | null` on nullable fields (e.g.
      // display_name) where downstream Zustand types use `string | undefined`.
      // Normalize null → undefined at this boundary so the store stays
      // null-free.
      const baseMessage = {
        id: data.id || crypto.randomUUID(),
        channel_id: channelId,
        user_id: userId,
        username: data.username || 'Unknown',
        display_name: data.display_name ?? undefined,
        avatar_url: data.avatar_url ?? undefined,
        key_version: keyVersion,
        reply_to_id: data.reply_to_id || undefined,
        replied_to: data.replied_to
          ? {
              ...data.replied_to,
              display_name: data.replied_to.display_name ?? undefined,
            }
          : undefined,
        attachments: data.attachments || undefined,
        created_at: data.created_at || new Date().toISOString(),
        updated_at: data.updated_at || new Date().toISOString(),
      };

      const shouldSurfaceNotification = shouldSurfaceChannelNotification(data, channelId);
      const shouldNotifyDesktop =
        shouldSurfaceNotification && shouldShowChannelDesktopNotification(data, channelId);

      if (shouldSurfaceNotification) {
        notificationSoundService.play('message', { focused: true });
      }

      const maybeSpeakTTS = (text: string) =>
        speakIfVoiceLinked(text, channelId, userId, data.display_name || data.username || '');

      // Insert a fail-closed row before decrypting so an edit received while the
      // original decrypt is pending has an existing message to update.
      const hasExistingMessage = useChatStore
        .getState()
        .messagesByChannel.get(channelId)
        ?.some((message) => message.id === baseMessage.id);
      if (!hasExistingMessage) {
        addMessage(channelId, {
          ...baseMessage,
          content: '',
          replied_to: baseMessage.replied_to
            ? { ...baseMessage.replied_to, content: '' }
            : undefined,
          decryptFailed: false,
          pendingKeys: false,
        });
        pendingMessagePlaceholders.set(messageContentPlaceholderKey(channelId, baseMessage.id), {
          channelId,
          messageId: baseMessage.id,
        });
      }
      decryptAndStoreMessageContent({
        channelId,
        messageId: baseMessage.id,
        ciphertext: data.content ?? '',
        keyVersion,
        storeContent: (content) => storeMessageContent(channelId, baseMessage.id, content),
        operation: beginMessageContentOperation(
          messageContentOperationOwner,
          channelId,
          baseMessage.id,
          'message-content'
        ),
        onPlaintext: (text, gifSlug) => {
          maybeSpeakTTS(text);
          notifyChannelMessagePreview(channelId, data, text, gifSlug, shouldNotifyDesktop);
        },
        onDecryptFailed: () =>
          notifyChannelMessagePreview(channelId, data, '', undefined, shouldNotifyDesktop),
        onSettled: () => settleMessagePlaceholder(channelId, baseMessage.id),
      });

      // Decrypt replied_to content. `rt` is the normalized form from
      // baseMessage (display_name already coerced null → undefined), so
      // the spread below preserves the store-compatible shape.
      const rt = baseMessage.replied_to;
      if (rt?.content && e2eeService.isInitialized) {
        const replyOperation = beginMessageContentOperation(
          messageContentOperationOwner,
          channelId,
          baseMessage.id,
          'reply-decrypt'
        );
        const rtKv = rt.key_version;
        const decryptFn =
          rtKv && rtKv > 1
            ? e2eeService.decryptForChannelWithVersion(channelId, rt.content, rtKv)
            : e2eeService.decryptForChannel(channelId, rt.content);
        decryptFn
          .then((plaintext) => {
            if (!claimMessageContentOperation(replyOperation)) return;
            updateMessage(channelId, baseMessage.id, {
              replied_to: { ...rt, content: plaintext },
            });
          })
          .catch(() => {
            claimMessageContentOperation(replyOperation);
            // Keep the reply preview fail-closed and blank; never render ciphertext.
          });
      }

      // NOTE: Unread tracking is handled exclusively by the 'unread_notify' handler
      // (server-level subscription). The 'message' event only arrives for the active
      // channel the user is subscribed to, so no unread increment is needed here.
    });

    // Message update handler — msg.data narrowed to MessageUpdatePayload.
    // edited_at is `string | null | undefined` at the wire; downstream state
    // wants `string | undefined`, so normalize at this boundary.
    const unsubUpdate = wsService.on('message_update', (msg) => {
      const data = msg.data;
      const metadata = {
        ...(data.edited_at !== undefined && { edited_at: data.edited_at ?? undefined }),
        updated_at: data.updated_at || data.edited_at || new Date().toISOString(),
        ...(data.embeds_suppressed !== undefined && {
          embeds_suppressed: data.embeds_suppressed,
        }),
      };

      updateMessage(data.channel_id, data.id, metadata);
      if (data.content === undefined) return;

      // An authoritative edit supersedes any indexed/backfilled plaintext
      // immediately. A successful decrypt below will re-index the new content.
      removeMessage(data.id);
      decryptAndStoreMessageContent({
        channelId: data.channel_id,
        messageId: data.id,
        ciphertext: data.content,
        keyVersion: data.key_version,
        storeContent: (content) => storeMessageContent(data.channel_id, data.id, content),
        operation: beginMessageContentOperation(
          messageContentOperationOwner,
          data.channel_id,
          data.id,
          'message-content'
        ),
        onSettled: () => settleMessagePlaceholder(data.channel_id, data.id),
      });
    });

    // Message delete handler — msg.data narrowed to MessageDeletePayload.
    const unsubDelete = wsService.on('message_delete', (msg) => {
      const data = msg.data;
      invalidateMessageContentOperation(messageContentOperationOwner, data.channel_id, data.id);
      settleMessagePlaceholder(data.channel_id, data.id);
      removeMessage(data.id);
      deleteMessage(data.channel_id, data.id);
    });

    // Bulk purge — an invalidation signal, never a diff. A whole-server purge emits
    // one channel_purged PER CHANNEL with deleted_count: 0, so the count is not
    // usable here; clear the scope and let the refetch establish truth.
    //
    // Ordering mirrors the canonical scope-loss pairing in channelStore /
    // dmStore: fence in-flight decrypts first, then drop searchable plaintext,
    // then clear the store. `removeScope` is load-bearing — it discards the
    // decrypted rows the local index holds, fences running backfills so one
    // cannot RE-index purged plaintext after the purge lands, and dismisses
    // copies already emitted into an open search pane.
    const clearScopeState = (scopeId: string) => {
      invalidateMessageContentOperationsForScope(messageContentOperationOwner, scopeId);
      settleMessagePlaceholdersForScope(scopeId);
      removeScope(scopeId);
      useChatStore.getState().clearMessages(scopeId);
    };

    // Every channel of one server the client knows about. `channels` holds the
    // current server's fetched list; `channelIdsByServer` retains ids seen for
    // other servers. Unioning both mirrors `removeServerChannels`, and the bound
    // is the correct one: the deletion is already authoritative server-side, and
    // a channel the client has never loaded holds no local plaintext to clear.
    const clearServerScopes = (serverId: string) => {
      const channelState = useChannelStore.getState();
      const scopeIds = new Set(channelState.channelIdsByServer[serverId] ?? []);
      for (const channel of channelState.channels) {
        if (channel.server_id === serverId) scopeIds.add(channel.id);
      }
      for (const scopeId of scopeIds) clearScopeState(scopeId);
    };

    // ONE clearing lane, two producers. The WebSocket echo is not sufficient on
    // its own: `channel_purged` reaches only subscribers of the purged channel —
    // in practice the one the client has mounted — so an actor who purges a
    // channel they are not currently viewing never receives their own purge, and
    // that channel's decrypted plaintext would stay searchable (#1741 forbids a
    // live deletion remaining reachable via the search path). `PurgeMessagesModal`
    // dispatches this same event locally on every terminal outcome, so the clear
    // belongs on the EVENT rather than inside each handler — that is what makes
    // the actor's own purge converge whether or not the echo reaches them.
    //
    // `bumpConversation` drops the conversation-list preview, a second copy of
    // decrypted plaintext held outside chatStore. It no-ops on an id it does not
    // know, so a channel scope passes through it harmlessly.
    //
    // Every listener here is synchronous, so the clear completes before any
    // refetch a sibling listener queues can observe the store.
    //
    // The detail's two fields carry DIFFERENT instructions and are not
    // alternatives: `scopeId` names the scope to REFETCH (null meaning "whatever
    // is mounted", which is how a server purge addresses a channel it cannot
    // name), while `serverId` names a server whose known channels must be
    // CLEARED. A server purge sets both.
    const handleMessagesPurged = (e: Event) => {
      const detail = (e as CustomEvent).detail as
        { scopeId?: string | null; serverId?: string } | undefined;
      if (detail?.serverId) clearServerScopes(detail.serverId);
      const scopeId = detail?.scopeId;
      if (typeof scopeId !== 'string') return;
      clearScopeState(scopeId);
      useDMStore.getState().bumpConversation(scopeId, null);
    };
    globalThis.addEventListener('messages-purged', handleMessagesPurged);

    const purgeScope = (scopeId: string) => {
      globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail: { scopeId } }));
    };

    const unsubChannelPurged = wsService.on('channel_purged', (msg) => {
      purgeScope(msg.data.channel_id);
    });

    // dmStore already routes conversation clears through chatStore.clearMessages,
    // so the DM scope uses the same primitive keyed by conversation_id.
    const unsubDmPurged = wsService.on('dm_purged', (msg) => {
      purgeScope(msg.data.conversation_id);
    });

    // Server-wide purge. `channel_purged` only reaches clients subscribed to that
    // one channel — i.e. the channel they have mounted — so on its own it would
    // leave every OTHER purged channel's decrypted plaintext in the local search
    // index, still reachable via multi-scope search.
    //
    // The echo dispatches rather than clearing directly, so the WS lane and the
    // actor's local lane run the SAME enumeration. That symmetry is load-bearing:
    // a server purge whose HTTP call fails is exactly the case where the echo is
    // least likely to arrive (a transport rejection means the WebSocket is
    // plausibly down too), so the actor's own dispatch must be able to clear the
    // server's scopes on its own.
    //
    // The scope stays null — `useMessageFetch` reads that as "refetch whatever is
    // mounted", which covers the mounted scope even when it was not enumerable,
    // instead of relying on the channel list being complete. A server id can
    // never equal a channel id, so a scoped dispatch could not address it anyway.
    const unsubServerPurged = wsService.on('server_purged', (msg) => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', {
          detail: { scopeId: null, serverId: msg.data.server_id },
        })
      );
    });

    // Reaction event handler (shared logic for added/removed)
    const applyReactionUpdate = (
      channelId: string,
      messageId: string,
      emoji: string,
      summary: {
        emoji: string;
        count: number;
        users: Array<{ user_id: string; username: string; display_name?: string }>;
        me: boolean;
      } | null
    ) => {
      const messages = useChatStore.getState().messagesByChannel.get(channelId);
      const message = messages?.find((m) => m.id === messageId);
      if (!message) return;

      const currentReactions = message.reactions || [];
      const selfId = useUserStore.getState().user?.id;
      let updatedReactions;

      if (summary && summary.count > 0) {
        const idx = currentReactions.findIndex((r) => r.emoji === emoji);
        updatedReactions = [...currentReactions];
        const withMe = { ...summary, me: summary.users.some((u) => u.user_id === selfId) };
        if (idx >= 0) {
          updatedReactions[idx] = withMe;
        } else {
          updatedReactions.push(withMe);
        }
      } else {
        updatedReactions = currentReactions.filter((r) => r.emoji !== emoji);
      }

      updateMessage(channelId, messageId, { reactions: updatedReactions });
    };

    // Reaction handlers — msg.data narrowed to MessageReaction{Added,Removed}Payload.
    const unsubReactionAdded = wsService.on('message_reaction_added', (msg) => {
      const data = msg.data;
      applyReactionUpdate(
        data.channel_id,
        data.message_id,
        data.emoji,
        data.reaction_summary ?? null
      );
    });

    const unsubReactionRemoved = wsService.on('message_reaction_removed', (msg) => {
      const data = msg.data;
      applyReactionUpdate(
        data.channel_id,
        data.message_id,
        data.emoji,
        data.reaction_summary ?? null
      );
    });

    // Pin event handlers — msg.data narrowed to MessagePinnedPayload / MessageUnpinnedPayload.
    const unsubMessagePinned = wsService.on('message_pinned', (msg) => {
      const data = msg.data;
      updateMessage(data.channel_id, data.message_id, {
        pinned_at: data.pinned_at,
        pinned_by: data.pinned_by,
      });
    });

    const unsubMessageUnpinned = wsService.on('message_unpinned', (msg) => {
      const data = msg.data;
      // channel_id is optional in the schema (DM path uses conversation_id);
      // the channel path of this handler bails out if absent. The DM path
      // is wired separately via the dm_message_delete/unpin flow.
      if (!data.channel_id) return;
      updateMessage(data.channel_id, data.message_id, {
        pinned_at: undefined,
        pinned_by: undefined,
      });
    });

    // Typing indicator handler — msg.data narrowed to TypingPayload.
    const unsubTyping = wsService.on('typing', (msg) => {
      const data = msg.data;
      setTyping(data.channel_id, data.user_id, data.is_typing, data.username ?? '');
    });

    // Message acknowledgment handler — msg.data narrowed to MessageAckPayload.
    // nonce is optional (the client-side correlation token); when absent, the
    // ack cannot be matched to a pending temp message so this is a no-op.
    const unsubAck = wsService.on('message_ack', (msg) => {
      const data = msg.data;
      if (data.nonce) {
        updateMessageStatus(data.channel_id, data.nonce, 'delivered', data.id);
      }
    });

    // Member joined — msg.data narrowed to MemberJoinedPayload.
    // server_id + user_id are schema-required.
    const unsubMemberJoined = wsService.on('member_joined', (msg) => {
      const data = msg.data;
      const activeServerId = useServerStore.getState().activeServerId;

      // Only update if the event is for the currently viewed server
      if (data.server_id === activeServerId) {
        // Schema types `role` permissively (z.string().optional()) but the
        // store interface narrows to the 3 known roles. Validate at the
        // boundary so an unexpected server-emitted role (e.g., a new role
        // added server-side before the client is updated) falls back to
        // 'member' instead of poisoning role-based UI/permission logic.
        // (PR #1184 review.)
        const KNOWN_ROLES = ['owner', 'admin', 'member'] as const;
        type KnownRole = (typeof KNOWN_ROLES)[number];
        const role: KnownRole = KNOWN_ROLES.includes(data.role as KnownRole)
          ? (data.role as KnownRole)
          : 'member';
        useMemberStore.getState().addMember({
          user_id: data.user_id,
          username: data.username || 'Unknown',
          display_name: data.display_name ?? undefined,
          avatar_url: data.avatar_url ?? undefined,
          role,
          joined_at: new Date().toISOString(),
          roles: [],
        });
      }
    });

    // Profile updated — msg.data narrowed to ProfileUpdatedPayload.
    // user_id + username are schema-required; nullable fields normalized
    // null → undefined at the store boundary.
    const unsubProfileUpdated = wsService.on('profile_updated', (msg) => {
      const data = msg.data;
      const userId = data.user_id;

      // Update all messages from this user across all channels
      updateUserInMessages(userId, {
        username: data.username,
        display_name: data.display_name ?? undefined,
        avatar_url: data.avatar_url ?? undefined,
      });

      // Update member list if applicable
      useMemberStore.getState().updateMemberProfile(userId, {
        username: data.username,
        display_name: data.display_name ?? undefined,
        avatar_url: data.avatar_url ?? undefined,
        header_image_url: data.header_image_url ?? undefined,
        color_scheme: data.color_scheme ?? undefined,
      });

      // Update DM participant profiles
      useDMStore.getState().updateParticipantProfile(userId, {
        username: data.username,
        displayName: data.display_name ?? undefined,
        avatarUrl: data.avatar_url ?? undefined,
        colorScheme: data.color_scheme ?? undefined,
      });

      // Update friend profiles
      useFriendStore.getState().updateFriendProfile(userId, {
        username: data.username,
        displayName: data.display_name ?? undefined,
        avatarUrl: data.avatar_url ?? undefined,
        colorScheme: data.color_scheme ?? undefined,
      });

      // Update voice participant (active call tiles)
      const voiceState = useVoiceStore.getState();
      if (voiceState.participants[userId]) {
        voiceState.updateParticipant(userId, {
          username: data.username,
          displayName: data.display_name ?? undefined,
          avatarUrl: data.avatar_url ?? undefined,
        });
      }

      // Update channel voice members (sidebar)
      for (const [channelId, members] of Object.entries(voiceState.channelVoiceMembers)) {
        const idx = members.findIndex((m) => m.userId === userId);
        if (idx !== -1) {
          const updated = [...members];
          updated[idx] = {
            ...updated[idx],
            username: data.username,
            displayName: data.display_name ?? undefined,
            avatarUrl: data.avatar_url ?? undefined,
          };
          voiceState.setChannelVoiceMembers(channelId, updated);
        }
      }

      // Update available screen shares
      if (voiceState.availableScreenShares.some((s) => s.userId === userId)) {
        useVoiceStore.setState({
          availableScreenShares: voiceState.availableScreenShares.map((s) =>
            s.userId === userId
              ? { ...s, username: data.username, displayName: data.display_name ?? undefined }
              : s
          ),
        });
      }
    });

    // Server updated — msg.data narrowed to ServerUpdatedPayload.
    // server_id is schema-required; remaining fields are optional (two
    // emit variants per schema comment).
    const unsubServerUpdated = wsService.on('server_updated', (msg) => {
      const data = msg.data;

      // Only include `allow_embedded_content` when the payload supplies a
      // boolean. One `server_updated` variant (services/control-plane/internal/
      // websocket/hub.go:2244-2252) omits the field entirely; defaulting it to
      // `false` would silently flip the user's setting off on that variant.
      useServerStore.getState().updateServer(data.server_id, {
        name: data.name,
        icon_url: data.icon_url ?? undefined,
        banner_url: data.banner_url ?? undefined,
        ...(typeof data.allow_embedded_content === 'boolean'
          ? { allow_embedded_content: data.allow_embedded_content }
          : {}),
      });
    });

    // Channel updated — msg.data narrowed to ChannelUpdatedPayload.
    // channel_id schema-required; `type` is z.string().optional() on the
    // wire but the store narrows to the 3 known channel kinds, so cast at
    // the boundary. Keep this in sync with `Channel.type` in types/chat.ts.
    // (PR #1184 review: previously omitted 'bulletin'.)
    const unsubChannelUpdated = wsService.on('channel_updated', (msg) => {
      const data = msg.data;
      useChannelStore.getState().updateChannel(data.channel_id, {
        name: data.name,
        type: data.type as 'text' | 'voice' | 'bulletin' | undefined,
        emoji: data.emoji ?? undefined,
        group_id: data.group_id ?? null,
      });
    });

    // Channel created — msg.data narrowed to ChannelCreatedPayload (envelope
    // required; channel object schema-validated at dispatch).
    // The wire shape (ChannelPayloadSchema) needs an `as Channel` cast
    // because the store type aliases that the renderer uses are richer
    // (e.g. derived UI state); the wire fields are a strict subset.
    const unsubChannelCreated = wsService.on('channel_created', (msg) => {
      const channel = msg.data.channel as unknown as Channel;
      const activeServerId = useServerStore.getState().activeServerId;
      if (channel.server_id === activeServerId) {
        useChannelStore.getState().addChannel(channel);
      }
    });

    // Channel deleted — msg.data narrowed to ChannelDeletedPayload.
    const unsubChannelDeleted = wsService.on('channel_deleted', (msg) => {
      useChannelStore.getState().removeChannel(msg.data.channel_id);
    });

    // Channel group created — msg.data narrowed to ChannelGroupCreatedPayload.
    const unsubGroupCreated = wsService.on('channel_group_created', (msg) => {
      const group = msg.data.channel_group as unknown as ChannelGroup;
      const activeServerId = useServerStore.getState().activeServerId;
      if (group.server_id === activeServerId) {
        useChannelStore.getState().addChannelGroup(group);
      }
    });

    // Channel group updated — msg.data narrowed to ChannelGroupUpdatedPayload.
    const unsubGroupUpdated = wsService.on('channel_group_updated', (msg) => {
      const group = msg.data.channel_group;
      useChannelStore.getState().updateChannelGroup(group.id, {
        name: group.name,
        position: group.position,
        updated_at: group.updated_at,
      });
    });

    // Channel group deleted — msg.data narrowed to ChannelGroupDeletedPayload.
    const unsubGroupDeleted = wsService.on('channel_group_deleted', (msg) => {
      useChannelStore.getState().removeChannelGroup(msg.data.group_id);
    });

    // Channels reordered — msg.data narrowed to ChannelsReorderedPayload.
    // channels array is schema-required, each entry validated against
    // ChannelReorderEntrySchema. No inline cast needed.
    const unsubChannelsReordered = wsService.on('channels_reordered', (msg) => {
      useChannelStore.getState().reorderChannels(
        msg.data.channels.map((c) => ({
          channel_id: c.channel_id,
          group_id: c.group_id,
          position: c.position,
        }))
      );
    });

    // Server deleted — msg.data narrowed to ServerDeletedPayload.
    const unsubServerDeleted = wsService.on('server_deleted', (msg) => {
      useServerStore.getState().removeServer(msg.data.server_id);
    });

    // Member removed — msg.data narrowed to MemberRemovedPayload.
    const unsubMemberRemoved = wsService.on('member_removed', (msg) => {
      const { server_id: serverId, user_id: removedUserId } = msg.data;

      const selfId = useUserStore.getState().user?.id;
      if (removedUserId === selfId) {
        // Current user was removed/left — purge server data entirely
        useServerStore.getState().removeServer(serverId);
      } else {
        // Another member was removed — update the member list
        const activeServerId = useServerStore.getState().activeServerId;
        if (serverId === activeServerId) {
          useMemberStore.getState().removeMember(removedUserId);
        }
      }
    });

    // Member timeout changed; msg.data narrowed to MemberTimeoutPayload.
    const unsubMemberTimeout = wsService.on('member_timeout', (msg) => {
      const { server_id: serverId, user_id: userId, timed_out_until: timedOutUntil } = msg.data;
      const activeServerId = useServerStore.getState().activeServerId;
      if (serverId === activeServerId) {
        useMemberStore.getState().setMemberTimeout(userId, timedOutUntil ?? null);
      }
    });

    // Lightweight unread notification handler — from server subscription
    const unsubUnreadNotify = wsService.on('unread_notify', (msg) => {
      handleUnreadNotify(msg);
    });

    // Key needed handler — auto-process pending key requests for new members
    const unsubKeyNeeded = wsService.on('key_needed', (msg) => {
      // PII/metadata-minimization (CWE-532): KeyNeededPayload carries server_id +
      // user_id + channel_ids, which together reveal key-distribution access
      // patterns. Log only the channel count — sufficient for debugging without
      // creating a per-user correlation record. See [internal]rules/observability.md.
      console.debug(
        '[WebSocket] key_needed event received, channel_count:',
        msg.data.channel_ids.length
      );
      if (e2eeService.isInitialized) {
        e2eeService.processPendingKeyRequests().catch((err) => {
          console.debug('[WebSocket] processPendingKeyRequests failed:', err);
        });
      } else {
        console.debug('[WebSocket] key_needed ignored — E2EE not initialized');
      }
    });

    // Key revocation — msg.data narrowed to KeyRevocationPayload. ALL
    // fields are .optional() in the schema (two emit shapes: server-channel
    // rotation vs friend-block). The handler is a no-op when channel_id
    // is absent (friend-block branch) — that's the intended structural
    // behavior, not a defensive guard.
    const unsubKeyRevocation = wsService.on('key_revocation', (msg) => {
      const data = msg.data;
      const channelId = data.channel_id;

      if (!channelId) return;
      console.debug(
        '[WebSocket] key_revocation:',
        channelId,
        'new_epoch:',
        data.new_epoch,
        'reason:',
        data.reason
      );

      // Invalidate cached key for this channel
      e2eeService.invalidateChannelKey(channelId);

      // Dispatch event for rotation coordinator
      globalThis.dispatchEvent(
        new CustomEvent('e2ee-key-rotation', {
          detail: {
            channelId,
            newEpoch: data.new_epoch,
            removedUserId: data.removed_user_id,
            reason: data.reason,
          },
        })
      );
    });

    // Key delivered — msg.data narrowed to KeyDeliveredPayload.
    // channel_id + user_id are schema-required UUIDs.
    const unsubKeyDelivered = wsService.on('key_delivered', (msg) => {
      const { channel_id: channelId, user_id: targetUserId } = msg.data;

      // Only act if this key delivery is for the current user
      const selfId = useUserStore.getState().user?.id;
      if (targetUserId !== selfId) return;

      // Invalidate the cached key so it gets re-fetched
      e2eeService.invalidateChannelKey(channelId);

      // Dispatch event so ChatView can re-fetch and decrypt messages
      globalThis.dispatchEvent(new CustomEvent('e2ee-key-delivered', { detail: { channelId } }));
    });

    // Preferences updated handler — another device pushed new preferences
    const unsubPreferencesUpdated = wsService.on('preferences_updated', () => {
      preferencesSyncService.fetchAndApply();
    });

    // Saved GIFs updated handler — another device saved/removed a GIF
    const unsubSavedGifsUpdated = wsService.on('saved_gifs_updated', () => {
      savedGifsSyncService.fetchAndApply();
    });

    // Friend organization updated — another device changed categories (#324)
    const unsubFriendOrgUpdated = wsService.on('friend_organization_updated', () => {
      friendOrgSyncService.fetchAndApply();
    });

    const unsubPresenceOverridesUpdated = wsService.on('presence_overrides_updated', (msg) => {
      void presenceOverrideSyncService.handleRemoteUpdate(msg.data.version).catch(() => {
        console.warn('[PresenceOverrideSync] remote-update refetch failed');
      });
    });

    // Entitlements changed (#1297) — server pushes the full capability set on
    // any tier change. The wire payload is validated at the dispatch boundary,
    // so update the store directly — no re-fetch round-trip.
    const unsubEntitlements = wsService.on('entitlements_changed', (msg) => {
      useSubscriptionStore.getState().setEntitlement(msg.data);
    });

    // Re-hydrate the entitlement set on every WS (re)connect so the capability
    // set converges after a reconnect — independent of whether a live
    // entitlements_changed push was delivered before the socket dropped (e.g. a
    // downgrade push racing the server's DisconnectUser). hydrate() fails closed
    // to the free floor, so a failed/raced re-hydrate never escalates. (#1297 /
    // Gitar review: hydratePostLogin only covers fresh login/SSO/restore.)
    const unsubEntitlementsResync = wsService.onConnectionChange((state) => {
      if (state === ConnectionState.CONNECTED) {
        useSubscriptionStore
          .getState()
          .hydrate()
          .catch((err) => {
            // hydrate() is internally fail-closed and never rejects; this handler
            // only satisfies no-floating-promises without the `void` operator (S3735).
            console.debug('entitlement re-hydrate on reconnect failed:', err);
          });
      }
    });

    // Presence snapshot — msg.data narrowed to PresenceSnapshotPayload.
    // Both `users` (enhanced) and `online_user_ids` (legacy back-compat)
    // are schema-optional; the handler prefers `users` when present.
    const unsubPresenceSnapshot = wsService.on('presence_snapshot', (msg) => {
      // A snapshot begins a fresh authoritative stream. Drop all previously
      // received custom text before the server replays the statuses this viewer
      // is still allowed to see. Otherwise an override applied while this
      // socket was being closed could leave revoked text cached indefinitely.
      // Preserve the self slice, which is owned by the settings REST flow.
      useRichPresenceStore.getState().clearAllCustomText();

      // #803: selfStatus is the source of truth MemberList/UserPopover read for
      // the self-user, but nothing reconciled it with the server — so a stale or
      // default value left the connected user showing Offline. The server's
      // snapshot includes self via resolveVisibleStatus, which is self-aware: the
      // self viewer always gets their REAL status (online/dnd/invisible), never
      // the broadcast-to-others 'offline'. So it is safe to adopt the self entry
      // here. The `presence` broadcast handler below intentionally still skips
      // self (that path carries raw status and is not self-aware).
      const selfId = useUserStore.getState().user?.id;
      if (msg.data.users) {
        const users = msg.data.users;
        useMemberStore.getState().setPresenceSnapshot(users);

        const selfEntry = selfId ? users.find((u) => u.user_id === selfId) : undefined;
        if (selfEntry) {
          useMemberStore.getState().setSelfStatus(selfEntry.status);
        }

        // Also update DM participant and friend presence
        const dmState = useDMStore.getState();
        const friendState = useFriendStore.getState();
        for (const u of users) {
          dmState.updateParticipantProfile(u.user_id, { status: u.status });
          friendState.updateFriendPresence(u.user_id, u.status);
        }
      } else if (msg.data.online_user_ids) {
        // Backward compatibility: simple list of online user IDs
        const userIds = msg.data.online_user_ids;
        useMemberStore.getState().setOnlineUsers(userIds);

        // #803: legacy snapshots carry no per-user status; presence in the
        // online list promotes a stale offline self → online (the #803 symptom)
        // but must NOT downgrade a deliberate dnd/invisible choice (mirrors
        // setOnlineUsers' preserve-non-offline rule). The current server always
        // sends users[]; this branch is back-compat only.
        if (
          selfId &&
          userIds.includes(selfId) &&
          useMemberStore.getState().selfStatus === 'offline'
        ) {
          useMemberStore.getState().setSelfStatus('online');
        }

        const dmState2 = useDMStore.getState();
        const friendState2 = useFriendStore.getState();
        for (const id of userIds) {
          dmState2.updateParticipantProfile(id, { status: 'online' });
          friendState2.updateFriendPresence(id, 'online');
        }
      }
    });

    // Presence change — msg.data narrowed to PresencePayload.
    // user_id (UUID) + status (PresenceStatusSchema enum) are schema-required.
    const unsubPresence = wsService.on('presence', (msg) => {
      const { user_id: userId, status, timestamp } = msg.data;

      // The server sends the real status to a user's own devices as the
      // acknowledgement of a successful status change. Other viewers receive
      // offline for Invisible, so never adopt an offline self broadcast.
      const selfId = useUserStore.getState().user?.id;
      if (userId === selfId) {
        if (status !== 'offline') {
          useMemberStore.getState().setSelfStatus(status);
        }
        return;
      }

      useMemberStore.getState().setUserStatus(userId, status);

      if (status === 'offline' && timestamp) {
        useMemberStore.getState().setUserLastSeen(userId, timestamp);
      }

      // Also update DM participant and friend presence
      useDMStore.getState().updateParticipantProfile(userId, { status });
      useFriendStore.getState().updateFriendPresence(userId, status);
    });

    // Server online counts handler — updates online_count on server objects after presence changes
    const unsubServerOnlineCounts = wsService.on('server_online_counts', (msg) => {
      // msg.data is narrowed to ServerOnlineCountsPayload via generic on()
      const counts = msg.data.counts;
      if (counts) {
        useServerStore.getState().updateOnlineCounts(counts);
      }
    });

    // Server voice counts handler — updates voice counts for tooltip display
    const unsubServerVoiceCounts = wsService.on('server_voice_counts', (msg) => {
      // msg.data is narrowed to ServerVoiceCountsPayload via generic on()
      const counts = msg.data.counts;
      if (counts) {
        useVoiceStore.getState().setServerVoiceCounts(counts);
      }
    });

    // Voice state update handler — from control plane via NATS
    const unsubVoiceState = wsService.on('voice_state_update', (msg) => {
      handleVoiceStateUpdate(msg);
    });

    // Voice move handler (#487) — directed signal: leave current VC, join target.
    const unsubVoiceMove = wsService.on('voice_move', (msg) => {
      handleVoiceMove(msg);
    });

    // Channel access revoked handler (#487 P4) — directed signal: purge the
    // channel from the sidebar + cached messages + cached key, leave voice if in it.
    const unsubChannelAccessRevoked = wsService.on('channel_access_revoked', (msg) => {
      handleChannelAccessRevoked(msg);
    });

    // ── DM event handlers ──────────────────────────────────────────────

    // DM message handler (mirrors 'message' handler for server channels)
    const unsubDMMessage = wsService.on('dm_message', (msg) => {
      // msg.data narrowed to DMMessagePayload — conversation_id + user_id
      // are schema-required UUIDs; the PR #704 runtime guards (length/typeof
      // checks) are now structurally guaranteed at the dispatch boundary.
      // channel_id is an optional back-compat alias; conversation_id is the
      // canonical key.
      const data = msg.data;
      const conversationId = data.conversation_id;
      if (!conversationId) return;

      const dmKeyVersion = data.key_version;
      const baseMessage = {
        id: data.id || crypto.randomUUID(),
        channel_id: conversationId,
        user_id: data.user_id,
        username: data.username || 'Unknown',
        display_name: data.display_name ?? undefined,
        avatar_url: data.avatar_url ?? undefined,
        key_version: dmKeyVersion,
        attachments: data.attachments || undefined,
        created_at: data.created_at || new Date().toISOString(),
        updated_at: data.updated_at || new Date().toISOString(),
      };

      // Update conversation list preview (keeps thread list in sync)
      const lastMessagePreview = {
        content: data.content || '',
        userId: data.user_id,
        username: data.username || 'Unknown',
        createdAt: data.created_at || new Date().toISOString(),
        ...(data.attachments?.length ? { attachmentType: data.attachments[0].file_type } : {}),
      };

      const selfId = useUserStore.getState().user?.id;
      const shouldSurfaceNotification = shouldSurfaceDMNotification(data, conversationId);
      const shouldNotifyDesktop =
        shouldSurfaceNotification && shouldShowDMDesktopNotification(conversationId);

      if (shouldSurfaceNotification) {
        notificationSoundService.play('dm', { focused: true });
      }

      // Keep the row present while decrypting so a newer edit can supersede
      // this operation without being dropped by updateMessage.
      const hasExistingMessage = useChatStore
        .getState()
        .messagesByChannel.get(conversationId)
        ?.some((message) => message.id === baseMessage.id);
      if (!hasExistingMessage) {
        addMessage(conversationId, {
          ...baseMessage,
          content: '',
          decryptFailed: false,
          pendingKeys: false,
        });
        pendingMessagePlaceholders.set(
          messageContentPlaceholderKey(conversationId, baseMessage.id),
          { channelId: conversationId, messageId: baseMessage.id }
        );
      }
      decryptAndStoreMessageContent({
        channelId: conversationId,
        messageId: baseMessage.id,
        ciphertext: data.content ?? '',
        keyVersion: dmKeyVersion,
        storeContent: (content) => storeMessageContent(conversationId, baseMessage.id, content),
        operation: beginMessageContentOperation(
          messageContentOperationOwner,
          conversationId,
          baseMessage.id,
          'message-content'
        ),
        onPlaintext: (text, gifSlug) =>
          notifyDMMessagePreview(conversationId, data, text, gifSlug, shouldNotifyDesktop),
        onDecryptFailed: () =>
          notifyDMMessagePreview(conversationId, data, '', undefined, shouldNotifyDesktop),
        onSettled: () => settleMessagePlaceholder(conversationId, baseMessage.id),
      });

      // Bump conversation to the top of the DM list with updated preview.
      // No-ops gracefully if the conversation isn't in state yet (initial-load race).
      // Only bump for messages from OTHER users — local sends already bumped optimistically.
      if (data.user_id !== selfId) {
        useDMStore.getState().bumpConversation(conversationId, lastMessagePreview);
      }
    });

    // DM message ack handler — msg.data narrowed to DMMessageAckPayload.
    // nonce, id, conversation_id are all schema-required.
    const unsubDMAck = wsService.on('dm_message_ack', (msg) => {
      const data = msg.data;
      updateMessageStatus(data.conversation_id, data.nonce, 'delivered', data.id);
    });

    // DM message update handler — msg.data narrowed to DMMessageUpdatePayload.
    const unsubDMUpdate = wsService.on('dm_message_update', (msg) => {
      const data = msg.data;
      const metadata = {
        ...(data.edited_at !== undefined && { edited_at: data.edited_at }),
        updated_at: data.updated_at || data.edited_at || new Date().toISOString(),
      };
      updateMessage(data.conversation_id, data.id, metadata);
      // Tombstone the prior plaintext before the asynchronous edit decrypt so
      // an older backfill cannot publish stale content in the interim.
      removeMessage(data.id);
      decryptAndStoreMessageContent({
        channelId: data.conversation_id,
        messageId: data.id,
        ciphertext: data.content,
        keyVersion: data.key_version,
        storeContent: (content) => storeMessageContent(data.conversation_id, data.id, content),
        operation: beginMessageContentOperation(
          messageContentOperationOwner,
          data.conversation_id,
          data.id,
          'message-content'
        ),
        onSettled: () => settleMessagePlaceholder(data.conversation_id, data.id),
      });
    });

    // DM message delete handler — msg.data narrowed to DMMessageDeletePayload.
    const unsubDMDelete = wsService.on('dm_message_delete', (msg) => {
      const data = msg.data;
      invalidateMessageContentOperation(
        messageContentOperationOwner,
        data.conversation_id,
        data.id
      );
      settleMessagePlaceholder(data.conversation_id, data.id);
      removeMessage(data.id);
      deleteMessage(data.conversation_id, data.id);
    });

    // DM typing indicator handler — msg.data narrowed to DMTypingPayload.
    const unsubDMTyping = wsService.on('dm_typing', (msg) => {
      const data = msg.data;
      setTyping(data.conversation_id, data.user_id, data.is_typing ?? false, data.username ?? '');
    });

    // DM unread notification — msg.data narrowed to DMUnreadNotifyPayload.
    // conversation_id is schema-required UUID; the runtime shape guard is
    // structural now.
    const unsubDMUnread = wsService.on('dm_unread_notify', (msg) => {
      const data = msg.data;
      const conversationId = data.conversation_id;
      const isDMMentioned = data.mentioned === true;

      // DND check: same rationale as in handleUnreadNotify — the broadest
      // suppression sits first.
      if (isDoNotDisturb()) return;

      // Mute check for DMs (#84 / epic #1029 close audit): a muted DM still
      // ACCUMULATES its hidden unread count so that unmuting later reveals
      // messages that arrived while muted. The ConversationList badge and the
      // ServerBar PM badge are render-gated on mute (nothing shows while muted),
      // and there is no unmute-time refetch to recover a dropped count — so
      // bailing here (the pre-audit behavior) permanently lost muted-window
      // unreads. Only the VISIBLE affordances are suppressed: the notification
      // sound, and the last-message preview advance (muting should make the
      // conversation feel quiet, not surface new content behind the user's
      // back — the original #84 intent).
      const muted = isDMMuted(conversationId);

      // Update last message preview if provided — skipped while muted per #84.
      if (!muted && data.last_message) {
        useDMStore.getState().updateLastMessage(conversationId, {
          content: data.last_message.content || '',
          userId: data.last_message.user_id || data.user_id || '',
          username: data.last_message.username || '',
          createdAt: data.last_message.created_at || new Date().toISOString(),
        });
      }

      if (conversationId === useDMStore.getState().activeConversationId) {
        useDMStore.getState().clearUnread(conversationId);
        useUnreadStore.getState().clearUnread(conversationId);
        return;
      }

      useDMStore.getState().incrementUnread(conversationId);
      if (isDMMentioned) {
        useUnreadStore.getState().incrementMention(conversationId);
      }

      // Notification sound for unfocused DM messages — silenced while muted.
      if (!muted) {
        notificationSoundService.play('dm');
      }
    });

    // DM conversation created — msg.data narrowed to DMConversationCreatedPayload.
    // The conversation envelope (id, is_group, created_at, participants) is
    // schema-validated at the dispatch boundary, so the per-field runtime
    // guards are structural now. Each participant in the array is
    // schema-narrowed to ParticipantSchema — no per-element type guard needed.
    const unsubDMConvCreated = wsService.on('dm_conversation_created', (msg) => {
      const conv = msg.data.conversation;
      // participants is schema-optional (server may omit on empty
      // conversations) — default to [] so map() is safe and the store gets
      // a normalized empty array rather than undefined.
      const participants = (conv.participants ?? []).map((p) => ({
        userId: p.user_id,
        username: p.username,
        displayName: p.display_name ?? undefined,
        avatarUrl: p.avatar_url ?? undefined,
        colorScheme: p.color_scheme ?? undefined,
        role: p.role,
      }));

      useDMStore.getState().addConversation({
        id: conv.id,
        isGroup: conv.is_group,
        isPersonal: conv.is_personal ?? false,
        name: conv.name ?? null,
        participants,
        iconUrl: conv.icon_url ?? undefined,
        createdBy: conv.created_by ?? undefined,
        lastMessage: null,
        unreadCount: 0,
        createdAt: conv.created_at,
      });
    });

    // DM participant added — msg.data narrowed to DMParticipantAddedPayload.
    // conversation_id + user_id schema-required; username/display_name/etc
    // are cast-only fields (server omits, handler tolerates absence).
    const unsubDMParticipantAdded = wsService.on('dm_participant_added', (msg) => {
      const data = msg.data;
      const conversationId = data.conversation_id;

      const dmStore = useDMStore.getState();
      const conv = dmStore.conversations.find((c) => c.id === conversationId);
      if (!conv) {
        console.debug(
          '[WebSocket] dm_participant_added: conversation not loaded, fetching',
          conversationId
        );
        dmStore.fetchConversations();
        return;
      }

      // Add participant locally if not already present
      if (!conv.participants.some((p) => p.userId === data.user_id)) {
        dmStore.updateConversation(conversationId, {
          participants: [
            ...conv.participants,
            {
              userId: data.user_id,
              username: data.username || 'Unknown',
              displayName: data.display_name ?? undefined,
              avatarUrl: data.avatar_url ?? undefined,
              colorScheme: data.color_scheme ?? undefined,
              role: 'member',
            },
          ],
        });
      }
    });

    // DM participant removed — msg.data narrowed to DMParticipantRemovedPayload.
    const unsubDMParticipantRemoved = wsService.on('dm_participant_removed', (msg) => {
      const { conversation_id: conversationId, user_id: removedUserId } = msg.data;

      // Check if we were the one removed
      const currentUser = useUserStore.getState().user;
      if (removedUserId === currentUser?.id) {
        useDMStore.getState().removeConversation(conversationId);
        return;
      }

      // Remove participant from conversation locally
      const dmStore = useDMStore.getState();
      const conv = dmStore.conversations.find((c) => c.id === conversationId);
      if (conv) {
        dmStore.updateConversation(conversationId, {
          participants: conv.participants.filter((p) => p.userId !== removedUserId),
        });
      }
    });

    // DM member role changed — msg.data narrowed to DMRoleChangedPayload.
    // role is z.enum(['admin', 'member']) so the cast is gone.
    const unsubDMRoleChanged = wsService.on('dm_role_changed', (msg) => {
      const { conversation_id: conversationId, user_id: userId, role } = msg.data;

      const dmStore = useDMStore.getState();
      const conv = dmStore.conversations.find((c) => c.id === conversationId);
      if (conv) {
        dmStore.updateConversation(conversationId, {
          participants: conv.participants.map((p) => (p.userId === userId ? { ...p, role } : p)),
        });
      }
    });

    // DM group deleted — msg.data narrowed to DMGroupDeletedPayload.
    const unsubDMGroupDeleted = wsService.on('dm_group_deleted', (msg) => {
      useDMStore.getState().removeConversation(msg.data.conversation_id);
    });

    // DM subscribed confirmation
    const unsubDMSubscribed = wsService.on('dm_subscribed', (msg) => {
      // msg.data is narrowed to DMSubscribedPayload via generic on()
      console.debug(
        '[WebSocket] Subscribed to DM:',
        msg.data.conversation_id || msg.data.channel_id
      );
    });

    // ── Friend event handlers ────────────────────────────────────────────

    // msg.data narrowed to FriendRequestPayload via the FriendRequestReceivedSchema.
    // id, from_user_id, from_username, created_at are schema-required.
    //
    // NOTE(#981): to_user_id and to_username are .optional() in the schema
    // pending the server-side fix that emits them. Until that lands, drop
    // records missing the to_* fields rather than polluting the request
    // list with one-sided records. Once #981 is fixed (server emits
    // to_* fields), tighten the schema to required and remove this guard.
    const unsubFriendRequestReceived = wsService.on('friend_request_received', (msg) => {
      const data = msg.data;

      // Play the friend-request sound up front (#1955). The event is delivered
      // over this authenticated user's socket, so a request genuinely arrived —
      // the sound must not depend on the to_* fields the server doesn't emit yet
      // (#981). The to_* guard below only suppresses the one-sided LIST record,
      // not the notification; keeping the sound here means it fires even while
      // the record is dropped. It still honors presence DND (DND suppresses ALL
      // notification surfacing — see isDoNotDisturb above).
      if (!isDoNotDisturb()) {
        notificationSoundService.play('friend-request');
      }

      if (!data.to_user_id || !data.to_username) {
        // PII-minimization (CWE-532): never log the full payload — it carries
        // from_username / from_display_name / from_avatar_url. Emit only the
        // request id + structural flags identifying which to_* fields were
        // missing. See [internal]rules/observability.md.
        console.warn('friend_request_received: missing to_* fields (see #981)', {
          id: data.id,
          has_to_user_id: !!data.to_user_id,
          has_to_username: !!data.to_username,
        });
        return;
      }
      useFriendStore.getState().addRequest({
        id: data.id,
        fromUserId: data.from_user_id,
        fromUsername: data.from_username,
        fromDisplayName: data.from_display_name ?? undefined,
        fromAvatarUrl: data.from_avatar_url ?? undefined,
        toUserId: data.to_user_id,
        toUsername: data.to_username,
        toDisplayName: data.to_display_name ?? undefined,
        toAvatarUrl: data.to_avatar_url ?? undefined,
        direction: 'received',
        createdAt: data.created_at,
      });
    });

    // msg.data narrowed to FriendRequestAcceptedPayload. Schema is permissive
    // (flat + nested forms both accepted). Handler branches on which form
    // arrived. Backend currently emits flat form.
    const unsubFriendRequestAccepted = wsService.on('friend_request_accepted', (msg) => {
      const data = msg.data;
      const store = useFriendStore.getState();

      // Remove the pending request (backend sends "id", not "request_id")
      const requestId = data.request_id || data.id;
      if (requestId) {
        store.removeRequest(requestId);
      }

      // Add the new friend — handle both nested and flat payload formats.
      if (data.friend) {
        store.addFriend({
          id: data.friend.id,
          userId: data.friend.user_id,
          username: data.friend.username,
          displayName: data.friend.display_name ?? undefined,
          avatarUrl: data.friend.avatar_url ?? undefined,
          colorScheme: data.friend.color_scheme ?? undefined,
          status: data.friend.status || 'offline',
        });
      } else if (data.user_id && data.username) {
        store.addFriend({
          id: requestId || '',
          userId: data.user_id,
          username: data.username,
          displayName: data.display_name ?? undefined,
          avatarUrl: data.avatar_url ?? undefined,
          colorScheme: data.color_scheme ?? undefined,
          status: 'online',
        });
      }
    });

    // msg.data narrowed to FriendRemovedPayload — user_id is schema-required.
    const unsubFriendRemoved = wsService.on('friend_removed', (msg) => {
      useFriendStore.getState().removeFriendByUserId(msg.data.user_id);
      // Prune the now-removed friend from any friend-org category (#324) — otherwise the
      // stale userId persists in the encrypted blob forever (Gitar review on #1704). The
      // store mutation re-pushes the blob via the friendOrgSync watcher.
      const validFriendIds = useFriendStore.getState().friends.map((f) => f.userId);
      useFriendOrgStore.getState().pruneFriends(validFriendIds);
    });

    // msg.data narrowed to FriendCodeClaimedPayload.
    // Known bug per ws-events.ts FriendCodeClaimedSchema comment: server
    // emits FLAT claimer fields but this handler currently only handles
    // the NESTED `friend`/`request` form. Schema accepts both shapes;
    // handler short-circuits silently on flat-form. Phase E follow-up tracks
    // wiring up flat-form admission.
    const unsubFriendCodeClaimed = wsService.on('friend_code_claimed', (msg) => {
      const data = msg.data;
      const store = useFriendStore.getState();

      if (data.status === 'accepted' && data.friend) {
        // Auto-accepted: add directly to friends list
        store.addFriend({
          id: data.friend.id,
          userId: data.friend.user_id,
          username: data.friend.username,
          displayName: data.friend.display_name ?? undefined,
          avatarUrl: data.friend.avatar_url ?? undefined,
          colorScheme: data.friend.color_scheme ?? undefined,
          status: data.friend.status || 'offline',
        });
      } else if (data.status === 'pending' && data.request) {
        // Pending: add to incoming requests. to_* fields may be missing per
        // #981 — fall back to empty string.
        store.addRequest({
          id: data.request.id,
          fromUserId: data.request.from_user_id,
          fromUsername: data.request.from_username,
          fromDisplayName: data.request.from_display_name ?? undefined,
          fromAvatarUrl: data.request.from_avatar_url ?? undefined,
          toUserId: data.request.to_user_id ?? '',
          toUsername: data.request.to_username ?? '',
          toDisplayName: data.request.to_display_name ?? undefined,
          toAvatarUrl: data.request.to_avatar_url ?? undefined,
          direction: 'received',
          createdAt: data.request.created_at,
        });
      }

      // Refresh friend code use counts
      store.refreshFriendCodeUseCounts();
    });

    // ── DM voice state update handler ────────────────────────────────────

    // msg.data narrowed to DMVoiceStateUpdatePayload — conversation_id + action
    // are schema-required (UUID + VoiceActionSchema enum).
    const unsubDMVoiceState = wsService.on('dm_voice_state_update', (msg) => {
      const { conversation_id: conversationId, action, user_id: userId } = msg.data;

      console.debug('[WebSocket] DM voice state update:', action, conversationId, userId);

      // Handle enforcement actions in DM calls
      if (
        ['server_muted', 'server_unmuted', 'server_deafened', 'server_undeafened'].includes(
          action
        ) &&
        userId
      ) {
        const voiceStore = useVoiceStore.getState();
        const update = VOICE_PARTICIPANT_UPDATES[action];
        if (update) {
          voiceStore.updateParticipant(userId, update);
        }

        const localUserId = useUserStore.getState().user?.id;
        if (userId === localUserId) {
          const messages: Record<string, string> = {
            server_muted: 'You have been server-muted by a moderator',
            server_unmuted: 'A moderator has removed your server mute',
            server_deafened: 'You have been server-deafened by a moderator',
            server_undeafened: 'A moderator has removed your server deafen',
          };
          if (messages[action]) {
            console.debug('[Voice]', messages[action]);
          }
        }
      }

      // DM call sound notifications. MUST run BEFORE applyDMVoiceState below:
      // the R9 ringtone-suppression gate reads the PRE-update activeDMCalls
      // roster to distinguish the initial ring (empty roster) from a
      // subsequent group join (#1219 R9).
      handleDMCallSounds(conversationId, action, userId, dmCallSoundState);

      // Active DM-call roster reducer (#1219 R4). Feeds the "Join voice call"
      // header affordance (R5) and the "N of M in call" list indicator (R6).
      // `total` is the conversation's member count (the "M" denominator).
      const conv = useDMStore.getState().conversations.find((c) => c.id === conversationId);
      useVoiceStore
        .getState()
        .applyDMVoiceState(conversationId, action, userId, conv?.participants.length ?? 0);

      // room_empty is broadcast only after the completed call-event row is
      // persisted, so this refetch safely advances the sidebar preview.
      if (action === 'room_empty') {
        refreshDMConversationPreviews();
      }
    });

    // Subscribed confirmation handler
    const unsubSubscribed = wsService.on('subscribed', (msg) => {
      // msg.data is narrowed to SubscribedPayload via generic on()
      console.debug('[WebSocket] Subscribed to channel:', msg.data.channel_id);
    });

    // Error handler — msg.data narrowed to ErrorPayload. All fields are
    // schema-optional; handler branches on the `epoch_revoked` code shape.
    const unsubError = wsService.on('error', (msg) => {
      const data = msg.data;
      if (data.code === 'epoch_revoked' && data.channel_id) {
        console.warn('[WebSocket] Epoch revoked for channel', data.channel_id, '→ re-fetching key');
        e2eeService.invalidateChannelKey(data.channel_id);
        globalThis.dispatchEvent(
          new CustomEvent('e2ee-key-rotation', {
            detail: {
              channelId: data.channel_id,
              newEpoch: data.current_epoch,
              reason: 'epoch_revoked',
            },
          })
        );
      } else {
        console.error('WebSocket error:', summarizeWsServerError(data));
      }
    });

    // Session revoked handler — server forcefully terminated our session.
    // Routes through the recovery system instead of immediately resetting,
    // so the user sees diagnostic info and can restart cleanly.
    const unsubSessionRevoked = wsService.on('session_revoked', () => {
      console.warn('[WebSocket] Session revoked by server');
      wsService.disconnect();
      const store = useConnectionStore.getState();
      store.setDiagnostics({
        internet: 'ok',
        serverReachable: 'ok',
        tokenValid: 'failed',
        sessionRevoked: true,
        rendererStable: 'ok',
      });
      store.enterFatal();
    });

    // ── DM voice call ring handlers (#1209 plan task E3) ─────────────────
    // Each handler is implemented in services/voiceService/callStateMachine.ts
    // (Task E2); this hook just wires the dispatch boundary. Payloads are
    // schema-validated upstream in websocketService.handleMessage per
    // [internal]rules/frontend.md "WebSocket payload validation" — handlers
    // operate on already-narrowed types.
    const unsubDMVoiceCallInvited = wsService.on('dm_voice_call_invited', (msg) => {
      handleCallInvited(msg.data);
    });
    const unsubDMVoiceCallCanceled = wsService.on('dm_voice_call_canceled', (msg) => {
      handleCallCanceled(msg.data);
      if (msg.data.canceled_by !== 'someone_accepted') {
        refreshDMConversationPreviews();
      }
    });
    const unsubDMVoiceCallDeclined = wsService.on('dm_voice_call_declined', (msg) => {
      handleCallDeclined(msg.data);
    });
    const unsubDMVoiceCallTimedOut = wsService.on('dm_voice_call_timed_out', (msg) => {
      handleCallTimedOut(msg.data);
      refreshDMConversationPreviews();
    });

    // ── Rich presence — custom text status (#1233) ──────────────────────
    // Payloads are schema-validated upstream at the dispatch boundary, so
    // msg.data is already narrowed (no casts). The store keys other users'
    // custom text by user_id; the self slice is owned by the settings flow.
    const unsubRichPresenceUpdate = wsService.on('rich_presence_update', (msg) => {
      if (msg.data.category !== 'custom_text') return;
      useRichPresenceStore.getState().setCustomText(msg.data.user_id, msg.data.payload);
    });
    const unsubRichPresenceClear = wsService.on('rich_presence_clear', (msg) => {
      if (msg.data.category !== 'custom_text') return;
      useRichPresenceStore.getState().clearCustomText(msg.data.user_id);
    });

    // Cleanup handlers
    return () => {
      const callState = useVoiceStore.getState().callState;
      const backgroundRingtoneIsStillOwned =
        dmCallSoundState.backgroundRingtoneConversationId !== null &&
        (callState.kind === 'idle' ||
          ((callState.kind === 'outgoing-ringing' ||
            callState.kind === 'incoming-ringing' ||
            callState.kind === 'joining') &&
            callState.conversationId === dmCallSoundState.backgroundRingtoneConversationId));
      if (backgroundRingtoneIsStillOwned) {
        notificationSoundService.stopLoop('call-ringing');
      }
      unsubscribeDMCallSoundState();
      deactivateMessageContentOperationOwner(messageContentOperationOwner);
      for (const { channelId, messageId } of pendingMessagePlaceholders.values()) {
        const placeholder = useChatStore
          .getState()
          .messagesByChannel.get(channelId)
          ?.find((message) => message.id === messageId);
        if (
          placeholder?.content !== '' ||
          placeholder.decryptFailed !== false ||
          placeholder.pendingKeys !== false
        ) {
          continue;
        }
        deleteMessage(channelId, messageId);
        if (useChatStore.getState().messagesByChannel.get(channelId)?.length === 0) {
          useChatStore.getState().clearMessages(channelId);
        }
      }
      pendingMessagePlaceholders.clear();
      unsubMessage();
      unsubUpdate();
      unsubDelete();
      unsubChannelPurged();
      unsubDmPurged();
      unsubServerPurged();
      globalThis.removeEventListener('messages-purged', handleMessagesPurged);
      unsubReactionAdded();
      unsubReactionRemoved();
      unsubMessagePinned();
      unsubMessageUnpinned();
      unsubTyping();
      unsubAck();
      unsubMemberJoined();
      unsubProfileUpdated();
      unsubServerUpdated();
      unsubChannelUpdated();
      unsubChannelCreated();
      unsubChannelDeleted();
      unsubGroupCreated();
      unsubGroupUpdated();
      unsubGroupDeleted();
      unsubChannelsReordered();
      unsubServerDeleted();
      unsubMemberRemoved();
      unsubMemberTimeout();
      unsubUnreadNotify();
      unsubKeyNeeded();
      unsubKeyRevocation();
      unsubKeyDelivered();
      unsubPreferencesUpdated();
      unsubSavedGifsUpdated();
      unsubFriendOrgUpdated();
      unsubPresenceOverridesUpdated();
      unsubEntitlements();
      unsubEntitlementsResync();
      unsubPresenceSnapshot();
      unsubPresence();
      unsubServerOnlineCounts();
      unsubServerVoiceCounts();
      unsubVoiceState();
      unsubVoiceMove();
      unsubChannelAccessRevoked();
      unsubDMMessage();
      unsubDMAck();
      unsubDMUpdate();
      unsubDMDelete();
      unsubDMTyping();
      unsubDMUnread();
      unsubDMConvCreated();
      unsubDMParticipantAdded();
      unsubDMParticipantRemoved();
      unsubDMRoleChanged();
      unsubDMGroupDeleted();
      unsubDMSubscribed();
      unsubFriendRequestReceived();
      unsubFriendRequestAccepted();
      unsubFriendRemoved();
      unsubFriendCodeClaimed();
      unsubDMVoiceState();
      unsubDMVoiceCallInvited();
      unsubDMVoiceCallCanceled();
      unsubDMVoiceCallDeclined();
      unsubDMVoiceCallTimedOut();
      unsubRichPresenceUpdate();
      unsubRichPresenceClear();
      unsubSubscribed();
      unsubError();
      unsubSessionRevoked();
    };
  }, [
    wsService,
    addMessage,
    updateMessage,
    updateMessageStatus,
    updateUserInMessages,
    deleteMessage,
    setTyping,
  ]);
}
