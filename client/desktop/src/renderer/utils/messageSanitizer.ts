/**
 * Message sanitizer — client-side embed suppression layer (Layer 3 of 3).
 *
 * Embed suppression follows a three-layer trust model with a one-way ratchet:
 *
 * LAYER 1 — SERVER POLICY (highest trust, only entity that can ALLOW embeds):
 *   The server stamps `embeds_suppressed` on every message at send time based
 *   on its `allow_embedded_content` setting. Server policy ON → flag false
 *   (allowed). Server policy OFF → flag true (suppressed). The server is the
 *   ONLY entity trusted to set the flag to "allow."
 *
 * LAYER 2 — MODERATOR (one-way ratchet: suppress only, never un-suppress):
 *   Users with PermManageAllMessages can flip a message's flag from false → true.
 *   Once suppressed, it stays suppressed — moderators cannot flip true → false.
 *
 * LAYER 3 — CLIENT USER PREFERENCE (this layer — local-only, one-way ratchet):
 *   If the user has "Allow Embedded Content" OFF (the default), the client
 *   forces embeds_suppressed = true at the store boundary, regardless of what
 *   the server sent. This is a local preference that only makes things more
 *   restrictive, never less. If the user allows embeds but the flag says
 *   suppressed, the client respects the flag and doesn't render.
 *
 * The flag is NOT an indicator of whether a message contains embeddable content.
 * It is a policy enforcement flag: "if there happens to be embeddable content,
 * should it render?" This avoids leaking metadata about message content.
 *
 * KEY PRINCIPLE: once suppressed, only the server policy can un-suppress
 * (and only on NEW messages). Clients and users can only make it more
 * restrictive, never less. We trust the server, but never the client or users.
 */

import type { MessageWithStatus } from '../types/chat';

/**
 * Sanitize a single message's embed flag based on the user's local preference.
 * Called at every message entry point (addMessage, updateMessage, setMessages, prependMessages).
 *
 * The caller is responsible for reading the privacy store and passing the current
 * `allowEmbeddedContent` setting — this keeps the utils/ layer free of store imports.
 *
 * Rules:
 * 1. User allowEmbeddedContent OFF → force embeds_suppressed = true (local override)
 * 2. Server/moderator set embeds_suppressed = true → preserve it (never un-suppress)
 * 3. Only when user allows AND the server flag allows → embeds may render
 */
export function sanitizeMessageEmbeds(
  message: MessageWithStatus,
  allowEmbeddedContent: boolean
): MessageWithStatus {
  // User has embeds disabled locally — force suppression regardless of server flag
  if (!allowEmbeddedContent) {
    if (message.embeds_suppressed) return message; // Already suppressed, no clone needed
    return { ...message, embeds_suppressed: true };
  }

  // User allows embeds — respect whatever the server stamped (including moderator suppression)
  return message;
}

/**
 * Sanitize an array of messages. Used for bulk operations (setMessages, prependMessages).
 *
 * The caller is responsible for reading the privacy store and passing the current
 * `allowEmbeddedContent` setting — this keeps the utils/ layer free of store imports.
 */
export function sanitizeMessagesEmbeds(
  messages: MessageWithStatus[],
  allowEmbeddedContent: boolean
): MessageWithStatus[] {
  // Fast path: if user allows embeds, no transformation needed — trust the server flags
  if (allowEmbeddedContent) return messages;

  // User has embeds disabled locally — suppress all
  return messages.map((msg) => (msg.embeds_suppressed ? msg : { ...msg, embeds_suppressed: true }));
}
