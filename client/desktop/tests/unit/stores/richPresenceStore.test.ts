import { describe, it, expect, beforeEach, vi } from 'vitest';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import * as richPresenceModule from '@/renderer/stores/ui/richPresenceStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { WebSocketService } from '@/renderer/services/messaging/websocketService';
import { resetAllStores } from '../../helpers/store-helpers';

describe('richPresenceStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts empty with default self presence', () => {
    expect(
      (useRichPresenceStore.getState() as unknown as { customTextByUser?: unknown })
        .customTextByUser
    ).toBeUndefined();
    expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
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
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'first' });
      useRichPresenceStore.getState().setCustomText('user-2', { emoji: '🚀', text: 'second' });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🚀',
        text: 'second',
      });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({
        emoji: '🚀',
        text: 'second',
      });
    });

    it('exposes the map for selective subscription', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'hi' });
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toEqual({ text: 'hi' });
    });

    it('returns undefined for an unknown user', () => {
      expect(useRichPresenceStore.getState().getCustomText('nobody')).toBeUndefined();
    });
  });

  describe('clearCustomText', () => {
    it('removes a stored entry', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'gaming' });
      useRichPresenceStore.getState().clearCustomText('user-2');
      expect(useRichPresenceStore.getState().getCustomText('user-2')).toBeUndefined();
      expect(
        (useRichPresenceStore.getState() as unknown as { customTextByUser?: unknown })
          .customTextByUser
      ).toBeUndefined();
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
      expect(
        (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
      ).toEqual({});
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
      useRichPresenceStore.getState().setSelfPresence({ tier: 1, customText: 'hello' });
      useRichPresenceStore.getState().setSelfPresence({ customTextEmoji: '👋' });
      expect(useRichPresenceStore.getState().self).toEqual({
        tier: 1,
        customText: 'hello',
        customTextEmoji: '👋',
      });
    });
  });

  describe('reset / resetAllStores', () => {
    it('reset() clears the map and restores default self', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { text: 'gaming' });
      useRichPresenceStore.getState().setSelfPresence({ tier: 3, customText: 'x' });
      useRichPresenceStore.getState().reset();
      expect(
        (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
      ).toEqual({});
      expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
    });

    it('resetAllStores() clears everything', () => {
      useRichPresenceStore.getState().setCustomText('user-2', { emoji: '🎮', text: 'gaming' });
      useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'busy' });
      resetAllStores();
      expect(
        (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
      ).toEqual({});
      expect(useRichPresenceStore.getState().self).toEqual({ tier: 0 });
    });
  });
});

