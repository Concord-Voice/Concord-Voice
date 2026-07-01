/**
 * WebSocket Event Types — discriminated union + zod schemas
 *
 * Single source of truth for the wire contract between the Go control-plane
 * (services/control-plane/internal/websocket/hub.go and sibling packages) and
 * the Electron renderer.
 *
 * The server's `OutgoingMessage.Data` is `map[string]interface{}` — there is
 * no Go-side per-event struct. This file IS the wire contract.
 *
 * Distinction from `chat.ts` etc.: the types here describe WIRE shapes (what
 * arrives from the server), while `chat.ts` defines APPLICATION models (what
 * stores hold). Handlers in useWebSocketMessages.ts transform wire → app.
 *
 * 63 schemas total: 61 subscriber events (handled via wsService.on) + 2
 * envelope-only events (connected, connection_ready) consumed internally
 * by wsService.handleMessage's switch — both must be in the union so that
 * the post-safeParse code accesses message.data fields without `as` casts.
 *
 * @see [internal]specs/2026-05-23-709-ws-discriminated-union-design.md
 * @see [internal]rules/frontend.md ("WebSocket payload validation" section)
 */

import { z } from 'zod';

// ════════════════════════════════════════════════════════════════════════
// 1. Reusable primitive schemas
// ════════════════════════════════════════════════════════════════════════

export const UUID = z.string().uuid();
export const ISOTimestamp = z.string().datetime({ offset: true });

/**
 * MediaURL — accepts the three URL shapes the control-plane emits for avatar
 * and icon fields:
 *  1. `/api/v1/media/...` relative paths (the canonical uploaded-media URL
 *     produced by `services/control-plane/internal/users/handlers.go:378`
 *     and `internal/servers/handlers.go`).
 *  2. `data:image/...` data URLs (allowed for client-supplied inline avatars
 *     per `users/handlers.go:379`; capped server-side at the free entitlement floor).
 *  3. Absolute `http(s)://...` URLs (kept for forward-compat with a future
 *     CDN-backed media path).
 *
 * `z.string().url()` rejects shapes 1 and 2, which would silently drop
 * legitimate profile_updated / member_joined / friend_request_received /
 * presence / voice_state_update events at the dispatch boundary (bumping
 * `wireViolationCount`). See PR #1184 review thread for the original report.
 */
export const MediaURL = z
  .string()
  .refine(
    (s) => s.startsWith('/api/v1/media/') || s.startsWith('data:image/') || /^https?:\/\//.test(s),
    { message: 'must be a /api/v1/media/* path, data:image/* URL, or http(s) URL' }
  );
export const PresenceStatusSchema = z.enum(['online', 'offline', 'dnd', 'invisible']);

// ════════════════════════════════════════════════════════════════════════
// 2. Shared sub-shape schemas (cross-domain payload pieces)
// ════════════════════════════════════════════════════════════════════════

// ── Chat / DM messages ────────────────────────────────────────────────

/**
 * AttachmentSummary — emitted by server in `message` (channel) and DM message
 * events. Mirrors services/control-plane/internal/models/attachments.go.
 */
const MessageAttachmentSchema = z.object({
  id: UUID,
  // Enum must mirror AttachmentSummary.file_type in types/chat.ts. Tightening
  // here was driven by Phase C1 typecheck — the renderer narrows on file_type
  // to switch render paths, so `z.string()` would re-introduce the
  // discriminated-union hole at the consumer site.
  file_type: z.enum(['photo', 'animated', 'video', 'audio', 'file']),
  mime_type: z.string(),
  file_size: z.number().int().nonnegative(),
  width: z.number().int().nonnegative().optional(),
  height: z.number().int().nonnegative().optional(),
});

/** ReactionUser — entry in a reaction summary's users array. */
const ReactionUserSchema = z.object({
  user_id: UUID,
  username: z.string(),
  display_name: z.string().optional(),
});

/** ReactionSummary — grouped reaction state for one emoji on one message. */
const ReactionSummarySchema = z.object({
  emoji: z.string(),
  count: z.number().int().nonnegative(),
  users: z.array(ReactionUserSchema),
  me: z.boolean(),
});

/** RepliedToSummary — preview of a parent message attached to a reply. */
const RepliedToSchema = z.object({
  id: UUID,
  user_id: UUID,
  username: z.string(),
  display_name: z.string().nullable().optional(),
  content: z.string(),
  key_version: z.number().int().nonnegative(),
});

// ── DM-specific ───────────────────────────────────────────────────────

/**
 * DMParticipantSchema — appears in dm_conversation_created.conversation.participants[]
 * and (in a flat-field form) in dm_participant_added. Mirrors the Go struct
 * services/control-plane/internal/dm/handlers.go participantResponse.
 */
const DMParticipantSchema = z.object({
  user_id: UUID,
  username: z.string(),
  display_name: z.string().nullable().optional(),
  avatar_url: MediaURL.nullable().optional(),
  color_scheme: z.string().nullable().optional(),
  role: z.enum(['admin', 'member']).optional(),
});

/**
 * DMConversationSummarySchema — appears as dm_conversation_created.data.conversation.
 * id/is_group/created_at are REQUIRED per PR #704 envelope runtime guards.
 */
const DMConversationSummarySchema = z.object({
  id: UUID,
  is_group: z.boolean(),
  is_personal: z.boolean().optional(),
  name: z.string().nullable().optional(),
  icon_url: MediaURL.nullable().optional(),
  created_by: z.string().optional(),
  participants: z.array(DMParticipantSchema).optional(),
  last_message: z
    .object({
      content: z.string(),
      user_id: UUID,
      created_at: ISOTimestamp,
    })
    .nullable()
    .optional(),
  unread_count: z.number().int().nonnegative().optional(),
  created_at: ISOTimestamp,
});

// ── Friend system ─────────────────────────────────────────────────────

/**
 * Wire shape for a friend record. Reused in friend_request_accepted (nested)
 * and friend_code_claimed (accepted-status nested).
 */
const FriendPayloadSchema = z.object({
  id: UUID,
  user_id: UUID,
  username: z.string(),
  display_name: z.string().nullable().optional(),
  avatar_url: MediaURL.nullable().optional(),
  color_scheme: z.string().nullable().optional(),
  status: PresenceStatusSchema.optional(),
});

/**
 * Wire shape for a pending friend-request record.
 * NOTE: to_* fields are .optional() pending server-side fix per #981.
 * Server currently emits only from_* fields. Once server emits to_*, flip
 * these to required.
 */
const FriendRequestPayloadSchema = z.object({
  id: UUID,
  from_user_id: UUID,
  from_username: z.string(),
  from_display_name: z.string().nullable().optional(),
  from_avatar_url: MediaURL.nullable().optional(),
  to_user_id: UUID.optional(), // optional pending server-side fix per #981
  to_username: z.string().optional(), // optional pending server-side fix per #981
  to_display_name: z.string().nullable().optional(), // optional pending server-side fix per #981
  to_avatar_url: MediaURL.nullable().optional(), // optional pending server-side fix per #981
  created_at: ISOTimestamp,
});

// ── Voice ─────────────────────────────────────────────────────────────

/** Constrained voice action enum used by both voice_state_update events. */
const VoiceActionSchema = z.enum([
  'joined',
  'left',
  'room_empty',
  'muted',
  'unmuted',
  'video_on',
  'video_off',
  'screen_on',
  'screen_off',
  'server_muted',
  'server_unmuted',
  'server_deafened',
  'server_undeafened',
]);

