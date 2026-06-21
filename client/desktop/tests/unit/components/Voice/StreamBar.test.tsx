import React from 'react';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { vi } from 'vitest';

// ── Service mock ─────────────────────────────────────────────────────────────
const mockTuneOut = vi.fn();
vi.mock('@/renderer/services/voiceService', () => ({
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
    expect(mockTuneOut).toHaveBeenCalledWith('p2');
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
    render(<StreamBar height={120} />);
    expect(screen.getByText('Paused')).toBeInTheDocument();
  });
});
