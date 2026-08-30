import React from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import type { ActiveScreenShare } from '@/renderer/stores/voice/voiceStore';

vi.mock('@/renderer/components/Voice/ShareTunePill.css', () => ({}));

// voiceService is imported dynamically inside the click handler — mock the module
const mockTuneIn = vi.fn().mockResolvedValue(undefined);
const mockTuneOut = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {
    tuneInToScreenShare: (...args: unknown[]) => mockTuneIn(...args),
    tuneOutOfScreenShare: (...args: unknown[]) => mockTuneOut(...args),
  },
}));

import ShareTunePill from '@/renderer/components/Voice/ShareTunePill';

const remoteShare = (overrides: Partial<ActiveScreenShare> = {}): ActiveScreenShare => ({
  producerId: 'prod-1',
  userId: 'user-1',
  username: 'user1',
  displayName: 'User 1',
  isLocal: false,
  ...overrides,
});

describe('ShareTunePill', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('renders Tune In with the accessible per-user name when not tuned in', () => {
    render(<ShareTunePill share={remoteShare()} tunedIn={false} atCap={false} />);
    expect(screen.getByRole('button', { name: "Tune in to User 1's screen" })).toBeInTheDocument();
    expect(screen.getByText('Tune In')).toBeInTheDocument();
  });

  it('dispatches tuneInToScreenShare on click when not tuned in', async () => {
    render(<ShareTunePill share={remoteShare()} tunedIn={false} atCap={false} />);
    fireEvent.click(screen.getByRole('button', { name: "Tune in to User 1's screen" }));
    await waitFor(() => expect(mockTuneIn).toHaveBeenCalledWith('prod-1', 'user-1'));
    expect(mockTuneOut).not.toHaveBeenCalled();
  });

  it('dispatches tuneOutOfScreenShare with suppressAutoTune when tuned in (#2088)', async () => {
    render(<ShareTunePill share={remoteShare()} tunedIn atCap={false} />);
    fireEvent.click(screen.getByRole('button', { name: "Tune out of User 1's screen" }));
    await waitFor(() =>
      expect(mockTuneOut).toHaveBeenCalledWith('prod-1', { suppressAutoTune: true })
    );
    expect(mockTuneIn).not.toHaveBeenCalled();
  });

  it('blocks Tune In at the tuned-in cap: aria-disabled, described reason, no dispatch', async () => {
    render(<ShareTunePill share={remoteShare()} tunedIn={false} atCap />);
    const btn = screen.getByRole('button', { name: "Tune in to User 1's screen" });
    // House locked-control pattern: aria-disabled keeps the control reachable
    // for keyboard/screen-reader users; the JS guard blocks activation.
    expect(btn).toHaveAttribute('aria-disabled', 'true');
    expect(btn).toHaveAccessibleDescription('Maximum 5 screen shares');
    expect(btn).toHaveAttribute('title', 'Maximum 5 screen shares');
    await waitFor(() => fireEvent.click(btn));
    expect(mockTuneIn).not.toHaveBeenCalled();
  });

  it('keeps Tune Out active at the cap (tuning out frees a slot)', async () => {
    render(<ShareTunePill share={remoteShare()} tunedIn atCap />);
    const btn = screen.getByRole('button', { name: "Tune out of User 1's screen" });
    expect(btn).not.toHaveAttribute('aria-disabled');
    fireEvent.click(btn);
    await waitFor(() =>
      expect(mockTuneOut).toHaveBeenCalledWith('prod-1', { suppressAutoTune: true })
    );
  });

  it('logs a scrubbed message when the tune action rejects (no unhandled rejection)', async () => {
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockTuneIn.mockRejectedValueOnce(new Error('consume failed'));
    render(<ShareTunePill share={remoteShare()} tunedIn={false} atCap={false} />);
    fireEvent.click(screen.getByRole('button', { name: "Tune in to User 1's screen" }));
    await waitFor(() =>
      expect(errSpy).toHaveBeenCalledWith('Screen-share tune action failed:', expect.any(String))
    );
    errSpy.mockRestore();
  });

  it('renders the sharer name inside the pill when showName is set', () => {
    render(<ShareTunePill share={remoteShare()} tunedIn={false} atCap={false} showName />);
    expect(screen.getByRole('button', { name: "Tune in to User 1's screen" })).toHaveTextContent(
      'User 1 — Tune In'
    );
  });

  it('renders nothing for the local share', () => {
    const { container } = render(
      <ShareTunePill share={remoteShare({ isLocal: true })} tunedIn={false} atCap={false} />
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('falls back to the username when displayName is absent', () => {
    render(
      <ShareTunePill
        share={remoteShare({ displayName: undefined })}
        tunedIn={false}
        atCap={false}
      />
    );
    expect(screen.getByRole('button', { name: "Tune in to user1's screen" })).toBeInTheDocument();
  });
});
