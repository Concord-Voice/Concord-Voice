// client/desktop/tests/unit/stores/voiceStore.mediaPolicy.test.ts
import { describe, it, expect, beforeEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';

describe('voiceStore media-policy slice (#2153)', () => {
  beforeEach(() => resetAllStores());

  it('records a latch per source and clears only the matching producer', () => {
    const s = useVoiceStore.getState();
    s.setMediaPolicyPaused('mic', 'mic-1');
    s.setMediaPolicyPaused('camera', 'cam-1');
    useVoiceStore.getState().clearMediaPolicyPausedByProducer('cam-1');
    expect(useVoiceStore.getState().mediaPolicyPaused).toEqual({ mic: 'mic-1' });
  });

  it('leaves state untouched when the producer holds no latch', () => {
    useVoiceStore.getState().setMediaPolicyPaused('mic', 'mic-1');
    const before = useVoiceStore.getState().mediaPolicyPaused;
    useVoiceStore.getState().clearMediaPolicyPausedByProducer('other');
    expect(useVoiceStore.getState().mediaPolicyPaused).toBe(before);
  });

  it('reset() wipes the call-scoped latch but PRESERVES the interrupt (handoff §1c)', () => {
    const s = useVoiceStore.getState();
    s.setMediaPolicyPaused('mic', 'mic-1');
    s.setMediaPolicyInterrupt({ reason: 'evicted', rejoinAt: 1_900_000 });
    useVoiceStore.getState().reset();
    const after = useVoiceStore.getState();
    expect(after.mediaPolicyPaused).toEqual({});
    expect(after.mediaPolicyInterrupt).toEqual({ reason: 'evicted', rejoinAt: 1_900_000 });
  });

  it('clearMediaPolicyInterrupt drops the interrupt (F13: was two identically-bodied actions)', () => {
    const s = useVoiceStore.getState();
    s.setMediaPolicyInterrupt({ reason: 'cooldown', rejoinAt: null });
    useVoiceStore.getState().clearMediaPolicyInterrupt();
    expect(useVoiceStore.getState().mediaPolicyInterrupt).toBeNull();
  });
});
