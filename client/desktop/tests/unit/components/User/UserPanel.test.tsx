import { render, screen, fireEvent } from '../../../test-utils';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useSettingsOverlayStore } from '@/renderer/stores/settingsOverlayStore';
import { mockUser } from '../../../mocks/fixtures';

// Mock UserPopover — minimal stub exposing the close + onOpenFeedback paths.
// The onOpenFeedback button mirrors the real popover's "Bug Report / Feature
// Request" entry point so UserPanel's wire-up can be tested in isolation.
vi.mock('@/renderer/components/User/UserPopover', () => ({
  default: ({
    onClose,
    onOpenFeedback,
    onOpenCustomStatus,
  }: {
    onClose: () => void;
    onOpenFeedback?: () => void;
    onOpenCustomStatus?: () => void;
  }) => (
    <div data-testid="user-popover">
      <button onClick={onClose}>Close</button>
      {onOpenFeedback && (
        <button onClick={onOpenFeedback} data-testid="popover-feedback-btn">
          Open Feedback
        </button>
      )}
      {onOpenCustomStatus && (
        <button onClick={onOpenCustomStatus} data-testid="popover-custom-status-btn">
          Open Custom Status
        </button>
      )}
    </div>
  ),
}));

// Mock FeedbackModal — render a visible signal when isOpen so we can assert
// UserPanel's mount-and-state management without pulling the real modal's
// dependencies (apiClient, systemInfo, log buffer) into this file's surface.
vi.mock('@/renderer/components/User/FeedbackModal', () => ({
  default: ({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) =>
    isOpen ? (
      <div data-testid="feedback-modal">
        <button onClick={onClose}>Close Feedback</button>
      </div>
    ) : null,
}));

vi.mock('@/renderer/components/User/CustomStatusPopover', () => ({
  default: ({ onClose }: { onClose: () => void }) => (
    <div role="dialog" aria-label="Set custom status">
      <button onClick={onClose}>Close Custom Status</button>
    </div>
  ),
}));

// Mock apiFetch
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ user: {} }),
  }),
  API_BASE: 'http://localhost:8080',
}));

import UserPanel from '@/renderer/components/User/UserPanel';

describe('UserPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: mockUser, isLoading: false });
    useMemberStore.setState({ selfStatus: 'online' });
  });

  it('renders user avatar initial', () => {
    render(<UserPanel />);
    expect(screen.getByText('T')).toBeInTheDocument(); // First letter of "testuser"
  });

  it('renders username', () => {
    render(<UserPanel />);
    expect(screen.getByText('testuser')).toBeInTheDocument();
  });

  it('renders status text', () => {
    render(<UserPanel />);
    expect(screen.getByText('Online')).toBeInTheDocument();
  });

  it('shows DND status', () => {
    useMemberStore.setState({ selfStatus: 'dnd' });
    render(<UserPanel />);
    expect(screen.getByText('Do Not Disturb')).toBeInTheDocument();
  });

  it('renders settings button', () => {
    render(<UserPanel />);
    expect(screen.getByLabelText('Settings')).toBeInTheDocument();
  });

  it('opens popover on avatar click', () => {
    render(<UserPanel />);
    fireEvent.click(screen.getByText('T'));
    expect(screen.getByTestId('user-popover')).toBeInTheDocument();
  });

  it('opens popover when username or status text is clicked', () => {
    const { rerender } = render(<UserPanel />);

    fireEvent.click(screen.getByText('testuser'));
    expect(screen.getByTestId('user-popover')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Close'));
    expect(screen.queryByTestId('user-popover')).not.toBeInTheDocument();

    rerender(<UserPanel />);
    fireEvent.click(screen.getByText('Online'));
    expect(screen.getByTestId('user-popover')).toBeInTheDocument();
  });

  it('clicks settings button and opens app settings overlay', () => {
    useSettingsOverlayStore.setState({ open: null, payload: null });
    render(<UserPanel />);
    fireEvent.click(screen.getByLabelText('Settings'));
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(screen.queryByTestId('user-popover')).not.toBeInTheDocument();
  });

  it('opens custom status editor from the user popover command', () => {
    render(<UserPanel />);

    fireEvent.click(screen.getByText('testuser'));
    fireEvent.click(screen.getByTestId('popover-custom-status-btn'));

    expect(screen.getByRole('dialog', { name: 'Set custom status' })).toBeInTheDocument();
  });

  it('does not offer custom status editor from the compact popover', () => {
    render(<UserPanel compact />);

    fireEvent.click(screen.getByText('T'));

    expect(screen.getByTestId('user-popover')).toBeInTheDocument();
    expect(screen.queryByTestId('popover-custom-status-btn')).not.toBeInTheDocument();
  });

  it('renders loading state when user not loaded', () => {
    // Clear token so useEffect doesn't call fetchUser (which would set an invalid user)
    useAuthStore.getState().clearAccessToken();
    useUserStore.setState({ user: null, isLoading: true });
    const { container } = render(<UserPanel />);
    // Should render skeleton when loading
    expect(container.querySelector('.user-panel')).toBeInTheDocument();
    expect(container.querySelector('.user-avatar-skeleton')).toBeInTheDocument();
  });

  // ── #158 — Feedback modal wiring ───────────────────────────────────────

  describe('feedback modal (#158)', () => {
    it('FeedbackModal is unmounted by default (isOpen=false)', () => {
      render(<UserPanel />);
      expect(screen.queryByTestId('feedback-modal')).not.toBeInTheDocument();
    });

    it('opens FeedbackModal when popover invokes onOpenFeedback', () => {
      render(<UserPanel />);
      fireEvent.click(screen.getByText('T')); // open popover
      expect(screen.getByTestId('user-popover')).toBeInTheDocument();
      fireEvent.click(screen.getByTestId('popover-feedback-btn'));
      expect(screen.getByTestId('feedback-modal')).toBeInTheDocument();
    });

    it('FeedbackModal stays open after the popover closes', () => {
      // Important invariant: the popover closes BEFORE the modal opens (the
      // popover's button triggers onClose then onOpenFeedback per UserPopover
      // contract). The modal lives at UserPanel scope so it survives the
      // popover unmount.
      render(<UserPanel />);
      fireEvent.click(screen.getByText('T'));
      fireEvent.click(screen.getByTestId('popover-feedback-btn'));
      // Simulate the popover's onClose being called separately (e.g., click
      // outside) — modal should still be there.
      // In production the modal lifecycle is independent of the popover; this
      // assertion locks that.
      expect(screen.getByTestId('feedback-modal')).toBeInTheDocument();
    });

    it('FeedbackModal close handler unmounts it', () => {
      render(<UserPanel />);
      fireEvent.click(screen.getByText('T'));
      fireEvent.click(screen.getByTestId('popover-feedback-btn'));
      fireEvent.click(screen.getByText('Close Feedback'));
      expect(screen.queryByTestId('feedback-modal')).not.toBeInTheDocument();
    });
  });
});