describe('rich presence category map and local selector (#2233)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  type Contract = {
    otherByUser: Record<string, Record<string, unknown>>;
    setOtherPresence: (userId: string, entry: { category: string }) => void;
    clearOtherPresence: (userId: string, category: string) => void;
    replaceOtherPresence: (next: Record<string, Record<string, unknown>>) => void;
    clearAllOtherPresence: () => void;
  };
  const contract = () => useRichPresenceStore.getState() as unknown as Contract;

  it('keeps Server Voice and Private Call for one user as distinct category tuples', () => {
    const store = contract();
    expect(typeof store.setOtherPresence).toBe('function');
    if (typeof store.setOtherPresence !== 'function') return;
    store.setOtherPresence('other-user', { category: 'server_voice' });
    store.setOtherPresence('other-user', { category: 'private_call' });
    expect(contract().otherByUser['other-user']).toEqual({
      server_voice: { category: 'server_voice' },
      private_call: { category: 'private_call' },
    });
  });

  it('clears one tuple and prunes an empty user without touching another category', () => {
    const store = contract();
    expect(typeof store.replaceOtherPresence).toBe('function');
    if (typeof store.replaceOtherPresence !== 'function') return;
    store.replaceOtherPresence({
      'other-user': {
        server_voice: { category: 'server_voice' },
        private_call: { category: 'private_call' },
      },
    });
    store.clearOtherPresence('other-user', 'server_voice');
    expect(contract().otherByUser['other-user']).toEqual({
      private_call: { category: 'private_call' },
    });
    store.clearOtherPresence('other-user', 'private_call');
    expect(contract().otherByUser).toEqual({});
  });

  it('replaces the full map atomically and local activity selection is settings-independent', () => {
    const store = contract();
    expect(typeof store.replaceOtherPresence).toBe('function');
    if (typeof store.replaceOtherPresence === 'function') {
      store.replaceOtherPresence({ prior: { custom_text: { category: 'custom_text' } } });
      store.replaceOtherPresence({ current: { server_voice: { category: 'server_voice' } } });
      expect(contract().otherByUser).toEqual({
        current: { server_voice: { category: 'server_voice' } },
      });
    }
    const select = (
      richPresenceModule as unknown as {
        selectLocalRichPresenceActivity?: (state: unknown) => unknown;
      }
    ).selectLocalRichPresenceActivity;
    expect(typeof select).toBe('function');
    if (select) {
      const beforePresence = useRichPresenceStore.getState();
      const send = vi.spyOn(WebSocketService.prototype, 'send');
      try {
        useVoiceStore.setState({
          connectionState: 'connected',
          activeChannelId: 'channel-1',
          activeServerId: 'server-1',
          activeChannelName: 'Lounge',
          isDMCall: false,
          callState: { kind: 'idle' },
        });
        const serverVoiceInput = useVoiceStore.getState();
        const serverVoiceSnapshot = {
          activeChannelId: serverVoiceInput.activeChannelId,
          activeChannelName: serverVoiceInput.activeChannelName,
          activeServerId: serverVoiceInput.activeServerId,
          callStateKind: serverVoiceInput.callState.kind,
          connectionState: serverVoiceInput.connectionState,
          isDMCall: serverVoiceInput.isDMCall,
        };
        expect(select(serverVoiceInput)).toEqual({
          category: 'server_voice',
          channelId: 'channel-1',
          channelName: 'Lounge',
          serverId: 'server-1',
        });
        expect(useVoiceStore.getState()).toBe(serverVoiceInput);
        expect({
          activeChannelId: serverVoiceInput.activeChannelId,
          activeChannelName: serverVoiceInput.activeChannelName,
          activeServerId: serverVoiceInput.activeServerId,
          callStateKind: serverVoiceInput.callState.kind,
          connectionState: serverVoiceInput.connectionState,
          isDMCall: serverVoiceInput.isDMCall,
        }).toEqual(serverVoiceSnapshot);
        for (const kind of [
          'idle',
          'outgoing-ringing',
          'incoming-ringing',
          'joining',
          'ending',
        ] as const) {
          if (kind === 'idle') {
            useVoiceStore.setState({ isDMCall: true, callState: { kind } });
            expect(select(useVoiceStore.getState())).toBeNull();
            useVoiceStore.setState({ isDMCall: false });
            continue;
          }
          useVoiceStore.setState({
            callState: kind === 'ending' ? { kind } : ({ kind, conversationId: 'call-1' } as never),
          });
          expect(select(useVoiceStore.getState())).toBeNull();
        }
        useVoiceStore.setState({
          connectionState: 'connected',
          isDMCall: true,
          callState: { kind: 'in-call' },
          isGroupDM: false,
        });
        expect(select(useVoiceStore.getState())).toEqual({
          category: 'private_call',
          callType: 'dm',
        });
        useVoiceStore.setState({ isGroupDM: true });
        useVoiceStore.setState({
          participants: { a: { userId: 'a' }, b: { userId: 'b' } } as never,
        });
        expect(select(useVoiceStore.getState())).toEqual({
          category: 'private_call',
          callType: 'group',
          participantCount: 2,
        });
        for (const connectionState of ['disconnected', 'reconnecting', 'error'] as const) {
          useVoiceStore.setState({ connectionState });
          expect(select(useVoiceStore.getState())).toBeNull();
        }
        expect(useVoiceStore.getState()).toEqual(
          expect.objectContaining({ connectionState: 'error' })
        );
        expect(useRichPresenceStore.getState()).toBe(beforePresence);
        expect(send).not.toHaveBeenCalled();
      } finally {
        send.mockRestore();
      }
    }
  });

  it('compatibility Custom Status APIs delegate to the canonical map and no second projection exists', () => {
    useRichPresenceStore.getState().setCustomText('other-user', { text: 'hello' });
    expect(useRichPresenceStore.getState().getCustomText('other-user')).toEqual({ text: 'hello' });
    const current = useRichPresenceStore.getState();
    const state = current as unknown as {
      otherByUser?: Record<string, unknown>;
      customTextByUser?: unknown;
    };
    expect(state.otherByUser).toMatchObject({ 'other-user': { custom_text: expect.anything() } });
    expect(state.customTextByUser).toBeUndefined();
    const selector = (
      richPresenceModule as unknown as {
        selectCustomText?: (userId: string) => (s: unknown) => unknown;
      }
    ).selectCustomText;
    expect(typeof selector).toBe('function');
    if (selector) expect(selector('other-user')(current)).toEqual({ text: 'hello' });
  });

  it('clearAllOtherPresence removes every remote category while self survives', () => {
    const store = contract();
    expect(typeof store.clearAllOtherPresence).toBe('function');
    if (typeof store.clearAllOtherPresence !== 'function') return;
    useRichPresenceStore.setState({
      otherByUser: {
        remote: {
          server_voice: { category: 'server_voice' },
          private_call: { category: 'private_call' },
          custom_text: { category: 'custom_text' },
        },
      },
    } as never);
    useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'mine' });
    useRichPresenceStore.getState().clearAllOtherPresence();
    expect(
      (useRichPresenceStore.getState() as unknown as { otherByUser?: unknown }).otherByUser
    ).toEqual({});
    expect(useRichPresenceStore.getState().self.customText).toBe('mine');
  });
});