// ── Presence ──────────────────────────────────────────────────────────

/** One user's presence as emitted inside presence_snapshot.users. Server: hub.go:346 userPresenceInfo. */
const UserPresenceInfoSchema = z.object({
  user_id: UUID,
  status: PresenceStatusSchema,
});

// ── Channel + server lifecycle ────────────────────────────────────────

/** Wire shape for a Channel in channel_created payload. Source: channels/handlers.go:449 channelToMap. */
const ChannelPayloadSchema = z.object({
  id: UUID,
  server_id: UUID,
  name: z.string(),
  type: z.string(), // 'text' | 'voice' | 'bulletin' — permissive on wire
  emoji: z.string().nullable().optional(),
  audio_quality_tier: z.string().nullable().optional(),
  group_id: UUID.nullable().optional(),
  position: z.number(),
  created_at: ISOTimestamp,
  updated_at: ISOTimestamp,
  linked_voice_channel_id: UUID.nullable().optional(),
  description: z.string().optional(),
  sync_permissions: z.boolean().optional(),
});

/** Wire shape for a ChannelGroup in channel_group_created/updated payloads. */
const ChannelGroupPayloadSchema = z.object({
  id: UUID,
  server_id: UUID,
  name: z.string(),
  position: z.number(),
  created_at: ISOTimestamp.optional(), // emitted on created, OMITTED on updated
  updated_at: ISOTimestamp,
});

/** Wire shape for per-channel reorder entry inside channels_reordered. */
const ChannelReorderEntrySchema = z.object({
  channel_id: UUID,
  group_id: UUID.nullable(),
  position: z.number(),
});

// ════════════════════════════════════════════════════════════════════════
// 3. Per-event schemas, grouped by domain
// ════════════════════════════════════════════════════════════════════════
//
// Annotation convention. The header at the top of this file documents the
// Go-side trust boundary (`hub.go` + sibling packages → `OutgoingMessage.Data`
// as `map[string]interface{}`). Where a schema's emit site is non-obvious or
// covers multiple Go files, the doc comment cites the file:line. Schemas
// without an explicit citation are emitted from the canonical location for
// the domain (e.g., `services/control-plane/internal/websocket/hub.go` for
// hub-relay events, `internal/messages/handlers.go` for chat REST mutations).
// When adding a new schema, prefer an explicit citation if the emitter is
// outside the domain's canonical file — it shortens the schema-vs-server-
// drift investigation in PR review.

// ──────────── Chat messages (9 events) — Task A3 ──────────────────────

/**
 * `message` — new chat message in a channel.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:1324`
 * (`OutgoingMessage{Type: "message", Data: broadcastData}`).
 */
export const MessageSchema = z.object({
  type: z.literal('message'),
  data: z.object({
    id: UUID,
    channel_id: UUID,
    user_id: UUID,
    username: z.string(),
    display_name: z.string().nullable().optional(),
    avatar_url: z.string().nullable().optional(),
    content: z.string(),
    key_version: z.number().int().nonnegative().optional(),
    embeds_suppressed: z.boolean().optional(),
    created_at: ISOTimestamp,
    updated_at: ISOTimestamp,
    reply_to_id: UUID.optional(),
    replied_to: RepliedToSchema.optional(),
    gif_slug: z.string().optional(),
    attachments: z.array(MessageAttachmentSchema).optional(),
    // Client-only reads (not in broadcast emission — likely enriched elsewhere or pending):
    mentioned: z.boolean().optional(),
    server_id: UUID.optional(),
  }),
});

/**
 * `message_update` — message content / metadata edited. Two emit paths;
 * only id+channel_id guaranteed.
 * Server emitters: `services/control-plane/internal/messages/handlers.go:623`
 * (edit path) and `:791` (embed-suppress path).
 */
export const MessageUpdateSchema = z.object({
  type: z.literal('message_update'),
  data: z.object({
    id: UUID,
    channel_id: UUID,
    content: z.string().optional(),
    key_version: z.number().int().nonnegative().optional(),
    embeds_suppressed: z.boolean().optional(),
    edited_at: ISOTimestamp.nullable().optional(),
    updated_at: ISOTimestamp.optional(),
  }),
});

/**
 * `message_delete` — message removed.
 * Server emitter: `services/control-plane/internal/messages/handlers.go:712`.
 */
export const MessageDeleteSchema = z.object({
  type: z.literal('message_delete'),
  data: z.object({
    id: UUID,
    channel_id: UUID,
  }),
});

/**
 * `message_reaction_added` — reaction added to a message.
 * Server emitter: `services/control-plane/internal/messages/reactions.go:272`
 * (shared with `message_reaction_removed` via a runtime `eventType` switch).
 */
export const MessageReactionAddedSchema = z.object({
  type: z.literal('message_reaction_added'),
  data: z.object({
    message_id: UUID,
    channel_id: UUID,
    emoji: z.string(),
    user_id: UUID,
    reaction_summary: ReactionSummarySchema.nullable().optional(),
  }),
});

/**
 * `message_reaction_removed` — reaction removed; null reaction_summary signals "group collapsed".
 * Server emitter: `services/control-plane/internal/messages/reactions.go:287`
 * (`OutgoingMessage{Type: eventType, ...}` where `eventType = "message_reaction_removed"` set at line 274).
 */
export const MessageReactionRemovedSchema = z.object({
  type: z.literal('message_reaction_removed'),
  data: z.object({
    message_id: UUID,
    channel_id: UUID,
    emoji: z.string(),
    user_id: UUID,
    reaction_summary: ReactionSummarySchema.nullable().optional(),
  }),
});

/**
 * `message_pinned` — message pinned in a channel.
 * Server emitter: `services/control-plane/internal/messages/pinning.go:402`
 * (`OutgoingMessage{Type: "message_pinned", ...}`).
 */
export const MessagePinnedSchema = z.object({
  type: z.literal('message_pinned'),
  data: z.object({
    message_id: UUID,
    channel_id: UUID,
    pinned_at: ISOTimestamp,
    pinned_by: UUID,
  }),
});

/**
 * `message_unpinned` — message unpinned. Accepts both channel_id and conversation_id (DM path).
 * Server emitter: `services/control-plane/internal/messages/pinning.go:201`
 * (channel path; DM path at `pinning.go:237`).
 */
export const MessageUnpinnedSchema = z.object({
  type: z.literal('message_unpinned'),
  data: z.object({
    message_id: UUID,
    channel_id: UUID.optional(),
    conversation_id: UUID.optional(),
  }),
});

/**
 * `typing` — typing indicator from another client in a channel.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:1411`
 * (re-broadcast of inbound `typing` client→hub command from `hub.go:508`).
 */
export const TypingSchema = z.object({
  type: z.literal('typing'),
  data: z.object({
    channel_id: UUID,
    user_id: UUID,
    username: z.string().optional(),
    is_typing: z.boolean(),
  }),
});

/**
 * `message_ack` — server acknowledgement of a client-sent message.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:1004`
 * (`OutgoingMessage{Type: "message_ack", Data: ackData}`).
 */
export const MessageAckSchema = z.object({
  type: z.literal('message_ack'),
  data: z.object({
    id: UUID,
    channel_id: UUID,
    nonce: z.string().optional(),
    created_at: ISOTimestamp.optional(),
    updated_at: ISOTimestamp.optional(),
    reply_to_id: UUID.optional(),
    gif_slug: z.string().optional(),
    attachments: z.array(MessageAttachmentSchema).optional(),
  }),
});

