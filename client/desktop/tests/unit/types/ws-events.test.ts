/**
 * ws-events.test.ts — zod schema coverage for the WebSocket wire contract.
 *
 * 4 describe-blocks: happy path (56), rejection (17), discriminator (3), PII scrubber (4).
 *
 * @see client/desktop/src/renderer/types/ws-events.ts
 * @see [internal]specs/2026-05-23-709-ws-discriminated-union-design.md §6
 */

import { describe, it, expect } from 'vitest';
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
  // Channel + server lifecycle (12)
  MemberJoinedSchema,
  MemberRemovedSchema,
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
  // Preferences sync (2)
  PreferencesUpdatedSchema,
  SavedGifsUpdatedSchema,
  FriendOrganizationUpdatedSchema,
  // System + envelope (5)
  SubscribedSchema,
  ErrorSchema,
  SessionRevokedSchema,
  ConnectedSchema,
  ConnectionReadySchema,
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
// 1. Happy path — 54 schemas, one canonical valid payload per schema
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

  // ──────────── Channel + server lifecycle (12) ────────────────────────

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

  // ──────────── Preferences sync (2) ───────────────────────────────────

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

  it('retains structural metadata (code, path, message) after scrubbing', () => {
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
        expect(issue).toHaveProperty('code');
        expect(typeof issue.code).toBe('string');
        expect(issue).toHaveProperty('path');
        expect(Array.isArray(issue.path)).toBe(true);
        expect(issue).toHaveProperty('message');
        expect(typeof issue.message).toBe('string');
      }
    }
  });

  it('handles an empty issues array (returns empty array)', () => {
    const scrubbed = scrubZodIssues([]);
    expect(scrubbed).toEqual([]);
  });

  it('handles multiple issues from one parse failure', () => {
    // Empty data on MessageSchema produces multiple missing-required-field issues.
    const result = MessageSchema.safeParse({
      type: 'message',
      data: {},
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const scrubbed = scrubZodIssues(result.error.issues);
      expect(scrubbed.length).toBeGreaterThan(1);
      // Every issue retains its three structural fields.
      for (const issue of scrubbed) {
        expect(issue.code).toBeTruthy();
        expect(Array.isArray(issue.path)).toBe(true);
        expect(typeof issue.message).toBe('string');
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
    maxVideoHeight: 1080,
    maxVideoFps: 60,
    maxVideoPixelRate: 62208000,
    maxManualBitrateBps: 5000000,
    maxWebcamPublishers: 8,
    maxScreensharePublishers: 1,
    maxMessageChars: 5120,
    maxAttachmentBytes: 26214400,
    maxAvatarBytes: 1048576,
    maxBannerBytes: 2097152,
    allowAnimatedProfile: false,
    usernameChangeIntervalSeconds: 31536000,
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
});

// ────────────────────────────────────────────────────────────────────────
// 5. Rich Presence — Custom Text Status (#1233)
// ────────────────────────────────────────────────────────────────────────

describe('Rich Presence — custom_text', () => {
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

  it('rejects custom_text over 140 characters', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'custom_text',
        payload: { text: 'x'.repeat(141) },
        updated_at: 3,
      },
    });
    expect(result.success).toBe(false);
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

  it('rejects an emoji over 32 characters', () => {
    const result = WebSocketEventSchema.safeParse({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'custom_text',
        payload: { text: 'hi', emoji: 'x'.repeat(33) },
        updated_at: 5,
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
