import { create } from 'zustand';
import { persist } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

/** Clamp a volume value to the 0–100 range */
const clamp = (value: number): number => Math.max(0, Math.min(100, value));

export interface NotificationSoundSettings {
  /** Master toggle for all notification sounds */
  enabled: boolean;
  /** Master volume level 0–100 (applied on top of per-category volume) */
  volume: number;
  /** Play sound for channel messages */
  messageSound: boolean;
  /** Per-category volume for channel messages 0–100 */
  messageVolume: number;
  /** Play distinct sound for @mentions */
  mentionSound: boolean;
  /** Per-category volume for @mentions 0–100 */
  mentionVolume: number;
  /** Play sound for DM messages */
  dmSound: boolean;
  /** Per-category volume for DM messages 0–100 */
  dmVolume: number;
  /** Play sound for friend requests */
  friendRequestSound: boolean;
  /** Per-category volume for friend requests 0–100 */
  friendRequestVolume: number;
  /** Play sounds for voice channel events (join, leave, mute, etc.) */
  voiceEventSounds: boolean;
  /** Per-category volume for voice events 0–100 */
  voiceEventVolume: number;
  /** Suppress sound when the window is focused on the channel that received the message */
  suppressWhenFocused: boolean;
  /** Master toggle for desktop notifications */
  desktopNotificationsEnabled: boolean;
  /** Show desktop notification for DM messages */
  desktopNotifyDMs: boolean;
  /** Show desktop notification for @mentions */
  desktopNotifyMentions: boolean;
  /** Show desktop notification for all channel messages */
  desktopNotifyAllMessages: boolean;
  /** Suppress all notifications when DND status is active */
  doNotDisturb: boolean;
  /** Enable quiet hours (time-based suppression) */
  quietHoursEnabled: boolean;
  /** Quiet hours start time (HH:MM format) */
  quietHoursStart: string;
  /** Quiet hours end time (HH:MM format) */
  quietHoursEnd: string;
}

interface NotificationStore extends NotificationSoundSettings {
  setEnabled: (enabled: boolean) => void;
  setVolume: (volume: number) => void;
  setMessageSound: (enabled: boolean) => void;
  setMessageVolume: (volume: number) => void;
  setMentionSound: (enabled: boolean) => void;
  setMentionVolume: (volume: number) => void;
  setDmSound: (enabled: boolean) => void;
  setDmVolume: (volume: number) => void;
  setFriendRequestSound: (enabled: boolean) => void;
  setFriendRequestVolume: (volume: number) => void;
  setVoiceEventSounds: (enabled: boolean) => void;
  setVoiceEventVolume: (volume: number) => void;
  setSuppressWhenFocused: (suppress: boolean) => void;
  setDesktopNotificationsEnabled: (enabled: boolean) => void;
  setDesktopNotifyDMs: (enabled: boolean) => void;
  setDesktopNotifyMentions: (enabled: boolean) => void;
  setDesktopNotifyAllMessages: (enabled: boolean) => void;
  setDoNotDisturb: (enabled: boolean) => void;
  setQuietHoursEnabled: (enabled: boolean) => void;
  setQuietHoursStart: (start: string) => void;
  setQuietHoursEnd: (end: string) => void;
}

export const useNotificationStore = wrapStore(create<NotificationStore>()(
  persist(
    (set) => ({
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

      setEnabled: (enabled) => set({ enabled }),
      setVolume: (volume) => set({ volume: clamp(volume) }),
      setMessageSound: (enabled) => set({ messageSound: enabled }),
      setMessageVolume: (volume) => set({ messageVolume: clamp(volume) }),
      setMentionSound: (enabled) => set({ mentionSound: enabled }),
      setMentionVolume: (volume) => set({ mentionVolume: clamp(volume) }),
      setDmSound: (enabled) => set({ dmSound: enabled }),
      setDmVolume: (volume) => set({ dmVolume: clamp(volume) }),
      setFriendRequestSound: (enabled) => set({ friendRequestSound: enabled }),
      setFriendRequestVolume: (volume) => set({ friendRequestVolume: clamp(volume) }),
      setVoiceEventSounds: (enabled) => set({ voiceEventSounds: enabled }),
      setVoiceEventVolume: (volume) => set({ voiceEventVolume: clamp(volume) }),
      setSuppressWhenFocused: (suppress) => set({ suppressWhenFocused: suppress }),
      setDesktopNotificationsEnabled: (enabled) => set({ desktopNotificationsEnabled: enabled }),
      setDesktopNotifyDMs: (enabled) => set({ desktopNotifyDMs: enabled }),
      setDesktopNotifyMentions: (enabled) => set({ desktopNotifyMentions: enabled }),
      setDesktopNotifyAllMessages: (enabled) => set({ desktopNotifyAllMessages: enabled }),
      setDoNotDisturb: (enabled) => set({ doNotDisturb: enabled }),
      setQuietHoursEnabled: (enabled) => set({ quietHoursEnabled: enabled }),
      setQuietHoursStart: (start) => set({ quietHoursStart: start }),
      setQuietHoursEnd: (end) => set({ quietHoursEnd: end }),
    }),
    {
      name: 'concord:notification-sounds',
      partialize: (state) => ({
        enabled: state.enabled,
        volume: state.volume,
        messageSound: state.messageSound,
        messageVolume: state.messageVolume,
        mentionSound: state.mentionSound,
        mentionVolume: state.mentionVolume,
        dmSound: state.dmSound,
        dmVolume: state.dmVolume,
        friendRequestSound: state.friendRequestSound,
        friendRequestVolume: state.friendRequestVolume,
        voiceEventSounds: state.voiceEventSounds,
        voiceEventVolume: state.voiceEventVolume,
        suppressWhenFocused: state.suppressWhenFocused,
        desktopNotificationsEnabled: state.desktopNotificationsEnabled,
        desktopNotifyDMs: state.desktopNotifyDMs,
        desktopNotifyMentions: state.desktopNotifyMentions,
        desktopNotifyAllMessages: state.desktopNotifyAllMessages,
        doNotDisturb: state.doNotDisturb,
        quietHoursEnabled: state.quietHoursEnabled,
        quietHoursStart: state.quietHoursStart,
        quietHoursEnd: state.quietHoursEnd,
      }),
      merge: (persisted, current) => ({
        ...current,
        ...(persisted as Partial<NotificationSoundSettings>),
      }),
    }
  )
));
