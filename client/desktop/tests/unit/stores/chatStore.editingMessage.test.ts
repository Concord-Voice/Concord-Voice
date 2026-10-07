import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockMessage, mockMessage2 } from '../../mocks/fixtures';

const SURFACE_A = 'surface-a';
const SURFACE_B = 'surface-b';

describe('chatStore editingMessage (#1959)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts as null (#1959)', () => {
    expect(useChatStore.getState().editingMessage).toBeNull();
  });

  it('setEditingMessage opens a row in the named surface (#1959)', () => {
    useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: SURFACE_A,
      messageId: 'msg-1',
    });
  });

  it('a second edit replaces the first: one edit is open at a time (#1959)', () => {
    useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: SURFACE_A,
      messageId: 'msg-1',
    });

    useChatStore.getState().setEditingMessage(SURFACE_B, 'msg-2');

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: SURFACE_B,
      messageId: 'msg-2',
    });
  });

  it('the same message opened from another surface is a different edit (#1959)', () => {
    useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');

    useChatStore.getState().setEditingMessage(SURFACE_B, 'msg-1');

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: SURFACE_B,
      messageId: 'msg-1',
    });
  });

  describe('same-value write', () => {
    it('notifies a subscriber on a real change (positive control, #1959)', () => {
      const listener = vi.fn();
      const unsubscribe = useChatStore.subscribe(listener);
      try {
        useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
        expect(listener).toHaveBeenCalledTimes(1);
      } finally {
        unsubscribe();
      }
    });

    it('keeps the same state object and does not notify when surface and message are unchanged (#1959)', () => {
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      const before = useChatStore.getState();
      const listener = vi.fn();
      const unsubscribe = useChatStore.subscribe(listener);
      try {
        useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
        expect(useChatStore.getState()).toBe(before);
        expect(listener).not.toHaveBeenCalled();
      } finally {
        unsubscribe();
      }
    });
  });

  describe('clearEditingMessage', () => {
    it('clears when the surface and the message both match (#1959)', () => {
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage).not.toBeNull();

      useChatStore.getState().clearEditingMessage(SURFACE_A, 'msg-1');

      expect(useChatStore.getState().editingMessage).toBeNull();
    });

    it('clears when the surface matches and no message id is given (#1959)', () => {
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage).not.toBeNull();

      useChatStore.getState().clearEditingMessage(SURFACE_A);

      expect(useChatStore.getState().editingMessage).toBeNull();
    });

    it('leaves an edit open in another surface alone and returns the same state object (#1959)', () => {
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      const before = useChatStore.getState();
      const listener = vi.fn();
      const unsubscribe = useChatStore.subscribe(listener);
      try {
        useChatStore.getState().clearEditingMessage(SURFACE_B, 'msg-1');
        useChatStore.getState().clearEditingMessage(SURFACE_B);

        expect(useChatStore.getState()).toBe(before);
        expect(useChatStore.getState().editingMessage).toEqual({
          surfaceId: SURFACE_A,
          messageId: 'msg-1',
        });
        expect(listener).not.toHaveBeenCalled();

        // Gate: the owning surface can still close it, so the refusals above were
        // the surface check and not an inert action.
        useChatStore.getState().clearEditingMessage(SURFACE_A, 'msg-1');
        expect(useChatStore.getState().editingMessage).toBeNull();
        expect(listener).toHaveBeenCalledTimes(1);
      } finally {
        unsubscribe();
      }
    });

    it('leaves the edit open when the surface matches but the message is different (#1959)', () => {
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      const before = useChatStore.getState();

      useChatStore.getState().clearEditingMessage(SURFACE_A, 'msg-2');

      expect(useChatStore.getState()).toBe(before);
      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE_A,
        messageId: 'msg-1',
      });

      // Gate: the matching message closes it.
      useChatStore.getState().clearEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage).toBeNull();
    });

    it('is a no-op that keeps the state object when nothing is open (#1959)', () => {
      const before = useChatStore.getState();
      const listener = vi.fn();
      const unsubscribe = useChatStore.subscribe(listener);
      try {
        useChatStore.getState().clearEditingMessage(SURFACE_A, 'msg-1');
        expect(useChatStore.getState()).toBe(before);
        expect(listener).not.toHaveBeenCalled();
      } finally {
        unsubscribe();
      }
    });
  });

  describe('deleteMessage', () => {
    it('clears editingMessage when the editing row is deleted (#1959)', () => {
      useChatStore.getState().setMessages('channel-1', [mockMessage, mockMessage2]);
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage?.messageId).toBe('msg-1');

      useChatStore.getState().deleteMessage('channel-1', 'msg-1');

      expect(useChatStore.getState().editingMessage).toBeNull();
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('channel-1')
          ?.map((m) => m.id)
      ).toEqual(['msg-2']);
    });

    it('leaves editingMessage alone when a different row is deleted (#1959)', () => {
      useChatStore.getState().setMessages('channel-1', [mockMessage, mockMessage2]);
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage?.messageId).toBe('msg-1');

      useChatStore.getState().deleteMessage('channel-1', 'msg-2');

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE_A,
        messageId: 'msg-1',
      });
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('channel-1')
          ?.map((m) => m.id)
      ).toEqual(['msg-1']);
    });
  });

  describe('clearMessages', () => {
    it('clears editingMessage when the cleared context held the editing row (#1959)', () => {
      useChatStore.getState().setMessages('channel-1', [mockMessage]);
      useChatStore
        .getState()
        .setMessages('channel-2', [{ ...mockMessage2, channel_id: 'channel-2' }]);
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage?.messageId).toBe('msg-1');

      useChatStore.getState().clearMessages('channel-1');

      expect(useChatStore.getState().editingMessage).toBeNull();
      expect(useChatStore.getState().messagesByChannel.has('channel-1')).toBe(false);
    });

    it('leaves editingMessage alone when another context is cleared (#1959)', () => {
      useChatStore.getState().setMessages('channel-1', [mockMessage]);
      useChatStore
        .getState()
        .setMessages('channel-2', [{ ...mockMessage2, channel_id: 'channel-2' }]);
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage?.messageId).toBe('msg-1');

      useChatStore.getState().clearMessages('channel-2');

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE_A,
        messageId: 'msg-1',
      });
      expect(useChatStore.getState().messagesByChannel.has('channel-2')).toBe(false);
      expect(useChatStore.getState().messagesByChannel.has('channel-1')).toBe(true);
    });

    it('leaves editingMessage alone when clearing a context that has no messages (#1959)', () => {
      useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
      expect(useChatStore.getState().editingMessage?.messageId).toBe('msg-1');

      useChatStore.getState().clearMessages('never-loaded');

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE_A,
        messageId: 'msg-1',
      });
    });
  });

  it('reset() clears editingMessage (#1959)', () => {
    useChatStore.getState().setEditingMessage(SURFACE_A, 'msg-1');
    expect(useChatStore.getState().editingMessage).not.toBeNull();

    useChatStore.getState().reset();

    expect(useChatStore.getState().editingMessage).toBeNull();
  });
});
