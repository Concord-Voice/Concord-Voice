import { describe, it, expect, beforeEach } from 'vitest';
import { useNotificationStore } from '../../../src/renderer/stores/notificationStore';

describe('notificationStore', () => {
  beforeEach(() => {
    localStorage.clear();
    // Reset store to defaults
    useNotificationStore.setState({
      enabled: true,
      volume: 80,
      messageSound: true,
      messageVolume: 100,
      mentionSound: true,
      mentionVolume: 100,
      dmSound: true,
      dmVolume: 100,
      friendRequestSound: true,
      friendRequestVolume: 100,
      voiceEventSounds: true,
      voiceEventVolume: 100,
      suppressWhenFocused: true,
      desktopNotificationsEnabled: true,
      desktopNotifyDMs: true,
      desktopNotifyMentions: true,
      desktopNotifyAllMessages: false,
      doNotDisturb: false,
      quietHoursEnabled: false,
      quietHoursStart: '22:00',
      quietHoursEnd: '08:00',
      notificationContent: 'full',
    });
  });

  it('has correct defaults', () => {
    const state = useNotificationStore.getState();
    expect(state.enabled).toBe(true);
    expect(state.volume).toBe(80);
    expect(state.messageSound).toBe(true);
    expect(state.mentionSound).toBe(true);
    expect(state.dmSound).toBe(true);
    expect(state.friendRequestSound).toBe(true);
    expect(state.voiceEventSounds).toBe(true);
    expect(state.suppressWhenFocused).toBe(true);
  });

  it('toggles master enabled', () => {
    useNotificationStore.getState().setEnabled(false);
    expect(useNotificationStore.getState().enabled).toBe(false);
    useNotificationStore.getState().setEnabled(true);
    expect(useNotificationStore.getState().enabled).toBe(true);
  });

  it('sets volume with clamping', () => {
    useNotificationStore.getState().setVolume(50);
    expect(useNotificationStore.getState().volume).toBe(50);

    useNotificationStore.getState().setVolume(-10);
    expect(useNotificationStore.getState().volume).toBe(0);

    useNotificationStore.getState().setVolume(200);
    expect(useNotificationStore.getState().volume).toBe(100);
  });

  it('toggles per-category sounds', () => {
    const { setMessageSound, setMentionSound, setDmSound, setFriendRequestSound } =
      useNotificationStore.getState();

    setMessageSound(false);
    expect(useNotificationStore.getState().messageSound).toBe(false);

    setMentionSound(false);
    expect(useNotificationStore.getState().mentionSound).toBe(false);

    setDmSound(false);
    expect(useNotificationStore.getState().dmSound).toBe(false);

    setFriendRequestSound(false);
    expect(useNotificationStore.getState().friendRequestSound).toBe(false);
  });

  it('toggles voiceEventSounds', () => {
    useNotificationStore.getState().setVoiceEventSounds(false);
    expect(useNotificationStore.getState().voiceEventSounds).toBe(false);
  });

  it('toggles suppressWhenFocused', () => {
    useNotificationStore.getState().setSuppressWhenFocused(false);
    expect(useNotificationStore.getState().suppressWhenFocused).toBe(false);
  });

  it('has correct defaults for desktop notification fields', () => {
    const state = useNotificationStore.getState();
    expect(state.desktopNotificationsEnabled).toBe(true);
    expect(state.desktopNotifyDMs).toBe(true);
    expect(state.desktopNotifyMentions).toBe(true);
    expect(state.desktopNotifyAllMessages).toBe(false);
    expect(state.doNotDisturb).toBe(false);
    expect(state.quietHoursEnabled).toBe(false);
    expect(state.quietHoursStart).toBe('22:00');
    expect(state.quietHoursEnd).toBe('08:00');
    expect(state.notificationContent).toBe('full');
  });

  it('sets notificationContent', () => {
    useNotificationStore.getState().setNotificationContent('sender_only');
    expect(useNotificationStore.getState().notificationContent).toBe('sender_only');

    useNotificationStore.getState().setNotificationContent('minimal');
    expect(useNotificationStore.getState().notificationContent).toBe('minimal');
  });

  it('persists notificationContent', () => {
    useNotificationStore.getState().setNotificationContent('minimal');

    const stored = JSON.parse(localStorage.getItem('concord:notification-sounds') || '{}');
    expect(stored.state.notificationContent).toBe('minimal');
  });

  it('fails closed for invalid persisted notificationContent values', async () => {
    localStorage.setItem(
      'concord:notification-sounds',
      JSON.stringify({ state: { notificationContent: 'full_body_preview' }, version: 0 })
    );

    await (
      useNotificationStore as typeof useNotificationStore & {
        persist: { rehydrate: () => Promise<void> };
      }
    ).persist.rehydrate();

    expect(useNotificationStore.getState().notificationContent).toBe('minimal');
  });

  it('toggles desktopNotificationsEnabled', () => {
    useNotificationStore.getState().setDesktopNotificationsEnabled(false);
    expect(useNotificationStore.getState().desktopNotificationsEnabled).toBe(false);
    useNotificationStore.getState().setDesktopNotificationsEnabled(true);
    expect(useNotificationStore.getState().desktopNotificationsEnabled).toBe(true);
  });

  it('toggles desktopNotifyDMs', () => {
    useNotificationStore.getState().setDesktopNotifyDMs(false);
    expect(useNotificationStore.getState().desktopNotifyDMs).toBe(false);
    useNotificationStore.getState().setDesktopNotifyDMs(true);
    expect(useNotificationStore.getState().desktopNotifyDMs).toBe(true);
  });

  it('toggles desktopNotifyMentions', () => {
    useNotificationStore.getState().setDesktopNotifyMentions(false);
    expect(useNotificationStore.getState().desktopNotifyMentions).toBe(false);
    useNotificationStore.getState().setDesktopNotifyMentions(true);
    expect(useNotificationStore.getState().desktopNotifyMentions).toBe(true);
  });

  it('toggles desktopNotifyAllMessages', () => {
    useNotificationStore.getState().setDesktopNotifyAllMessages(true);
    expect(useNotificationStore.getState().desktopNotifyAllMessages).toBe(true);
    useNotificationStore.getState().setDesktopNotifyAllMessages(false);
    expect(useNotificationStore.getState().desktopNotifyAllMessages).toBe(false);
  });

  it('toggles doNotDisturb', () => {
    useNotificationStore.getState().setDoNotDisturb(true);
    expect(useNotificationStore.getState().doNotDisturb).toBe(true);
    useNotificationStore.getState().setDoNotDisturb(false);
    expect(useNotificationStore.getState().doNotDisturb).toBe(false);
  });

  it('toggles quietHoursEnabled', () => {
    useNotificationStore.getState().setQuietHoursEnabled(true);
    expect(useNotificationStore.getState().quietHoursEnabled).toBe(true);
    useNotificationStore.getState().setQuietHoursEnabled(false);
    expect(useNotificationStore.getState().quietHoursEnabled).toBe(false);
  });

  it('sets quietHoursStart', () => {
    useNotificationStore.getState().setQuietHoursStart('23:30');
    expect(useNotificationStore.getState().quietHoursStart).toBe('23:30');
  });

  it('sets quietHoursEnd', () => {
    useNotificationStore.getState().setQuietHoursEnd('07:00');
    expect(useNotificationStore.getState().quietHoursEnd).toBe('07:00');
  });

  describe('per-category volume', () => {
    it('has default per-category volumes of 100', () => {
      const state = useNotificationStore.getState();
      expect(state.messageVolume).toBe(100);
      expect(state.mentionVolume).toBe(100);
      expect(state.dmVolume).toBe(100);
      expect(state.friendRequestVolume).toBe(100);
      expect(state.voiceEventVolume).toBe(100);
    });

    it('sets messageVolume with clamping', () => {
      useNotificationStore.getState().setMessageVolume(60);
      expect(useNotificationStore.getState().messageVolume).toBe(60);

      useNotificationStore.getState().setMessageVolume(-5);
      expect(useNotificationStore.getState().messageVolume).toBe(0);

      useNotificationStore.getState().setMessageVolume(150);
      expect(useNotificationStore.getState().messageVolume).toBe(100);
    });

    it('sets mentionVolume with clamping', () => {
      useNotificationStore.getState().setMentionVolume(75);
      expect(useNotificationStore.getState().mentionVolume).toBe(75);

      useNotificationStore.getState().setMentionVolume(-1);
      expect(useNotificationStore.getState().mentionVolume).toBe(0);

      useNotificationStore.getState().setMentionVolume(101);
      expect(useNotificationStore.getState().mentionVolume).toBe(100);
    });

    it('sets dmVolume with clamping', () => {
      useNotificationStore.getState().setDmVolume(42);
      expect(useNotificationStore.getState().dmVolume).toBe(42);

      useNotificationStore.getState().setDmVolume(-50);
      expect(useNotificationStore.getState().dmVolume).toBe(0);

      useNotificationStore.getState().setDmVolume(500);
      expect(useNotificationStore.getState().dmVolume).toBe(100);
    });

    it('sets friendRequestVolume with clamping', () => {
      useNotificationStore.getState().setFriendRequestVolume(20);
      expect(useNotificationStore.getState().friendRequestVolume).toBe(20);

      useNotificationStore.getState().setFriendRequestVolume(-100);
      expect(useNotificationStore.getState().friendRequestVolume).toBe(0);

      useNotificationStore.getState().setFriendRequestVolume(999);
      expect(useNotificationStore.getState().friendRequestVolume).toBe(100);
    });

    it('sets voiceEventVolume with clamping', () => {
      useNotificationStore.getState().setVoiceEventVolume(33);
      expect(useNotificationStore.getState().voiceEventVolume).toBe(33);

      useNotificationStore.getState().setVoiceEventVolume(-1);
      expect(useNotificationStore.getState().voiceEventVolume).toBe(0);

      useNotificationStore.getState().setVoiceEventVolume(1000);
      expect(useNotificationStore.getState().voiceEventVolume).toBe(100);
    });

    it('per-category volumes are independent', () => {
      const store = useNotificationStore.getState();
      store.setMessageVolume(10);
      store.setMentionVolume(20);
      store.setDmVolume(30);
      store.setFriendRequestVolume(40);
      store.setVoiceEventVolume(50);

      const after = useNotificationStore.getState();
      expect(after.messageVolume).toBe(10);
      expect(after.mentionVolume).toBe(20);
      expect(after.dmVolume).toBe(30);
      expect(after.friendRequestVolume).toBe(40);
      expect(after.voiceEventVolume).toBe(50);
    });
  });
});
