import { useNotificationStore } from '../stores/notificationStore';

/** Chat notification sound types */
export type ChatSoundType = 'message' | 'mention' | 'dm' | 'friend-request';

/** Voice event sound types */
export type VoiceSoundType =
  | 'voice-join'
  | 'voice-leave'
  | 'user-join'
  | 'user-leave'
  | 'mute'
  | 'unmute'
  | 'deafen'
  | 'undeafen'
  | 'video-on'
  | 'video-off'
  | 'screen-on'
  | 'screen-off'
  | 'disconnect';

/** DM call sound types */
export type CallSoundType =
  | 'call-ringing'
  | 'call-outgoing'
  | 'call-connected'
  | 'call-ended'
  | 'call-declined'
  | 'call-busy';

export type NotificationSoundType = ChatSoundType | VoiceSoundType | CallSoundType;

const SOUND_PATHS: Record<NotificationSoundType, string> = {
  message: './sounds/message.wav',
  mention: './sounds/mention.wav',
  dm: './sounds/dm.wav',
  'friend-request': './sounds/friend-request.wav',
  'voice-join': './sounds/voice-join.wav',
  'voice-leave': './sounds/voice-leave.wav',
  'user-join': './sounds/user-join.wav',
  'user-leave': './sounds/user-leave.wav',
  mute: './sounds/mute.wav',
  unmute: './sounds/unmute.wav',
  deafen: './sounds/deafen.wav',
  undeafen: './sounds/undeafen.wav',
  'video-on': './sounds/video-on.wav',
  'video-off': './sounds/video-off.wav',
  'screen-on': './sounds/screen-on.wav',
  'screen-off': './sounds/screen-off.wav',
  disconnect: './sounds/disconnect.wav',
  'call-ringing': './sounds/call-ringing.wav',
  'call-outgoing': './sounds/call-outgoing.wav',
  'call-connected': './sounds/call-connected.wav',
  'call-ended': './sounds/call-ended.wav',
  'call-declined': './sounds/call-declined.wav',
  'call-busy': './sounds/call-busy.wav',
};

/** Minimum interval between chat notification sounds (ms) */
const CHAT_DEBOUNCE_MS = 2000;

/** No-op handler for swallowing autoplay rejections */
const noop = () => {
  /* intentionally empty */
};

const clampVolume = (volume: number): number => Math.max(0, Math.min(1, volume));

type NotificationSoundSettings = ReturnType<typeof useNotificationStore.getState>;

/** Maps each chat sound type to the store key that controls it */
const CHAT_TOGGLE: Record<ChatSoundType, keyof NotificationSoundSettings> = {
  message: 'messageSound',
  mention: 'mentionSound',
  dm: 'dmSound',
  'friend-request': 'friendRequestSound',
};

/** Maps each chat sound type to its per-category volume store key */
const CHAT_VOLUME: Record<ChatSoundType, keyof NotificationSoundSettings> = {
  message: 'messageVolume',
  mention: 'mentionVolume',
  dm: 'dmVolume',
  'friend-request': 'friendRequestVolume',
};

/** Set of sound types that are voice-event feedback (no debounce, no focus suppression) */
const VOICE_SOUNDS = new Set<NotificationSoundType>([
  'voice-join',
  'voice-leave',
  'user-join',
  'user-leave',
  'mute',
  'unmute',
  'deafen',
  'undeafen',
  'video-on',
  'video-off',
  'screen-on',
  'screen-off',
  'disconnect',
  'call-ringing',
  'call-outgoing',
  'call-connected',
  'call-ended',
  'call-declined',
  'call-busy',
]);

/** Sound types that loop until explicitly stopped */
const LOOPING_SOUNDS = new Set<NotificationSoundType>(['call-ringing', 'call-outgoing']);

class NotificationSoundService {
  private readonly sounds = new Map<NotificationSoundType, HTMLAudioElement>();
  private readonly activeLoops = new Set<NotificationSoundType>();
  private lastChatSoundTime = 0;
  private initialized = false;

  /** Pre-load all notification audio elements */
  init(): void {
    if (this.initialized) return;

    for (const [type, path] of Object.entries(SOUND_PATHS)) {
      const audio = new Audio(path);
      audio.preload = 'auto';
      this.sounds.set(type as NotificationSoundType, audio);
    }

    this.initialized = true;
  }

