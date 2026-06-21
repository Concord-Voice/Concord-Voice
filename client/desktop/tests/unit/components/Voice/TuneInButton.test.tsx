import React from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { useVoiceStore, type AvailableScreenShare } from '@/renderer/stores/voiceStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/TuneInButton.css', () => ({}));

// Mock voiceService dynamic import
const mockTuneIn = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {
    tuneInToScreenShare: (...args: unknown[]) => mockTuneIn(...args),
  },
}));

import TuneInButton, { TuneInOverlay } from '@/renderer/components/Voice/TuneInButton';

const mockShare: AvailableScreenShare = {
  producerId: 'producer-1',
  userId: 'user-1',
  username: 'alice',
  displayName: 'Alice',
};

describe('TuneInButton', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useVoiceStore.setState({ tunedInScreenShares: {} });
  });

  it('renders button with display name', () => {
    render(<TuneInButton share={mockShare} />);
    expect(screen.getByText(/Alice/)).toBeInTheDocument();
    expect(screen.getByText('Tune In')).toBeInTheDocument();
  });

  it('falls back to username when displayName is undefined', () => {
    render(<TuneInButton share={{ ...mockShare, displayName: undefined }} />);
    expect(screen.getByText(/alice/)).toBeInTheDocument();
  });

  it('shows correct title tooltip', () => {
    render(<TuneInButton share={mockShare} />);
    const button = screen.getByRole('button');
    expect(button).toHaveAttribute('title', "Tune in to Alice's screen");
  });

  it('calls voiceService.tuneInToScreenShare on click', async () => {
    render(<TuneInButton share={mockShare} />);
    fireEvent.click(screen.getByRole('button'));
    await waitFor(() => {
      expect(mockTuneIn).toHaveBeenCalledWith('producer-1', 'user-1');
    });
  });

  it('disables button when at 5-stream limit', () => {
    useVoiceStore.setState({
      tunedInScreenShares: {
        a: {} as never,
        b: {} as never,
        c: {} as never,
        d: {} as never,
        e: {} as never,
      },
    });
    render(<TuneInButton share={mockShare} />);
    const button = screen.getByRole('button');
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute('title', 'Maximum 5 screen shares');
  });

  it('does not call tuneIn when at limit', async () => {
    useVoiceStore.setState({
      tunedInScreenShares: {
        a: {} as never,
        b: {} as never,
        c: {} as never,
        d: {} as never,
        e: {} as never,
      },
    });
    render(<TuneInButton share={mockShare} />);
    fireEvent.click(screen.getByRole('button'));
    expect(mockTuneIn).not.toHaveBeenCalled();
  });

  it('logs error when tuneInToScreenShare throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockTuneIn.mockRejectedValueOnce(new Error('tune in failed'));

    render(<TuneInButton share={mockShare} />);
    fireEvent.click(screen.getByRole('button'));

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to tune in:', 'tune in failed');
    });
    consoleSpy.mockRestore();
  });
});

describe('TuneInOverlay', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('renders nothing when no available screen shares', () => {
    useVoiceStore.setState({ availableScreenShares: [] });
    const { container } = render(<TuneInOverlay />);
    expect(container.firstChild).toBeNull();
  });

  it('renders a button for each available screen share', () => {
    useVoiceStore.setState({
      availableScreenShares: [
        mockShare,
        {
          ...mockShare,
          producerId: 'producer-2',
          userId: 'user-2',
          username: 'bob',
          displayName: 'Bob',
        },
      ],
      tunedInScreenShares: {},
    });
    render(<TuneInOverlay />);
    expect(screen.getByText(/Alice/)).toBeInTheDocument();
    expect(screen.getByText(/Bob/)).toBeInTheDocument();
  });
});
