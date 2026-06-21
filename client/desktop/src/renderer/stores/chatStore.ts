/**
 * Chat Store - Real-time message and typing indicator state management
 *
 * Integrates with WebSocketService to handle real-time updates
 */

import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import type { MessageWithStatus } from '../types/chat';
import { sanitizeMessageEmbeds, sanitizeMessagesEmbeds } from '../utils/messageSanitizer';
import { usePrivacyStore } from './privacyStore';

export interface TypingUser {
  userId: string;
  username?: string;
  timestamp: number;
}

interface ChatState {
  // Messages by channel ID
  messagesByChannel: Map<string, MessageWithStatus[]>;

  // Typing indicators by channel ID
  typingByChannel: Map<string, Map<string, TypingUser>>;

  // Connection status
  isConnected: boolean;
  connectionState: 'connected' | 'connecting' | 'disconnected';
  connectionClientId: string | null;

  // Actions
  addMessage: (channelId: string, message: MessageWithStatus) => void;
  updateMessage: (
    channelId: string,
    messageId: string,
    updates: Partial<MessageWithStatus>
  ) => void;
  updateMessageStatus: (
    channelId: string,
    clientMessageId: string,
    status: 'pending' | 'sent' | 'delivered' | 'failed',
    serverId?: string,
    error?: string
  ) => void;
  deleteMessage: (channelId: string, messageId: string) => void;
  setMessages: (channelId: string, messages: MessageWithStatus[]) => void;
  prependMessages: (channelId: string, messages: MessageWithStatus[]) => void; // For pagination
  clearMessages: (channelId: string) => void;

  // User profile updates across all messages
  updateUserInMessages: (
    userId: string,
    updates: { username?: string; display_name?: string | null; avatar_url?: string | null }
  ) => void;

  // Typing indicators
  setTyping: (channelId: string, userId: string, isTyping: boolean, username?: string) => void;
  getTypingUsers: (channelId: string) => TypingUser[];
  clearOldTypingIndicators: (channelId: string, maxAge?: number) => void;

  // Reply state per channel
  replyingTo: Map<string, MessageWithStatus>;
  setReplyingTo: (channelId: string, message: MessageWithStatus | null) => void;

  // Connection
  setConnectionStatus: (
    isConnected: boolean,
    clientId?: string,
    connectionState?: 'connected' | 'connecting' | 'disconnected'
  ) => void;

  // Cleanup
  reset: () => void;
}

