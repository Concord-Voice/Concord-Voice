import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { isTrayStatusSnapshot, type TrayStatusSnapshot } from '@/shared/trayStatus';
import {
  buildTraySnapshot,
  startTraySync,
  TRAY_COALESCE_MS,
} from '@/renderer/services/system/traySync';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useNotificationPrefsStore } from '@/renderer/stores/ui/notificationPrefsStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useSettingsStore } from '@/renderer/stores/ui/settingsStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';

const bridge = globalThis.electron as Record<string, unknown>;
const original = bridge.traySetStatus;
const traySetStatus = vi.fn<(s: TrayStatusSnapshot) => Promise<boolean | void>>();

function reset() {
  useAuthStore.setState({ accessToken: 'tok' } as never);
  useUserStore.setState({ user: { id: 'me' } } as never);
  useMemberStore.setState({ selfStatus: 'online' });
  useConnectionStore.setState({ phase: 'stable' } as never);
  useVoiceStore.setState({
    activeChannelId: null,
    activeServerId: null,
    isDMCall: false,
    isMuted: false,
    isDeafened: false,
    isScreenSharing: false,
    mediaPolicyPaused: {},
    participants: {},
  } as never);
  useUnreadStore.getState().clearAll();
  useDMStore.setState({ conversations: [] });
}

function joinVoice(over: Record<string, unknown> = {}, me: Record<string, unknown> = {}) {
  useVoiceStore.setState({
    activeChannelId: 'v1',
    activeServerId: 's1',
    participants: { me: { isSpeaking: false, serverMuted: false, serverDeafened: false, ...me } },
    ...over,
  } as never);
}