// ──────────── Direct messages (12 events) — Task A4 ───────────────────

/**
 * `dm_message` — DM chat message broadcast.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:1967`
 * (`OutgoingMessage{Type: "dm_message", Data: dmBroadcastData}`).
 */
export const DMMessageSchema = z.object({
  type: z.literal('dm_message'),
  data: z.object({
    conversation_id: UUID,
    channel_id: UUID.optional(), // back-compat alias
    id: UUID.optional(),
    user_id: UUID,
    username: z.string().optional(),
    display_name: z.string().nullable().optional(),
    avatar_url: MediaURL.nullable().optional(),
    content: z.string().optional(),
    key_version: z.number().int().nonnegative().optional(),
    gif_slug: z.string().optional(),
    attachments: z.array(MessageAttachmentSchema).optional(),
    created_at: ISOTimestamp.optional(),
    updated_at: ISOTimestamp.optional(),
  }),
});

/**
 * `dm_message_ack` — server acknowledgement of a sent DM.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:1917`
 * (`OutgoingMessage{Type: "dm_message_ack", Data: dmAckData}`).
 */
export const DMMessageAckSchema = z.object({
  type: z.literal('dm_message_ack'),
  data: z.object({
    nonce: z.string(),
    id: UUID,
    conversation_id: UUID,
    channel_id: UUID.optional(),
    created_at: ISOTimestamp.optional(),
    updated_at: ISOTimestamp.optional(),
    gif_slug: z.string().optional(),
    attachments: z.array(MessageAttachmentSchema).optional(),
  }),
});

/**
 * `dm_message_update` — DM message edited.
 * Server emitter: `services/control-plane/internal/dm/handlers.go:1781`
 * (`OutgoingMessage{Type: "dm_message_update", ...}`).
 */
export const DMMessageUpdateSchema = z.object({
  type: z.literal('dm_message_update'),
  data: z.object({
    conversation_id: UUID,
    channel_id: UUID.optional(),
    id: UUID,
    content: z.string(),
    key_version: z.number().int().nonnegative().optional(),
    edited_at: ISOTimestamp.optional(),
    updated_at: ISOTimestamp.optional(),
  }),
});

/**
 * `dm_message_delete` — DM message deleted.
 * Server emitter: `services/control-plane/internal/dm/handlers.go:1838`
 * (`OutgoingMessage{Type: "dm_message_delete", ...}`).
 */
export const DMMessageDeleteSchema = z.object({
  type: z.literal('dm_message_delete'),
  data: z.object({
    conversation_id: UUID,
    channel_id: UUID.optional(),
    id: UUID,
  }),
});

/**
 * `dm_typing` — DM typing indicator.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:2143`
 * (re-broadcast of inbound `dm_typing` command).
 */
export const DMTypingSchema = z.object({
  type: z.literal('dm_typing'),
  data: z.object({
    conversation_id: UUID,
    channel_id: UUID.optional(),
    user_id: UUID,
    is_typing: z.boolean().optional(),
    username: z.string().optional(),
  }),
});

/**
 * `dm_unread_notify` — DM unread notification for unsubscribed clients.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:2028`
 * (mention-enriched variant at `internal/websocket/mentions.go:507`).
 */
export const DMUnreadNotifySchema = z.object({
  type: z.literal('dm_unread_notify'),
  data: z.object({
    conversation_id: UUID,
    mentioned: z.boolean().optional(),
    user_id: UUID.optional(), // cast-only; not in server emission
    last_message: z
      .object({
        content: z.string().optional(),
        user_id: UUID.optional(),
        username: z.string().optional(),
        created_at: ISOTimestamp.optional(),
      })
      .optional(),
  }),
});

/**
 * `dm_conversation_created` — new DM conversation visible to the user.
 * Server emitter: `services/control-plane/internal/dm/handlers.go:520`
 * (also emitted at `dm/handlers.go:639` and `dm/handlers.go:896` for group-create paths).
 */
export const DMConversationCreatedSchema = z.object({
  type: z.literal('dm_conversation_created'),
  data: z.object({
    conversation: DMConversationSummarySchema,
  }),
});

/**
 * `dm_participant_added` — user added to group DM.
 * Server emitter: `services/control-plane/internal/dm/handlers.go:883`
 * (`OutgoingMessage{Type: "dm_participant_added", ...}`).
 */
export const DMParticipantAddedSchema = z.object({
  type: z.literal('dm_participant_added'),
  data: z.object({
    conversation_id: UUID,
    user_id: UUID,
    added_by: UUID.optional(),
    // Cast-only fields (server does NOT emit these — handler tolerates absence):
    username: z.string().optional(),
    display_name: z.string().nullable().optional(),
    avatar_url: MediaURL.nullable().optional(),
    color_scheme: z.string().nullable().optional(),
  }),
});

/**
 * `dm_participant_removed` — user removed from group DM.
 * Server emitter: `services/control-plane/internal/dm/handlers.go:1088`
 * (`OutgoingMessage{Type: "dm_participant_removed", ...}`).
 */
export const DMParticipantRemovedSchema = z.object({
  type: z.literal('dm_participant_removed'),
  data: z.object({
    conversation_id: UUID,
    user_id: UUID,
    removed_by: UUID.optional(),
    was_self_leave: z.boolean().optional(),
  }),
});

/**
 * `dm_role_changed` — DM group member role changed.
 * Server emitter: `services/control-plane/internal/dm/handlers.go:1100`
 * (also `internal/dm/group_handlers.go:109` for explicit role-change endpoint).
 */
export const DMRoleChangedSchema = z.object({
  type: z.literal('dm_role_changed'),
  data: z.object({
    conversation_id: UUID,
    user_id: UUID,
    role: z.enum(['admin', 'member']),
    changed_by: UUID.optional(),
  }),
});

/**
 * `dm_group_deleted` — group DM deleted by admin.
 * Server emitter: `services/control-plane/internal/dm/group_handlers.go:180`
 * (`OutgoingMessage{Type: "dm_group_deleted", ...}`).
 */
export const DMGroupDeletedSchema = z.object({
  type: z.literal('dm_group_deleted'),
  data: z.object({
    conversation_id: UUID,
    deleted_by: UUID.optional(),
  }),
});

/**
 * `dm_subscribed` — subscription confirmation for a DM conversation.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:1670`
 * (`OutgoingMessage{Type: "dm_subscribed", ...}`).
 */
export const DMSubscribedSchema = z.object({
  type: z.literal('dm_subscribed'),
  data: z.object({
    conversation_id: UUID,
    channel_id: UUID.optional(),
  }),
});

// ──────────── Friend system (4 events) — Task A5 (#981 drift locus) ───

/**
 * `friend_request_received` — addressee receives a pending request. Flat payload.
 * Server emitter: `services/control-plane/internal/friends/handlers.go:232`
 * (also re-emitted at `friends/handlers.go:966` on code-claim path).
 */
export const FriendRequestReceivedSchema = z.object({
  type: z.literal('friend_request_received'),
  data: FriendRequestPayloadSchema,
});

/**
 * `friend_request_accepted` — requester learns acceptance. Permissive (flat + nested test-compat).
 * Server emitter: `services/control-plane/internal/friends/handlers.go:345`
 * (also `friends/handlers.go:952` for code-claim auto-accept path).
 */
