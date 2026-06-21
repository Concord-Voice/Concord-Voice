import { describe, it, expect, beforeEach } from 'vitest';
import { useRichPresenceStore } from '@/renderer/stores/richPresenceStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('richPresenceStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts empty with default self presence', () => {
    const state = useRichPresenceStore.getState();
    expect(state.customTextByUser).toEqual({});
    expect(state.self).toEqual({ tier: 0 });
  });

  describe('setCustomText / getCustomText', () => {
    it('stores and retrieves another user custom text', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { emoji: '🎮', text: 'gaming' });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🎮',
        text: 'gaming',
      });
    });

    it('stores text without an emoji', () => {
      useRichPresenceStore.getState().setCustomText('user-3', { text: 'heads down' });
      expect(useRichPresenceStore.getState().getCustomText('user-3')).toEqual({
        text: 'heads down',
      });
    });

    it('replaces an existing entry for the same user', () => {
      const store = useRichPresenceStore.getState();
      store.setCustomText('user-2', { text: 'first' });
      store.setCustomText('user-2', { emoji: '🚀', text: 'second' });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🚀',
        text: 'second',
      });
      expect(Object.keys(useRichPresenceStore.getState().customTextByUser)).toHaveLength(1);
    });

    it('exposes the map for selective subscription', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'hi' });
      expect(useRichPresenceStore.getState().customTextByUser['user-2']).toEqual({ text: 'hi' });
    });

    it('returns undefined for an unknown user', () => {
      expect(useRichPresenceStore.getState().getCustomText('nobody')).toBeUndefined();
    });
  });

  describe('clearCustomText', () => {
    it('removes a stored entry', () => {
      const store = useRichPresenceStore.getState();
      store.setCustomText('user-2', { text: 'gaming' });
      store.clearCustomText('user-2');
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toBeUndefined();
      expect(useRichPresenceStore.getState().customTextByUser).toEqual({});
    });

    it('leaves other users untouched', () => {
      const store = useRichPresenceStore.getState();
      store.setCustomText('user-2', { text: 'a' });
      store.setCustomText('user-3', { text: 'b' });
      store.clearCustomText('user-2');
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toBeUndefined();
      expect(useRichPresenceStore.getState().getCustomText('user-3')).toEqual({ text: 'b' });
    });

    it('is a no-op for an unknown user', () => {
      useRichPresenceStore.getState().clearCustomText('nobody');
      expect(useRichPresenceStore.getState().customTextByUser).toEqual({});
    });
  });

  describe('setSelfPresence', () => {
    it('patches tier and custom text fields', () => {
      useRichPresenceStore
        .getState()
        .setSelfPresence({ tier: 2, customText: 'working', customTextEmoji: '💻' });
      expect(useRichPresenceStore.getState().self).toEqual({
        tier: 2,
        customText: 'working',
        customTextEmoji: '💻',
      });
    });

    it('merges partial updates without dropping prior fields', () => {
      const store = useRichPresenceStore.getState();
      store.setSelfPresence({ tier: 1, customText: 'hello' });
      store.setSelfPresence({ customTextEmoji: '👋' });
      expect(useRichPresenceStore.getState().self).toEqual({
        tier: 1,
        customText: 'hello',
        customTextEmoji: '👋',
      });
    });
  });

  describe('reset / resetAllStores', () => {
    it('reset() clears the map and restores default self', () => {
      const store = useRichPresenceStore.getState();
      store.setCustomText('user-2', { text: 'gaming' });
      store.setSelfPresence({ tier: 3, customText: 'x' });
      store.reset();
      expect(useRichPresenceStore.getState().customTextByUser).toEqual({});
      expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
    });

    it('resetAllStores() clears everything', () => {
      const store = useRichPresenceStore.getState();
      store.setCustomText('user-2', { emoji: '🎮', text: 'gaming' });
      store.setSelfPresence({ tier: 2, customText: 'busy' });
      resetAllStores();
      expect(useRichPresenceStore.getState().customTextByUser).toEqual({});
      expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
    });
  });
});
