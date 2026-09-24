import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voice/voiceStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { vi } from 'vitest';

// ── Service mock ─────────────────────────────────────────────────────────────
const mockTuneOut = vi.fn();
vi.mock('@/renderer/services/voice/voiceService', () => ({
  voiceService: {
    tuneOutOfScreenShare: mockTuneOut,
  },
}));

// ── CSS mock ─────────────────────────────────────────────────────────────────
vi.mock('@/renderer/components/Voice/StreamBar.css', () => ({}));

import StreamBar from '@/renderer/components/Voice/StreamBar';

// ── Helpers ──────────────────────────────────────────────────────────────────
// Mock MediaStream — jsdom does not provide it
class MockMediaStream {
  id = 'mock-stream';
  active = true;
  getTracks() {
    return [];
  }
  getAudioTracks() {
    return [];
  }
  getVideoTracks() {
    return [];
  }
  addTrack() {}
  removeTrack() {}
  clone() {
    return new MockMediaStream();
  }
  addEventListener() {}
  removeEventListener() {}
  dispatchEvent() {
    return true;
  }
}
globalThis.MediaStream = MockMediaStream as unknown as typeof MediaStream;

const mockParticipant = (overrides: Partial<VoiceParticipant> = {}): VoiceParticipant => ({
  userId: 'user-1',
  username: 'alice',
  displayName: 'Alice',
  isMuted: false,
  isDeafened: false,
  isVideoOn: false,
  isScreenSharing: true,
  isSpeaking: false,
  screenStream: new MockMediaStream() as unknown as MediaStream,
  ...overrides,
});

function setStreamBarState(overrides: Record<string, unknown> = {}) {
  useVoiceStore.setState({
    tunedInScreenShares: {},
    dominantScreenShareId: null,
    participants: {},
    localStreamPaused: false,
    ...overrides,
  });
}

describe('StreamBar', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    HTMLVideoElement.prototype.play = vi.fn().mockResolvedValue(undefined);
  });

  // ── Render conditions ────────────────────────────────────────────────────

  it('returns null when no non-dominant shares exist', () => {
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1' },
      dominantScreenShareId: 'p1',
    });
    const { container } = render(<StreamBar height={120} />);
    expect(container.firstChild).toBeNull();
  });

  it('returns null when tunedInScreenShares is empty', () => {
    setStreamBarState({ tunedInScreenShares: {} });
    const { container } = render(<StreamBar height={120} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders thumbnails for non-dominant shares', () => {
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      dominantScreenShareId: 'p1',
      participants: {
        'user-1': mockParticipant(),
      },
    });
    render(<StreamBar height={120} />);
    // p2 is non-dominant, should show a thumbnail
    expect(screen.getByTitle(/View .+'s screen/)).toBeInTheDocument();
  });

  it('applies height from props', () => {
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      dominantScreenShareId: 'p1',
      participants: { 'user-1': mockParticipant() },
    });
    const { container } = render(<StreamBar height={150} />);
    const bar = container.querySelector('.stream-bar');
    expect(bar).toHaveStyle({ height: '150px' });
  });

  // ── Interaction ──────────────────────────────────────────────────────────

  it('sets dominant screen share when thumbnail is clicked', () => {
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      dominantScreenShareId: 'p1',
      participants: { 'user-1': mockParticipant() },
    });
    render(<StreamBar height={120} />);
    fireEvent.click(screen.getByTitle(/View .+'s screen/));
    expect(useVoiceStore.getState().dominantScreenShareId).toBe('p2');
  });

  it('calls tuneOut when close button is clicked', async () => {
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      dominantScreenShareId: 'p1',
      participants: { 'user-1': mockParticipant() },
    });
    render(<StreamBar height={120} />);
    await act(async () => {
      fireEvent.click(screen.getByTitle('Tune out'));
    });
    // Manual tune-out from the bar suppresses auto re-tune (#2088)
    expect(mockTuneOut).toHaveBeenCalledWith('p2', { suppressAutoTune: true });
  });

  // ── Sharer name ──────────────────────────────────────────────────────────

  it('displays sharer name on thumbnail', () => {
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      dominantScreenShareId: 'p1',
      participants: {
        'user-1': mockParticipant({ displayName: 'Alice' }),
      },
    });
    // Owner metadata always accompanies availability/tune-in in production (#2088)
    useVoiceStore.getState().registerActiveScreenShare({
      producerId: 'p2',
      userId: 'user-1',
      username: 'alice',
      displayName: 'Alice',
      isLocal: false,
    });
    render(<StreamBar height={120} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
  });

  it('shows Unknown when no matching participant found', () => {
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      dominantScreenShareId: 'p1',
      participants: {},
    });
    render(<StreamBar height={120} />);
    expect(screen.getByText('Unknown')).toBeInTheDocument();
  });

  // ── Paused state ─────────────────────────────────────────────────────────

  it('shows paused text for local user when stream is paused', () => {
    useUserStore.setState({
      user: {
        id: 'user-1',
        username: 'alice',
        display_name: 'Alice',
        email: 'alice@test.com',
        bio: null,
        avatar_url: null,
        header_image_url: null,
        links: [],
        email_verified: false,
        age_verified: true,
        created_at: '',
        updated_at: '',
      },
    });
    setStreamBarState({
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      dominantScreenShareId: 'p1',
      participants: {
        'user-1': mockParticipant({ userId: 'user-1' }),
      },
      localStreamPaused: true,
    });
    // Local-share metadata carries isLocal — the paused branch keys on it (#2088)
    useVoiceStore.getState().registerActiveScreenShare({
      producerId: 'p2',
      userId: 'user-1',
      username: 'alice',
      displayName: 'Alice',
      isLocal: true,
    });
    render(<StreamBar height={120} />);
    expect(screen.getByText('Paused')).toBeInTheDocument();
  });

  // ── Multi-sharer owner labels (#2088) ────────────────────────────────────

  it('labels each stream with its own owner when two users share (#2088)', () => {
    setStreamBarState({
      tunedInScreenShares: { pa: 'ca', pb: 'cb' },
      dominantScreenShareId: 'pa',
      participants: {
        'user-1': mockParticipant(),
        'user-2': mockParticipant({ userId: 'user-2', username: 'bob', displayName: 'Bob' }),
      },
    });
    const store = useVoiceStore.getState();
    store.registerActiveScreenShare({
      producerId: 'pa',
      userId: 'user-1',
      username: 'alice',
      displayName: 'Alice',
      isLocal: false,
    });
    store.registerActiveScreenShare({
      producerId: 'pb',
      userId: 'user-2',
      username: 'bob',
      displayName: 'Bob',
      isLocal: false,
    });
    render(<StreamBar height={120} />);
    // Only the non-dominant share (Bob's) renders in the bar — labeled with ITS
    // owner, not the first sharer found in participants.
    expect(screen.getByText('Bob')).toBeInTheDocument();
    expect(screen.queryByText('Alice')).not.toBeInTheDocument();
  });
});