export const FriendRequestAcceptedSchema = z.object({
  type: z.literal('friend_request_accepted'),
  data: z.object({
    // Flat-form fields (current server emission)
    id: UUID.optional(),
    user_id: UUID.optional(),
    username: z.string().optional(),
    display_name: z.string().nullable().optional(),
    avatar_url: MediaURL.nullable().optional(),
    color_scheme: z.string().nullable().optional(),
    // Nested-form fields (test-fixture compat — no server emitter)
    request_id: UUID.optional(),
    friend: FriendPayloadSchema.optional(),
  }),
});

/**
 * `friend_removed` — friendship removed or user blocked.
 * Server emitter: `services/control-plane/internal/friends/handlers.go:443`
 * (also `friends/handlers.go:542` on block path; the latter pairs with `key_revocation`).
 */
export const FriendRemovedSchema = z.object({
  type: z.literal('friend_removed'),
  data: z.object({
    user_id: UUID,
  }),
});

/**
 * `friend_code_claimed` — friend-code owner learns someone redeemed their code.
 * Server emitter: `services/control-plane/internal/friends/handlers.go:936`
 * (`OutgoingMessage{Type: "friend_code_claimed", ...}`).
 *
 * KNOWN BUG (separate from #981): server emits FLAT claimer fields, but
 * handler at useWebSocketMessages.ts:1645 expects NESTED `friend`/`request`
 * objects. Schema accepts BOTH forms for status-quo compat; handler
 * currently short-circuits silently on flat-form. Tracked as Phase E
 * follow-up.
 */
export const FriendCodeClaimedSchema = z.object({
  type: z.literal('friend_code_claimed'),
  data: z.object({
    // Server flat form (handlers.go:937-946)
    friendship_id: UUID.optional(),
    code_id: UUID.optional(),
    status: z.string().optional(),
    user_id: UUID.optional(),
    username: z.string().optional(),
    display_name: z.string().nullable().optional(),
    avatar_url: MediaURL.nullable().optional(),
    created_at: ISOTimestamp.optional(),
    // Client-expected nested form (useWebSocketMessages.ts:1648-1650)
    friend: FriendPayloadSchema.optional(),
    request: FriendRequestPayloadSchema.optional(),
  }),
});

// ──────────── Voice (6 events) — Task A6 + #1209 DM ring ──────────────

/**
 * `voice_state_update` — server-channel voice state.
 * Server emitter: `services/control-plane/internal/voice/nats.go:168`
 * (NATS-fanout broadcast; also emitted at nats.go:226 and nats.go:276 for other voice transitions).
 */
export const VoiceStateUpdateSchema = z.object({
  type: z.literal('voice_state_update'),
  data: z.object({
    channel_id: UUID,
    action: VoiceActionSchema,
    server_id: UUID.optional(),
    user_id: UUID.optional(),
    username: z.string().optional(),
    display_name: z.string().optional(),
  }),
});

/**
 * `voice_move` — directed signal telling the target client to relocate to
 * another voice channel in the same server (#487 Scope B). Server emits this
 * via `hub.BroadcastToUser` from `internal/voice/handlers.go` `ServerMove`,
 * AFTER granting any temporary SBAC override on the destination channel
 * (grant-before-signal ordering is load-bearing — the client's subsequent
 * `AuthorizeJoin` checks `PermJoinVoice`). All four fields are server-emitted
 * and required.
 */
export const VoiceMoveSchema = z.object({
  type: z.literal('voice_move'),
  data: z.object({
    user_id: UUID,
    from_channel_id: UUID,
    to_channel_id: UUID,
    server_id: UUID,
  }),
});

/**
 * `channel_access_revoked` — directed signal telling the affected user that a
 * (temporary) channel grant was revoked (#487 P4). Server emits this via
 * `hub.BroadcastToUser` from `internal/voice/temp_grants.go`
 * `revokeTemporaryChannelAccess`. The renderer removes the channel from the
 * sidebar, purges cached messages for it, invalidates the cached channel key,
 * and leaves voice if currently in that channel. Distinct from `member_removed`
 * (whole server) and `channel_deleted` (whole channel). `reason` is a stable
 * identifier string, e.g. 'temp_access_revoked'.
 */
export const ChannelAccessRevokedSchema = z.object({
  type: z.literal('channel_access_revoked'),
  data: z.object({
    channel_id: UUID,
    server_id: UUID,
    reason: z.string(),
  }),
});

/**
 * `dm_voice_state_update` — DM call voice state.
 * Server emitter: `services/control-plane/internal/voice/nats.go:143`
 * (NATS-fanout; also nats.go:211 and nats.go:263 for other DM voice transitions).
 */
export const DMVoiceStateUpdateSchema = z.object({
  type: z.literal('dm_voice_state_update'),
  data: z.object({
    conversation_id: UUID,
    action: VoiceActionSchema,
    user_id: UUID.optional(),
    username: z.string().optional(),
    display_name: z.string().optional(),
  }),
});

/**
 * `dm_voice_call_invited` — DM voice call ring invitation (#1209).
 * Server emitter: `services/control-plane/internal/dm/handlers.go`
 * (RingDMCall — broadcasts directly via hub.BroadcastToUser per ringing
 * user ID, no NATS fanout in single-replica mode per spec §6.3).
 * Sent to callees when caller hits POST /dm/conversations/{id}/voice/ring.
 */
export const DMVoiceCallInvitedSchema = z.object({
  type: z.literal('dm_voice_call_invited'),
  data: z.object({
    conversation_id: UUID,
    is_group: z.boolean(),
    caller: z.object({
      user_id: UUID,
      username: z.string(),
      display_name: z.string().optional(),
      avatar_url: z.string().optional(),
    }),
    ring_id: UUID,
    ring_started_at: ISOTimestamp,
    ring_timeout_seconds: z.number().int().positive(),
  }),
});

/**
 * `dm_voice_call_canceled` — DM voice call ring canceled (#1209).
 * Server emitter: `services/control-plane/internal/dm/handlers.go`
 * (CancelDMCall + AuthorizeVoiceJoin accept-path + onRingTimeout via
 * hub.BroadcastToDM). Sent to all participants when:
 *   - caller hits POST /voice/cancel ('caller')
 *   - all callees have declined ('all_declined')
 *   - any callee accepts, ring auto-cancels for others ('someone_accepted')
 *   - server-side error ('server_error')
 */
export const DMVoiceCallCanceledSchema = z.object({
  type: z.literal('dm_voice_call_canceled'),
  data: z.object({
    conversation_id: UUID,
    ring_id: UUID,
    canceled_by: z.enum(['caller', 'all_declined', 'someone_accepted', 'server_error']),
  }),
});

/**
 * `dm_voice_call_declined` — single callee declined the ring (#1209).
 * Server emitter: `services/control-plane/internal/dm/handlers.go`
 * (DeclineDMCall via hub.BroadcastToUser to the caller).
 * Sent ONLY to the caller (not other callees).
 * For DM 1:1: this is the terminal decline. For groups (#1219): the caller's
 * tally UI updates; ring may continue for other callees.
 */
export const DMVoiceCallDeclinedSchema = z.object({
  type: z.literal('dm_voice_call_declined'),
  data: z.object({
    conversation_id: UUID,
    ring_id: UUID,
    decliner_user_id: UUID,
  }),
});

