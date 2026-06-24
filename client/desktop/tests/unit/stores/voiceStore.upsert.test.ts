import { describe, it, expect, beforeEach } from 'vitest';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voiceStore';
import { resetAllStores } from '../../helpers/store-helpers';

// jsdom has no MediaStream constructor; a typed sentinel is enough — the store
// only stores the reference.
const stream = (): MediaStream => ({}) as MediaStream;

const base: VoiceParticipant = {
  userId: 'u1',
  username: 'alice',
  isMuted: false,
  isDeafened: false,
  serverMuted: false,
  serverDeafened: false,
  isVideoOn: false,
  isScreenSharing: false,
  isSpeaking: false,
};

describe('voiceStore.upsertParticipant (#1873)', () => {
  beforeEach(() => {
    resetAllStores();
    useVoiceStore.getState().reset();
    useVoiceStore.getState().clearParticipants();
  });

  it('creates a participant when absent, with the stream attached', () => {
    const audio = stream();
    useVoiceStore.getState().upsertParticipant('u1', { audioStream: audio });
    const p = useVoiceStore.getState().participants['u1'];
    expect(p).toBeDefined();
    expect(p.userId).toBe('u1');
    expect(p.audioStream).toBe(audio);
    expect(p.username).toBe(''); // backfilled later by the user-joined roster entry
  });

  it('merges when present, preserving existing media streams across a later roster backfill', () => {
    const audio = stream();
    useVoiceStore.getState().upsertParticipant('u1', { audioStream: audio });
    // user-joined arrives after the consume → must backfill name, not clobber the stream
    useVoiceStore.getState().upsertParticipant('u1', { username: 'alice', avatarUrl: '/a.png' });
    const p = useVoiceStore.getState().participants['u1'];
    expect(p.audioStream).toBe(audio);
    expect(p.username).toBe('alice');
    expect(p.avatarUrl).toBe('/a.png');
  });

  it('does not disturb an unrelated existing participant', () => {
    useVoiceStore.getState().addParticipant(base);
    useVoiceStore.getState().upsertParticipant('u2', { audioStream: stream() });
    expect(useVoiceStore.getState().participants['u1'].username).toBe('alice');
    expect(useVoiceStore.getState().participants['u2'].audioStream).toBeDefined();
  });
});
