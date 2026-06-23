import { vi, describe, it, expect, beforeEach } from 'vitest';
import { useTTSSettingsStore } from '@/renderer/stores/ttsSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Mock SpeechSynthesis API
const mockSpeak = vi.fn();
const mockCancel = vi.fn();
const mockGetVoices = vi.fn().mockReturnValue([]);

// Mock both SpeechSynthesis and SpeechSynthesisUtterance (jsdom doesn't have them)
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

// Must import AFTER mocking
import {
  speak,
  stop,
  getVoices,
  isSpeaking,
  preview,
  TTS_PREVIEW_TEXT,
} from '@/renderer/services/ttsService';

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
});

describe('ttsService', () => {
  describe('speak', () => {
    it('creates utterance with correct text', () => {
      speak('Hello world');
      expect(mockSpeak).toHaveBeenCalledTimes(1);
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.text).toBe('Hello world');
    });

    it('formats text with sender name', () => {
      speak('Hello', 'Alice');
      expect(mockSpeak).toHaveBeenCalledTimes(1);
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.text).toContain('Alice says: Hello');
    });

    it('does nothing when TTS is disabled', () => {
      useTTSSettingsStore.getState().setTtsEnabled(false);
      speak('Hello');
      expect(mockSpeak).not.toHaveBeenCalled();
    });

    it('uses rate and volume from settings', () => {
      useTTSSettingsStore.setState({ ttsRate: 1.5, ttsVolume: 0.5, ttsEnabled: true });
      speak('Test');
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.rate).toBe(1.5);
      expect(utterance.volume).toBe(0.5);
    });

    it('truncates long messages at 200 chars', () => {
      const longText = 'a'.repeat(300);
      speak(longText);
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.text.length).toBeLessThanOrEqual(200);
      expect(utterance.text).toContain('...');
    });
  });

  describe('preview', () => {
    it('speaks the preview phrase even when TTS playback is disabled', () => {
      const mockVoice = { voiceURI: 'preview-voice', name: 'Preview Voice', lang: 'en-US' };
      mockGetVoices.mockReturnValue([mockVoice]);
      useTTSSettingsStore.setState({
        ttsEnabled: false,
        ttsVoice: 'preview-voice',
        ttsRate: 1.4,
        ttsVolume: 0.35,
      });

      expect(preview()).toBe(true);

      expect(mockCancel).toHaveBeenCalledTimes(1);
      expect(mockSpeak).toHaveBeenCalledTimes(1);
      const utterance = mockSpeak.mock.calls[0][0];
      expect(utterance.text).toBe(TTS_PREVIEW_TEXT);
      expect(utterance.rate).toBe(1.4);
      expect(utterance.volume).toBe(0.35);
      expect(utterance.voice).toBe(mockVoice);
    });

    it('returns false without speaking when no voices are available', () => {
      mockGetVoices.mockReturnValue([]);

      expect(preview()).toBe(false);

      expect(mockCancel).not.toHaveBeenCalled();
      expect(mockSpeak).not.toHaveBeenCalled();
    });
  });

  describe('stop', () => {
    it('cancels all speech', () => {
      stop();
      expect(mockCancel).toHaveBeenCalledTimes(1);
    });
  });

  describe('getVoices', () => {
    it('returns available voices', () => {
      const result = getVoices();
      expect(mockGetVoices).toHaveBeenCalled();
      expect(result).toEqual([]);
    });
  });

  describe('isSpeaking', () => {
    it('returns false when not speaking', () => {
      expect(isSpeaking()).toBe(false);
    });
  });
});