describe('#2153 remote screen pause in the stream bar', () => {
  // Top-level describe: inherits no hook from describe('StreamBar').
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    HTMLVideoElement.prototype.play = vi.fn().mockResolvedValue(undefined);
  });

  // scr-2 is dominant, so only scr-1 renders as a thumbnail.
  const state = (paused?: boolean) => ({
    tunedInScreenShares: { 'scr-1': 'c-1', 'scr-2': 'c-2' },
    dominantScreenShareId: 'scr-2',
    activeScreenShares: {
      'scr-1': { producerId: 'scr-1', userId: 'peer', username: 'peer', isLocal: false, paused },
      'scr-2': { producerId: 'scr-2', userId: 'peer2', username: 'peer2', isLocal: false },
    },
    participants: {
      peer: mockParticipant({ userId: 'peer', username: 'peer' }),
      peer2: mockParticipant({ userId: 'peer2', username: 'peer2' }),
    },
  });

  it('shows "Screen share paused" on a paused remote thumbnail', () => {
    setStreamBarState(state(true));
    const { container } = render(<StreamBar height={120} />);
    expect(screen.getByText('Screen share paused')).toBeInTheDocument();
    expect(container.querySelectorAll('video')).toHaveLength(0);
  });

  it('CONTROL: an unpaused remote thumbnail renders its video', () => {
    setStreamBarState(state());
    const { container } = render(<StreamBar height={120} />);
    expect(screen.queryByText('Screen share paused')).toBeNull();
    expect(container.querySelectorAll('video')).toHaveLength(1);
  });

  it('resuming re-attaches the live stream to the new thumbnail <video>', () => {
    const paused = state(true);
    setStreamBarState(paused);
    const { container } = render(<StreamBar height={120} />);
    act(() => useVoiceStore.setState({ activeScreenShares: state(false).activeScreenShares }));
    const video = container.querySelector('video') as HTMLVideoElement;
    expect(video.srcObject).toBe(paused.participants.peer.screenStream);
  });
});
