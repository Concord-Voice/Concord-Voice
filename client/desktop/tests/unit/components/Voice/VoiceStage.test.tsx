import React from 'react';
import { render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore, type VoiceParticipant } from '@/renderer/stores/voiceStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { vi } from 'vitest';

// ── CSS mock ─────────────────────────────────────────────────────────────────
vi.mock('@/renderer/components/Voice/VoiceStage.css', () => ({}));

import VoiceStage from '@/renderer/components/Voice/VoiceStage';

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

function setStageState(overrides: Record<string, unknown> = {}) {
  useVoiceStore.setState({
    dominantScreenShareId: null,
    tunedInScreenShares: {},
    participants: {},
    stageLayout: 'focus',
    localStreamPaused: false,
    ...overrides,
  });
}

describe('VoiceStage', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // Mock HTMLVideoElement.play since jsdom does not support it
    HTMLVideoElement.prototype.play = vi.fn().mockResolvedValue(undefined);
  });

  // ── Empty state ──────────────────────────────────────────────────────────

  it('shows empty state when no tuned-in screen shares', () => {
    setStageState({ tunedInScreenShares: {} });
    render(<VoiceStage />);
    expect(screen.getByText('No screen share selected')).toBeInTheDocument();
  });

  // ── Equal layout mode ────────────────────────────────────────────────────

  it('renders grid in equal layout with tuned-in shares', () => {
    setStageState({
      stageLayout: 'equal',
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      participants: { 'user-1': mockParticipant() },
    });
    const { container } = render(<VoiceStage />);
    expect(container.querySelector('.voice-stage--equal')).toBeInTheDocument();
    expect(container.querySelector('.voice-stage__grid')).toBeInTheDocument();
  });

  it('shows sharer name overlay in equal layout', () => {
    setStageState({
      stageLayout: 'equal',
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      participants: {
        'user-1': mockParticipant({ displayName: 'Alice' }),
      },
    });
    render(<VoiceStage />);
    expect(screen.getByText(/Alice\u2019s screen/)).toBeInTheDocument();
  });

  it('shows layout toggle in equal mode when multiple shares', () => {
    setStageState({
      stageLayout: 'equal',
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      participants: {
        'user-1': mockParticipant(),
        'user-2': mockParticipant({ userId: 'user-2', username: 'bob', displayName: 'Bob' }),
      },
    });
    render(<VoiceStage />);
    expect(screen.getByTitle('Switch to focus mode')).toBeInTheDocument();
  });

  it('does not show layout toggle in equal mode with single share', () => {
    setStageState({
      stageLayout: 'equal',
      tunedInScreenShares: { p1: 'c1' },
      participants: { 'user-1': mockParticipant() },
    });
    render(<VoiceStage />);
    expect(screen.queryByTitle('Switch to focus mode')).not.toBeInTheDocument();
  });

  // ── Focus layout mode ────────────────────────────────────────────────────

  it('renders dominant stream in focus layout', () => {
    setStageState({
      stageLayout: 'focus',
      dominantScreenShareId: 'producer-1',
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      participants: {
        'user-1': mockParticipant({ displayName: 'Alice' }),
      },
    });
    render(<VoiceStage />);
    // The overlay shows "Name's screen"
    expect(screen.getByText(/Alice/)).toBeInTheDocument();
  });

  it('shows cycle buttons in focus mode with multiple shares', () => {
    setStageState({
      stageLayout: 'focus',
      dominantScreenShareId: 'p1',
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      participants: {
        'user-1': mockParticipant(),
        'user-2': mockParticipant({ userId: 'user-2', username: 'bob' }),
      },
    });
    render(<VoiceStage />);
    expect(screen.getByTitle(/Previous screen share/)).toBeInTheDocument();
    expect(screen.getByTitle(/Next screen share/)).toBeInTheDocument();
  });

  it('does not show cycle buttons in focus mode with single share', () => {
    setStageState({
      stageLayout: 'focus',
      dominantScreenShareId: 'p1',
      tunedInScreenShares: { p1: 'c1' },
      participants: { 'user-1': mockParticipant() },
    });
    render(<VoiceStage />);
    expect(screen.queryByTitle(/Previous screen share/)).not.toBeInTheDocument();
    expect(screen.queryByTitle(/Next screen share/)).not.toBeInTheDocument();
  });

  it('shows counter in focus mode with multiple shares', () => {
    setStageState({
      stageLayout: 'focus',
      dominantScreenShareId: 'p1',
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      participants: {
        'user-1': mockParticipant(),
        'user-2': mockParticipant({ userId: 'user-2', username: 'bob' }),
      },
    });
    render(<VoiceStage />);
    expect(screen.getByText('1 / 2')).toBeInTheDocument();
  });

  it('shows layout toggle in focus mode with multiple shares', () => {
    setStageState({
      stageLayout: 'focus',
      dominantScreenShareId: 'p1',
      tunedInScreenShares: { p1: 'c1', p2: 'c2' },
      participants: {
        'user-1': mockParticipant(),
        'user-2': mockParticipant({ userId: 'user-2', username: 'bob' }),
      },
    });
    render(<VoiceStage />);
    expect(screen.getByTitle('Switch to equal layout')).toBeInTheDocument();
  });

  // ── Paused state ─────────────────────────────────────────────────────────

  it('shows paused placeholder when local stream is paused in equal mode', () => {
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
    setStageState({
      stageLayout: 'equal',
      tunedInScreenShares: { 'producer-1': 'consumer-1' },
      participants: {
        'user-1': mockParticipant({ userId: 'user-1' }),
      },
      localStreamPaused: true,
    });
    render(<VoiceStage />);
    expect(screen.getByText('Your Screen Is Still Streaming')).toBeInTheDocument();
  });

  // ── Unknown sharer ───────────────────────────────────────────────────────

  it('shows Unknown for sharer name when no matching participant', () => {
    setStageState({
      stageLayout: 'focus',
      dominantScreenShareId: 'p1',
      tunedInScreenShares: { p1: 'c1' },
      participants: {},
    });
    render(<VoiceStage />);
    expect(screen.getByText(/Unknown/)).toBeInTheDocument();
  });
});