describe('traySync', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(1_000_000);
    traySetStatus.mockReset();
    traySetStatus.mockResolvedValue(undefined);
    bridge.traySetStatus = traySetStatus;
    reset();
  });
  afterEach(() => {
    bridge.traySetStatus = original;
    vi.useRealTimers();
  });

  describe('buildTraySnapshot', () => {
    it('signed out', () => {
      useAuthStore.setState({ accessToken: null } as never);
      const s = buildTraySnapshot();
      expect(isTrayStatusSnapshot(s)).toBe(true);
      expect(s.auth).toBe('signed-out');
      expect(s.presence).toBe('offline');
    });

    it('idle', () => {
      const s = buildTraySnapshot();
      expect(isTrayStatusSnapshot(s)).toBe(true);
      expect(s).toMatchObject({
        auth: 'signed-in',
        voice: 'none',
        conn: 'ok',
        unread: 0,
        mention: false,
      });
    });

    it('in voice with locks, mute, share and speaking', () => {
      joinVoice(
        { isMuted: true, isScreenSharing: true, mediaPolicyPaused: { mic: 'p' } },
        { isSpeaking: true, serverDeafened: true }
      );
      const s = buildTraySnapshot();
      expect(isTrayStatusSnapshot(s)).toBe(true);
      expect(s).toMatchObject({
        voice: 'channel',
        muted: true,
        sharing: true,
        speaking: true,
        micLocked: true,
        deafenLocked: true,
      });
    });

    it('maps DM calls (server_id is the empty string) to dm-call', () => {
      joinVoice({ activeServerId: '', isDMCall: true });
      expect(buildTraySnapshot().voice).toBe('dm-call');
      joinVoice({ activeServerId: null, isDMCall: false });
      expect(buildTraySnapshot().voice).toBe('dm-call');
    });

    it('clears voice-only flags outside voice', () => {
      useVoiceStore.setState({ isMuted: true, isDeafened: true, isScreenSharing: true } as never);
      expect(buildTraySnapshot()).toMatchObject({ muted: false, deafened: false, sharing: false });
    });

    it.each([
      ['stable', 'ok'],
      ['grace_period', 'ok'],
      ['preflight', 'reconnecting'],
      ['recovery_a', 'reconnecting'],
      ['recovery_b', 'reconnecting'],
      ['fatal', 'lost'],
    ] as const)('conn phase %s -> %s', (phase, conn) => {
      useConnectionStore.setState({ phase } as never);
      const s = buildTraySnapshot();
      expect(isTrayStatusSnapshot(s)).toBe(true);
      expect(s.conn).toBe(conn);
    });

    it('clamps unread to 99 and flags mentions', () => {
      useDMStore.setState({ conversations: [{ id: 'd', unreadCount: 250 }] } as never);
      expect(buildTraySnapshot().unread).toBe(99);
      useUnreadStore.getState().incrementMention('c1');
      expect(buildTraySnapshot().mention).toBe(true);
      useUnreadStore.getState().clearAll();
      useUnreadStore.setState({ serverMentionSet: new Set(['s1']) });
      expect(buildTraySnapshot().mention).toBe(true);
    });

    it('ignores mentions in muted DMs, channels and servers', () => {
      const muted = { muted: true, mutedUntil: null, updatedAt: new Date(0) };
      useDMStore.setState({ conversations: [{ id: 'd-muted', unreadCount: 1 }] } as never);
      useUnreadStore.setState({
        mentionCounts: new Map([
          ['d-muted', 1],
          ['c-muted', 2],
        ]),
        allUnreadCounts: new Map([['c-muted', { serverId: 's-any', count: 2 }]]),
        serverMentionSet: new Set(['s-muted']),
      });
      useNotificationPrefsStore.setState({
        mutedDMs: new Map([['d-muted', muted]]),
        mutedChannels: new Map([['c-muted', muted]]),
        mutedServers: new Map([['s-muted', muted]]),
      });
      try {
        expect(buildTraySnapshot().mention).toBe(false);
        useNotificationPrefsStore.setState({ mutedDMs: new Map() });
        expect(buildTraySnapshot().mention).toBe(true);
      } finally {
        useNotificationPrefsStore.setState({
          mutedDMs: new Map(),
          mutedChannels: new Map(),
          mutedServers: new Map(),
        });
      }
    });

    it('keeps a mention from an unmuted override channel in a muted server', () => {
      const muted = { muted: true, mutedUntil: null, updatedAt: new Date(0) };
      useUnreadStore.setState({
        serverMentionSet: new Set(['s-muted']),
        serverUnreadChannelWinsSet: new Set(['s-muted']),
      });
      useNotificationPrefsStore.setState({ mutedServers: new Map([['s-muted', muted]]) });
      try {
        expect(buildTraySnapshot().mention).toBe(true);
        useUnreadStore.setState({ serverUnreadChannelWinsSet: new Set() });
        expect(buildTraySnapshot().mention).toBe(false);
      } finally {
        useNotificationPrefsStore.setState({ mutedServers: new Map() });
      }
    });

    it('resolves an unknown channel against the active server for mute', () => {
      const muted = { muted: true, mutedUntil: null, updatedAt: new Date(0) };
      useUnreadStore.setState({ mentionCounts: new Map([['c2', 1]]), allUnreadCounts: new Map() });
      useServerStore.setState({ activeServerId: 's1' } as never);
      useNotificationPrefsStore.setState({ mutedServers: new Map([['s1', muted]]) });
      try {
        expect(buildTraySnapshot().mention).toBe(false);
      } finally {
        useNotificationPrefsStore.setState({ mutedServers: new Map() });
        useServerStore.setState({ activeServerId: null } as never);
      }
    });

    it('reads iconStyle and reducedMotion from settings', () => {
      const prev = useSettingsStore.getState().appearance;
      useSettingsStore.setState({
        appearance: { ...prev, trayIconStyle: 'dark', reduceAnimations: true },
      });
      expect(buildTraySnapshot()).toMatchObject({ iconStyle: 'dark', reducedMotion: true });
      useSettingsStore.setState({ appearance: prev });
    });
  });

  describe('startTraySync', () => {
    it('sends once on start', () => {
      const stop = startTraySync();
      expect(traySetStatus).toHaveBeenCalledTimes(1);
      expect(isTrayStatusSnapshot(traySetStatus.mock.calls[0][0])).toBe(true);
      stop();
    });

    it('is edge-triggered: an unchanged snapshot is not re-sent', () => {
      const stop = startTraySync();
      useUnreadStore.getState().clearAll();
      useMemberStore.setState({ selfStatus: 'online' });
      vi.advanceTimersByTime(500);
      expect(traySetStatus).toHaveBeenCalledTimes(1);
      stop();
    });

    it('coalesces: 5 changes in 50 ms send at most 2', () => {
      const stop = startTraySync();
      traySetStatus.mockClear();
      vi.advanceTimersByTime(200);
      const statuses = ['dnd', 'invisible', 'online', 'dnd', 'invisible'] as const;
      for (const st of statuses) {
        useMemberStore.setState({ selfStatus: st });
        vi.advanceTimersByTime(10);
      }
      vi.advanceTimersByTime(200);
      expect(traySetStatus.mock.calls.length).toBeLessThanOrEqual(2);
      expect(traySetStatus.mock.calls.at(-1)?.[0].presence).toBe('invisible');
      stop();
    });

    it('resends a snapshot main dropped under its rate limit', async () => {
      joinVoice();
      const stop = startTraySync();
      traySetStatus.mockClear();
      traySetStatus.mockResolvedValueOnce(false);
      useVoiceStore.setState({ isMuted: true } as never);
      expect(traySetStatus).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(TRAY_COALESCE_MS);
      expect(traySetStatus).toHaveBeenCalledTimes(2);
      expect(traySetStatus.mock.calls[1][0].muted).toBe(true);
      // Accepted this time: no further resend.
      await vi.advanceTimersByTimeAsync(500);
      expect(traySetStatus).toHaveBeenCalledTimes(2);
      stop();
    });

    it('does not resend a dropped snapshot once a newer one has gone out', async () => {
      joinVoice();
      const stop = startTraySync();
      traySetStatus.mockClear();
      let release: (v: boolean) => void = () => {};
      traySetStatus.mockReturnValueOnce(new Promise<boolean>((r) => (release = r)));
      useVoiceStore.setState({ isMuted: true } as never);
      useVoiceStore.setState({ isDeafened: true } as never);
      release(false);
      await vi.advanceTimersByTimeAsync(500);
      expect(traySetStatus).toHaveBeenCalledTimes(2);
      expect(traySetStatus.mock.calls[1][0].deafened).toBe(true);
      stop();
    });

    it('mute edge is sent immediately, inside the coalescing window', () => {
      joinVoice();
      const stop = startTraySync();
      traySetStatus.mockClear();
      useMemberStore.setState({ selfStatus: 'dnd' });
      useVoiceStore.setState({ isMuted: true } as never);
      expect(traySetStatus.mock.calls.at(-1)?.[0].muted).toBe(true);
      useVoiceStore.setState({ isDeafened: true } as never);
      expect(traySetStatus.mock.calls.at(-1)?.[0].deafened).toBe(true);
      stop();
    });

    it('speaking: false->true immediate, true->false only after the 300 ms hold', () => {
      joinVoice();
      const stop = startTraySync();
      traySetStatus.mockClear();
      useVoiceStore.setState({ participants: { me: { isSpeaking: true } } } as never);
      expect(traySetStatus.mock.calls.at(-1)?.[0].speaking).toBe(true);
      const sent = traySetStatus.mock.calls.length;
      useVoiceStore.setState({ participants: { me: { isSpeaking: false } } } as never);
      vi.advanceTimersByTime(299);
      expect(traySetStatus.mock.calls.length).toBe(sent);
      vi.advanceTimersByTime(2);
      expect(traySetStatus.mock.calls.at(-1)?.[0].speaking).toBe(false);
      stop();
    });

    it('speaking resuming inside the hold cancels the fall', () => {
      joinVoice();
      const stop = startTraySync();
      useVoiceStore.setState({ participants: { me: { isSpeaking: true } } } as never);
      traySetStatus.mockClear();
      useVoiceStore.setState({ participants: { me: { isSpeaking: false } } } as never);
      vi.advanceTimersByTime(200);
      useVoiceStore.setState({ participants: { me: { isSpeaking: true } } } as never);
      vi.advanceTimersByTime(1000);
      expect(traySetStatus).not.toHaveBeenCalled();
      stop();
    });

    it('traySync no-ops when the bridge lacks traySetStatus', () => {
      bridge.traySetStatus = undefined;
      let stop: () => void = () => undefined;
      expect(() => {
        stop = startTraySync();
        useMemberStore.setState({ selfStatus: 'dnd' });
        vi.advanceTimersByTime(500);
        stop();
      }).not.toThrow();
      expect(traySetStatus).not.toHaveBeenCalled();
    });

    it('catches a rejected promise and a throwing bridge', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
      traySetStatus.mockRejectedValueOnce(new Error('secret'));
      const stop = startTraySync();
      await vi.advanceTimersByTimeAsync(0);
      expect(warn).toHaveBeenCalledWith('[tray] status push failed');
      warn.mockClear();
      traySetStatus.mockImplementationOnce(() => {
        throw new Error('sync boom');
      });
      vi.advanceTimersByTime(200);
      expect(() => useMemberStore.setState({ selfStatus: 'dnd' })).not.toThrow();
      await vi.advanceTimersByTimeAsync(0);
      expect(warn).toHaveBeenCalledWith('[tray] status push failed');
      expect(JSON.stringify(warn.mock.calls)).not.toContain('secret');
      stop();
      warn.mockRestore();
    });

    it('stops sending after cleanup', () => {
      const stop = startTraySync();
      stop();
      traySetStatus.mockClear();
      vi.advanceTimersByTime(200);
      useMemberStore.setState({ selfStatus: 'dnd' });
      vi.advanceTimersByTime(500);
      expect(traySetStatus).not.toHaveBeenCalled();
    });

    describe('__concordTrayForce (dev only)', () => {
      type Force = (s: TrayStatusSnapshot | null) => void;
      const force = () =>
        (globalThis as unknown as { __concordTrayForce?: Force }).__concordTrayForce;

      it('exists under DEV, pauses live sync, and null resumes', () => {
        expect(import.meta.env.DEV).toBe(true);
        const stop = startTraySync();
        const forced = {
          ...buildTraySnapshot(),
          voice: 'channel',
          muted: true,
        } as TrayStatusSnapshot;
        traySetStatus.mockClear();
        force()?.(forced);
        expect(traySetStatus).toHaveBeenLastCalledWith(forced);
        vi.advanceTimersByTime(200);
        useMemberStore.setState({ selfStatus: 'dnd' });
        vi.advanceTimersByTime(500);
        expect(traySetStatus).toHaveBeenCalledTimes(1);
        force()?.(null);
        expect(traySetStatus.mock.calls.at(-1)?.[0].presence).toBe('dnd');
        stop();
        expect(force()).toBeUndefined();
      });

      it('does not send a malformed forced value', () => {
        const stop = startTraySync();
        traySetStatus.mockClear();
        force()?.({ nope: true } as never);
        expect(traySetStatus).not.toHaveBeenCalled();
        stop();
      });

      it('is not installed when import.meta.env.DEV is false', () => {
        vi.stubEnv('DEV', false);
        const stop = startTraySync();
        expect(force()).toBeUndefined();
        stop();
        vi.unstubAllEnvs();
      });
    });
  });
});
