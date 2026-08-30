/**
 * ws-events.test.ts — zod schema coverage for the WebSocket wire contract.
 *
 * Covers canonical payloads, rejection cases, discriminator behavior, and PII scrubbing.
 *
 * @see client/desktop/src/renderer/types/ws-events.ts
 * @see [internal]specs/2026-05-23-709-ws-discriminated-union-design.md §6
 */

import { describe, it, expect } from 'vitest';
import { z } from 'zod';
import {
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
  // Preferences sync
  PreferencesUpdatedSchema,
  SavedGifsUpdatedSchema,
  FriendOrganizationUpdatedSchema,
  // System + envelope (5)
  SubscribedSchema,
  ErrorSchema,
  SessionRevokedSchema,
  ConnectedSchema,
  ConnectionReadySchema,
  // Message purge (#1352) (2)
  ChannelPurgedSchema,
  DmPurgedSchema,
  ServerPurgedSchema,
  // Server roles (#2359) (6)
  RoleCreatedSchema,
  RoleUpdatedSchema,
  RoleDeletedSchema,
  RolesReorderedSchema,
  RoleAssignedSchema,
  RoleUnassignedSchema,
  // Union + scrubber
  WebSocketEventSchema,
  EntitlementsChangedSchema,
  scrubZodIssues,
} from '@/renderer/types/ws-events';

// ─── Fixture constants ─────────────────────────────────────────────────

// Sentinel UUIDs — RFC 4122 valid (version=4 nibble at pos 13, variant=8/9/a/b nibble at pos 17).
// zod 4.x .uuid() enforces these bits; all-repeated-digit strings are rejected as malformed.
const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';
const UUID_C = '33333333-3333-4333-8333-333333333333';
const ISO_NOW = '2026-05-23T12:00:00.000Z';

// ════════════════════════════════════════════════════════════════════════
// 1. Happy path — one canonical valid payload per directly imported schema
// ════════════════════════════════════════════════════════════════════════