export const useChatStore = wrapStore(create<ChatState>()(
  devtools(
    (set, get) => ({
      messagesByChannel: new Map(),
      typingByChannel: new Map(),
      replyingTo: new Map(),
      isConnected: false,
      connectionState: 'disconnected',
      connectionClientId: null,

      // Add a new message to a channel
      addMessage: (channelId, message) =>
        set((state) => {
          const { allowEmbeddedContent } = usePrivacyStore.getState().settings;
          const sanitized = sanitizeMessageEmbeds(message, allowEmbeddedContent);
          const messages = state.messagesByChannel.get(channelId) || [];

          // Check if message already exists (deduplication)
          const existingIndex = messages.findIndex((m) => m.id === sanitized.id);
          if (existingIndex >= 0) {
            // Update existing message
            const updatedMessages = [...messages];
            updatedMessages[existingIndex] = sanitized;
            state.messagesByChannel.set(channelId, updatedMessages);
          } else {
            // Add new message (append to end, newest last)
            state.messagesByChannel.set(channelId, [...messages, sanitized]);
          }

          return {
            messagesByChannel: new Map(state.messagesByChannel),
          };
        }),

      // Update an existing message
      updateMessage: (channelId, messageId, updates) =>
        set((state) => {
          const messages = state.messagesByChannel.get(channelId) || [];
          const index = messages.findIndex((m) => m.id === messageId);

          if (index >= 0) {
            const updatedMessages = [...messages];
            const { allowEmbeddedContent } = usePrivacyStore.getState().settings;
            updatedMessages[index] = sanitizeMessageEmbeds(
              {
                ...updatedMessages[index],
                ...updates,
              },
              allowEmbeddedContent
            );
            state.messagesByChannel.set(channelId, updatedMessages);
          }

          return {
            messagesByChannel: new Map(state.messagesByChannel),
          };
        }),

      // Update message status (for delivery tracking)
      updateMessageStatus: (channelId, clientMessageId, status, serverId, error) =>
        set((state) => {
          const messages = state.messagesByChannel.get(channelId) || [];
          const index = messages.findIndex((m) => m.clientMessageId === clientMessageId);

          if (index >= 0) {
            const updatedMessages = [...messages];
            updatedMessages[index] = {
              ...updatedMessages[index],
              status,
              error,
              // If server assigned a permanent ID, update the message ID
              ...(serverId && { id: serverId }),
            };
            state.messagesByChannel.set(channelId, updatedMessages);
          }

          return {
            messagesByChannel: new Map(state.messagesByChannel),
          };
        }),

      // Delete a message
      deleteMessage: (channelId, messageId) =>
        set((state) => {
          const messages = state.messagesByChannel.get(channelId) || [];
          const filteredMessages = messages.filter((m) => m.id !== messageId);
          state.messagesByChannel.set(channelId, filteredMessages);

          return {
            messagesByChannel: new Map(state.messagesByChannel),
          };
        }),

      // Set all messages for a channel (replace)
      setMessages: (channelId, messages) =>
        set((state) => {
          const { allowEmbeddedContent } = usePrivacyStore.getState().settings;
          state.messagesByChannel.set(
            channelId,
            sanitizeMessagesEmbeds(messages, allowEmbeddedContent)
          );
          return {
            messagesByChannel: new Map(state.messagesByChannel),
          };
        }),

      // Prepend messages (for pagination - loading older messages)
      prependMessages: (channelId, messages) =>
        set((state) => {
          const { allowEmbeddedContent } = usePrivacyStore.getState().settings;
          const sanitized = sanitizeMessagesEmbeds(messages, allowEmbeddedContent);
          const existing = state.messagesByChannel.get(channelId) || [];

          // Deduplicate by ID
          const existingIds = new Set(existing.map((m) => m.id));
          const newMessages = sanitized.filter((m) => !existingIds.has(m.id));

          state.messagesByChannel.set(channelId, [...newMessages, ...existing]);

          return {
            messagesByChannel: new Map(state.messagesByChannel),
          };
        }),

      // Clear all messages for a channel
      clearMessages: (channelId) =>
        set((state) => {
          state.messagesByChannel.delete(channelId);
          return {
            messagesByChannel: new Map(state.messagesByChannel),
          };
        }),

      // Update user info across all messages in all channels
      updateUserInMessages: (userId, updates) =>
        set((state) => {
          let changed = false;
          for (const [channelId, messages] of state.messagesByChannel) {
            const updated = messages.map((m) => {
              if (m.user_id !== userId) return m;
              changed = true;
              return {
                ...m,
                ...(updates.username !== undefined && { username: updates.username }),
                ...(updates.display_name !== undefined && {
                  display_name: updates.display_name || undefined,
                }),
                ...(updates.avatar_url !== undefined && {
                  avatar_url: updates.avatar_url || undefined,
                }),
              };
            });
            state.messagesByChannel.set(channelId, updated);
          }
          if (!changed) return state;
          return { messagesByChannel: new Map(state.messagesByChannel) };
        }),

      // Set typing indicator for a user in a channel
      setReplyingTo: (channelId, message) =>
        set((state) => {
          if (message) {
            state.replyingTo.set(channelId, message);
          } else {
            state.replyingTo.delete(channelId);
          }
          return { replyingTo: new Map(state.replyingTo) };
        }),

      setTyping: (channelId, userId, isTyping, username) =>
        set((state) => {
          let channelTyping = state.typingByChannel.get(channelId);

          if (!channelTyping) {
            channelTyping = new Map();
            state.typingByChannel.set(channelId, channelTyping);
          }

          if (isTyping) {
            channelTyping.set(userId, {
              userId,
              username,
              timestamp: Date.now(),
            });
          } else {
            channelTyping.delete(userId);
          }

          return {
            typingByChannel: new Map(state.typingByChannel),
          };
        }),

      // Get typing users for a channel
      getTypingUsers: (channelId) => {
        const state = get();
        const channelTyping = state.typingByChannel.get(channelId);

        if (!channelTyping) {
          return [];
        }

        return Array.from(channelTyping.values());
      },

      // Clear typing indicators older than maxAge (default 5 seconds)
      clearOldTypingIndicators: (channelId, maxAge = 5000) =>
        set((state) => {
          const channelTyping = state.typingByChannel.get(channelId);

          if (!channelTyping) {
            return state;
          }

          const now = Date.now();
          let hasChanges = false;

          for (const [userId, user] of channelTyping) {
            if (now - user.timestamp > maxAge) {
              channelTyping.delete(userId);
              hasChanges = true;
            }
          }

          if (hasChanges) {
            return {
              typingByChannel: new Map(state.typingByChannel),
            };
          }

          return state;
        }),

      // Set connection status
      setConnectionStatus: (isConnected, clientId, connectionState) =>
        set({
          isConnected,
          connectionState: connectionState || (isConnected ? 'connected' : 'disconnected'),
          connectionClientId: clientId || null,
        }),

      // Reset all state
      reset: () =>
        set({
          messagesByChannel: new Map(),
          typingByChannel: new Map(),
          replyingTo: new Map(),
          isConnected: false,
          connectionState: 'disconnected',
          connectionClientId: null,
        }),
    }),
    { name: 'ChatStore' }
  )
));
