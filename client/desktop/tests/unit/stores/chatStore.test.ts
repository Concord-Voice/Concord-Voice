import { useChatStore } from '@/renderer/stores/chatStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockMessage, mockMessage2, mockPendingMessage } from '../../mocks/fixtures';

describe('chatStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  describe('addMessage', () => {
    it('adds a message to a channel', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(1);
      expect(msgs![0].id).toBe('msg-1');
    });

    it('deduplicates by message id', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().addMessage('channel-1', { ...mockMessage, content: 'updated' });
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(1);
      expect(msgs![0].content).toBe('updated');
    });

    it('adds multiple messages in order', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().addMessage('channel-1', mockMessage2);
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(2);
    });
  });

  describe('updateMessage', () => {
    it('updates an existing message', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().updateMessage('channel-1', 'msg-1', { content: 'edited' });
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs![0].content).toBe('edited');
    });

    it('does nothing for nonexistent message', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().updateMessage('channel-1', 'nonexistent', { content: 'x' });
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(1);
      expect(msgs![0].content).toBe('Hello, world!');
    });
  });

  describe('deleteMessage', () => {
    it('removes a message', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().addMessage('channel-1', mockMessage2);
      useChatStore.getState().deleteMessage('channel-1', 'msg-1');
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(1);
      expect(msgs![0].id).toBe('msg-2');
    });
  });

  describe('setMessages', () => {
    it('replaces all messages for a channel', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().setMessages('channel-1', [mockMessage2]);
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(1);
      expect(msgs![0].id).toBe('msg-2');
    });
  });

  describe('prependMessages', () => {
    it('prepends messages for pagination', () => {
      useChatStore.getState().setMessages('channel-1', [mockMessage2]);
      useChatStore.getState().prependMessages('channel-1', [mockMessage]);
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(2);
      expect(msgs![0].id).toBe('msg-1');
      expect(msgs![1].id).toBe('msg-2');
    });

    it('deduplicates when prepending', () => {
      useChatStore.getState().setMessages('channel-1', [mockMessage]);
      useChatStore.getState().prependMessages('channel-1', [mockMessage]);
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs).toHaveLength(1);
    });
  });

  describe('clearMessages', () => {
    it('clears messages for a specific channel', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().addMessage('channel-2', mockMessage2);
      useChatStore.getState().clearMessages('channel-1');
      expect(useChatStore.getState().messagesByChannel.get('channel-1')).toBeUndefined();
      expect(useChatStore.getState().messagesByChannel.get('channel-2')).toHaveLength(1);
    });
  });

  describe('typing indicators', () => {
    it('sets and gets typing users', () => {
      useChatStore.getState().setTyping('channel-1', 'user-2', true, 'testuser2');
      const typing = useChatStore.getState().getTypingUsers('channel-1');
      expect(typing).toHaveLength(1);
      expect(typing[0].userId).toBe('user-2');
    });

    it('removes typing user', () => {
      useChatStore.getState().setTyping('channel-1', 'user-2', true, 'testuser2');
      useChatStore.getState().setTyping('channel-1', 'user-2', false);
      const typing = useChatStore.getState().getTypingUsers('channel-1');
      expect(typing).toHaveLength(0);
    });

    it('clears old typing indicators', () => {
      useChatStore.getState().setTyping('channel-1', 'user-2', true, 'testuser2');
      // Force old timestamp
      const channel = useChatStore.getState().typingByChannel.get('channel-1');
      if (channel) {
        channel.set('user-2', {
          userId: 'user-2',
          username: 'testuser2',
          timestamp: Date.now() - 10000,
        });
      }
      useChatStore.getState().clearOldTypingIndicators('channel-1');
      const typing = useChatStore.getState().getTypingUsers('channel-1');
      expect(typing).toHaveLength(0);
    });
  });

  describe('connectionStatus', () => {
    it('sets connected state', () => {
      useChatStore.getState().setConnectionStatus(true, 'client-123', 'connected');
      const state = useChatStore.getState();
      expect(state.isConnected).toBe(true);
      expect(state.connectionClientId).toBe('client-123');
      expect(state.connectionState).toBe('connected');
    });

    it('sets disconnected state', () => {
      useChatStore.getState().setConnectionStatus(true, 'client-123', 'connected');
      useChatStore.getState().setConnectionStatus(false, undefined, 'disconnected');
      expect(useChatStore.getState().isConnected).toBe(false);
      expect(useChatStore.getState().connectionState).toBe('disconnected');
    });
  });

  describe('updateMessageStatus', () => {
    it('updates pending message to sent with server id', () => {
      useChatStore.getState().addMessage('channel-1', mockPendingMessage);
      useChatStore
        .getState()
        .updateMessageStatus('channel-1', 'client-msg-1', 'sent', 'server-msg-1');
      const msgs = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(msgs![0].status).toBe('sent');
      expect(msgs![0].id).toBe('server-msg-1');
    });
  });

  describe('reset', () => {
    it('clears all state', () => {
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useChatStore.getState().setConnectionStatus(true, 'client-1', 'connected');
      useChatStore.getState().reset();
      expect(useChatStore.getState().messagesByChannel.size).toBe(0);
      expect(useChatStore.getState().isConnected).toBe(false);
      expect(useChatStore.getState().replyingTo.size).toBe(0);
    });
  });

  describe('setReplyingTo', () => {
    it('sets replyingTo for a channel', () => {
      const msg = { ...mockMessage, id: 'reply-target' };
      useChatStore.getState().setReplyingTo('channel-1', msg);
      expect(useChatStore.getState().replyingTo.get('channel-1')).toBeDefined();
      expect(useChatStore.getState().replyingTo.get('channel-1')?.id).toBe('reply-target');
    });

    it('clears replyingTo when set to null', () => {
      const msg = { ...mockMessage, id: 'reply-target' };
      useChatStore.getState().setReplyingTo('channel-1', msg);
      useChatStore.getState().setReplyingTo('channel-1', null);
      expect(useChatStore.getState().replyingTo.get('channel-1')).toBeUndefined();
    });

    it('returns undefined for channels with no reply', () => {
      expect(useChatStore.getState().replyingTo.get('no-reply-channel')).toBeUndefined();
    });
  });
});
