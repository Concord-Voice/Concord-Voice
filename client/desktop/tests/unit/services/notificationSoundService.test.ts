import { describe, it, expect, beforeEach, vi } from 'vitest';
import { useNotificationStore } from '../../../src/renderer/stores/notificationStore';

// Must mock Audio before importing the service
const mockPlay = vi.fn().mockResolvedValue(undefined);
const mockPause = vi.fn();

class MockAudio {
  preload = '';
  volume = 1;
  currentTime = 0;
  loop = false;
  play = mockPlay;
  pause = mockPause;
  constructor(_src?: string) {
    // Track creation
  }
}

vi.stubGlobal('Audio', MockAudio);

// Import after mocking
const { notificationSoundService } =
  await import('../../../src/renderer/services/notificationSoundService');

describe('NotificationSoundService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockPlay.mockResolvedValue(undefined);

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
    });

    // Reset service internal state
    (notificationSoundService as Record<string, unknown>)['initialized'] = false;
    (notificationSoundService as Record<string, unknown>)['lastChatSoundTime'] = 0;
    (notificationSoundService as Record<string, unknown>)['sounds'] = new Map();
    ((notificationSoundService as Record<string, unknown>)['activeLoops'] as Set<string>).clear();
  });

  it('initializes and preloads all sounds', () => {
    notificationSoundService.init();
    const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
      string,
      unknown
    >;
    expect(sounds.size).toBe(23);
  });

  it('auto-initializes on first play', () => {
    notificationSoundService.play('message');
    const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
      string,
      unknown
    >;
    expect(sounds.size).toBe(23);
  });

  it('does not play when master toggle is off', () => {
    useNotificationStore.getState().setEnabled(false);
    notificationSoundService.play('message');
    expect(mockPlay).not.toHaveBeenCalled();
  });

  it('does not play when message toggle is off', () => {
    useNotificationStore.getState().setMessageSound(false);
    notificationSoundService.play('message');
    expect(mockPlay).not.toHaveBeenCalled();
  });

  it('does not play when mention toggle is off', () => {
    useNotificationStore.getState().setMentionSound(false);
    notificationSoundService.play('mention');
    expect(mockPlay).not.toHaveBeenCalled();
  });

  it('does not play when dm toggle is off', () => {
    useNotificationStore.getState().setDmSound(false);
    notificationSoundService.play('dm');
    expect(mockPlay).not.toHaveBeenCalled();
  });

  it('does not play when friend-request toggle is off', () => {
    useNotificationStore.getState().setFriendRequestSound(false);
    notificationSoundService.play('friend-request');
    expect(mockPlay).not.toHaveBeenCalled();
  });

  it('plays sound when enabled', () => {
    notificationSoundService.play('message');
    expect(mockPlay).toHaveBeenCalledTimes(1);
  });

  it('rate-limits sounds to 2 second intervals', () => {
    notificationSoundService.play('message');
    expect(mockPlay).toHaveBeenCalledTimes(1);

    // Second play within 2s should be suppressed
    notificationSoundService.play('mention');
    expect(mockPlay).toHaveBeenCalledTimes(1);
  });

  it('allows sound after rate limit expires', () => {
    notificationSoundService.play('message');
    expect(mockPlay).toHaveBeenCalledTimes(1);

    // Simulate time passing beyond debounce
    (notificationSoundService as Record<string, unknown>)['lastChatSoundTime'] = Date.now() - 3000;

    notificationSoundService.play('dm');
    expect(mockPlay).toHaveBeenCalledTimes(2);
  });

  it('sets volume from store', () => {
    useNotificationStore.getState().setVolume(50);
    notificationSoundService.play('message');
    const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
      string,
      MockAudio
    >;
    const audio = sounds.get('message');
    expect(audio?.volume).toBe(0.5);
  });

  describe('effective volume (master × category)', () => {
    const getAudio = (type: string): MockAudio | undefined => {
      const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
        string,
        MockAudio
      >;
      return sounds.get(type);
    };

    it('applies messageVolume on top of master volume', () => {
      useNotificationStore.getState().setVolume(100);
      useNotificationStore.getState().setMessageVolume(50);
      notificationSoundService.play('message');
      expect(getAudio('message')?.volume).toBeCloseTo(0.5, 5);
    });

    it('applies mentionVolume on top of master volume', () => {
      useNotificationStore.getState().setVolume(80);
      useNotificationStore.getState().setMentionVolume(50);
      notificationSoundService.play('mention');
      expect(getAudio('mention')?.volume).toBeCloseTo(0.4, 5);
    });

    it('applies dmVolume on top of master volume', () => {
      useNotificationStore.getState().setVolume(60);
      useNotificationStore.getState().setDmVolume(25);
      notificationSoundService.play('dm');
      expect(getAudio('dm')?.volume).toBeCloseTo(0.15, 5);
    });

    it('applies friendRequestVolume on top of master volume', () => {
      useNotificationStore.getState().setVolume(50);
      useNotificationStore.getState().setFriendRequestVolume(50);
      notificationSoundService.play('friend-request');
      expect(getAudio('friend-request')?.volume).toBeCloseTo(0.25, 5);
    });

    it('applies voiceEventVolume to voice sounds', () => {
      useNotificationStore.getState().setVolume(100);
      useNotificationStore.getState().setVoiceEventVolume(50);
      notificationSoundService.play('voice-join');
      expect(getAudio('voice-join')?.volume).toBeCloseTo(0.5, 5);
    });

    it('applies voiceEventVolume to call sounds', () => {
      useNotificationStore.getState().setVolume(100);
      useNotificationStore.getState().setVoiceEventVolume(40);
      notificationSoundService.play('call-connected');
      expect(getAudio('call-connected')?.volume).toBeCloseTo(0.4, 5);
    });

    it('applies voiceEventVolume to looping sounds', () => {
      useNotificationStore.getState().setVolume(80);
      useNotificationStore.getState().setVoiceEventVolume(50);
      notificationSoundService.playLoop('call-ringing');
      expect(getAudio('call-ringing')?.volume).toBeCloseTo(0.4, 5);
    });

    it('zero category volume silences only that category', () => {
      useNotificationStore.getState().setVolume(100);
      useNotificationStore.getState().setMessageVolume(0);
      useNotificationStore.getState().setMentionVolume(100);
      notificationSoundService.play('message');
      expect(getAudio('message')?.volume).toBe(0);

      // Reset debounce so the mention plays
      (notificationSoundService as Record<string, unknown>)['lastChatSoundTime'] = 0;
      notificationSoundService.play('mention');
      expect(getAudio('mention')?.volume).toBe(1);
    });

    it('per-category volume is independent of chat sounds vs voice sounds', () => {
      useNotificationStore.getState().setVolume(100);
      useNotificationStore.getState().setMessageVolume(100);
      useNotificationStore.getState().setVoiceEventVolume(20);
      notificationSoundService.play('message');
      expect(getAudio('message')?.volume).toBe(1);

      notificationSoundService.play('mute');
      expect(getAudio('mute')?.volume).toBeCloseTo(0.2, 5);
    });
  });

  it('handles play() returning undefined', () => {
    mockPlay.mockReturnValue(undefined);
    expect(() => notificationSoundService.play('message')).not.toThrow();
  });

  it('suppresses focused sound when suppressWhenFocused is on and window is focused', () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    useNotificationStore.getState().setSuppressWhenFocused(true);
    notificationSoundService.play('message', { focused: true });
    expect(mockPlay).not.toHaveBeenCalled();
  });

  it('plays focused sound when suppressWhenFocused is off', () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    useNotificationStore.getState().setSuppressWhenFocused(false);
    notificationSoundService.play('message', { focused: true });
    expect(mockPlay).toHaveBeenCalledTimes(1);
  });

  it('plays focused sound when window is not focused', () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(false);
    useNotificationStore.getState().setSuppressWhenFocused(true);
    notificationSoundService.play('message', { focused: true });
    expect(mockPlay).toHaveBeenCalledTimes(1);
  });

  // Voice event sound tests
  it('plays voice sounds when voiceEventSounds is enabled', () => {
    notificationSoundService.play('mute');
    expect(mockPlay).toHaveBeenCalledTimes(1);
  });

  it('does not play voice sounds when voiceEventSounds is off', () => {
    useNotificationStore.getState().setVoiceEventSounds(false);
    notificationSoundService.play('mute');
    expect(mockPlay).not.toHaveBeenCalled();
  });

  it('voice sounds bypass chat debounce', () => {
    notificationSoundService.play('message');
    expect(mockPlay).toHaveBeenCalledTimes(1);

    // Chat sound would be debounced, but voice sound should still play
    notificationSoundService.play('mute');
    expect(mockPlay).toHaveBeenCalledTimes(2);
  });

  it('voice sounds ignore focused option', () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(true);
    useNotificationStore.getState().setSuppressWhenFocused(true);
    notificationSoundService.play('voice-join', { focused: true });
    expect(mockPlay).toHaveBeenCalledTimes(1);
  });

  it('voice sounds do not affect chat debounce timer', () => {
    // Play a voice sound first
    notificationSoundService.play('mute');
    expect(mockPlay).toHaveBeenCalledTimes(1);

    // Chat sound should still play (voice didn't start the debounce)
    notificationSoundService.play('message');
    expect(mockPlay).toHaveBeenCalledTimes(2);
  });

  it('double init is a no-op', () => {
    notificationSoundService.init();
    const sounds1 = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
      string,
      unknown
    >;
    const size1 = sounds1.size;

    notificationSoundService.init();
    const sounds2 = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
      string,
      unknown
    >;
    expect(sounds2.size).toBe(size1);
  });

  it('all voice sound types play correctly', () => {
    const voiceTypes = [
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
    ] as const;

    for (const type of voiceTypes) {
      mockPlay.mockClear();
      notificationSoundService.play(type);
      expect(mockPlay).toHaveBeenCalledTimes(1);
    }
  });

  it('all chat sound types play correctly', () => {
    const chatTypes = ['message', 'mention', 'dm', 'friend-request'] as const;

    for (const type of chatTypes) {
      mockPlay.mockClear();
      // Reset debounce timer between each
      (notificationSoundService as Record<string, unknown>)['lastChatSoundTime'] = 0;
      notificationSoundService.play(type);
      expect(mockPlay).toHaveBeenCalledTimes(1);
    }
  });

  it('handles play rejection gracefully', () => {
    mockPlay.mockRejectedValueOnce(new DOMException('Autoplay blocked'));
    expect(() => notificationSoundService.play('message')).not.toThrow();
  });

  it('resets currentTime before playing', () => {
    notificationSoundService.play('message');
    const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
      string,
      MockAudio
    >;
    const audio = sounds.get('message');
    expect(audio?.currentTime).toBe(0);
  });

  // Call sound tests
  it('all call sound types play correctly', () => {
    const callTypes = ['call-connected', 'call-ended', 'call-declined', 'call-busy'] as const;

    for (const type of callTypes) {
      mockPlay.mockClear();
      notificationSoundService.play(type);
      expect(mockPlay).toHaveBeenCalledTimes(1);
    }
  });

  it('does not play call sounds when voiceEventSounds is off', () => {
    useNotificationStore.getState().setVoiceEventSounds(false);
    notificationSoundService.play('call-connected');
    expect(mockPlay).not.toHaveBeenCalled();
  });

  // Looping sound tests
  describe('looping sounds', () => {
    beforeEach(() => {
      mockPlay.mockClear();
      mockPause.mockClear();
    });

    it('playLoop starts a looping sound', () => {
      notificationSoundService.playLoop('call-ringing');
      expect(mockPlay).toHaveBeenCalledTimes(1);
      expect(notificationSoundService.isLooping('call-ringing')).toBe(true);

      const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
        string,
        MockAudio
      >;
      const audio = sounds.get('call-ringing');
      expect(audio?.loop).toBe(true);
    });

    it('playLoop is a no-op for non-looping sound types', () => {
      notificationSoundService.playLoop('message');
      expect(mockPlay).not.toHaveBeenCalled();
      expect(notificationSoundService.isLooping('message')).toBe(false);
    });

    it('playLoop is a no-op if already looping', () => {
      notificationSoundService.playLoop('call-ringing');
      expect(mockPlay).toHaveBeenCalledTimes(1);

      notificationSoundService.playLoop('call-ringing');
      expect(mockPlay).toHaveBeenCalledTimes(1); // Still 1, not 2
    });

    it('stopLoop stops a looping sound', () => {
      notificationSoundService.playLoop('call-ringing');
      expect(notificationSoundService.isLooping('call-ringing')).toBe(true);

      notificationSoundService.stopLoop('call-ringing');
      expect(mockPause).toHaveBeenCalledTimes(1);
      expect(notificationSoundService.isLooping('call-ringing')).toBe(false);

      const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
        string,
        MockAudio
      >;
      const audio = sounds.get('call-ringing');
      expect(audio?.loop).toBe(false);
      expect(audio?.currentTime).toBe(0);
    });

    it('stopLoop is a no-op for non-looping sound', () => {
      notificationSoundService.stopLoop('call-ringing');
      expect(mockPause).not.toHaveBeenCalled();
    });

    it('stopAllLoops stops all active loops', () => {
      notificationSoundService.playLoop('call-ringing');
      notificationSoundService.playLoop('call-outgoing');
      expect(notificationSoundService.isLooping('call-ringing')).toBe(true);
      expect(notificationSoundService.isLooping('call-outgoing')).toBe(true);

      notificationSoundService.stopAllLoops();
      expect(notificationSoundService.isLooping('call-ringing')).toBe(false);
      expect(notificationSoundService.isLooping('call-outgoing')).toBe(false);
      expect(mockPause).toHaveBeenCalledTimes(2);
    });

    it('playLoop respects master toggle', () => {
      useNotificationStore.getState().setEnabled(false);
      notificationSoundService.playLoop('call-ringing');
      expect(mockPlay).not.toHaveBeenCalled();
    });

    it('playLoop respects voiceEventSounds toggle', () => {
      useNotificationStore.getState().setVoiceEventSounds(false);
      notificationSoundService.playLoop('call-ringing');
      expect(mockPlay).not.toHaveBeenCalled();
    });

    it('playLoop sets volume from store', () => {
      useNotificationStore.getState().setVolume(40);
      notificationSoundService.playLoop('call-outgoing');
      const sounds = (notificationSoundService as Record<string, unknown>)['sounds'] as Map<
        string,
        MockAudio
      >;
      const audio = sounds.get('call-outgoing');
      expect(audio?.volume).toBe(0.4);
    });

    it('isLooping returns false for non-active loop', () => {
      expect(notificationSoundService.isLooping('call-ringing')).toBe(false);
    });
  });
});