/**
 * `dm_voice_call_timed_out` — ring timeout fired with no accept (#1209).
 * Server emitter: `services/control-plane/internal/dm/handlers.go`
 * (onRingTimeout via hub.BroadcastToDM). Sent to all participants when
 * the 45s ring-timeout timer expires without anyone accepting.
 */
export const DMVoiceCallTimedOutSchema = z.object({
  type: z.literal('dm_voice_call_timed_out'),
  data: z.object({
    conversation_id: UUID,
    ring_id: UUID,
  }),
});

// ──────────── Presence (5 events) — Task A7 (#803-related) ────────────

/**
 * `presence` — peer presence transition (online/offline/dnd/invisible).
 * Server emitter: `services/control-plane/internal/websocket/hub.go:2370`.
 *
 * #803 (self-user shows Offline despite connected) is fixed in the
 * `presence_snapshot` handler (useWebSocketMessages.ts), which reconciles the
 * self-user's status from the self-aware snapshot into memberStore.selfStatus.
 * The `presence` self-skip in that handler stays correct: this broadcast
 * carries raw status (the server emits 'offline' for invisible users) and is
 * NOT self-aware, so it must not override the local selfStatus. This schema
 * locks the shape against drift.
 */
export const PresenceSchema = z.object({
  type: z.literal('presence'),
  data: z.object({
    user_id: UUID,
    status: PresenceStatusSchema,
    timestamp: z.number().int().optional(),
  }),
});

/**
 * `presence_snapshot` — bulk presence sent on connect. Carries both
 * `online_user_ids` (legacy back-compat) and `users` (enhanced). Renderer
 * prefers `users`, falls back to `online_user_ids`.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:375`
 * (`OutgoingMessage{Type: "presence_snapshot", ...}` on client connect).
 */
export const PresenceSnapshotSchema = z.object({
  type: z.literal('presence_snapshot'),
  data: z.object({
    online_user_ids: z.array(UUID).optional(),
    users: z.array(UserPresenceInfoSchema).optional(),
  }),
});

/**
 * `server_online_counts` — debounced (500ms) per-server visible-online counts.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:2431`
 * (`OutgoingMessage{Type: "server_online_counts", ...}`).
 */
export const ServerOnlineCountsSchema = z.object({
  type: z.literal('server_online_counts'),
  data: z.object({
    counts: z.record(UUID, z.number().int().nonnegative()).optional(),
  }),
});

/**
 * `server_voice_counts` — per-server voice-participant counts.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:412`
 * (also `hub.go:2578` for the debounced global-broadcast path).
 */
export const ServerVoiceCountsSchema = z.object({
  type: z.literal('server_voice_counts'),
  data: z.object({
    counts: z.record(UUID, z.number().int().nonnegative()).optional(),
  }),
});

/**
 * `profile_updated` — user profile updated; server always emits user_id.
 * Server emitter: `services/control-plane/internal/users/handlers.go:613`
 * (broadcast-to-all via `BroadcastToAll(OutgoingMessage{Type: "profile_updated", ...})`).
 */
export const ProfileUpdatedSchema = z.object({
  type: z.literal('profile_updated'),
  data: z.object({
    user_id: UUID,
    username: z.string(),
    display_name: z.string().nullable().optional(),
    avatar_url: z.string().nullable().optional(),
    header_image_url: z.string().nullable().optional(),
    color_scheme: z.string().nullable().optional(),
  }),
});

// ──────────── Rich Presence — Custom Text Status (#1233, 2 events) ────
// Minimal framework slice: only the custom_text category is wired end-to-end.
// The category enum lists the full taxonomy for forward-compatibility; other
// categories' payload schemas are added when those features land. Custom text is
// DB-persistent + audience-filtered server-side (risk: privacy) — see
// [internal]specs/2026-06-18-1233-custom-text-status-design.md §5.
export const RichPresenceCategorySchema = z.enum([
  'server_voice',
  'private_call',
  'games',
  'music',
  'streaming',
  'browser',
  'productivity',
  'creator',
  'custom_text',
]);

export const CustomTextPresencePayloadSchema = z.object({
  emoji: z.string().max(32).optional(),
  text: z.string().min(1).max(140),
});

export const RichPresenceUpdateSchema = z.object({
  type: z.literal('rich_presence_update'),
  data: z.object({
    user_id: UUID,
    category: RichPresenceCategorySchema,
    payload: CustomTextPresencePayloadSchema,
    updated_at: z.number().int().positive(),
  }),
});

export const RichPresenceClearSchema = z.object({
  type: z.literal('rich_presence_clear'),
  data: z.object({
    user_id: UUID,
    category: RichPresenceCategorySchema,
  }),
});

// ──────────── Channel + server lifecycle (13 events) — Task A8 ────────

/**
 * `member_joined` — user joined server via invite.
 * Server emitter: `services/control-plane/internal/invites/handlers.go:208`
 * (`OutgoingMessage{Type: "member_joined", ...}`).
 */
export const MemberJoinedSchema = z.object({
  type: z.literal('member_joined'),
  data: z.object({
    server_id: UUID,
    user_id: UUID,
    username: z.string().optional(),
    display_name: z.string().nullable().optional(),
    avatar_url: z.string().nullable().optional(),
    role: z.string().optional(),
  }),
});

/**
 * `member_removed` — user kicked/left/banned. Reason: 'banned' only on ban path.
 * Server emitter: `services/control-plane/internal/members/handlers.go:594`
 * (also `members/handlers.go:721` for the ban path which sets reason='banned').
 */
export const MemberRemovedSchema = z.object({
  type: z.literal('member_removed'),
  data: z.object({
    server_id: UUID,
    user_id: UUID,
    reason: z.string().optional(),
  }),
});

/**
 * member_timeout: server member timeout set or cleared. timed_out_until is null when cleared.
 * Server emitter: services/control-plane/internal/members/handlers.go timeout endpoints.
 */
export const MemberTimeoutSchema = z.object({
  type: z.literal('member_timeout'),
  data: z.object({
    server_id: UUID,
    user_id: UUID,
    timed_out_until: ISOTimestamp.nullable().optional(),
  }),
});

/**
 * `server_updated` — TWO variants in production: hub.go emits 4 fields, servers/handlers.go
 * adds allow_embedded_content. All fields optional accommodates both.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:2340`
 * (canonical 4-field variant; extended variant in `internal/servers/handlers.go`).
 */
export const ServerUpdatedSchema = z.object({
  type: z.literal('server_updated'),
  data: z.object({
    server_id: UUID,
    name: z.string().optional(),
    icon_url: z.string().nullable().optional(),
    banner_url: z.string().nullable().optional(),
    allow_embedded_content: z.boolean().optional(),
  }),
});

/**
 * `server_deleted` — server purged.
 * Server emitter: `services/control-plane/internal/servers/handlers.go:548`
 * (`OutgoingMessage{Type: "server_deleted", ...}`).
 */
export const ServerDeletedSchema = z.object({
  type: z.literal('server_deleted'),
  data: z.object({
    server_id: UUID,
  }),
});

/**
 * `channel_updated` — channel metadata edited.
 * Server emitter: `services/control-plane/internal/channels/handlers.go:638`
 * (`OutgoingMessage{Type: "channel_updated", ...}`).
 */
export const ChannelUpdatedSchema = z.object({
  type: z.literal('channel_updated'),
  data: z.object({
    channel_id: UUID,
    server_id: UUID.optional(),
    name: z.string().optional(),
    type: z.string().optional(),
    emoji: z.string().nullable().optional(),
    audio_quality_tier: z.string().nullable().optional(),
    group_id: UUID.nullable().optional(),
  }),
});

