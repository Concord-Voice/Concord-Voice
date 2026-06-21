import { render, screen, fireEvent } from '../../../test-utils';
import { useUserStore } from '@/renderer/stores/userStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useSettingsOverlayStore } from '@/renderer/stores/settingsOverlayStore';
import { mockUser } from '../../../mocks/fixtures';

// Mock websocketService
const mockSendSetStatus = vi.fn();
vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({
    sendSetStatus: mockSendSetStatus,
  }),
}));

import UserPopover from '@/renderer/components/User/UserPopover';

describe('UserPopover', () => {
  const mockOnClose = vi.fn();
  const defaultProps = {
    user: mockUser,
    onClose: mockOnClose,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    useUserStore.setState({ user: mockUser });
    useMemberStore.setState({ selfStatus: 'online' });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders user avatar initial', () => {
    render(<UserPopover {...defaultProps} />);
    expect(screen.getByText('T')).toBeInTheDocument(); // testuser
  });

  it('renders username', () => {
    render(<UserPopover {...defaultProps} />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('renders current status', () => {
    render(<UserPopover {...defaultProps} />);
    // "Online" appears as status display AND as status option
    const onlineElements = screen.getAllByText('Online');
    expect(onlineElements.length).toBeGreaterThanOrEqual(2);
  });

  it('renders email when provided', () => {
    render(<UserPopover {...defaultProps} />);
    expect(screen.getByText(mockUser.email)).toBeInTheDocument();
  });

  it('renders status options', () => {
    render(<UserPopover {...defaultProps} />);
    expect(screen.getByText('Status')).toBeInTheDocument();
    // "Online" appears as both current status and status option -
    // check that Do Not Disturb and Invisible are option items
    expect(screen.getByText('Do Not Disturb')).toBeInTheDocument();
    expect(screen.getByText('Invisible')).toBeInTheDocument();
  });

  it('renders My Profile menu item', () => {
    render(<UserPopover {...defaultProps} />);
    expect(screen.getByText('My Profile')).toBeInTheDocument();
  });

  it('renders Settings menu item', () => {
    render(<UserPopover {...defaultProps} />);
    expect(screen.getByText('Settings')).toBeInTheDocument();
  });

  it('renders Log Out button', () => {
    render(<UserPopover {...defaultProps} />);
    expect(screen.getByText('Log Out')).toBeInTheDocument();
  });

  it('changes status on click and sends to WebSocket', () => {
    render(<UserPopover {...defaultProps} />);
    fireEvent.click(screen.getByText('Do Not Disturb'));
    expect(useMemberStore.getState().selfStatus).toBe('dnd');
    expect(mockSendSetStatus).toHaveBeenCalledWith('dnd');
  });

  it('closes on Escape key', () => {
    render(<UserPopover {...defaultProps} />);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('renders avatar image when user has avatar_url', () => {
    const userWithAvatar = { ...mockUser, avatar_url: 'https://example.com/avatar.png' };
    render(<UserPopover {...defaultProps} user={userWithAvatar} />);
    const img = screen.getByAltText('testuser');
    expect(img).toHaveAttribute('src', 'https://example.com/avatar.png');
  });

  it('clicks My Profile and opens profile settings overlay', () => {
    useSettingsOverlayStore.setState({ open: null, payload: null });
    render(<UserPopover {...defaultProps} />);
    fireEvent.click(screen.getByText('My Profile'));
    expect(useSettingsOverlayStore.getState().open).toBe('profile');
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('clicks Settings and opens app settings overlay', () => {
    useSettingsOverlayStore.setState({ open: null, payload: null });
    render(<UserPopover {...defaultProps} />);
    fireEvent.click(screen.getByText('Settings'));
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(mockOnClose).toHaveBeenCalled();
  });

  // ── #158 — Feedback button ──────────────────────────────────────────────

  describe('feedback button (#158)', () => {
    it('does NOT render the button when onOpenFeedback is not provided', () => {
      render(<UserPopover {...defaultProps} />);
      // The button is gated on the callback being passed — the popover is
      // usable without it (e.g., legacy mount points).
      expect(screen.queryByText(/Bug Report \/ Feature Request/)).not.toBeInTheDocument();
    });

    it('renders the button when onOpenFeedback IS provided', () => {
      const onOpenFeedback = vi.fn();
      render(<UserPopover {...defaultProps} onOpenFeedback={onOpenFeedback} />);
      expect(screen.getByText('Bug Report / Feature Request')).toBeInTheDocument();
    });

    it('clicking the button calls onClose then onOpenFeedback (order matters)', () => {
      const onOpenFeedback = vi.fn();
      // Use a custom onClose that records the order with respect to
      // onOpenFeedback so we can lock the contract — the popover must close
      // BEFORE the feedback modal opens so they don't visually overlap.
      const calls: string[] = [];
      const onClose = vi.fn(() => calls.push('close'));
      onOpenFeedback.mockImplementation(() => calls.push('feedback'));

      render(<UserPopover {...defaultProps} onClose={onClose} onOpenFeedback={onOpenFeedback} />);
      fireEvent.click(screen.getByText('Bug Report / Feature Request'));

      expect(onClose).toHaveBeenCalled();
      expect(onOpenFeedback).toHaveBeenCalled();
      expect(calls).toEqual(['close', 'feedback']);
    });
  });
});
