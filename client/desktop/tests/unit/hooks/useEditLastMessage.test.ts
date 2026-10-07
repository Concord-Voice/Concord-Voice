import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useEditLastMessage } from '@/renderer/hooks/messaging/useEditLastMessage';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import type { MessageWithStatus } from '@/renderer/types/chat';
import { mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';

const ME = mockMessage.user_id;
const SURFACE = 'surface-main';
const OTHER_SURFACE = 'surface-voice';

const msg = (overrides: Partial<MessageWithStatus>): MessageWithStatus => ({
  ...mockMessage,
  ...overrides,
});

describe('useEditLastMessage', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('opens the newest own delivered message for editing', () => {
    const messages = [msg({ id: 'older' }), msg({ id: 'newest' })];
    const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));
    expect(useChatStore.getState().editingMessage).toBeNull();

    act(() => result.current());

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: SURFACE,
      messageId: 'newest',
    });
  });

  it('opens the edit under the surface id it was given', () => {
    const messages = [msg({ id: 'newest' })];
    const { result } = renderHook(() => useEditLastMessage(messages, ME, OTHER_SURFACE));

    act(() => result.current());

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: OTHER_SURFACE,
      messageId: 'newest',
    });
  });

  it('opens nothing when the newest own message is still pending, even with an older delivered one', () => {
    const messages = [msg({ id: 'older' }), msg({ id: 'newest', status: 'pending' })];
    const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));

    act(() => result.current());

    expect(useChatStore.getState().editingMessage).toBeNull();
  });

  it('opens nothing when the list holds no message by the current user', () => {
    const messages = [msg({ id: 'theirs', user_id: 'user-2' })];
    const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));

    act(() => result.current());

    expect(useChatStore.getState().editingMessage).toBeNull();
  });

  it('opens nothing when the current user id is empty', () => {
    const messages = [msg({ id: 'own' })];
    const { result } = renderHook(() => useEditLastMessage(messages, '', SURFACE));

    act(() => result.current());

    expect(useChatStore.getState().editingMessage).toBeNull();
  });

  it.each(['call_event', 'expiration_event'] as const)(
    'skips a newer %s system row and opens the user message beneath it',
    (type) => {
      const messages = [msg({ id: 'user-row' }), msg({ id: 'system-row', type })];
      const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));

      act(() => result.current());

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE,
        messageId: 'user-row',
      });
    }
  );

  it('reflects re-rendered inputs: a newer own message becomes the target', () => {
    const initial = [msg({ id: 'first' })];
    const { result, rerender } = renderHook(
      ({ messages }: { messages: MessageWithStatus[] }) =>
        useEditLastMessage(messages, ME, SURFACE),
      { initialProps: { messages: initial } }
    );
    act(() => result.current());
    expect(useChatStore.getState().editingMessage?.messageId).toBe('first');

    // Close the first edit so the next press is free to open the newer row.
    act(() => useChatStore.getState().clearEditingMessage(SURFACE));
    rerender({ messages: [...initial, msg({ id: 'second' })] });
    act(() => result.current());

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: SURFACE,
      messageId: 'second',
    });
  });

  it('never clears: a null selector result leaves an already-open edit untouched', () => {
    useChatStore.getState().setEditingMessage(SURFACE, 'already-open');
    const messages = [msg({ id: 'newest', status: 'pending' })];
    const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));

    act(() => result.current());

    expect(useChatStore.getState().editingMessage).toEqual({
      surfaceId: SURFACE,
      messageId: 'already-open',
    });
  });

  describe('an edit already open', () => {
    it('is not replaced while this surface edits a different, still-editable message', () => {
      const messages = [msg({ id: 'older' }), msg({ id: 'newest' })];
      const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));

      // Gate: with nothing open, the same press opens the newest row, so the no-op below
      // is the open-edit check and not an inert callback.
      act(() => result.current());
      expect(useChatStore.getState().editingMessage?.messageId).toBe('newest');
      act(() => useChatStore.getState().clearEditingMessage(SURFACE));

      useChatStore.getState().setEditingMessage(SURFACE, 'older');
      const before = useChatStore.getState().editingMessage;
      act(() => result.current());

      expect(useChatStore.getState().editingMessage).toBe(before);
      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE,
        messageId: 'older',
      });
    });

    it('is replaced when the open edit belongs to ANOTHER surface', () => {
      useChatStore.getState().setEditingMessage(OTHER_SURFACE, 'newest');
      const messages = [msg({ id: 'older' }), msg({ id: 'newest' })];
      const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));

      act(() => result.current());

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE,
        messageId: 'newest',
      });
    });

    it('is replaced when this surface edits a message that is no longer in the list', () => {
      useChatStore.getState().setEditingMessage(SURFACE, 'gone');
      const messages = [msg({ id: 'older' }), msg({ id: 'newest' })];
      const { result } = renderHook(() => useEditLastMessage(messages, ME, SURFACE));

      act(() => result.current());

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE,
        messageId: 'newest',
      });
    });

    it('is replaced when this surface edits a message that is no longer editable', () => {
      useChatStore.getState().setEditingMessage(SURFACE, 'older');
      const editable = [msg({ id: 'older' }), msg({ id: 'newest' })];

      // Gate: while 'older' is still editable the open edit stands.
      const { result, rerender } = renderHook(
        ({ messages }: { messages: MessageWithStatus[] }) =>
          useEditLastMessage(messages, ME, SURFACE),
        { initialProps: { messages: editable } }
      );
      act(() => result.current());
      expect(useChatStore.getState().editingMessage?.messageId).toBe('older');

      rerender({
        messages: [msg({ id: 'older', decryptFailed: true }), msg({ id: 'newest' })],
      });
      act(() => result.current());

      expect(useChatStore.getState().editingMessage).toEqual({
        surfaceId: SURFACE,
        messageId: 'newest',
      });
    });
  });
});
