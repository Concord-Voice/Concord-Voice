// --- Chat Context ---

/** Identifies the rendering context so display logic can enforce isolation boundaries.
 *  Server roles apply in 'channel'/'voice'; DM-specific styling (#324) applies in 'dm'. */
export type ChatContextType = 'channel' | 'dm' | 'voice';

/** Controller-level context for routing operations (send/edit/delete/typing/pin)
 *  to the correct transport (WS method, REST endpoint) and permission model. */
export interface ChatContext {
  type: ChatContextType;
  /** channelId for channels/voice, conversationId for DMs */
  id: string;
  /** Required for RBAC permission lookups (channels/voice only) */
  serverId?: string;
}

// --- Call-event Types (for #1209 / #1219) ---

/** Status of a DM voice call as recorded in the dm_messages.call_event_payload
 *  JSONB column (plaintext server metadata; no client-side decryption). */
export type CallEventStatus = 'completed' | 'missed' | 'declined' | 'canceled';

/** Shape of dm_messages.call_event_payload for rows where type === 'call_event'.
 *  Centralized here (#1219) so the Message type, the CallEventMessage component,
 *  and the message-fetch path share one definition. */
export interface CallEventPayload {
  ring_id?: string;
  caller_user_id?: string;
  participant_user_ids?: string[];
  started_at: string;
  ended_at?: string;
  status: CallEventStatus;
  duration_seconds: number;
}

// --- Attachment Types (for #178) ---

export interface AttachmentSummary {
  id: string;
  file_type: 'photo' | 'animated' | 'video' | 'audio' | 'file';
  mime_type: string;
  file_size: number;
  /** Intrinsic image dimensions, when known. Used by the renderer to reserve
   *  the correct vertical space before the image bytes finish loading,
   *  preventing layout shift on send. Only populated for photo/animated. */
  width?: number;
  height?: number;
}

export interface Message {
  id: string;
  channel_id: string;
  user_id: string;
  content: string;
  key_version?: number;
  gif_slug?: string;
  edited_at?: string;
  reply_to_id?: string;
  pinned_at?: string;
  pinned_by?: string;
  attachments?: AttachmentSummary[];
  /** Present only for system rows where type === 'call_event' (#1219). */
  type?: string;
  call_event_payload?: CallEventPayload;
  created_at: string;
  updated_at: string;
}

export interface MessageWithUser extends Message {
  username: string;
  display_name?: string;
  avatar_url?: string;
  reactions?: ReactionSummary[];
  replied_to?: RepliedToMessage;
}

/**
 * Extended message type with delivery status tracking
 * Used for local message state management
 */
export interface MessageWithStatus extends MessageWithUser {
  status?: 'pending' | 'sent' | 'delivered' | 'failed';
  clientMessageId?: string; // Temporary ID before server assigns permanent ID
  error?: string; // Error message if status is 'failed'
  decryptFailed?: boolean; // True if E2EE decryption failed (never show raw ciphertext)
  pendingKeys?: boolean; // True if channel key not yet distributed (waiting for key delivery)
  embeds_suppressed?: boolean; // True if embeds should not be rendered for this message
}

// --- Reaction Types (for #169) ---

export interface ReactionUser {
  user_id: string;
  username: string;
  display_name?: string;
}

export interface ReactionSummary {
  emoji: string;
  count: number;
  users: ReactionUser[];
  me: boolean;
}

// --- Reply Types (for #170) ---

export interface RepliedToMessage {
  id: string;
  user_id: string;
  username: string;
  display_name?: string;
  content: string;
  key_version?: number;
}

export interface Channel {
  id: string;
  server_id: string;
  name: string;
  description?: string;
  type: 'text' | 'voice' | 'bulletin';
  emoji?: string; // Optional custom emoji set by admins
  audio_quality_tier?: string | null; // Admin override: voice/standard/high/hifi/studio or null (personal)
  group_id?: string | null; // FK to channel_groups; null = uncategorized
  linked_voice_channel_id?: string | null; // Non-null = hidden text chat attached to a voice channel
  sync_permissions?: boolean;
  position: number;
  created_at: string;
  updated_at: string;
}

export interface ChannelGroup {
  id: string;
  server_id: string;
  name: string;
  position: number;
  created_at: string;
  updated_at: string;
}

export interface UpdateMessageRequest {
  content: string;
}

export interface GetMessagesResponse {
  messages: MessageWithUser[];
  count: number;
}
