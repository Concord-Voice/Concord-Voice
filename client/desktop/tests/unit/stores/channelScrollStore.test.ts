import { describe, it, expect, beforeEach } from 'vitest';
import { useChannelScrollStore } from '@/renderer/stores/channelScrollStore';

beforeEach(() => {
  useChannelScrollStore.setState({ positions: {}, latestMessageIds: {} });
});

describe('channelScrollStore', () => {
  it('starts with empty positions', () => {
    expect(useChannelScrollStore.getState().positions).toEqual({});
  });

  it('saveScroll stores a scrollTop for a given id', () => {
    useChannelScrollStore.getState().saveScroll('channel-1', 420);
    expect(useChannelScrollStore.getState().getScroll('channel-1')).toBe(420);
  });

  it('getScroll returns undefined for unknown id', () => {
    expect(useChannelScrollStore.getState().getScroll('missing')).toBeUndefined();
  });

  it('saveScroll overwrites previous value', () => {
    const { saveScroll, getScroll } = useChannelScrollStore.getState();
    saveScroll('channel-1', 100);
    saveScroll('channel-1', 250);
    expect(useChannelScrollStore.getState().getScroll('channel-1')).toBe(250);
    // Make sure getScroll reads from current state, not snapshot
    expect(getScroll('channel-1')).toBe(250);
  });

  it('keeps per-key positions independent', () => {
    const { saveScroll } = useChannelScrollStore.getState();
    saveScroll('channel-1', 100);
    saveScroll('channel-2', 200);
    saveScroll('dm-conv-1', 300);
    const s = useChannelScrollStore.getState();
    expect(s.getScroll('channel-1')).toBe(100);
    expect(s.getScroll('channel-2')).toBe(200);
    expect(s.getScroll('dm-conv-1')).toBe(300);
  });

  it('clearScroll removes the saved position for an id', () => {
    const { saveScroll, clearScroll } = useChannelScrollStore.getState();
    saveScroll('channel-1', 100);
    saveScroll('channel-2', 200);
    clearScroll('channel-1');
    const s = useChannelScrollStore.getState();
    expect(s.getScroll('channel-1')).toBeUndefined();
    expect(s.getScroll('channel-2')).toBe(200);
  });

  it('clearScroll on unknown id is a no-op', () => {
    const { saveScroll, clearScroll } = useChannelScrollStore.getState();
    saveScroll('channel-1', 100);
    clearScroll('missing');
    expect(useChannelScrollStore.getState().positions).toEqual({ 'channel-1': 100 });
  });

  it('supports scrollTop value 0', () => {
    useChannelScrollStore.getState().saveScroll('channel-1', 0);
    expect(useChannelScrollStore.getState().getScroll('channel-1')).toBe(0);
  });
});
