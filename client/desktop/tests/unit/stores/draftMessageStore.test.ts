import { describe, it, expect, beforeEach } from 'vitest';
import {
  useDraftMessageStore,
  type DraftContent,
} from '../../../src/renderer/stores/draftMessageStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('draftMessageStore', () => {
  beforeEach(() => {
    resetAllStores();
    useDraftMessageStore.setState({ drafts: {} });
  });

  it('has correct defaults (empty drafts)', () => {
    const state = useDraftMessageStore.getState();
    expect(state.drafts).toEqual({});
  });

  it('setDraft creates a draft', () => {
    const draft: DraftContent = { text: 'hello', updatedAt: Date.now() };
    useDraftMessageStore.getState().setDraft('channel-1', draft);
    expect(useDraftMessageStore.getState().drafts['channel-1']).toEqual(draft);
  });

  it('setDraft updates existing draft', () => {
    const draft1: DraftContent = { text: 'first', updatedAt: 1000 };
    const draft2: DraftContent = { text: 'second', updatedAt: 2000 };
    useDraftMessageStore.getState().setDraft('channel-1', draft1);
    useDraftMessageStore.getState().setDraft('channel-1', draft2);
    expect(useDraftMessageStore.getState().drafts['channel-1']).toEqual(draft2);
  });

  it('clearDraft removes a draft', () => {
    const draft: DraftContent = { text: 'hello', updatedAt: Date.now() };
    useDraftMessageStore.getState().setDraft('channel-1', draft);
    useDraftMessageStore.getState().clearDraft('channel-1');
    expect(useDraftMessageStore.getState().drafts['channel-1']).toBeUndefined();
  });

  it('clearDraft on non-existent key is a no-op', () => {
    const draft: DraftContent = { text: 'hello', updatedAt: Date.now() };
    useDraftMessageStore.getState().setDraft('channel-1', draft);
    useDraftMessageStore.getState().clearDraft('channel-999');
    expect(useDraftMessageStore.getState().drafts['channel-1']).toEqual(draft);
    expect(Object.keys(useDraftMessageStore.getState().drafts)).toHaveLength(1);
  });

  it('getDraft returns the draft when it exists', () => {
    const draft: DraftContent = { text: 'hello', updatedAt: Date.now() };
    useDraftMessageStore.getState().setDraft('channel-1', draft);
    expect(useDraftMessageStore.getState().getDraft('channel-1')).toEqual(draft);
  });

  it('getDraft returns undefined when not found', () => {
    expect(useDraftMessageStore.getState().getDraft('nonexistent')).toBeUndefined();
  });

  it('hasDraft returns true when draft exists', () => {
    const draft: DraftContent = { text: 'hello', updatedAt: Date.now() };
    useDraftMessageStore.getState().setDraft('channel-1', draft);
    expect(useDraftMessageStore.getState().hasDraft('channel-1')).toBe(true);
  });

  it('hasDraft returns false when no draft', () => {
    expect(useDraftMessageStore.getState().hasDraft('nonexistent')).toBe(false);
  });

  it('clearAllDrafts empties everything', () => {
    useDraftMessageStore.getState().setDraft('ch-1', { text: 'a', updatedAt: 1000 });
    useDraftMessageStore.getState().setDraft('ch-2', { text: 'b', updatedAt: 2000 });
    useDraftMessageStore.getState().setDraft('ch-3', { text: 'c', updatedAt: 3000 });
    useDraftMessageStore.getState().clearAllDrafts();
    expect(useDraftMessageStore.getState().drafts).toEqual({});
  });

  it('clearStaleDrafts removes drafts older than threshold', () => {
    const now = Date.now();
    const thirtyOneDaysAgo = now - 31 * 24 * 60 * 60 * 1000;
    useDraftMessageStore.getState().setDraft('old', { text: 'stale', updatedAt: thirtyOneDaysAgo });
    useDraftMessageStore.getState().setDraft('new', { text: 'fresh', updatedAt: now });
    useDraftMessageStore.getState().clearStaleDrafts();
    expect(useDraftMessageStore.getState().drafts['old']).toBeUndefined();
    expect(useDraftMessageStore.getState().drafts['new']).toBeDefined();
  });

  it('clearStaleDrafts keeps fresh drafts', () => {
    const now = Date.now();
    useDraftMessageStore.getState().setDraft('ch-1', { text: 'a', updatedAt: now - 1000 });
    useDraftMessageStore.getState().setDraft('ch-2', { text: 'b', updatedAt: now });
    useDraftMessageStore.getState().clearStaleDrafts();
    expect(Object.keys(useDraftMessageStore.getState().drafts)).toHaveLength(2);
  });

  it('clearStaleDrafts with custom maxAge', () => {
    const now = Date.now();
    const oneHourMs = 60 * 60 * 1000;
    useDraftMessageStore
      .getState()
      .setDraft('recent', { text: 'a', updatedAt: now - 30 * 60 * 1000 });
    useDraftMessageStore.getState().setDraft('old', { text: 'b', updatedAt: now - 2 * oneHourMs });
    useDraftMessageStore.getState().clearStaleDrafts(oneHourMs);
    expect(useDraftMessageStore.getState().drafts['recent']).toBeDefined();
    expect(useDraftMessageStore.getState().drafts['old']).toBeUndefined();
  });

  it('multiple drafts for different channels coexist', () => {
    const now = Date.now();
    const drafts: Record<string, DraftContent> = {
      'channel-1': { text: 'msg 1', updatedAt: now },
      'channel-2': { text: 'msg 2', updatedAt: now },
      'dm-conv-3': { text: 'dm msg', updatedAt: now },
    };
    for (const [id, draft] of Object.entries(drafts)) {
      useDraftMessageStore.getState().setDraft(id, draft);
    }
    const state = useDraftMessageStore.getState();
    expect(Object.keys(state.drafts)).toHaveLength(3);
    expect(state.getDraft('channel-1')?.text).toBe('msg 1');
    expect(state.getDraft('channel-2')?.text).toBe('msg 2');
    expect(state.getDraft('dm-conv-3')?.text).toBe('dm msg');
  });

  it('draft with reply context stores all fields', () => {
    const draft: DraftContent = {
      text: 'replying here',
      replyToId: 'msg-abc',
      replyToUserId: 'user-123',
      replyToUsername: 'Alice',
      updatedAt: Date.now(),
    };
    useDraftMessageStore.getState().setDraft('channel-5', draft);
    const stored = useDraftMessageStore.getState().getDraft('channel-5');
    expect(stored).toEqual(draft);
    expect(stored?.replyToId).toBe('msg-abc');
    expect(stored?.replyToUserId).toBe('user-123');
    expect(stored?.replyToUsername).toBe('Alice');
  });
});
