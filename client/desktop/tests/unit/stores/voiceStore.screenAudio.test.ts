/**
 * The DURABLE screen-audio indicator (#3195, ADR-0043, design section 6c).
 *
 * The property under test is a NEGATIVE one, and it is the whole reason this state
 * exists separately from `videoSlotError`: nothing auto-dismisses it.
 *
 * `VoiceControls.tsx` clears `videoSlotError` from a `setTimeout(..., 5000)`, which is
 * the right lifetime for "that click did nothing" and the wrong lifetime for "this
 * share has been video-only since you started it". A user who looks away for a minute
 * sees a healthy-looking toolbar over a silent share. So the tests below do not just
 * assert that the state was stored -- they let real timer time pass and assert it is
 * still there afterwards, and they assert that storing it armed NO timer at all, which
 * is the mechanism rather than the symptom.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { resetAllStores } from '../../helpers/store-helpers';

/** The `VoiceControls.tsx` auto-dismiss window this state must outlive. */
const VIDEO_SLOT_ERROR_AUTO_CLEAR_MS = 5000;

describe('voiceStore.screenAudio', () => {
  beforeEach(() => {
    resetAllStores();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('starts off, with no reason and no overrun', () => {
    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });

  it('keeps a degraded verdict past the 5000 ms videoSlotError auto-clear window', async () => {
    const store = useVoiceStore.getState();

    store.setScreenAudioState({ mode: 'degraded', reason: 'child-crash', overrun: 0 });

    // The mechanism: a durable indicator schedules nothing. Asserted BEFORE advancing,
    // because after the advance "still degraded" is equally consistent with "a timer
    // ran and happened to be a no-op". The control for this assertion is the line
    // below it -- an armed `setTimeout` does move the count -- so a `getTimerCount`
    // that is structurally stuck at zero cannot pass both.
    expect(vi.getTimerCount()).toBe(0);
    const control = setTimeout(() => {}, VIDEO_SLOT_ERROR_AUTO_CLEAR_MS);
    expect(vi.getTimerCount()).toBe(1);
    clearTimeout(control);

    await vi.advanceTimersByTimeAsync(VIDEO_SLOT_ERROR_AUTO_CLEAR_MS + 1000);

    expect(useVoiceStore.getState().screenAudio).toEqual({
      mode: 'degraded',
      reason: 'child-crash',
      overrun: 0,
    });
  });

  // The contrast that motivates the field. Same store, same elapsed time, opposite
  // outcome -- except the toast's timer lives in the component, so this test drives the
  // component's own 5000 ms clear rather than pretending the store owns it.
  it('outlives the transient error it sits beside', async () => {
    const store = useVoiceStore.getState();
    store.setVideoSlotError('Screen audio could not be captured.');
    store.setScreenAudioState({ mode: 'degraded', reason: 'handshake-timeout', overrun: 0 });

    // What VoiceControls.tsx does on a `videoSlotError` change.
    setTimeout(() => useVoiceStore.getState().setVideoSlotError(null), 5000);
    await vi.advanceTimersByTimeAsync(6000);

    expect(useVoiceStore.getState().videoSlotError).toBeNull();
    expect(useVoiceStore.getState().screenAudio.mode).toBe('degraded');
  });

  // A REPLACE, not a merge. A merge would let the mechanism string from a previous
  // degrade ride along into the state that succeeded it, so a recovered share would
  // still be able to explain a fault it no longer has.
  it('drops the degrade reason when the share recovers', () => {
    const store = useVoiceStore.getState();
    store.setScreenAudioState({ mode: 'degraded', reason: 'load-fault', overrun: 4 });

    store.setScreenAudioState({ mode: 'system', overrun: 0 });

    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'system', overrun: 0 });
    expect(useVoiceStore.getState().screenAudio.reason).toBeUndefined();
  });

  it('carries the overrun count as a plain scalar, undimensioned by cause', () => {
    const store = useVoiceStore.getState();

    store.setScreenAudioState({ mode: 'per-process', overrun: 17 });

    expect(useVoiceStore.getState().screenAudio.overrun).toBe(17);
  });

  it('returns to off when the voice session is reset', () => {
    useVoiceStore
      .getState()
      .setScreenAudioState({ mode: 'degraded', reason: 'no-backend', overrun: 2 });

    useVoiceStore.getState().reset();

    expect(useVoiceStore.getState().screenAudio).toEqual({ mode: 'off', overrun: 0 });
  });
});