/**
 * `channel_created` — new channel with full wire shape.
 * Server emitter: `services/control-plane/internal/channels/handlers.go:432`
 * (also `channels/handlers.go:440` for the secondary group-aware broadcast).
 */
export const ChannelCreatedSchema = z.object({
  type: z.literal('channel_created'),
  data: z.object({
    channel: ChannelPayloadSchema,
  }),
});

/**
 * `channel_deleted` — channel purged.
 * Server emitter: `services/control-plane/internal/channels/handlers.go:702`
 * (`OutgoingMessage{Type: "channel_deleted", ...}`).
 */
export const ChannelDeletedSchema = z.object({
  type: z.literal('channel_deleted'),
  data: z.object({
    channel_id: UUID,
    server_id: UUID.optional(),
  }),
});

/**
 * `channel_group_created` — new channel group.
 * Server emitter: `services/control-plane/internal/channels/groups.go:155`
 * (`OutgoingMessage{Type: "channel_group_created", ...}`).
 */
export const ChannelGroupCreatedSchema = z.object({
  type: z.literal('channel_group_created'),
  data: z.object({
    channel_group: ChannelGroupPayloadSchema,
  }),
});

/**
 * `channel_group_updated` — channel group renamed/repositioned.
 * Server emitter: `services/control-plane/internal/channels/groups.go:234`
 * (`OutgoingMessage{Type: "channel_group_updated", ...}`).
 */
export const ChannelGroupUpdatedSchema = z.object({
  type: z.literal('channel_group_updated'),
  data: z.object({
    channel_group: ChannelGroupPayloadSchema,
  }),
});

/**
 * `channel_group_deleted` — channel group purged.
 * Server emitter: `services/control-plane/internal/channels/groups.go:297`
 * (`OutgoingMessage{Type: "channel_group_deleted", ...}`).
 */
export const ChannelGroupDeletedSchema = z.object({
  type: z.literal('channel_group_deleted'),
  data: z.object({
    group_id: UUID,
    server_id: UUID.optional(),
  }),
});

/**
 * `channels_reordered` — bulk channel position update.
 * Server emitter: `services/control-plane/internal/channels/groups.go:373`
 * (`OutgoingMessage{Type: "channels_reordered", ...}`).
 */
export const ChannelsReorderedSchema = z.object({
  type: z.literal('channels_reordered'),
  data: z.object({
    server_id: UUID,
    channels: z.array(ChannelReorderEntrySchema),
  }),
});

/**
 * `unread_notify` — TWO variants: hub.go basic {channel_id, server_id}; mentions.go
 * mention-enriched {…, mentioned, mention_everyone, mention_here}. Handler reads
 * `mentioned === true` for sound + mention-count side effects.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:1351`
 * (basic variant; mention-enriched variant at `internal/websocket/mentions.go:397`).
 */
export const UnreadNotifySchema = z.object({
  type: z.literal('unread_notify'),
  data: z.object({
    channel_id: UUID.optional(),
    server_id: UUID.optional(),
    mentioned: z.boolean().optional(),
    mention_everyone: z.boolean().optional(),
    mention_here: z.boolean().optional(),
  }),
});

// ──────────── E2EE (3 events) — Task A9 (security-verified) ───────────

/**
 * `key_needed` — server requests E2EE key rewrap for new channel members.
 * Server emitter: `services/control-plane/internal/invites/handlers.go:252`.
 * Triggers `e2eeService.processPendingKeyRequests()` if E2EE is initialized.
 * Carries routing-only metadata (no key material).
 */
export const KeyNeededSchema = z.object({
  type: z.literal('key_needed'),
  data: z.object({
    server_id: UUID,
    user_id: UUID,
    channel_ids: z.array(UUID),
  }),
});

/**
 * `key_revocation` — TWO emit shapes (members/handlers.go server-channel rotation;
 * friends/handlers.go DM friend-block). All fields .optional() to accommodate both;
 * handler early-returns on missing channel_id (friend-block branch is no-op).
 * Server emitter: `services/control-plane/internal/members/handlers.go:855`
 * (channel-rotation variant; friend-block variant at `internal/friends/handlers.go:550`).
 */
export const KeyRevocationSchema = z.object({
  type: z.literal('key_revocation'),
  data: z.object({
    channel_id: UUID.optional(),
    server_id: UUID.optional(),
    revoked_epoch: z.number().int().nonnegative().optional(),
    new_epoch: z.number().int().nonnegative().optional(),
    reason: z.string().optional(),
    removed_user_id: UUID.optional(),
    blocked_user_id: UUID.optional(),
  }),
});

/**
 * `key_delivered` — invalidate-and-refetch trigger for the new wrapped key.
 * Carries routing-only metadata; the wrapped key blob is delivered out-of-band
 * via REST (channel_keys / dm_channel_keys tables). NO key material on this WS event.
 * Server emitter: `services/control-plane/internal/channels/handlers.go:1139`
 * (server-channel variant; DM variant at `internal/dm/handlers.go:1390`).
 */
export const KeyDeliveredSchema = z.object({
  type: z.literal('key_delivered'),
  data: z.object({
    channel_id: UUID,
    user_id: UUID,
  }),
});

// ──────────── Preferences sync (2 events) — Task A10 ──────────────────

/**
 * `preferences_updated` — server emits `{version: int}`; handler currently ignores
 * the body and unconditionally re-fetches via preferencesSyncService.
 * Server emitter: `services/control-plane/internal/users/handlers.go:969`
 * (`OutgoingMessage{Type: cfg.broadcastType, ...}` with `broadcastType = "preferences_updated"` at line 990).
 */
export const PreferencesUpdatedSchema = z.object({
  type: z.literal('preferences_updated'),
  data: z.object({
    version: z.number().int().positive(),
  }),
});

/**
 * `saved_gifs_updated` — same shape as preferences_updated; trigger to re-fetch saved GIFs.
 * Server emitter: `services/control-plane/internal/users/handlers.go:969`
 * (shared broadcast path; `broadcastType = "saved_gifs_updated"` configured at line 997).
 */
export const SavedGifsUpdatedSchema = z.object({
  type: z.literal('saved_gifs_updated'),
  data: z.object({
    version: z.number().int().positive(),
  }),
});

/**
 * `friend_organization_updated` — same shape as saved_gifs_updated; trigger to re-fetch
 * the encrypted friend-organization (categories) blob (#324). Server emitter shares the
 * common broadcast path with `broadcastType = "friend_organization_updated"`.
 */
export const FriendOrganizationUpdatedSchema = z.object({
  type: z.literal('friend_organization_updated'),
  data: z.object({
    version: z.number().int().positive(),
  }),
});

// ──────────── Entitlements (1) — #1297 ─────────────────

/**
 * `entitlements_changed` — server pushes the full capability set on any tier
 * change (upgrade/lateral live-update; downgrade also triggers session_revoked).
 * Server emitter: entitlements.OnTierChange via the api.EntitlementNotifier
 * adapter (services/control-plane/internal/api/entitlements_wiring.go) →
 * hub.BroadcastToUser. Wire shape = EntitlementDTO
 * (services/control-plane/internal/entitlements/dto.go) — keep in lockstep.
 */