  /**
   * Compute the effective volume (0.0–1.0) for a sound by combining
   * the master volume with the per-category volume.
   */
  private effectiveVolume(type: NotificationSoundType, state: NotificationSoundSettings): number {
    const master = state.volume / 100;
    const category = VOICE_SOUNDS.has(type)
      ? (state.voiceEventVolume as number) / 100
      : ((state[CHAT_VOLUME[type as ChatSoundType]] as number) ?? 100) / 100;
    return master * category;
  }

  /** Check whether a chat sound should be suppressed */
  private isChatSuppressed(
    type: ChatSoundType,
    state: NotificationSoundSettings,
    focused: boolean
  ): boolean {
    if (focused && state.suppressWhenFocused && document.hasFocus()) return true;
    if (!state[CHAT_TOGGLE[type]]) return true;
    if (Date.now() - this.lastChatSoundTime < CHAT_DEBOUNCE_MS) return true;
    return false;
  }

  /** Check whether settings allow this sound type to play. */
  private isTypeEnabled(type: NotificationSoundType, state: NotificationSoundSettings): boolean {
    if (VOICE_SOUNDS.has(type)) return state.voiceEventSounds;
    return Boolean(state[CHAT_TOGGLE[type as ChatSoundType]]);
  }

  /**
   * Play a notification sound if settings allow it.
   * @param type - Sound category to play
   * @param options.focused - If true, the user is viewing the source channel/conversation.
   *   Only applies to chat sounds (voice sounds ignore this).
   */
  play(type: NotificationSoundType, options?: { focused?: boolean }): void {
    if (!this.initialized) this.init();

    const state = useNotificationStore.getState();
    if (!state.enabled) return;

    const isVoice = VOICE_SOUNDS.has(type);

    // Voice sounds: check the single voiceEventSounds toggle
    if (isVoice && !state.voiceEventSounds) return;

    // Chat sounds: per-category toggle, focus suppression, debounce
    if (
      !isVoice &&
      this.isChatSuppressed(type as ChatSoundType, state, options?.focused ?? false)
    ) {
      return;
    }

    const audio = this.sounds.get(type);
    if (!audio) return;

    audio.volume = this.effectiveVolume(type, state);
    audio.currentTime = 0;
    Promise.resolve(audio.play()).catch(noop);

    if (!isVoice) {
      this.lastChatSoundTime = Date.now();
    }
  }

  /**
   * Play a user-initiated preview sound at a supplied effective volume.
   * Bypasses chat debounce and focus suppression, but still respects sound toggles.
   */
  playPreview(type: NotificationSoundType, volume: number): void {
    if (!this.initialized) this.init();

    const state = useNotificationStore.getState();
    if (!state.enabled || !this.isTypeEnabled(type, state)) return;

    const audio = this.sounds.get(type);
    if (!audio) return;

    audio.volume = clampVolume(volume);
    audio.currentTime = 0;
    Promise.resolve(audio.play()).catch(noop);
  }

  /**
   * Start a looping sound (e.g., ringtone, ringback).
   * Only works for sound types in LOOPING_SOUNDS.
   * Call stopLoop() to stop. Starting a loop that's already playing is a no-op.
   */
  playLoop(type: NotificationSoundType): void {
    if (!LOOPING_SOUNDS.has(type)) return;
    if (this.activeLoops.has(type)) return;
    if (!this.initialized) this.init();

    const state = useNotificationStore.getState();
    if (!state.enabled || !state.voiceEventSounds) return;

    const audio = this.sounds.get(type);
    if (!audio) return;

    audio.loop = true;
    audio.volume = this.effectiveVolume(type, state);
    audio.currentTime = 0;
    Promise.resolve(audio.play()).catch(noop);
    this.activeLoops.add(type);
  }

  /**
   * Stop a looping sound. No-op if the sound isn't currently looping.
   */
  stopLoop(type: NotificationSoundType): void {
    const audio = this.sounds.get(type);
    if (!audio) return;

    audio.loop = false;
    audio.pause();
    audio.currentTime = 0;
    this.activeLoops.delete(type);
  }

  /** Stop all currently looping sounds */
  stopAllLoops(): void {
    for (const type of this.activeLoops) {
      this.stopLoop(type);
    }
  }

  /** Check if a specific sound is currently looping */
  isLooping(type: NotificationSoundType): boolean {
    return this.activeLoops.has(type);
  }
}

export const notificationSoundService = new NotificationSoundService();
