import type { ScreenAudioInterruptReason } from '@/shared/audiocapProtocol';

// ONE SENTENCE FOR EVERY REASON. The user's remedy is the same, and the reason
// is a mechanism label, never a message (#3394 PR 2 §4.6). No REMEDY suffix:
// "share a whole screen" would widen the capture (#2161).
const INTERRUPTED =
  'App sound stopped. Your screen is still being shared. Turn on Share sound to try again.';

const SCREEN_AUDIO_INTERRUPT_COPY: Readonly<Record<ScreenAudioInterruptReason, string>> = {
  'capture-interrupted': INTERRUPTED,
  'child-crash': INTERRUPTED,
  'protocol-fault': INTERRUPTED,
};

/**
 * Guarded like its sibling `screenAudioDegradeMessage`: `voiceStore.screenAudio.reason`
 * is readable by any caller, and a bare index would hand back an inherited member
 * (`'constructor'` returns a function). Two closed-set lookups over the same store
 * field should not disagree about whether their input is trusted.
 */
export function screenAudioInterruptMessage(reason: ScreenAudioInterruptReason): string {
  if (typeof reason === 'string' && Object.hasOwn(SCREEN_AUDIO_INTERRUPT_COPY, reason)) {
    return SCREEN_AUDIO_INTERRUPT_COPY[reason];
  }
  return INTERRUPTED;
}
