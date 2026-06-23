import { useTTSSettingsStore } from '../stores/ttsSettingsStore';

const MAX_UTTERANCE_LENGTH = 200;
const MAX_QUEUE_SIZE = 3;
const MIN_INTERVAL_MS = 2000;
export const TTS_PREVIEW_TEXT = 'This is a preview of text-to-speech in Concord Voice.';

let lastSpeakTime = 0;

interface UtteranceSettings {
  voiceURI: string | null;
  rate: number;
  volume: number;
}

export interface TTSPreviewOptions {
  voiceURI?: string | null;
  rate?: number;
  volume?: number;
  onEnd?: () => void;
  onError?: () => void;
}

function createConfiguredUtterance(
  text: string,
  settings: UtteranceSettings,
  voices = globalThis.speechSynthesis?.getVoices() ?? []
): SpeechSynthesisUtterance {
  const utterance = new SpeechSynthesisUtterance(text);
  utterance.rate = settings.rate;
  utterance.volume = settings.volume;

  if (settings.voiceURI) {
    const match = voices.find((v) => v.voiceURI === settings.voiceURI);
    if (match) {
      utterance.voice = match;
    }
  }

  return utterance;
}

/**
 * Speak text aloud using the Web Speech API.
 * Respects the user's TTS settings (voice, rate, volume).
 * Rate-limited: max 1 utterance per 2 seconds, queue of 3.
 */
export function speak(text: string, senderName?: string): void {
  if (!globalThis.speechSynthesis) return;

  const { ttsEnabled, ttsVoice, ttsRate, ttsVolume } = useTTSSettingsStore.getState();
  if (!ttsEnabled) return;

  // Rate limiting
  const now = Date.now();
  if (now - lastSpeakTime < MIN_INTERVAL_MS) {
    // Check queue size — drop if too many pending
    if (globalThis.speechSynthesis.pending && getQueueSize() >= MAX_QUEUE_SIZE) {
      return;
    }
  }
  lastSpeakTime = now;

  // Truncate long messages
  let utteranceText = text;
  if (senderName) {
    utteranceText = `${senderName} says: ${text}`;
  }
  if (utteranceText.length > MAX_UTTERANCE_LENGTH) {
    utteranceText = utteranceText.slice(0, MAX_UTTERANCE_LENGTH - 3) + '...';
  }

  const utterance = createConfiguredUtterance(utteranceText, {
    voiceURI: ttsVoice,
    rate: ttsRate,
    volume: ttsVolume,
  });

  globalThis.speechSynthesis.speak(utterance);
}

/**
 * Speak a fixed preview phrase from Settings.
 * Bypasses the TTS enabled gate and message rate limit because this is user-initiated.
 */
export function preview(options: TTSPreviewOptions = {}): boolean {
  const synthesis = globalThis.speechSynthesis;
  if (!synthesis) return false;

  const voices = synthesis.getVoices();
  if (voices.length === 0) return false;

  const { ttsVoice, ttsRate, ttsVolume } = useTTSSettingsStore.getState();
  const voiceURI = options.voiceURI === undefined ? ttsVoice : options.voiceURI;
  const rate = options.rate ?? ttsRate;
  const volume = options.volume ?? ttsVolume;

  const utterance = createConfiguredUtterance(
    TTS_PREVIEW_TEXT,
    {
      voiceURI: voiceURI ?? null,
      rate,
      volume,
    },
    voices
  );
  utterance.onend = () => options.onEnd?.();
  utterance.onerror = () => options.onError?.();

  synthesis.cancel();
  synthesis.speak(utterance);
  return true;
}

/**
 * Stop all current and queued TTS speech.
 */
export function stop(): void {
  if (!globalThis.speechSynthesis) return;
  globalThis.speechSynthesis.cancel();
}

/**
 * Get available TTS voices.
 */
export function getVoices(): SpeechSynthesisVoice[] {
  if (!globalThis.speechSynthesis) return [];
  return globalThis.speechSynthesis.getVoices();
}

/**
 * Check if TTS is currently speaking.
 */
export function isSpeaking(): boolean {
  if (!globalThis.speechSynthesis) return false;
  return globalThis.speechSynthesis.speaking;
}

// Approximate queue size (pending + speaking)
function getQueueSize(): number {
  // SpeechSynthesis doesn't expose exact queue size;
  // we use the pending flag as a proxy
  let count = 0;
  if (globalThis.speechSynthesis.speaking) count++;
  if (globalThis.speechSynthesis.pending) count++;
  return count;
}