export const EntitlementsChangedSchema = z.object({
  type: z.literal('entitlements_changed'),
  data: z.object({
    tier: z.string(),
    allowCustomScheme: z.boolean(),
    allowedAudioTiers: z.array(z.string()),
    minPtimeMs: z.number().int(),
    allowMusicMode: z.boolean(),
    maxAudioLastN: z.number().int(),
    maxVideoHeight: z.number().int(),
    maxVideoFps: z.number().int(),
    maxVideoPixelRate: z.number().int(),
    maxManualBitrateBps: z.number().int(),
    maxWebcamPublishers: z.number().int(),
    maxScreensharePublishers: z.number().int(),
    maxMessageChars: z.number().int(),
    maxAttachmentBytes: z.number().int(),
    maxAvatarBytes: z.number().int(),
    maxBannerBytes: z.number().int(),
    allowAnimatedProfile: z.boolean(),
    usernameChangeIntervalSeconds: z.number().int(),
  }),
});

// ──────────── System + envelope (5 events) — Task A11 ─────────────────

/**
 * `subscribed` — server confirms a subscribe request for a channel/server/DM.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:588`
 * (`OutgoingMessage{Type: "subscribed", ...}`).
 */
export const SubscribedSchema = z.object({
  type: z.literal('subscribed'),
  data: z.object({
    channel_id: UUID.optional(),
    server_id: UUID.optional(),
    conversation_id: UUID.optional(),
  }),
});

/**
 * `error` — server-side error. Handler branches on `data.code`; only 'epoch_revoked'
 * gets special-case handling today. A nested z.discriminatedUnion('code', ...) is
 * a follow-up (spec out-of-scope #2).
 * Server emitter: `services/control-plane/internal/websocket/hub.go:707`
 * (generic error; also `hub.go:735` for the epoch-revoked specialization).
 */
export const ErrorSchema = z.object({
  type: z.literal('error'),
  data: z.object({
    code: z.string().optional(),
    channel_id: UUID.optional(),
    current_epoch: z.number().int().nonnegative().optional(),
  }),
});

/**
 * `session_revoked` — server forcefully terminated the session. Body is intentionally empty.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:2215`
 * (`OutgoingMessage{Type: sessionRevoked, ...}` where `sessionRevoked = "session_revoked"`
 * at `hub.go:40`; also re-emitted at `hub.go:2243`).
 */
export const SessionRevokedSchema = z.object({
  type: z.literal('session_revoked'),
  data: z.object({}),
});

/**
 * `connected` — ENVELOPE-ONLY. Consumed by wsService.handleMessage's internal
 * switch, NEVER subscribed via wsService.on(). In the union because handleMessage
 * accesses message.data.client_id and message.data.user_id without casts.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:283`
 * (`OutgoingMessage{Type: "connected", Data: {client_id, user_id}}`).
 */
export const ConnectedSchema = z.object({
  type: z.literal('connected'),
  data: z.object({
    client_id: UUID,
    user_id: UUID,
  }),
});

/**
 * `connection_ready` — ENVELOPE-ONLY barrier signal. Consumed by wsService
 * internally; resolves the connection-ready promise. Body intentionally empty.
 * Server emitter: `services/control-plane/internal/websocket/hub.go:326`
 * (`OutgoingMessage{Type: "connection_ready", ...}`).
 */
export const ConnectionReadySchema = z.object({
  type: z.literal('connection_ready'),
  data: z.object({}),
});

// ════════════════════════════════════════════════════════════════════════
// 4. The discriminated union (63 schemas: 61 subscriber + 2 envelope)
// ════════════════════════════════════════════════════════════════════════

export const WebSocketEventSchema = z.discriminatedUnion('type', [
  // Chat messages (9)
  MessageSchema,
  MessageUpdateSchema,
  MessageDeleteSchema,
  MessageReactionAddedSchema,
  MessageReactionRemovedSchema,
  MessagePinnedSchema,
  MessageUnpinnedSchema,
  TypingSchema,
  MessageAckSchema,

  // Direct messages (12)
  DMMessageSchema,
  DMMessageAckSchema,
  DMMessageUpdateSchema,
  DMMessageDeleteSchema,
  DMTypingSchema,
  DMUnreadNotifySchema,
  DMConversationCreatedSchema,
  DMParticipantAddedSchema,
  DMParticipantRemovedSchema,
  DMRoleChangedSchema,
  DMGroupDeletedSchema,
  DMSubscribedSchema,

  // Friend system (4)
  FriendRequestReceivedSchema,
  FriendRequestAcceptedSchema,
  FriendRemovedSchema,
  FriendCodeClaimedSchema,

  // Voice (8)
  VoiceStateUpdateSchema,
  VoiceMoveSchema,
  ChannelAccessRevokedSchema,
  DMVoiceStateUpdateSchema,
  DMVoiceCallInvitedSchema,
  DMVoiceCallCanceledSchema,
  DMVoiceCallDeclinedSchema,
  DMVoiceCallTimedOutSchema,

  // Presence (5)
  PresenceSchema,
  PresenceSnapshotSchema,
  ServerOnlineCountsSchema,
  ServerVoiceCountsSchema,
  ProfileUpdatedSchema,

  // Rich Presence — Custom Text (#1233)
  RichPresenceUpdateSchema,
  RichPresenceClearSchema,

  // Channel + server lifecycle (13)
  MemberJoinedSchema,
  MemberRemovedSchema,
  MemberTimeoutSchema,
  ServerUpdatedSchema,
  ServerDeletedSchema,
  ChannelUpdatedSchema,
  ChannelCreatedSchema,
  ChannelDeletedSchema,
  ChannelGroupCreatedSchema,
  ChannelGroupUpdatedSchema,
  ChannelGroupDeletedSchema,
  ChannelsReorderedSchema,
  UnreadNotifySchema,

  // E2EE (3)
  KeyNeededSchema,
  KeyRevocationSchema,
  KeyDeliveredSchema,

  // Preferences sync (3)
  PreferencesUpdatedSchema,
  SavedGifsUpdatedSchema,
  FriendOrganizationUpdatedSchema,

  // Entitlements (1)
  EntitlementsChangedSchema,

  // System + envelope (5: 3 subscriber + 2 envelope-only)
  SubscribedSchema,
  ErrorSchema,
  SessionRevokedSchema,
  ConnectedSchema,
  ConnectionReadySchema,
]);
// TOTAL: 63 schemas (60 base + rich_presence ×2 #1233 + entitlements_changed #1297)

// ════════════════════════════════════════════════════════════════════════
// 5. Derived types
// ════════════════════════════════════════════════════════════════════════

export type WebSocketEvent = z.infer<typeof WebSocketEventSchema>;
export type WSEventType = WebSocketEvent['type'];
// Literal union: 'message' | 'message_update' | ... | 'connected' | 'connection_ready'

// Per-event payload aliases (used when a function takes a single payload by type)
// Chat messages
export type MessagePayload = z.infer<typeof MessageSchema>['data'];
export type MessageUpdatePayload = z.infer<typeof MessageUpdateSchema>['data'];
export type MessageDeletePayload = z.infer<typeof MessageDeleteSchema>['data'];
export type MessageReactionAddedPayload = z.infer<typeof MessageReactionAddedSchema>['data'];
export type MessageReactionRemovedPayload = z.infer<typeof MessageReactionRemovedSchema>['data'];
export type MessagePinnedPayload = z.infer<typeof MessagePinnedSchema>['data'];
export type MessageUnpinnedPayload = z.infer<typeof MessageUnpinnedSchema>['data'];
export type TypingPayload = z.infer<typeof TypingSchema>['data'];
export type MessageAckPayload = z.infer<typeof MessageAckSchema>['data'];

