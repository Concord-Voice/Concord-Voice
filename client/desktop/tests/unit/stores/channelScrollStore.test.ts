import { describe, it, expect, beforeEach } from 'vitest';
import { useChannelScrollStore } from '@/renderer/stores/chat/channelScrollStore';

beforeEach(() => {
  useChannelScrollStore.setState({ anchors: {} });
});

describe('channelScrollStore', () => {
  it('starts with no anchors', () => {
    expect(useChannelScrollStore.getState().anchors).toEqual({});
  });

  it('saveAnchor stores an anchor for a given id', () => {
    useChannelScrollStore.getState().saveAnchor('channel-1', { messageId: 'm-1', offset: 12 });
    expect(useChannelScrollStore.getState().getAnchor('channel-1')).toEqual({
      messageId: 'm-1',
      offset: 12,
    });
  });

  it('getAnchor returns undefined for unknown id', () => {
    expect(useChannelScrollStore.getState().getAnchor('missing')).toBeUndefined();
  });

  it('saveAnchor overwrites the previous anchor', () => {
    const { saveAnchor, getAnchor } = useChannelScrollStore.getState();
    saveAnchor('channel-1', { messageId: 'm-1', offset: 0 });
    saveAnchor('channel-1', { messageId: 'm-2', offset: 40 });
    expect(useChannelScrollStore.getState().getAnchor('channel-1')).toEqual({
      messageId: 'm-2',
      offset: 40,
    });
    // Make sure getAnchor reads from current state, not a snapshot
    expect(getAnchor('channel-1')).toEqual({ messageId: 'm-2', offset: 40 });
  });

  it('keeps per-key anchors independent', () => {
    const { saveAnchor } = useChannelScrollStore.getState();
    saveAnchor('channel-1', { messageId: 'a', offset: 0 });
    saveAnchor('channel-2', { messageId: 'b', offset: 0 });
    saveAnchor('dm-conv-1', { messageId: 'c', offset: 0 });
    const s = useChannelScrollStore.getState();
    expect(s.getAnchor('channel-1')?.messageId).toBe('a');
    expect(s.getAnchor('channel-2')?.messageId).toBe('b');
    expect(s.getAnchor('dm-conv-1')?.messageId).toBe('c');
  });

  it('clearAnchor removes the saved anchor for an id', () => {
    const { saveAnchor, clearAnchor } = useChannelScrollStore.getState();
    saveAnchor('channel-1', { messageId: 'a', offset: 0 });
    saveAnchor('channel-2', { messageId: 'b', offset: 0 });
    clearAnchor('channel-1');
    const s = useChannelScrollStore.getState();
    expect(s.getAnchor('channel-1')).toBeUndefined();
    expect(s.getAnchor('channel-2')?.messageId).toBe('b');
  });

  it('clearAnchor on unknown id is a no-op', () => {
    const { saveAnchor, clearAnchor } = useChannelScrollStore.getState();
    saveAnchor('channel-1', { messageId: 'a', offset: 0 });
    clearAnchor('missing');
    expect(useChannelScrollStore.getState().anchors).toEqual({
      'channel-1': { messageId: 'a', offset: 0 },
    });
  });

  it('supports a negative offset (divider between the row and the top edge)', () => {
    useChannelScrollStore.getState().saveAnchor('channel-1', { messageId: 'a', offset: -28 });
    expect(useChannelScrollStore.getState().getAnchor('channel-1')?.offset).toBe(-28);
  });
});
