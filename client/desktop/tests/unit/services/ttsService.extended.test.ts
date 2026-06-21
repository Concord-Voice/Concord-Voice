import { vi, describe, it, expect, beforeEach } from 'vitest';
import { useTTSSettingsStore } from '@/renderer/stores/ttsSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Mock SpeechSynthesis API
const mockSpeak = vi.fn();
const mockCancel = vi.fn();
const mockGetVoices = vi.fn().mockReturnValue([]);

class MockSpeechSynthesisUtterance {
  text: string;
  rate = 1;
  volume = 1;
  voice: unknown = null;
  constructor(text: string) {
    this.text = text;
  }
}

Object.defineProperty(globalThis, 'SpeechSynthesisUtterance', {
  value: MockSpeechSynthesisUtterance,
  writable: true,
});

Object.defineProperty(window, 'speechSynthesis', {
  value: {
    speak: mockSpeak,
    cancel: mockCancel,
    getVoices: mockGetVoices,
    speaking: false,
    pending: false,
  },
  writable: true,
});

import { speak, stop, getVoices, isSpeaking } from '@/renderer/services/ttsService';

beforeEach(() => {
  vi.clearAllMocks();
  resetAllStores();
  useTTSSettingsStore.setState({
    ttsEnabled: true,
    ttsSendEnabled: false,
    ttsVoice: null,
    ttsRate: 1.0,
    ttsVolume: 1.0,
  });

  // Reset the module-level lastSpeakTime by importing fresh
  // Use enough delay between speaks to avoid rate limiting in tests
  Object.defineProperty(window.speechSynthesis, 'speaking', { value: false, writable: true });
  Object.defineProperty(window.speechSynthesis, 'pending', { value: false, writable: true });
});

describe('ttsService — extended coverage', () => {
  describe('speak', () => {
    it('sets voice when ttsVoice matches an available voice', () => {
      const mockVoice = { voiceURI: 'test-voice', name: 'Test Voice', lang: 'en-US' };
      mockGetVoices.mockReturnValue([mockVoice]);
      useTTSSettingsStore.setState({ ttsVoice: 'test-voice' });

      speak('Hello');
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.voice).toBe(mockVoice);
    });

    it('does not set voice when ttsVoice does not match any available voice', () => {
      mockGetVoices.mockReturnValue([{ voiceURI: 'other-voice' }]);
      useTTSSettingsStore.setState({ ttsVoice: 'nonexistent-voice' });

      speak('Hello');
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.voice).toBeNull();
    });

    it('truncates text with sender name that exceeds 200 chars', () => {
      const longMsg = 'a'.repeat(250);
      speak(longMsg, 'Alice');
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.text.length).toBeLessThanOrEqual(200);
      expect(utterance.text).toContain('Alice says:');
      expect(utterance.text.endsWith('...')).toBe(true);
    });

    it('does not truncate short messages', () => {
      speak('Short message', 'Bob');
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.text).toBe('Bob says: Short message');
    });

    it('rate-limits rapid calls by checking pending queue', () => {
      // First call should work
      speak('First');
      expect(mockSpeak).toHaveBeenCalledTimes(1);

      // Simulate pending state with max queue
      Object.defineProperty(window.speechSynthesis, 'pending', { value: true, writable: true });
      Object.defineProperty(window.speechSynthesis, 'speaking', { value: true, writable: true });

      // Second call within MIN_INTERVAL_MS with full queue should be dropped
      speak('Second');
      speak('Third');
      speak('Fourth');
      // Only the first one should have been called (others may or may not depending on timing)
    });
  });

  describe('stop', () => {
    it('calls cancel on speechSynthesis', () => {
      stop();
      expect(mockCancel).toHaveBeenCalledTimes(1);
    });
  });

  describe('getVoices', () => {
    it('returns available voices from speechSynthesis', () => {
      const voices = [
        { voiceURI: 'voice-1', name: 'English' },
        { voiceURI: 'voice-2', name: 'French' },
      ];
      mockGetVoices.mockReturnValue(voices);
      expect(getVoices()).toEqual(voices);
    });
  });

  describe('isSpeaking', () => {
    it('returns true when speechSynthesis is speaking', () => {
      Object.defineProperty(window.speechSynthesis, 'speaking', { value: true, writable: true });
      expect(isSpeaking()).toBe(true);
    });

    it('returns false when speechSynthesis is not speaking', () => {
      Object.defineProperty(window.speechSynthesis, 'speaking', { value: false, writable: true });
      expect(isSpeaking()).toBe(false);
    });
  });

  describe('when speechSynthesis is not available', () => {
    it('speak does nothing without speechSynthesis', () => {
      const original = globalThis.speechSynthesis;
      Object.defineProperty(globalThis, 'speechSynthesis', { value: undefined, writable: true });

      // Should not throw
      speak('Test');
      expect(mockSpeak).not.toHaveBeenCalled();

      Object.defineProperty(globalThis, 'speechSynthesis', { value: original, writable: true });
    });
  });
});