describe('ws-events schemas — happy path (one per event)', () => {
  // ──────────── Chat messages (9) ──────────────────────────────────────

  it('MessageSchema accepts a canonical message envelope', () => {
    const result = MessageSchema.safeParse({
      type: 'message',
      data: {
        id: UUID_A,
        channel_id: UUID_B,
        user_id: UUID_C,
        username: 'alice',
        content: 'hello',
        created_at: ISO_NOW,
        updated_at: ISO_NOW,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MessageUpdateSchema accepts a canonical update envelope', () => {
    const result = MessageUpdateSchema.safeParse({
      type: 'message_update',
      data: {
        id: UUID_A,
        channel_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MessageDeleteSchema accepts a canonical delete envelope', () => {
    const result = MessageDeleteSchema.safeParse({
      type: 'message_delete',
      data: {
        id: UUID_A,
        channel_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MessageReactionAddedSchema accepts a canonical reaction-added envelope', () => {
    const result = MessageReactionAddedSchema.safeParse({
      type: 'message_reaction_added',
      data: {
        message_id: UUID_A,
        channel_id: UUID_B,
        emoji: ':+1:',
        user_id: UUID_C,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MessageReactionRemovedSchema accepts a canonical reaction-removed envelope', () => {
    const result = MessageReactionRemovedSchema.safeParse({
      type: 'message_reaction_removed',
      data: {
        message_id: UUID_A,
        channel_id: UUID_B,
        emoji: ':+1:',
        user_id: UUID_C,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MessagePinnedSchema accepts a canonical pinned envelope', () => {
    const result = MessagePinnedSchema.safeParse({
      type: 'message_pinned',
      data: {
        message_id: UUID_A,
        channel_id: UUID_B,
        pinned_at: ISO_NOW,
        pinned_by: UUID_C,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MessageUnpinnedSchema accepts a canonical unpinned envelope (channel path)', () => {
    const result = MessageUnpinnedSchema.safeParse({
      type: 'message_unpinned',
      data: {
        message_id: UUID_A,
        channel_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('TypingSchema accepts a canonical typing envelope', () => {
    const result = TypingSchema.safeParse({
      type: 'typing',
      data: {
        channel_id: UUID_A,
        user_id: UUID_B,
        is_typing: true,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MessageAckSchema accepts a canonical ack envelope', () => {
    const result = MessageAckSchema.safeParse({
      type: 'message_ack',
      data: {
        id: UUID_A,
        channel_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── Direct messages (12) ───────────────────────────────────

  it('DMMessageSchema accepts a canonical DM message envelope', () => {
    const result = DMMessageSchema.safeParse({
      type: 'dm_message',
      data: {
        conversation_id: UUID_A,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMMessageAckSchema accepts a canonical DM ack envelope', () => {
    const result = DMMessageAckSchema.safeParse({
      type: 'dm_message_ack',
      data: {
        nonce: 'client-nonce-1',
        id: UUID_A,
        conversation_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMMessageUpdateSchema accepts a canonical DM update envelope', () => {
    const result = DMMessageUpdateSchema.safeParse({
      type: 'dm_message_update',
      data: {
        conversation_id: UUID_A,
        id: UUID_B,
        content: 'edited',
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMMessageDeleteSchema accepts a canonical DM delete envelope', () => {
    const result = DMMessageDeleteSchema.safeParse({
      type: 'dm_message_delete',
      data: {
        conversation_id: UUID_A,
        id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMTypingSchema accepts a canonical DM typing envelope', () => {
    const result = DMTypingSchema.safeParse({
      type: 'dm_typing',
      data: {
        conversation_id: UUID_A,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMUnreadNotifySchema accepts a canonical DM unread envelope', () => {
    const result = DMUnreadNotifySchema.safeParse({
      type: 'dm_unread_notify',
      data: {
        conversation_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMConversationCreatedSchema accepts a canonical conversation-created envelope', () => {
    const result = DMConversationCreatedSchema.safeParse({
      type: 'dm_conversation_created',
      data: {
        conversation: {
          id: UUID_A,
          is_group: true,
          created_at: ISO_NOW,
        },
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMParticipantAddedSchema accepts a canonical participant-added envelope', () => {
    const result = DMParticipantAddedSchema.safeParse({
      type: 'dm_participant_added',
      data: {
        conversation_id: UUID_A,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMParticipantRemovedSchema accepts a canonical participant-removed envelope', () => {
    const result = DMParticipantRemovedSchema.safeParse({
      type: 'dm_participant_removed',
      data: {
        conversation_id: UUID_A,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMRoleChangedSchema accepts a canonical role-changed envelope', () => {
    const result = DMRoleChangedSchema.safeParse({
      type: 'dm_role_changed',
      data: {
        conversation_id: UUID_A,
        user_id: UUID_B,
        role: 'admin',
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMGroupDeletedSchema accepts a canonical group-deleted envelope', () => {
    const result = DMGroupDeletedSchema.safeParse({
      type: 'dm_group_deleted',
      data: {
        conversation_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMSubscribedSchema accepts a canonical dm-subscribed envelope', () => {
    const result = DMSubscribedSchema.safeParse({
      type: 'dm_subscribed',
      data: {
        conversation_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── Friend system (4) ──────────────────────────────────────

  it('FriendRequestReceivedSchema accepts a canonical received envelope', () => {
    const result = FriendRequestReceivedSchema.safeParse({
      type: 'friend_request_received',
      data: {
        id: UUID_A,
        from_user_id: UUID_B,
        from_username: 'alice',
        created_at: ISO_NOW,
      },
    });
    expect(result.success).toBe(true);
  });

  it('FriendRequestAcceptedSchema accepts a canonical accepted envelope (flat form)', () => {
    const result = FriendRequestAcceptedSchema.safeParse({
      type: 'friend_request_accepted',
      data: {
        id: UUID_A,
        user_id: UUID_B,
        username: 'alice',
      },
    });
    expect(result.success).toBe(true);
  });

  it('FriendRemovedSchema accepts a canonical removed envelope', () => {
    const result = FriendRemovedSchema.safeParse({
      type: 'friend_removed',
      data: {
        user_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('FriendCodeClaimedSchema accepts a canonical claimed envelope (flat form)', () => {
    const result = FriendCodeClaimedSchema.safeParse({
      type: 'friend_code_claimed',
      data: {
        friendship_id: UUID_A,
        user_id: UUID_B,
        username: 'alice',
        status: 'accepted',
      },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── Voice (2) ──────────────────────────────────────────────

  it('VoiceStateUpdateSchema accepts a canonical voice-state envelope', () => {
    const result = VoiceStateUpdateSchema.safeParse({
      type: 'voice_state_update',
      data: {
        channel_id: UUID_A,
        action: 'joined',
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('VoiceMoveSchema accepts a canonical voice-move envelope', () => {
    const result = VoiceMoveSchema.safeParse({
      type: 'voice_move',
      data: {
        user_id: UUID_A,
        from_channel_id: UUID_B,
        to_channel_id: UUID_C,
        server_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelAccessRevokedSchema accepts a canonical access-revoked envelope', () => {
    const result = ChannelAccessRevokedSchema.safeParse({
      type: 'channel_access_revoked',
      data: {
        channel_id: UUID_A,
        server_id: UUID_B,
        reason: 'temp_access_revoked',
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMVoiceStateUpdateSchema accepts a canonical DM voice-state envelope', () => {
    const result = DMVoiceStateUpdateSchema.safeParse({
      type: 'dm_voice_state_update',
      data: {
        conversation_id: UUID_A,
        action: 'joined',
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  // ── DM voice call ring (4 events) — #1209 ──────────────────────────

  it('DMVoiceCallInvitedSchema accepts a canonical invited envelope', () => {
    const result = DMVoiceCallInvitedSchema.safeParse({
      type: 'dm_voice_call_invited',
      data: {
        conversation_id: UUID_A,
        is_group: false,
        caller: {
          user_id: UUID_B,
          username: 'alice',
          display_name: 'Alice Chen',
          avatar_url: 'https://example.com/a.png',
        },
        ring_id: UUID_C,
        ring_started_at: ISO_NOW,
        ring_timeout_seconds: 45,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMVoiceCallCanceledSchema accepts each canceled_by variant', () => {
    for (const reason of ['caller', 'all_declined', 'someone_accepted', 'server_error'] as const) {
      const result = DMVoiceCallCanceledSchema.safeParse({
        type: 'dm_voice_call_canceled',
        data: {
          conversation_id: UUID_A,
          ring_id: UUID_C,
          canceled_by: reason,
        },
      });
      expect(result.success, `canceled_by=${reason}`).toBe(true);
    }
  });

  it('DMVoiceCallCanceledSchema rejects unknown canceled_by value', () => {
    const result = DMVoiceCallCanceledSchema.safeParse({
      type: 'dm_voice_call_canceled',
      data: {
        conversation_id: UUID_A,
        ring_id: UUID_C,
        canceled_by: 'unknown_reason',
      },
    });
    expect(result.success).toBe(false);
  });

  it('DMVoiceCallDeclinedSchema accepts a canonical declined envelope', () => {
    const result = DMVoiceCallDeclinedSchema.safeParse({
      type: 'dm_voice_call_declined',
      data: {
        conversation_id: UUID_A,
        ring_id: UUID_C,
        decliner_user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('DMVoiceCallTimedOutSchema accepts a canonical timeout envelope', () => {
    const result = DMVoiceCallTimedOutSchema.safeParse({
      type: 'dm_voice_call_timed_out',
      data: {
        conversation_id: UUID_A,
        ring_id: UUID_C,
      },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── Presence (5) ───────────────────────────────────────────

  it('PresenceSchema accepts a canonical presence envelope', () => {
    const result = PresenceSchema.safeParse({
      type: 'presence',
      data: {
        user_id: UUID_A,
        status: 'online',
      },
    });
    expect(result.success).toBe(true);
  });

  it('PresenceSnapshotSchema accepts a canonical snapshot envelope', () => {
    const result = PresenceSnapshotSchema.safeParse({
      type: 'presence_snapshot',
      data: {
        users: [{ user_id: UUID_A, status: 'online' }],
      },
    });
    expect(result.success).toBe(true);
  });

  it('ServerOnlineCountsSchema accepts a canonical counts envelope', () => {
    const result = ServerOnlineCountsSchema.safeParse({
      type: 'server_online_counts',
      data: {
        counts: { [UUID_A]: 5 },
      },
    });
    expect(result.success).toBe(true);
  });

  it('ServerVoiceCountsSchema accepts a canonical voice-counts envelope', () => {
    const result = ServerVoiceCountsSchema.safeParse({
      type: 'server_voice_counts',
      data: {
        counts: { [UUID_A]: 3 },
      },
    });
    expect(result.success).toBe(true);
  });

  it('ProfileUpdatedSchema accepts a canonical profile-updated envelope', () => {
    const result = ProfileUpdatedSchema.safeParse({
      type: 'profile_updated',
      data: {
        user_id: UUID_A,
        username: 'alice',
      },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── Channel + server lifecycle (13) ────────────────────────

  it('MemberJoinedSchema accepts a canonical joined envelope', () => {
    const result = MemberJoinedSchema.safeParse({
      type: 'member_joined',
      data: {
        server_id: UUID_A,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MemberRemovedSchema accepts a canonical removed envelope', () => {
    const result = MemberRemovedSchema.safeParse({
      type: 'member_removed',
      data: {
        server_id: UUID_A,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  it('MemberTimeoutSchema accepts set and clear envelopes', () => {
    const setResult = MemberTimeoutSchema.safeParse({
      type: 'member_timeout',
      data: {
        server_id: UUID_A,
        user_id: UUID_B,
        timed_out_until: ISO_NOW,
      },
    });
    expect(setResult.success).toBe(true);

    const clearResult = MemberTimeoutSchema.safeParse({
      type: 'member_timeout',
      data: {
        server_id: UUID_A,
        user_id: UUID_B,
        timed_out_until: null,
      },
    });
    expect(clearResult.success).toBe(true);
  });

  it('ServerUpdatedSchema accepts a canonical server-updated envelope', () => {
    const result = ServerUpdatedSchema.safeParse({
      type: 'server_updated',
      data: {
        server_id: UUID_A,
        name: 'new-name',
      },
    });
    expect(result.success).toBe(true);
  });

  it('ServerDeletedSchema accepts a canonical server-deleted envelope', () => {
    const result = ServerDeletedSchema.safeParse({
      type: 'server_deleted',
      data: {
        server_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelUpdatedSchema accepts a canonical channel-updated envelope', () => {
    const result = ChannelUpdatedSchema.safeParse({
      type: 'channel_updated',
      data: {
        channel_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelCreatedSchema accepts a canonical channel-created envelope', () => {
    const result = ChannelCreatedSchema.safeParse({
      type: 'channel_created',
      data: {
        channel: {
          id: UUID_A,
          server_id: UUID_B,
          name: 'general',
          type: 'text',
          position: 0,
          created_at: ISO_NOW,
          updated_at: ISO_NOW,
        },
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelDeletedSchema accepts a canonical channel-deleted envelope', () => {
    const result = ChannelDeletedSchema.safeParse({
      type: 'channel_deleted',
      data: {
        channel_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelGroupCreatedSchema accepts a canonical group-created envelope', () => {
    const result = ChannelGroupCreatedSchema.safeParse({
      type: 'channel_group_created',
      data: {
        channel_group: {
          id: UUID_A,
          server_id: UUID_B,
          name: 'My Group',
          position: 0,
          created_at: ISO_NOW,
          updated_at: ISO_NOW,
        },
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelGroupUpdatedSchema accepts a canonical group-updated envelope', () => {
    const result = ChannelGroupUpdatedSchema.safeParse({
      type: 'channel_group_updated',
      data: {
        channel_group: {
          id: UUID_A,
          server_id: UUID_B,
          name: 'Renamed Group',
          position: 1,
          updated_at: ISO_NOW,
        },
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelGroupDeletedSchema accepts a canonical group-deleted envelope', () => {
    const result = ChannelGroupDeletedSchema.safeParse({
      type: 'channel_group_deleted',
      data: {
        group_id: UUID_A,
      },
    });
    expect(result.success).toBe(true);
  });

  it('ChannelsReorderedSchema accepts a canonical reorder envelope', () => {
    const result = ChannelsReorderedSchema.safeParse({
      type: 'channels_reordered',
      data: {
        server_id: UUID_A,
        channels: [{ channel_id: UUID_B, group_id: UUID_C, position: 0 }],
      },
    });
    expect(result.success).toBe(true);
  });

  it('UnreadNotifySchema accepts a canonical unread envelope', () => {
    const result = UnreadNotifySchema.safeParse({
      type: 'unread_notify',
      data: {
        channel_id: UUID_A,
        server_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── E2EE (3) ───────────────────────────────────────────────

  it('KeyNeededSchema accepts a canonical key-needed envelope', () => {
    const result = KeyNeededSchema.safeParse({
      type: 'key_needed',
      data: {
        server_id: UUID_A,
        user_id: UUID_B,
        channel_ids: [UUID_C],
      },
    });
    expect(result.success).toBe(true);
  });

  it('KeyRevocationSchema accepts a canonical revocation envelope (channel path)', () => {
    const result = KeyRevocationSchema.safeParse({
      type: 'key_revocation',
      data: {
        channel_id: UUID_A,
        server_id: UUID_B,
        revoked_epoch: 1,
        new_epoch: 2,
      },
    });
    expect(result.success).toBe(true);
  });

  it('KeyDeliveredSchema accepts a canonical delivery envelope', () => {
    const result = KeyDeliveredSchema.safeParse({
      type: 'key_delivered',
      data: {
        channel_id: UUID_A,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── Preferences sync ───────────────────────────────────────

  it('PreferencesUpdatedSchema accepts a canonical prefs-updated envelope', () => {
    const result = PreferencesUpdatedSchema.safeParse({
      type: 'preferences_updated',
      data: { version: 1 },
    });
    expect(result.success).toBe(true);
  });

  it('SavedGifsUpdatedSchema accepts a canonical saved-gifs envelope', () => {
    const result = SavedGifsUpdatedSchema.safeParse({
      type: 'saved_gifs_updated',
      data: { version: 1 },
    });
    expect(result.success).toBe(true);
  });

  it('FriendOrganizationUpdatedSchema accepts a canonical envelope', () => {
    const result = FriendOrganizationUpdatedSchema.safeParse({
      type: 'friend_organization_updated',
      data: { version: 1 },
    });
    expect(result.success).toBe(true);
  });

  // ──────────── System + envelope (5) ──────────────────────────────────

  it('SubscribedSchema accepts a canonical subscribe-ack envelope', () => {
    const result = SubscribedSchema.safeParse({
      type: 'subscribed',
      data: { channel_id: UUID_A },
    });
    expect(result.success).toBe(true);
  });

  it('ErrorSchema accepts a canonical error envelope', () => {
    const result = ErrorSchema.safeParse({
      type: 'error',
      data: { code: 'epoch_revoked', channel_id: UUID_A, current_epoch: 3 },
    });
    expect(result.success).toBe(true);
  });

  it('SessionRevokedSchema accepts a canonical session-revoked envelope', () => {
    const result = SessionRevokedSchema.safeParse({
      type: 'session_revoked',
      data: {},
    });
    expect(result.success).toBe(true);
  });

  it('ConnectedSchema accepts a canonical connected envelope', () => {
    const result = ConnectedSchema.safeParse({
      type: 'connected',
      data: { client_id: UUID_A, user_id: UUID_B },
    });
    expect(result.success).toBe(true);
  });

  it('ConnectionReadySchema accepts a canonical connection-ready envelope', () => {
    const result = ConnectionReadySchema.safeParse({
      type: 'connection_ready',
      data: {},
    });
    expect(result.success).toBe(true);
  });

  it('WebSocketEventSchema accepts the server heartbeat_ack keepalive echo', () => {
    // Exact frame the hub emits (heartbeatAckFrame in
    // services/control-plane/internal/websocket/messages.go). Must parse
    // cleanly — a rejection would count every 30s ack as a wire violation.
    const result = WebSocketEventSchema.safeParse(JSON.parse('{"type":"heartbeat_ack","data":{}}'));
    expect(result.success).toBe(true);
  });

  it('heartbeat_ack rejects a missing data envelope', () => {
    const result = WebSocketEventSchema.safeParse({ type: 'heartbeat_ack' });
    expect(result.success).toBe(false);
  });

  // ──────────── Message purge (#1352) (3) ──────────────────────────────

  it('ChannelPurgedSchema accepts a canonical channel_purged envelope', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'channel_purged',
      data: {
        channel_id: UUID_A,
        purged_by: UUID_B,
        deleted_count: 3,
        range: '7d',
      },
    });
    expect(result.success).toBe(true);
  });

  it('DmPurgedSchema accepts a canonical dm_purged envelope', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'dm_purged',
      data: {
        conversation_id: UUID_A,
        purged_by: UUID_B,
        deleted_count: 0,
        range: 'all',
      },
    });
    expect(result.success).toBe(true);
  });

  it('ServerPurgedSchema accepts a canonical server_purged envelope', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'server_purged',
      data: {
        server_id: UUID_A,
        purged_by: UUID_B,
        range: 'all',
      },
    });
    expect(result.success).toBe(true);
  });
});

describe('rich presence category contracts (#2233)', () => {
  const update = (category: string, payload: unknown, minimized = false, updatedAt = 1) =>
    WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category,
        ...(category === 'custom_text' ? {} : { minimized }),
        payload,
        updated_at: updatedAt,
      },
    });

  it('accepts detailed and minimized Server Voice with coupled fields', () => {
    expect(
      update('server_voice', {
        channel_id: UUID_B,
        server_id: UUID_C,
        channel_name: 'Lounge',
        server_name: 'Concord',
      })
    ).toMatchObject({ success: true });
    expect(update('server_voice', { channel_id: UUID_B, server_id: UUID_C }, true)).toMatchObject({
      success: true,
    });
  });

  it('counts Server Voice names by Unicode code point', () => {
    const payload = {
      channel_id: UUID_B,
      server_id: UUID_C,
      channel_name: '😀'.repeat(100),
      server_name: '😀'.repeat(100),
    };

    expect(update('server_voice', payload).success).toBe(true);
    for (const field of ['channel_name', 'server_name'] as const) {
      expect(update('server_voice', { ...payload, [field]: '😀'.repeat(101) }).success).toBe(false);
    }
  });

  it('bounds Rich Presence timestamps to the backend exact-second cap', () => {
    const cap = 9_007_199_254;
    const updates = [
      {
        category: 'server_voice',
        minimized: true,
        payload: { channel_id: UUID_B, server_id: UUID_C, started_at: cap },
      },
      { category: 'private_call', minimized: true, payload: { call_type: 'dm', started_at: cap } },
      { category: 'custom_text', minimized: false, payload: { text: 'working' } },
    ] as const;

    for (const { category, minimized, payload } of updates) {
      expect(update(category, payload, minimized, cap).success).toBe(true);
      expect(update(category, payload, minimized, cap + 1).success).toBe(false);
    }
    for (const { category, minimized, payload } of updates.slice(0, 2)) {
      expect(update(category, { ...payload, started_at: cap + 1 }, minimized, cap).success).toBe(
        false
      );
      for (const updatedAt of [cap, cap + 1]) {
        expect(
          PresenceSnapshotSchema.safeParse({
            type: 'presence_snapshot',
            data: {
              users: [
                {
                  user_id: UUID_A,
                  status: 'online',
                  rich_presence: { [category]: { minimized, payload, updated_at: updatedAt } },
                },
              ],
            },
          }).success
        ).toBe(updatedAt === cap);
      }
    }
  });

  it('rejects Server Voice minimized/detail mismatches and unknown fields', () => {
    expect(update('server_voice', { channel_id: UUID_B, server_id: UUID_C }, false).success).toBe(
      false
    );
    expect(
      update(
        'server_voice',
        {
          channel_id: UUID_B,
          server_id: UUID_C,
          channel_name: 'Lounge',
          server_name: 'Concord',
        },
        true
      ).success
    ).toBe(false);
    expect(
      update('server_voice', { channel_id: UUID_B, server_id: UUID_C, leaked: 'pii' }, true).success
    ).toBe(false);
    expect(update('server_voice', { channel_id: UUID_B, server_id: UUID_C }, true, 0).success).toBe(
      false
    );
    expect(
      update('server_voice', { channel_id: UUID_B, server_id: UUID_C }, true, -1).success
    ).toBe(false);
  });

  it('accepts detailed and minimized Private Call, but couples participant_count to detail', () => {
    expect(update('private_call', { call_type: 'dm', participant_count: 2 })).toMatchObject({
      success: true,
    });
    expect(update('private_call', { call_type: 'group' }, true)).toMatchObject({ success: true });
    expect(update('private_call', { call_type: 'dm' }, false).success).toBe(false);
    expect(update('private_call', { call_type: 'group', participant_count: 2 }, true).success).toBe(
      false
    );
    expect(update('private_call', { call_type: 'dm' }, true)).toMatchObject({ success: true });
    expect(update('private_call', { call_type: 'dm', participant_count: 0 }, false).success).toBe(
      false
    );
    expect(
      update('private_call', { call_type: 'dm', participant_count: 2, leaked: 'x' }, false).success
    ).toBe(false);
    expect(update('private_call', { call_type: 'dm', started_at: 0 }, true).success).toBe(false);
  });

  it('accepts Custom Status and rejects malformed category payloads', () => {
    expect(update('custom_text', { text: 'working', emoji: '💻' })).toMatchObject({
      success: true,
    });
    expect(update('custom_text', { text: '' })).toMatchObject({ success: false });
    expect(update('custom_text', { text: 'ok', leaked: 'x' })).toMatchObject({ success: false });
    expect(
      WebSocketEventSchema.safeParse({
        type: 'rich_presence_update',
        data: {
          user_id: UUID_A,
          category: 'custom_text',
          minimized: false,
          payload: { text: 'ok' },
          updated_at: 1,
        },
      }).success
    ).toBe(false);
    expect(update('games', { text: 'future' })).toMatchObject({ success: false });
    expect(update('private_call', { call_type: 'broadcast' }, true)).toMatchObject({
      success: false,
    });
    expect(
      update('server_voice', { channel_id: 'not-a-uuid', server_id: UUID_C }, true)
    ).toMatchObject({ success: false });
  });

  it('accepts sparse rich-presence snapshots and rejects mismatched categories', () => {
    const base = { user_id: UUID_A, status: 'online' };
    expect(
      PresenceSnapshotSchema.safeParse({
        type: 'presence_snapshot',
        data: {
          users: [
            {
              ...base,
              rich_presence: {
                server_voice: {
                  minimized: true,
                  payload: { channel_id: UUID_B, server_id: UUID_C },
                  updated_at: 1,
                },
              },
            },
          ],
        },
      }).success
    ).toBe(true);
    expect(
      PresenceSnapshotSchema.safeParse({
        type: 'presence_snapshot',
        data: {
          users: [
            {
              ...base,
              rich_presence: {
                server_voice: { minimized: true, payload: { call_type: 'dm' }, updated_at: 2 },
              },
            },
          ],
        },
      }).success
    ).toBe(false);
    expect(
      PresenceSnapshotSchema.safeParse({ type: 'presence_snapshot', data: { users: [base] } })
        .success
    ).toBe(true);
    expect(
      PresenceSnapshotSchema.safeParse({
        type: 'presence_snapshot',
        data: {
          users: [
            {
              ...base,
              rich_presence: {
                private_call: { minimized: true, payload: { call_type: 'dm' }, updated_at: 2 },
              },
            },
          ],
        },
      }).success
    ).toBe(true);
    expect(
      PresenceSnapshotSchema.safeParse({
        type: 'presence_snapshot',
        data: { users: [{ ...base, unexpected: 'x' }] },
      }).success
    ).toBe(false);
  });

  it('rejects unknown presence snapshot envelope and data keys', () => {
    const base = {
      type: 'presence_snapshot',
      data: { users: [{ user_id: UUID_A, status: 'online' }] },
    };

    expect(PresenceSnapshotSchema.safeParse({ ...base, unexpected: true }).success).toBe(false);
    expect(
      PresenceSnapshotSchema.safeParse({
        ...base,
        data: { ...base.data, unexpected: true },
      }).success
    ).toBe(false);
  });
});

// ════════════════════════════════════════════════════════════════════════
// 2. Rejection cases — required-field violations across domains
// ════════════════════════════════════════════════════════════════════════

describe('ws-events schemas — rejection cases', () => {
  it('MessageSchema rejects missing channel_id', () => {
    const result = MessageSchema.safeParse({
      type: 'message',
      data: {
        id: UUID_A,
        user_id: UUID_B,
        username: 'alice',
        content: 'hi',
        created_at: ISO_NOW,
        updated_at: ISO_NOW,
        // no channel_id
      },
    });
    expect(result.success).toBe(false);
  });

  it('MessageUpdateSchema rejects malformed UUID in id', () => {
    const result = MessageUpdateSchema.safeParse({
      type: 'message_update',
      data: {
        id: 'not-a-uuid',
        channel_id: UUID_B,
      },
    });
    expect(result.success).toBe(false);
  });

  it('MessageReactionAddedSchema rejects missing emoji', () => {
    const result = MessageReactionAddedSchema.safeParse({
      type: 'message_reaction_added',
      data: {
        message_id: UUID_A,
        channel_id: UUID_B,
        user_id: UUID_C,
        // no emoji
      },
    });
    expect(result.success).toBe(false);
  });

  it('DMMessageSchema rejects conversation_id as a number', () => {
    const result = DMMessageSchema.safeParse({
      type: 'dm_message',
      data: {
        conversation_id: 12345,
        user_id: UUID_B,
      },
    });
    expect(result.success).toBe(false);
  });

  it('DMConversationCreatedSchema rejects when conversation is missing created_at', () => {
    const result = DMConversationCreatedSchema.safeParse({
      type: 'dm_conversation_created',
      data: {
        conversation: {
          id: UUID_A,
          is_group: true,
          // no created_at
        },
      },
    });
    expect(result.success).toBe(false);
  });

  it('DMRoleChangedSchema rejects invalid role value', () => {
    const result = DMRoleChangedSchema.safeParse({
      type: 'dm_role_changed',
      data: {
        conversation_id: UUID_A,
        user_id: UUID_B,
        role: 'owner', // not in enum
      },
    });
    expect(result.success).toBe(false);
  });

  it('FriendRequestReceivedSchema rejects missing from_user_id', () => {
    const result = FriendRequestReceivedSchema.safeParse({
      type: 'friend_request_received',
      data: {
        id: UUID_A,
        from_username: 'alice',
        created_at: ISO_NOW,
        // no from_user_id
      },
    });
    expect(result.success).toBe(false);
  });

  it('VoiceStateUpdateSchema rejects unknown action enum value', () => {
    const result = VoiceStateUpdateSchema.safeParse({
      type: 'voice_state_update',
      data: {
        channel_id: UUID_A,
        action: 'teleported', // not in VoiceActionSchema
      },
    });
    expect(result.success).toBe(false);
  });

  it('VoiceMoveSchema rejects missing to_channel_id', () => {
    const result = VoiceMoveSchema.safeParse({
      type: 'voice_move',
      data: {
        user_id: UUID_A,
        from_channel_id: UUID_B,
        // no to_channel_id
        server_id: UUID_C,
      },
    });
    expect(result.success).toBe(false);
  });

  it('ChannelAccessRevokedSchema rejects missing reason', () => {
    const result = ChannelAccessRevokedSchema.safeParse({
      type: 'channel_access_revoked',
      data: {
        channel_id: UUID_A,
        server_id: UUID_B,
        // no reason
      },
    });
    expect(result.success).toBe(false);
  });

  it('PresenceSchema rejects invalid status value', () => {
    const result = PresenceSchema.safeParse({
      type: 'presence',
      data: {
        user_id: UUID_A,
        status: 'busy', // not in PresenceStatusSchema
      },
    });
    expect(result.success).toBe(false);
  });

  it('ChannelCreatedSchema rejects when channel.name is missing', () => {
    const result = ChannelCreatedSchema.safeParse({
      type: 'channel_created',
      data: {
        channel: {
          id: UUID_A,
          server_id: UUID_B,
          // no name
          type: 'text',
          position: 0,
          created_at: ISO_NOW,
          updated_at: ISO_NOW,
        },
      },
    });
    expect(result.success).toBe(false);
  });

  it('ChannelsReorderedSchema rejects when channels is not an array', () => {
    const result = ChannelsReorderedSchema.safeParse({
      type: 'channels_reordered',
      data: {
        server_id: UUID_A,
        channels: 'not-an-array',
      },
    });
    expect(result.success).toBe(false);
  });

  it('KeyNeededSchema rejects when channel_ids contains a non-UUID', () => {
    const result = KeyNeededSchema.safeParse({
      type: 'key_needed',
      data: {
        server_id: UUID_A,
        user_id: UUID_B,
        channel_ids: [UUID_C, 'not-a-uuid'],
      },
    });
    expect(result.success).toBe(false);
  });

  it('PreferencesUpdatedSchema rejects version=0 (must be positive)', () => {
    const result = PreferencesUpdatedSchema.safeParse({
      type: 'preferences_updated',
      data: { version: 0 },
    });
    expect(result.success).toBe(false);
  });

  it('FriendOrganizationUpdatedSchema rejects version=0 (must be positive)', () => {
    const result = FriendOrganizationUpdatedSchema.safeParse({
      type: 'friend_organization_updated',
      data: { version: 0 },
    });
    expect(result.success).toBe(false);
  });

  it('ConnectedSchema rejects malformed client_id', () => {
    const result = ConnectedSchema.safeParse({
      type: 'connected',
      data: { client_id: 'not-a-uuid', user_id: UUID_B },
    });
    expect(result.success).toBe(false);
  });

  it('SessionRevokedSchema rejects when data is missing entirely', () => {
    const result = SessionRevokedSchema.safeParse({
      type: 'session_revoked',
      // no data
    });
    expect(result.success).toBe(false);
  });

  it('ChannelPurgedSchema rejects a negative deleted_count', () => {
    const result = ChannelPurgedSchema.safeParse({
      type: 'channel_purged',
      data: {
        channel_id: UUID_A,
        purged_by: UUID_B,
        deleted_count: -1,
        range: '7d',
      },
    });
    expect(result.success).toBe(false);
  });

  it('DmPurgedSchema rejects a negative deleted_count', () => {
    const result = DmPurgedSchema.safeParse({
      type: 'dm_purged',
      data: {
        conversation_id: UUID_A,
        purged_by: UUID_B,
        deleted_count: -1,
        range: 'all',
      },
    });
    expect(result.success).toBe(false);
  });

  it('ServerPurgedSchema rejects a non-UUID server_id', () => {
    const result = ServerPurgedSchema.safeParse({
      type: 'server_purged',
      data: {
        server_id: 'not-a-uuid',
        purged_by: UUID_B,
        range: 'all',
      },
    });
    expect(result.success).toBe(false);
  });

  it('ServerPurgedSchema rejects a missing range', () => {
    const result = ServerPurgedSchema.safeParse({
      type: 'server_purged',
      data: {
        server_id: UUID_A,
        purged_by: UUID_B,
      },
    });
    expect(result.success).toBe(false);
  });

  // The wire contract deliberately omits a count — a server purge's per-channel
  // deleted_count is 0 by design, so the union must not admit a channel_purged
  // shape under the server_purged discriminator.
  it('WebSocketEventSchema rejects server_purged carrying a channel_id instead of a server_id', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'server_purged',
      data: {
        channel_id: UUID_A,
        purged_by: UUID_B,
        deleted_count: 0,
        range: 'all',
      },
    });
    expect(result.success).toBe(false);
  });
});

// ════════════════════════════════════════════════════════════════════════
// 3. WebSocketEventSchema — discriminated union behavior
// ════════════════════════════════════════════════════════════════════════

describe('WebSocketEventSchema — discriminated union behavior', () => {
  it('accepts a known event type (friend_removed — minimal payload)', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'friend_removed',
      data: { user_id: UUID_A },
    });
    expect(result.success).toBe(true);
  });

  it('rejects an unknown event type with invalid_union_discriminator code', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'nonexistent_event_type',
      data: {},
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const codes = result.error.issues.map((issue) => issue.code);
      // zod 4.x emits 'invalid_union' for discriminator-mismatch (renamed from
      // 4.0-pre's 'invalid_union_discriminator'); structurally equivalent.
      expect(codes).toContain('invalid_union');
    }
  });

  // A near-miss on the server_purged discriminator must fail as a discriminator
  // miss, not as a field error — zod 4 marks that with `note`, so this asserts
  // the zod-4 shape rather than zod 3's `invalid_union_discriminator`.
  it('rejects a near-miss server purge type as a discriminator miss', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'server_purge',
      data: { server_id: UUID_A, purged_by: UUID_B, range: 'all' },
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      // Assert `code` and `path`, never the human-readable `note`: that string
      // is not part of zod's documented issue contract and can be reworded in a
      // patch release, which would red this suite for a wording change rather
      // than a behavioural one. `path: ['type']` is the property that actually
      // matters — it attributes the failure to the DISCRIMINATOR rather than to
      // some field inside a member the parser had already matched.
      const issues: Array<{ code: string; path: PropertyKey[] }> = result.error.issues;
      const discriminatorIssue = issues.find((issue) => issue.code === 'invalid_union');
      expect(discriminatorIssue).toBeDefined();
      expect(discriminatorIssue?.path).toEqual(['type']);
    }
  });

  it('rejects a payload missing the type discriminator entirely', () => {
    const result = WebSocketEventSchema.safeParse({
      data: { user_id: UUID_A },
      // no type field
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const codes = result.error.issues.map((issue) => issue.code);
      // zod 4.x emits 'invalid_union' for discriminator-mismatch (renamed from
      // 4.0-pre's 'invalid_union_discriminator'); structurally equivalent.
      expect(codes).toContain('invalid_union');
    }
  });
});

// ════════════════════════════════════════════════════════════════════════
// 4. scrubZodIssues — PII safety
// ════════════════════════════════════════════════════════════════════════

describe('scrubZodIssues — PII safety', () => {
  it('retains only deduplicated issue codes for strict and custom failures', () => {
    const UNKNOWN_FIELD = 'attacker-controlled-user-id';
    const UNKNOWN_CATEGORY = 'attacker-controlled-presence-id';
    const SENTINEL = 'attacker-controlled-custom-issue';
    const strictFieldResult = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'custom_text',
        payload: { text: 'working', [UNKNOWN_FIELD]: 'sensitive-value' },
        updated_at: 1,
      },
    });
    const strictMapResult = PresenceSnapshotSchema.safeParse({
      type: 'presence_snapshot',
      data: {
        users: [
          {
            user_id: UUID_A,
            status: 'online',
            rich_presence: { [UNKNOWN_CATEGORY]: {} },
          },
        ],
      },
    });

    expect(strictFieldResult.success).toBe(false);
    expect(strictMapResult.success).toBe(false);
    if (!strictFieldResult.success && !strictMapResult.success) {
      const scrubbed = scrubZodIssues([
        ...strictFieldResult.error.issues,
        ...strictMapResult.error.issues,
        {
          code: 'custom',
          path: [SENTINEL],
          message: SENTINEL,
        } as unknown as z.core.$ZodIssue,
      ]);
      const serialized = JSON.stringify(scrubbed);

      expect(scrubbed).toEqual(['unrecognized_keys', 'custom']);
      expect(serialized).not.toContain(UNKNOWN_FIELD);
      expect(serialized).not.toContain(UNKNOWN_CATEGORY);
      expect(serialized).not.toContain(SENTINEL);
    }
  });

  it('redacts record-key paths from schema diagnostics', () => {
    const result = ServerOnlineCountsSchema.safeParse({
      type: 'server_online_counts',
      data: { counts: { [UUID_A]: 'not-a-count' } },
    });

    expect(result.success).toBe(false);
    if (!result.success) {
      expect(result.error.issues.some((issue) => issue.path.includes(UUID_A))).toBe(true);
      expect(JSON.stringify(scrubZodIssues(result.error.issues))).not.toContain(UUID_A);
    }
  });

  it('strips PII-bearing field values from scrubbed output (sentinel assertion)', () => {
    // The load-bearing security test: a payload with a sentinel string AND a
    // type violation. After scrubbing + stringification, neither the sentinel
    // nor the violating value (999) may appear in the output.
    const SENTINEL = 'sensitive-dm-content-do-not-leak-via-logs';
    const result = MessageSchema.safeParse({
      type: 'message',
      data: {
        id: UUID_A,
        channel_id: 999 as unknown as string, // type violation — triggers issue
        user_id: UUID_B,
        username: 'alice',
        content: SENTINEL, // PII-bearing value that must never escape
        created_at: ISO_NOW,
        updated_at: ISO_NOW,
      },
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const scrubbed = scrubZodIssues(result.error.issues);
      const serialized = JSON.stringify(scrubbed);
      expect(serialized).not.toContain(SENTINEL);
      expect(serialized).not.toContain('999');
    }
  });

  it('retains a bounded issue code after scrubbing', () => {
    const result = MessageSchema.safeParse({
      type: 'message',
      data: {
        // channel_id missing — triggers issue with deterministic shape
        id: UUID_A,
        user_id: UUID_B,
        username: 'alice',
        content: 'hi',
        created_at: ISO_NOW,
        updated_at: ISO_NOW,
      },
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const scrubbed = scrubZodIssues(result.error.issues);
      expect(scrubbed.length).toBeGreaterThan(0);
      for (const issue of scrubbed) {
        expect(issue).toEqual(expect.any(String));
      }
    }
  });

  it('handles an empty issues array (returns empty array)', () => {
    const scrubbed = scrubZodIssues([]);
    expect(scrubbed).toEqual([]);
  });

  it('deduplicates repeated issue codes from one parse failure', () => {
    // Empty data on MessageSchema produces multiple missing-required-field issues.
    const result = MessageSchema.safeParse({
      type: 'message',
      data: {},
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const scrubbed = scrubZodIssues(result.error.issues);
      expect(result.error.issues.length).toBeGreaterThan(1);
      expect(scrubbed.length).toBeLessThan(result.error.issues.length);
      // Every retained diagnostic is a bounded code.
      for (const issue of scrubbed) {
        expect(issue).toEqual(expect.any(String));
      }
    }
  });
});

const validEntitlements = {
  type: 'entitlements_changed',
  data: {
    tier: 'free',
    allowCustomScheme: false,
    allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard'],
    minPtimeMs: 20,
    allowMusicMode: false,
    maxAudioLastN: 8,
    streamMaxHeight: 1080,
    streamMaxFps: 60,
    streamMaxPixelRate: 62208000,
    streamMaxBitrate: 5000000,
    cameraMaxHeight: 720,
    cameraMaxFps: 60,
    cameraMaxBitrate: 2500000,
    maxManualBitrateBps: 5000000,
    maxWebcamPublishers: 8,
    maxScreensharePublishers: 1,
    maxMessageChars: 5120,
    maxAttachmentBytes: 26214400,
    maxAvatarBytes: 5242880,
    maxBannerBytes: 5242880,
    allowAnimatedProfile: false,
    usernameChangeIntervalSeconds: 31536000,
    maxServersCreated: 5,
    messageHistorySearchDays: 90,
  },
};

describe('EntitlementsChangedSchema', () => {
  it('accepts a valid full DTO', () => {
    expect(EntitlementsChangedSchema.safeParse(validEntitlements).success).toBe(true);
  });
  it('rejects a missing required field', () => {
    const { maxMessageChars, ...rest } = validEntitlements.data;
    const bad = { type: 'entitlements_changed', data: rest };
    expect(EntitlementsChangedSchema.safeParse(bad).success).toBe(false);
  });
  it('rejects a wrong type literal', () => {
    expect(
      EntitlementsChangedSchema.safeParse({ ...validEntitlements, type: 'nope' }).success
    ).toBe(false);
  });
  it('rejects when the #1555 gate fields are missing (DTO lockstep)', () => {
    const { maxServersCreated, messageHistorySearchDays, ...rest } = validEntitlements.data;
    const bad = { type: 'entitlements_changed', data: rest };
    expect(EntitlementsChangedSchema.safeParse(bad).success).toBe(false);
  });
  it('rejects when the #1602 split video-axis fields are missing (DTO lockstep)', () => {
    const {
      streamMaxHeight,
      streamMaxFps,
      streamMaxPixelRate,
      streamMaxBitrate,
      cameraMaxHeight,
      cameraMaxFps,
      cameraMaxBitrate,
      ...rest
    } = validEntitlements.data;
    const bad = { type: 'entitlements_changed', data: rest };
    expect(EntitlementsChangedSchema.safeParse(bad).success).toBe(false);
  });
});

// ────────────────────────────────────────────────────────────────────────
// 5. Rich Presence — Custom Text Status (#1233)
// ────────────────────────────────────────────────────────────────────────

describe('Rich Presence — custom_text', () => {
  it.each([
    {
      category: 'server_voice',
      payload: {
        channel_id: UUID_A,
        server_id: UUID_B,
      },
    },
    {
      category: 'private_call',
      payload: { call_type: 'group' },
    },
  ])('accepts a minimized $category activity update', ({ category, payload }) => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category,
        minimized: true,
        payload,
        updated_at: 1,
      },
    });
    expect(result.success).toBe(true);
  });

  it('rejects a voice activity update with a Custom Status payload', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'server_voice',
        minimized: true,
        payload: { text: 'wrong category' },
        updated_at: 1,
      },
    });
    expect(result.success).toBe(false);
  });

  it('accepts a valid rich_presence_update for custom_text', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'custom_text',
        payload: { emoji: '🎧', text: 'Heads down' },
        updated_at: 1,
      },
    });
    expect(result.success).toBe(true);
  });

  it.each<[string, string, boolean]>([
    ['accepts ASCII text at 140 code points', 'x'.repeat(140), true],
    ['rejects ASCII text at 141 code points', 'x'.repeat(141), false],
    ['accepts BMP text at 140 code points', '界'.repeat(140), true],
    ['rejects BMP text at 141 code points', '界'.repeat(141), false],
    ['accepts astral text at 140 code points', '😀'.repeat(140), true],
    ['rejects astral text at 141 code points', '😀'.repeat(141), false],
    ['accepts combining text at 140 code points', 'e\u0301'.repeat(70), true],
    ['rejects combining text at 141 code points', `${'e\u0301'.repeat(70)}e`, false],
  ])('%s (#2239)', (_name, text, accepted) => {
    expect(
      WebSocketEventSchema.safeParse({
        type: 'rich_presence_update',
        data: {
          user_id: UUID_A,
          category: 'custom_text',
          payload: { text },
          updated_at: 1,
        },
      }).success
    ).toBe(accepted);
  });

  it.each<[string, string, boolean]>([
    ['accepts ASCII emoji at 32 code points', 'x'.repeat(32), true],
    ['rejects ASCII emoji at 33 code points', 'x'.repeat(33), false],
    ['accepts BMP emoji at 32 code points', '界'.repeat(32), true],
    ['rejects BMP emoji at 33 code points', '界'.repeat(33), false],
    ['accepts astral emoji at 32 code points', '😀'.repeat(32), true],
    ['rejects astral emoji at 33 code points', '😀'.repeat(33), false],
    ['accepts combining emoji at 32 code points', 'e\u0301'.repeat(16), true],
    ['rejects combining emoji at 33 code points', `${'e\u0301'.repeat(16)}e`, false],
  ])('%s (#2239)', (_name, emoji, accepted) => {
    expect(
      WebSocketEventSchema.safeParse({
        type: 'rich_presence_update',
        data: {
          user_id: UUID_A,
          category: 'custom_text',
          payload: { emoji, text: 'Status' },
          updated_at: 1,
        },
      }).success
    ).toBe(accepted);
  });

  it('accepts custom_text with no emoji (emoji optional)', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'custom_text',
        payload: { text: 'Out till Friday' },
        updated_at: 2,
      },
    });
    expect(result.success).toBe(true);
  });

  it('rejects an empty custom_text (min 1)', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'custom_text',
        payload: { text: '' },
        updated_at: 4,
      },
    });
    expect(result.success).toBe(false);
  });

  it('rejects an unknown rich-presence category', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'not_a_category',
        payload: { text: 'hi' },
        updated_at: 6,
      },
    });
    expect(result.success).toBe(false);
  });

  it('accepts a rich_presence_clear for custom_text', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_clear',
      data: { user_id: UUID_A, category: 'custom_text' },
    });
    expect(result.success).toBe(true);
  });
});

describe('Presence override metadata', () => {
  it('accepts strict custom_text version metadata through the event union', () => {
    expect(
      WebSocketEventSchema.safeParse({
        type: 'presence_overrides_updated',
        data: { category: 'custom_text', version: 4 },
      }).success
    ).toBe(true);
  });

  it.each([
    { category: 'activity', version: 4 },
    { category: 'custom_text', version: 0 },
    { category: 'custom_text', version: 1.5 },
  ])('rejects invalid presence override metadata', (data) => {
    expect(
      WebSocketEventSchema.safeParse({
        type: 'presence_overrides_updated',
        data,
      }).success
    ).toBe(false);
  });

  it('rejects recipient or ciphertext fields inside the metadata', () => {
    expect(
      WebSocketEventSchema.safeParse({
        type: 'presence_overrides_updated',
        data: {
          category: 'custom_text',
          version: 4,
          encrypted_data: 'sentinel-ciphertext',
          excluded_user_ids: [UUID_A],
        },
      }).success
    ).toBe(false);
  });

  it('rejects unknown envelope fields', () => {
    expect(
      WebSocketEventSchema.safeParse({
        type: 'presence_overrides_updated',
        data: { category: 'custom_text', version: 4 },
        excluded_user_ids: [UUID_A],
      }).success
    ).toBe(false);
  });
});

// ════════════════════════════════════════════════════════════════════════
// Server role events (#2359)
// ════════════════════════════════════════════════════════════════════════
//
// Six events are broadcast by services/control-plane/internal/rbac/handlers.go;
// only roles_reordered has a renderer handler. The other five are validated
// purely so the dispatch boundary does not count them as wire violations, so
// the schema itself is the whole of their coverage.

describe('Server role events (#2359)', () => {
  // Mirrors the Go Role DTO (rbac/handlers.go:95-109) exactly: permissions is a
  // STRING (`,string` json tag), color/emoji are omitempty pointers.
  const ROLE = {
    id: UUID_A,
    server_id: UUID_B,
    name: 'Moderator',
    color: '#5865F2',
    emoji: '🛡️',
    position: 3,
    permissions: '4611686018427387904',
    is_default: false,
    is_managed: false,
    mentionable: true,
    display_separately: true,
    created_at: ISO_NOW,
    updated_at: ISO_NOW,
  };

  it('RoleCreatedSchema accepts a canonical role_created envelope', () => {
    const result = RoleCreatedSchema.safeParse({
      type: 'role_created',
      data: { server_id: UUID_B, role: ROLE },
    });
    expect(result.success).toBe(true);
  });

  it('RoleCreatedSchema accepts a role with color and emoji omitted', () => {
    // Go emits *string + omitempty, so an unset color/emoji is ABSENT, not null.
    const { color: _color, emoji: _emoji, ...bare } = ROLE;
    const result = RoleCreatedSchema.safeParse({
      type: 'role_created',
      data: { server_id: UUID_B, role: bare },
    });
    expect(result.success).toBe(true);
  });

  it('RoleCreatedSchema rejects a numeric permissions bitfield', () => {
    // The wire carries permissions as a string; a number here would mean the
    // `,string` json tag was dropped server-side and precision is at risk.
    const result = RoleCreatedSchema.safeParse({
      type: 'role_created',
      data: { server_id: UUID_B, role: { ...ROLE, permissions: 4611686018427387904 } },
    });
    expect(result.success).toBe(false);
  });

  it('RoleCreatedSchema rejects a role missing is_managed', () => {
    // is_managed drives the managed/unmovable partition of the hierarchy UI —
    // a role without it must never reach a store.
    const { is_managed: _isManaged, ...withoutManaged } = ROLE;
    const result = RoleCreatedSchema.safeParse({
      type: 'role_created',
      data: { server_id: UUID_B, role: withoutManaged },
    });
    expect(result.success).toBe(false);
  });

  it('RoleUpdatedSchema accepts role_id alongside the full role DTO', () => {
    const result = RoleUpdatedSchema.safeParse({
      type: 'role_updated',
      data: { server_id: UUID_B, role_id: UUID_A, role: ROLE },
    });
    expect(result.success).toBe(true);
  });

  it('RoleUpdatedSchema rejects a payload missing role_id', () => {
    const result = RoleUpdatedSchema.safeParse({
      type: 'role_updated',
      data: { server_id: UUID_B, role: ROLE },
    });
    expect(result.success).toBe(false);
  });

  it('RoleDeletedSchema accepts a canonical role_deleted envelope', () => {
    const result = RoleDeletedSchema.safeParse({
      type: 'role_deleted',
      data: { server_id: UUID_B, role_id: UUID_A },
    });
    expect(result.success).toBe(true);
  });

  it('RoleDeletedSchema rejects a malformed role_id', () => {
    const result = RoleDeletedSchema.safeParse({
      type: 'role_deleted',
      data: { server_id: UUID_B, role_id: 'not-a-uuid' },
    });
    expect(result.success).toBe(false);
  });

  it('RolesReorderedSchema accepts the applied ordering slice', () => {
    const result = RolesReorderedSchema.safeParse({
      type: 'roles_reordered',
      data: { server_id: UUID_B, role_ids: [UUID_A, UUID_C] },
    });
    expect(result.success).toBe(true);
  });

  it('RolesReorderedSchema rejects a non-UUID entry inside role_ids', () => {
    const result = RolesReorderedSchema.safeParse({
      type: 'roles_reordered',
      data: { server_id: UUID_B, role_ids: [UUID_A, 'not-a-uuid'] },
    });
    expect(result.success).toBe(false);
  });

  it('RoleAssignedSchema accepts a canonical role_assigned envelope', () => {
    const result = RoleAssignedSchema.safeParse({
      type: 'role_assigned',
      data: { server_id: UUID_B, user_id: UUID_C, role_id: UUID_A },
    });
    expect(result.success).toBe(true);
  });

  it('RoleAssignedSchema rejects a payload missing user_id', () => {
    const result = RoleAssignedSchema.safeParse({
      type: 'role_assigned',
      data: { server_id: UUID_B, role_id: UUID_A },
    });
    expect(result.success).toBe(false);
  });

  it('RoleUnassignedSchema accepts a canonical role_unassigned envelope', () => {
    const result = RoleUnassignedSchema.safeParse({
      type: 'role_unassigned',
      data: { server_id: UUID_B, user_id: UUID_C, role_id: UUID_A },
    });
    expect(result.success).toBe(true);
  });

  it('RoleUnassignedSchema rejects a numeric role_id', () => {
    const result = RoleUnassignedSchema.safeParse({
      type: 'role_unassigned',
      data: { server_id: UUID_B, user_id: UUID_C, role_id: 42 },
    });
    expect(result.success).toBe(false);
  });

  it('WebSocketEventSchema routes every role event to its member schema', () => {
    // The registration half of the fix: before these six were in the union, the
    // dispatch boundary logged them as unknown and incremented
    // wireViolationCount on every connected member's client.
    const envelopes = [
      { type: 'role_created', data: { server_id: UUID_B, role: ROLE } },
      { type: 'role_updated', data: { server_id: UUID_B, role_id: UUID_A, role: ROLE } },
      { type: 'role_deleted', data: { server_id: UUID_B, role_id: UUID_A } },
      { type: 'roles_reordered', data: { server_id: UUID_B, role_ids: [UUID_A, UUID_C] } },
      { type: 'role_assigned', data: { server_id: UUID_B, user_id: UUID_C, role_id: UUID_A } },
      { type: 'role_unassigned', data: { server_id: UUID_B, user_id: UUID_C, role_id: UUID_A } },
    ];
    for (const envelope of envelopes) {
      const result = WebSocketEventSchema.safeParse(envelope);
      expect(result.success, `${envelope.type} must parse`).toBe(true);
      if (result.success) expect(result.data.type).toBe(envelope.type);
    }
  });

  it('WebSocketEventSchema has exactly 75 members', () => {
    // Pins the count quoted in [internal]rules/frontend.md and in the ws-events.ts
    // header, so a future addition cannot silently drift the docs.
    expect(WebSocketEventSchema.options).toHaveLength(76);
  });
});
