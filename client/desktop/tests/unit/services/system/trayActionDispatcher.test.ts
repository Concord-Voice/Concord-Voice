import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

const toggleMute = vi.fn();
const toggleDeafen = vi.fn();
const leaveChannel = vi.fn();
const toggleScreenShare = vi.fn();
vi.mock('@/renderer/services/voice/voiceService', () => ({
  voiceService: {
    toggleMute: (...a: unknown[]) => toggleMute(...a),
    toggleDeafen: (...a: unknown[]) => toggleDeafen(...a),
    leaveChannel: (...a: unknown[]) => leaveChannel(...a),
    toggleScreenShare: (...a: unknown[]) => toggleScreenShare(...a),
  },
}));
const setOwnPresence = vi.fn();
vi.mock('@/renderer/services/presence/setOwnPresence', () => ({
  setOwnPresence: (...a: unknown[]) => setOwnPresence(...a),
}));

import {
  handleTrayAction,
  startTrayActionDispatcher,
} from '@/renderer/services/system/trayActionDispatcher';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useNotificationPrefsStore } from '@/renderer/stores/ui/notificationPrefsStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useNotificationNavigationStore } from '@/renderer/stores/ui/notificationNavigationStore';

function setVoice(over: Record<string, unknown> = {}, me: Record<string, unknown> = {}) {
  useUserStore.setState({ user: { id: 'me' } } as never);
  useVoiceStore.setState({
    activeChannelId: 'v1',
    isMuted: false,
    isDeafened: false,
    isScreenSharing: false,
    mediaPolicyPaused: {},
    participants: { me: { serverMuted: false, serverDeafened: false, ...me } },
    ...over,
  } as never);
}