// Direct messages
export type DMMessagePayload = z.infer<typeof DMMessageSchema>['data'];
export type DMMessageAckPayload = z.infer<typeof DMMessageAckSchema>['data'];
export type DMMessageUpdatePayload = z.infer<typeof DMMessageUpdateSchema>['data'];
export type DMMessageDeletePayload = z.infer<typeof DMMessageDeleteSchema>['data'];
export type DMTypingPayload = z.infer<typeof DMTypingSchema>['data'];
export type DMUnreadNotifyPayload = z.infer<typeof DMUnreadNotifySchema>['data'];
export type DMConversationCreatedPayload = z.infer<typeof DMConversationCreatedSchema>['data'];
export type DMParticipantAddedPayload = z.infer<typeof DMParticipantAddedSchema>['data'];
export type DMParticipantRemovedPayload = z.infer<typeof DMParticipantRemovedSchema>['data'];
export type DMRoleChangedPayload = z.infer<typeof DMRoleChangedSchema>['data'];
export type DMGroupDeletedPayload = z.infer<typeof DMGroupDeletedSchema>['data'];
export type DMSubscribedPayload = z.infer<typeof DMSubscribedSchema>['data'];

// Friend system
export type FriendRequestReceivedPayload = z.infer<typeof FriendRequestReceivedSchema>['data'];
export type FriendRequestAcceptedPayload = z.infer<typeof FriendRequestAcceptedSchema>['data'];
export type FriendRemovedPayload = z.infer<typeof FriendRemovedSchema>['data'];
export type FriendCodeClaimedPayload = z.infer<typeof FriendCodeClaimedSchema>['data'];
export type FriendPayload = z.infer<typeof FriendPayloadSchema>;
export type FriendRequestPayload = z.infer<typeof FriendRequestPayloadSchema>;

// Voice
export type VoiceStateUpdatePayload = z.infer<typeof VoiceStateUpdateSchema>['data'];
export type VoiceMovePayload = z.infer<typeof VoiceMoveSchema>['data'];
export type ChannelAccessRevokedPayload = z.infer<typeof ChannelAccessRevokedSchema>['data'];
export type DMVoiceStateUpdatePayload = z.infer<typeof DMVoiceStateUpdateSchema>['data'];
export type DMVoiceCallInvitedPayload = z.infer<typeof DMVoiceCallInvitedSchema>['data'];
export type DMVoiceCallCanceledPayload = z.infer<typeof DMVoiceCallCanceledSchema>['data'];
export type DMVoiceCallDeclinedPayload = z.infer<typeof DMVoiceCallDeclinedSchema>['data'];
export type DMVoiceCallTimedOutPayload = z.infer<typeof DMVoiceCallTimedOutSchema>['data'];

// Presence
export type PresencePayload = z.infer<typeof PresenceSchema>['data'];
export type PresenceSnapshotPayload = z.infer<typeof PresenceSnapshotSchema>['data'];
export type ServerOnlineCountsPayload = z.infer<typeof ServerOnlineCountsSchema>['data'];
export type ServerVoiceCountsPayload = z.infer<typeof ServerVoiceCountsSchema>['data'];
export type ProfileUpdatedPayload = z.infer<typeof ProfileUpdatedSchema>['data'];

// Rich Presence — Custom Text (#1233)
export type RichPresenceUpdatePayload = z.infer<typeof RichPresenceUpdateSchema>['data'];
export type RichPresenceClearPayload = z.infer<typeof RichPresenceClearSchema>['data'];
export type CustomTextPresencePayload = z.infer<typeof CustomTextPresencePayloadSchema>;

// Channel + server lifecycle
export type MemberJoinedPayload = z.infer<typeof MemberJoinedSchema>['data'];
export type MemberRemovedPayload = z.infer<typeof MemberRemovedSchema>['data'];
export type MemberTimeoutPayload = z.infer<typeof MemberTimeoutSchema>['data'];
export type ServerUpdatedPayload = z.infer<typeof ServerUpdatedSchema>['data'];
export type ServerDeletedPayload = z.infer<typeof ServerDeletedSchema>['data'];
export type ChannelUpdatedPayload = z.infer<typeof ChannelUpdatedSchema>['data'];
export type ChannelCreatedPayload = z.infer<typeof ChannelCreatedSchema>['data'];
export type ChannelDeletedPayload = z.infer<typeof ChannelDeletedSchema>['data'];
export type ChannelGroupCreatedPayload = z.infer<typeof ChannelGroupCreatedSchema>['data'];
export type ChannelGroupUpdatedPayload = z.infer<typeof ChannelGroupUpdatedSchema>['data'];
export type ChannelGroupDeletedPayload = z.infer<typeof ChannelGroupDeletedSchema>['data'];
export type ChannelsReorderedPayload = z.infer<typeof ChannelsReorderedSchema>['data'];
export type UnreadNotifyPayload = z.infer<typeof UnreadNotifySchema>['data'];

// E2EE
export type KeyNeededPayload = z.infer<typeof KeyNeededSchema>['data'];
export type KeyRevocationPayload = z.infer<typeof KeyRevocationSchema>['data'];
export type KeyDeliveredPayload = z.infer<typeof KeyDeliveredSchema>['data'];

// Preferences sync
export type PreferencesUpdatedPayload = z.infer<typeof PreferencesUpdatedSchema>['data'];
export type SavedGifsUpdatedPayload = z.infer<typeof SavedGifsUpdatedSchema>['data'];
export type FriendOrganizationUpdatedPayload = z.infer<
  typeof FriendOrganizationUpdatedSchema
>['data'];

// Entitlements
export type EntitlementsChangedPayload = z.infer<typeof EntitlementsChangedSchema>['data'];

// System + envelope
export type SubscribedPayload = z.infer<typeof SubscribedSchema>['data'];
export type ErrorPayload = z.infer<typeof ErrorSchema>['data'];
export type SessionRevokedPayload = z.infer<typeof SessionRevokedSchema>['data'];
export type ConnectedPayload = z.infer<typeof ConnectedSchema>['data'];
export type ConnectionReadyPayload = z.infer<typeof ConnectionReadySchema>['data'];

// ════════════════════════════════════════════════════════════════════════
// 6. PII-safe issue scrubber
// ════════════════════════════════════════════════════════════════════════

/**
 * A zod issue with PII-bearing fields stripped. Used for logging schema
 * validation failures without leaking payload values (which can carry PII
 * — usernames, DM content, avatar URLs, etc.).
 *
 * @see [internal]rules/observability.md (wire-violation log discipline)
 */
export interface ScrubbedIssue {
  code: string;
  path: PropertyKey[];
  message: string; // zod's pre-formatted message — does NOT include the received value
}

/**
 * Strip PII-bearing fields from each zod issue. The returned `ScrubbedIssue[]`
 * contains only structural metadata safe for log sinks.
 *
 * Test-enforced via sentinel-string assertion in
 * client/desktop/tests/unit/types/ws-events.test.ts.
 */
export function scrubZodIssues(issues: readonly z.core.$ZodIssue[]): ScrubbedIssue[] {
  return issues.map(({ code, path, message }) => ({ code, path: [...path], message }));
}
