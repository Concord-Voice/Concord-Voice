import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useDraftMessage } from '@/renderer/hooks/useDraftMessage';
import { useDraftMessageStore } from '@/renderer/stores/draftMessageStore';
import { resetAllStores } from '../../helpers/store-helpers';
import type { MessageWithStatus } from '@/renderer/types/chat';

function makeDraftMessage(overrides: Partial<MessageWithStatus> = {}): MessageWithStatus {
  return {
    id: 'msg-1',
    channel_id: 'ch-1',
    user_id: 'user-1',
    username: 'alice',
    display_name: 'Alice',
    content: 'Hello, world! This is a test message.',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

describe('useDraftMessage', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetAllStores();
    useDraftMessageStore.setState({ drafts: {} });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  // ── Initial draft loading ───────────────────────────────────────────

  it('returns undefined initialDraft when no draft exists', () => {
    const { result } = renderHook(() => useDraftMessage('channel-1'));
    expect(result.current.initialDraft).toBeUndefined();
  });

  it('returns initialDraft matching stored draft on mount', () => {
    const stored = {
      text: 'saved draft',
      updatedAt: 1000,
    };
    useDraftMessageStore.getState().setDraft('channel-1', stored);

    const { result } = renderHook(() => useDraftMessage('channel-1'));
    expect(result.current.initialDraft).toBeDefined();
    expect(result.current.initialDraft!.text).toBe('saved draft');
  });

  it('returns undefined when targetId is undefined', () => {
    const { result } = renderHook(() => useDraftMessage(undefined));
    expect(result.current.initialDraft).toBeUndefined();
  });

  // ── saveDraft debounce behavior ─────────────────────────────────────

  it('saveDraft debounces — store not updated until 500ms', () => {
    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('hello');
    });

    // Before debounce fires, store should be empty
    expect(useDraftMessageStore.getState().getDraft('channel-1')).toBeUndefined();

    act(() => {
      vi.advanceTimersByTime(499);
    });

    // Still not fired
    expect(useDraftMessageStore.getState().getDraft('channel-1')).toBeUndefined();
  });

  it('saveDraft actually saves after debounce fires', () => {
    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('hello');
    });

    act(() => {
      vi.advanceTimersByTime(500);
    });

    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft).not.toBeNull();
    expect(draft!.text).toBe('hello');
  });

  it('saveDraft with empty text and no reply calls clearDraft', () => {
    // Pre-populate a draft
    useDraftMessageStore.getState().setDraft('channel-1', {
      text: 'existing draft',
      updatedAt: 1000,
    });

    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('');
    });

    act(() => {
      vi.advanceTimersByTime(500);
    });

    expect(useDraftMessageStore.getState().getDraft('channel-1')).toBeUndefined();
  });

  it('saveDraft with reply context saves reply fields', () => {
    const replyMsg = makeDraftMessage({
      id: 'reply-msg-1',
      user_id: 'user-2',
      username: 'bob',
      display_name: 'Bob',
      content: 'Original message content to reply to',
    });

    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('my reply', replyMsg);
    });

    act(() => {
      vi.advanceTimersByTime(500);
    });

    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft).not.toBeNull();
    expect(draft!.text).toBe('my reply');
    expect(draft!.replyToId).toBe('reply-msg-1');
    expect(draft!.replyToUserId).toBe('user-2');
    expect(draft!.replyToUsername).toBe('bob');
    // replyToPreview is not stored (E2EE privacy: no plaintext in localStorage)
    expect(draft!).not.toHaveProperty('replyToPreview');
  });

  it('saveDraft does not store message content preview (E2EE safety)', () => {
    const replyMsg = makeDraftMessage({ content: 'A'.repeat(200) });
    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('reply', replyMsg);
    });
    act(() => {
      vi.advanceTimersByTime(500);
    });

    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft!).not.toHaveProperty('replyToPreview');
  });

  it('saveDraft uses display_name as fallback when username is empty', () => {
    const replyMsg = makeDraftMessage({
      username: '',
      display_name: 'DisplayOnly',
    });

    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('reply', replyMsg);
    });

    act(() => {
      vi.advanceTimersByTime(500);
    });

    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft!.replyToUsername).toBe('DisplayOnly');
  });

  // ── clearDraft behavior ─────────────────────────────────────────────

  it('clearDraft immediately removes from store (no debounce)', () => {
    useDraftMessageStore.getState().setDraft('channel-1', {
      text: 'a draft',
      updatedAt: 1000,
    });

    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.clearDraft();
    });

    // Should be cleared immediately without advancing timers
    expect(useDraftMessageStore.getState().getDraft('channel-1')).toBeUndefined();
  });

  it('clearDraft cancels pending debounce', () => {
    const { result } = renderHook(() => useDraftMessage('channel-1'));

    // Start a debounced save
    act(() => {
      result.current.saveDraft('pending text');
    });

    // Clear immediately — should cancel the pending save
    act(() => {
      result.current.clearDraft();
    });

    // Advance timers past debounce — the save should NOT fire
    act(() => {
      vi.advanceTimersByTime(500);
    });

    expect(useDraftMessageStore.getState().getDraft('channel-1')).toBeUndefined();
  });

  // ── targetId change behavior ────────────────────────────────────────

  it('changing targetId flushes pending save for old target', () => {
    const { result, rerender } = renderHook(({ id }) => useDraftMessage(id), {
      initialProps: { id: 'channel-1' },
    });

    // Start a debounced save on channel-1
    act(() => {
      result.current.saveDraft('draft for channel 1');
    });

    // Switch to channel-2 before debounce fires
    rerender({ id: 'channel-2' });

    // The pending save for channel-1 should have been flushed immediately
    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft).not.toBeNull();
    expect(draft!.text).toBe('draft for channel 1');
  });

  it('changing targetId loads new target draft as initialDraft', () => {
    useDraftMessageStore.getState().setDraft('channel-2', {
      text: 'draft for channel 2',
      updatedAt: 2000,
    });

    const { result, rerender } = renderHook(({ id }) => useDraftMessage(id), {
      initialProps: { id: 'channel-1' },
    });

    expect(result.current.initialDraft).toBeUndefined();

    rerender({ id: 'channel-2' });

    expect(result.current.initialDraft).toBeDefined();
    expect(result.current.initialDraft!.text).toBe('draft for channel 2');
  });

  // ── Unmount behavior ────────────────────────────────────────────────

  it('cleanup on unmount flushes pending debounce', () => {
    const { result, unmount } = renderHook(() => useDraftMessage('channel-1'));

    // Start a debounced save
    act(() => {
      result.current.saveDraft('unsaved text');
    });

    // Unmount before debounce fires
    unmount();

    // The pending save should have been flushed on unmount
    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft).not.toBeNull();
    expect(draft!.text).toBe('unsaved text');
  });

  // ── Edge cases ──────────────────────────────────────────────────────

  it('saveDraft does nothing when targetId is undefined', () => {
    const { result } = renderHook(() => useDraftMessage(undefined));

    act(() => {
      result.current.saveDraft('should not save');
    });

    act(() => {
      vi.advanceTimersByTime(500);
    });

    expect(useDraftMessageStore.getState().hasDraft('undefined')).toBe(false);
  });

  it('clearDraft does nothing when targetId is undefined', () => {
    const { result } = renderHook(() => useDraftMessage(undefined));

    act(() => {
      result.current.clearDraft();
    });

    // Store should remain empty — no draft was created or removed
    expect(Object.keys(useDraftMessageStore.getState().drafts)).toHaveLength(0);
  });

  it('multiple rapid saveDraft calls only keep the last', () => {
    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('first');
      result.current.saveDraft('second');
      result.current.saveDraft('third');
    });

    act(() => {
      vi.advanceTimersByTime(500);
    });

    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft).not.toBeNull();
    expect(draft!.text).toBe('third');
  });

  it('saveDraft with empty text but with reply saves the draft', () => {
    const replyMsg = makeDraftMessage();

    const { result } = renderHook(() => useDraftMessage('channel-1'));

    act(() => {
      result.current.saveDraft('', replyMsg);
    });

    act(() => {
      vi.advanceTimersByTime(500);
    });

    const draft = useDraftMessageStore.getState().getDraft('channel-1');
    expect(draft).not.toBeNull();
    expect(draft!.text).toBe('');
    expect(draft!.replyToId).toBe('msg-1');
  });
});