describe('trayActionDispatcher', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useVoiceStore.setState({
      activeChannelId: null,
      participants: {},
      mediaPolicyPaused: {},
    } as never);
    useUnreadStore.getState().clearAll();
    useDMStore.setState({ conversations: [] });
    useServerStore.setState({ activeServerId: null });
    useNotificationNavigationStore.setState({ pendingNavigation: null });
  });
  afterEach(() => vi.restoreAllMocks());

  it('set-muted true while unmuted calls toggleMute once', () => {
    setVoice();
    handleTrayAction({ kind: 'set-muted', value: true });
    expect(toggleMute).toHaveBeenCalledTimes(1);
  });

  it('set-muted true while already muted is a no-op', () => {
    setVoice({ isMuted: true });
    handleTrayAction({ kind: 'set-muted', value: true });
    expect(toggleMute).not.toHaveBeenCalled();
  });

  it('set-muted false while muted unmutes', () => {
    setVoice({ isMuted: true });
    handleTrayAction({ kind: 'set-muted', value: false });
    expect(toggleMute).toHaveBeenCalledTimes(1);
  });

  it('locked control is not called (server-muted, policy-paused, server-deafened)', () => {
    setVoice({}, { serverMuted: true });
    handleTrayAction({ kind: 'set-muted', value: true });
    setVoice({ mediaPolicyPaused: { mic: 'p1' } });
    handleTrayAction({ kind: 'set-muted', value: true });
    expect(toggleMute).not.toHaveBeenCalled();
    setVoice({}, { serverDeafened: true });
    handleTrayAction({ kind: 'set-deafened', value: true });
    expect(toggleDeafen).not.toHaveBeenCalled();
  });

  it('set-muted is ignored while deafened and outside voice', () => {
    setVoice({ isDeafened: true });
    handleTrayAction({ kind: 'set-muted', value: true });
    useVoiceStore.setState({ activeChannelId: null } as never);
    handleTrayAction({ kind: 'set-muted', value: true });
    handleTrayAction({ kind: 'set-deafened', value: true });
    expect(toggleMute).not.toHaveBeenCalled();
    expect(toggleDeafen).not.toHaveBeenCalled();
  });

  it('set-deafened toggles only on a state change', () => {
    setVoice();
    handleTrayAction({ kind: 'set-deafened', value: true });
    expect(toggleDeafen).toHaveBeenCalledTimes(1);
    setVoice({ isDeafened: true });
    handleTrayAction({ kind: 'set-deafened', value: true });
    expect(toggleDeafen).toHaveBeenCalledTimes(1);
  });

  it('leave-voice calls leaveChannel only in voice', () => {
    handleTrayAction({ kind: 'leave-voice' });
    expect(leaveChannel).not.toHaveBeenCalled();
    setVoice();
    handleTrayAction({ kind: 'leave-voice' });
    expect(leaveChannel).toHaveBeenCalledTimes(1);
  });

  it('stop-share only when sharing', () => {
    setVoice();
    handleTrayAction({ kind: 'stop-share' });
    expect(toggleScreenShare).not.toHaveBeenCalled();
    setVoice({ isScreenSharing: true });
    handleTrayAction({ kind: 'stop-share' });
    expect(toggleScreenShare).toHaveBeenCalledTimes(1);
  });

  it('set-presence dnd calls setOwnPresence', () => {
    handleTrayAction({ kind: 'set-presence', value: 'dnd' });
    expect(setOwnPresence).toHaveBeenCalledWith('dnd');
  });

  it('swallows a rejected or throwing voice call', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    setVoice();
    toggleMute.mockRejectedValueOnce(new Error('secret-id-123'));
    handleTrayAction({ kind: 'set-muted', value: true });
    toggleDeafen.mockImplementationOnce(() => {
      throw new Error('boom');
    });
    handleTrayAction({ kind: 'set-deafened', value: true });
    await Promise.resolve();
    await Promise.resolve();
    expect(warn).toHaveBeenCalledTimes(2);
    expect(JSON.stringify(warn.mock.calls)).not.toContain('secret');
  });

  describe('open-unread (LIFO)', () => {
    it('navigates to the newest still-unread conversation', () => {
      useUnreadStore.getState().incrementUnread('c-old');
      useUnreadStore.getState().bumpChannelUnread('c-new', 's1');
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation).toEqual({
        type: 'channel',
        targetId: 'c-new',
        serverId: 's1',
      });
    });

    it('skips entries already read and falls through to the next', () => {
      useUnreadStore.getState().incrementUnread('c-old');
      useUnreadStore.getState().bumpChannelUnread('c-new', 's1');
      // Read without going through clearUnread: the stack still holds c-new.
      useUnreadStore.setState({ allUnreadCounts: new Map() });
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation?.targetId).toBe('c-old');
    });

    it('opens a DM and omits serverId', () => {
      useDMStore.setState({ conversations: [{ id: 'd1', unreadCount: 0 }] } as never);
      useDMStore.getState().incrementUnread('d1');
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation).toEqual({
        type: 'dm',
        targetId: 'd1',
      });
    });

    it('falls back to the most active unread DM when restored unread never hit the stack', () => {
      useDMStore.setState({
        conversations: [
          { id: 'd-read', unreadCount: 0 },
          { id: 'd-new', unreadCount: 2 },
          { id: 'd-old', unreadCount: 5 },
        ],
      } as never);
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation).toEqual({
        type: 'dm',
        targetId: 'd-new',
      });
    });

    it('falls back to a restored unread channel when no DM is unread', () => {
      useUnreadStore.setState({
        allUnreadCounts: new Map([
          ['c-zero', { serverId: 's1', count: 0 }],
          ['c-restored', { serverId: 's2', count: 3 }],
        ]),
      });
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation).toEqual({
        type: 'channel',
        targetId: 'c-restored',
        serverId: 's2',
      });
    });

    it('skips a muted DM on the live stack, even one muted after it was pushed', () => {
      const muted = { muted: true, mutedUntil: null, updatedAt: new Date(0) };
      useUnreadStore.getState().bumpChannelUnread('c-open', 's1');
      useDMStore.setState({ conversations: [{ id: 'd-muted', unreadCount: 0 }] } as never);
      useDMStore.getState().incrementUnread('d-muted');
      useNotificationPrefsStore.setState({ mutedDMs: new Map([['d-muted', muted]]) });
      try {
        handleTrayAction({ kind: 'open-unread' });
        expect(useNotificationNavigationStore.getState().pendingNavigation?.targetId).toBe(
          'c-open'
        );
      } finally {
        useNotificationPrefsStore.setState({ mutedDMs: new Map() });
      }
    });

    it('skips muted conversations in the fallback', () => {
      const muted = { muted: true, mutedUntil: null, updatedAt: new Date(0) };
      useDMStore.setState({ conversations: [{ id: 'd-muted', unreadCount: 4 }] } as never);
      useUnreadStore.setState({
        allUnreadCounts: new Map([
          ['c-muted', { serverId: 's1', count: 2 }],
          ['c-open', { serverId: 's1', count: 1 }],
        ]),
      });
      useNotificationPrefsStore.setState({
        mutedDMs: new Map([['d-muted', muted]]),
        mutedChannels: new Map([['c-muted', muted]]),
      });
      try {
        handleTrayAction({ kind: 'open-unread' });
        expect(useNotificationNavigationStore.getState().pendingNavigation?.targetId).toBe(
          'c-open'
        );
      } finally {
        useNotificationPrefsStore.setState({ mutedDMs: new Map(), mutedChannels: new Map() });
      }
    });

    // Active-server unread lives only in `unreadCounts` (e.g. setUnreadCount on
    // leaving a scrolled-up channel), so the fallback must read it like the badge does.
    it('falls back to an active-server channel held only in unreadCounts', () => {
      useServerStore.setState({ activeServerId: 's1' });
      useUnreadStore.getState().setInitialUnreads(new Map([['c-active', 3]]), 's1');
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation).toEqual({
        type: 'channel',
        targetId: 'c-active',
        serverId: 's1',
      });
    });

    it('skips stale active-server bulk rows once unreadCounts owns the active server', () => {
      useServerStore.setState({ activeServerId: 's1' });
      useUnreadStore.getState().setInitialUnreads(new Map([['c-read', 0]]), 's1');
      useUnreadStore.setState({
        allUnreadCounts: new Map([
          ['c-read', { serverId: 's1', count: 4 }],
          ['c-bg', { serverId: 's2', count: 1 }],
        ]),
      });
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation?.targetId).toBe('c-bg');
    });

    it('ignores a previous server snapshot and falls back to the bulk map', () => {
      useServerStore.setState({ activeServerId: 's2' });
      useUnreadStore.getState().setInitialUnreads(new Map([['c-old-server', 5]]), 's1');
      useUnreadStore.setState({
        allUnreadCounts: new Map([['c-s2', { serverId: 's2', count: 1 }]]),
      });
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation).toEqual({
        type: 'channel',
        targetId: 'c-s2',
        serverId: 's2',
      });
    });

    it('skips a muted active-server channel in unreadCounts', () => {
      const muted = { muted: true, mutedUntil: null, updatedAt: new Date(0) };
      useServerStore.setState({ activeServerId: 's1' });
      useUnreadStore.getState().setInitialUnreads(
        new Map([
          ['c-muted', 2],
          ['c-open', 1],
        ]),
        's1'
      );
      useNotificationPrefsStore.setState({ mutedChannels: new Map([['c-muted', muted]]) });
      try {
        handleTrayAction({ kind: 'open-unread' });
        expect(useNotificationNavigationStore.getState().pendingNavigation?.targetId).toBe(
          'c-open'
        );
      } finally {
        useNotificationPrefsStore.setState({ mutedChannels: new Map() });
      }
    });

    it('does nothing when nothing is unread', () => {
      handleTrayAction({ kind: 'open-unread' });
      expect(useNotificationNavigationStore.getState().pendingNavigation).toBeNull();
    });
  });

  describe('startTrayActionDispatcher', () => {
    const original = (globalThis.electron as Record<string, unknown>).onTrayAction;
    afterEach(() => {
      (globalThis.electron as Record<string, unknown>).onTrayAction = original;
    });

    it('is a no-op when the bridge lacks onTrayAction', () => {
      (globalThis.electron as Record<string, unknown>).onTrayAction = undefined;
      expect(() => startTrayActionDispatcher()()).not.toThrow();
    });

    it('routes valid actions, drops invalid ones, and unsubscribes', () => {
      let cb: (a: unknown) => void = () => undefined;
      const unsub = vi.fn();
      (globalThis.electron as Record<string, unknown>).onTrayAction = (f: (a: unknown) => void) => {
        cb = f;
        return unsub;
      };
      const stop = startTrayActionDispatcher();
      cb({ kind: 'set-presence', value: 'invisible' });
      cb({ kind: 'evil' });
      expect(setOwnPresence).toHaveBeenCalledTimes(1);
      expect(setOwnPresence).toHaveBeenCalledWith('invisible');
      stop();
      expect(unsub).toHaveBeenCalled();
    });

    it('tolerates a bridge that returns no unsubscribe', () => {
      (globalThis.electron as Record<string, unknown>).onTrayAction = () => undefined;
      expect(() => startTrayActionDispatcher()()).not.toThrow();
    });
  });
});
