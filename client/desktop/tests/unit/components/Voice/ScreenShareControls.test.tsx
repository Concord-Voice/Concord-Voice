import React from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/ScreenShareControls.css', () => ({}));

// Mock voiceService dynamic import
const mockTuneIn = vi.fn();
const mockTuneOut = vi.fn();
const mockTuneInAll = vi.fn();
const mockTuneOutAll = vi.fn();
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {
    tuneInToScreenShare: (...a: unknown[]) => mockTuneIn(...a),
    tuneOutOfScreenShare: (...a: unknown[]) => mockTuneOut(...a),
    tuneInAllScreenShares: (...a: unknown[]) => mockTuneInAll(...a),
    tuneOutAllScreenShares: (...a: unknown[]) => mockTuneOutAll(...a),
  },
}));

import ScreenShareControls from '@/renderer/components/Voice/ScreenShareControls';

const remote = (n: number) => ({
  producerId: `prod-${n}`,
  userId: `user-${n}`,
  username: `user${n}`,
  displayName: `User ${n}`,
  isLocal: false,
});

describe('ScreenShareControls (#2088)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('renders nothing with no active shares', () => {
    const { container } = render(<ScreenShareControls />);
    expect(container.firstChild).toBeNull();
  });

  it('renders Tune In for available and Tune Out for tuned-in shares, with owner names', () => {
    const store = useVoiceStore.getState();
    store.registerActiveScreenShare(remote(1));
    store.registerActiveScreenShare(remote(2));
    store.tuneIn('prod-2', 'cons-2');
    render(<ScreenShareControls />);
    expect(screen.getByRole('button', { name: "Tune in to User 1's screen" })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: "Tune out of User 2's screen" })).toBeInTheDocument();
  });

  it('tunes out the DOMINANT share through its row control', async () => {
    const store = useVoiceStore.getState();
    store.registerActiveScreenShare(remote(1));
    store.tuneIn('prod-1', 'cons-1');
    store.setDominantScreenShare('prod-1');
    render(<ScreenShareControls />);
    fireEvent.click(screen.getByRole('button', { name: "Tune out of User 1's screen" }));
    await waitFor(() =>
      expect(mockTuneOut).toHaveBeenCalledWith('prod-1', { suppressAutoTune: true })
    );
  });

  it('local row is informational — no tune buttons', () => {
    useVoiceStore.getState().registerActiveScreenShare({
      producerId: 'p-local',
      userId: 'me',
      username: 'me',
      displayName: 'Me',
      isLocal: true,
    });
    useVoiceStore.getState().tuneIn('p-local', 'local-screen');
    render(<ScreenShareControls />);
    expect(screen.getByText(/Me \(You\)/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Tune (in|out)/i })).not.toBeInTheDocument();
  });

  it('Tune In All appears with available shares and dispatches; disabled at cap', async () => {
    const store = useVoiceStore.getState();
    store.registerActiveScreenShare(remote(1));
    render(<ScreenShareControls />);
    const btn = screen.getByRole('button', { name: 'Tune In All' });
    fireEvent.click(btn);
    // The handler awaits the voiceService dynamic import before dispatching
    await waitFor(() => expect(mockTuneInAll).toHaveBeenCalled());
    for (let i = 0; i < 5; i++) store.tuneIn(`t-${i}`, `c-${i}`);
    render(<ScreenShareControls />);
    expect(screen.getAllByRole('button', { name: 'Tune In All' }).at(-1)).toBeDisabled();
  });

  it('Tune Out All appears with tuned-in remote shares and dispatches', async () => {
    const store = useVoiceStore.getState();
    store.registerActiveScreenShare(remote(1));
    store.tuneIn('prod-1', 'cons-1');
    render(<ScreenShareControls />);
    fireEvent.click(screen.getByRole('button', { name: 'Tune Out All' }));
    // The handler awaits the voiceService dynamic import before dispatching
    await waitFor(() => expect(mockTuneOutAll).toHaveBeenCalled());
  });

  it('per-row Tune In disabled at the 5-share cap', () => {
    const store = useVoiceStore.getState();
    for (let i = 0; i < 5; i++) store.tuneIn(`t-${i}`, `c-${i}`);
    store.registerActiveScreenShare(remote(9));
    render(<ScreenShareControls />);
    expect(screen.getByRole('button', { name: "Tune in to User 9's screen" })).toBeDisabled();
  });
});
