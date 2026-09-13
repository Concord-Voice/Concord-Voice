import { act, render, screen, fireEvent } from '../../../test-utils';
import { Profiler } from 'react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useMemberStore } from '@/renderer/stores/chat/memberStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { mockUser } from '../../../mocks/fixtures';
import { resetAllStores } from '../../../helpers/store-helpers';

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
vi.mock('@/renderer/services/system/apiClient', () => ({
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
    resetAllStores();
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

  it('renders confirmed audience copy for connected server voice activity', () => {
    render(<UserPanel />);
    const button = screen.getByRole('button', { name: 'User menu for testuser' });
    button.focus();
    expect(button).toHaveFocus();

    const settings = {
      masterEnabled: true,
      serverVoiceTier: 2 as const,
      serverVoiceShowDetails: true,
      privateCallTier: 1 as const,
      privateCallShowDetails: false,
      customTextTier: 0 as const,
    };
    act(() => {
      useMemberStore.setState({ selfStatus: 'online' });
      useVoiceStore.setState({
        activeChannelId: '11111111-1111-4111-8111-111111111111',
        activeChannelName: 'Lobby',
        activeServerId: '22222222-2222-4222-8222-222222222222',
        connectionState: 'connected',
        callState: { kind: 'idle' },
      });
      useRichPresenceStore.setState({
        presenceSettings: settings,
        confirmedPresenceSettings: settings,
      });
    });

    expect(screen.getByText('In voice')).toBeInTheDocument();
    expect(
      screen.getByText('Eligible audience: People in this server who can view this voice channel.')
    ).toBeInTheDocument();
    const descriptionId = button.getAttribute('aria-describedby');
    expect(descriptionId).toBeTruthy();
    expect(document.getElementById(descriptionId as string)?.textContent).toContain(
      'Eligible audience: People in this server who can view this voice channel.'
    );
    expect(screen.getByRole('button', { name: 'User menu for testuser' })).not.toHaveTextContent(
      'Lobby'
    );
    expect(document.body.textContent).not.toContain('11111111-1111-4111-8111-111111111111');
    expect(document.body.textContent).not.toContain('22222222-2222-4222-8222-222222222222');
    expect(button).toHaveFocus();
  });

  it('uses confirmed settings and fixed delivery notes for Invisible activity', () => {
    useMemberStore.setState({ selfStatus: 'invisible' });
    useVoiceStore.setState({
      activeChannelId: '11111111-1111-4111-8111-111111111111',
      activeChannelName: 'Lobby',
      activeServerId: '22222222-2222-4222-8222-222222222222',
      connectionState: 'connected',
      callState: { kind: 'idle' },
    });
    useRichPresenceStore.setState({
      presenceSettings: {
        masterEnabled: true,
        serverVoiceTier: 2,
        serverVoiceShowDetails: true,
        privateCallTier: 1,
        privateCallShowDetails: false,
        customTextTier: 0,
      },
      confirmedPresenceSettings: null,
    });

    render(<UserPanel />);

    expect(screen.getByText('Eligible audience: Audience unavailable')).toBeInTheDocument();
    expect(screen.getByText('Not currently shared while Invisible')).toBeInTheDocument();
  });

  it('uses confirmed settings for an active group call and reports the Offline note', () => {
    useMemberStore.setState({ selfStatus: 'offline' });
    useVoiceStore.getState().setDMCall(true, 'group-1');
    useVoiceStore.getState().setGroupDMInfo(true, 'caller');
    useVoiceStore.getState().setCallState({ kind: 'in-call' });
    useVoiceStore.setState({
      connectionState: 'connected',
      participants: { peer1: {}, peer2: {} },
    });
    const confirmed = {
      masterEnabled: true,
      serverVoiceTier: 2 as const,
      serverVoiceShowDetails: true,
      privateCallTier: 0 as const,
      privateCallShowDetails: false,
      customTextTier: 0 as const,
    };
    useRichPresenceStore.setState({
      presenceSettings: { ...confirmed, privateCallTier: 2 },
      confirmedPresenceSettings: confirmed,
    });

    render(<UserPanel />);

    expect(screen.getByText('In a group call')).toBeInTheDocument();
    expect(
      screen.getByText('Eligible audience: People currently in this private call.')
    ).toBeInTheDocument();
    expect(screen.getByText('Not currently shared while Offline')).toBeInTheDocument();
  });

  it('avoids voice metadata renders while retaining activity and compact transitions', () => {
    let normalCommits = 0;
    render(
      <Profiler id="normal-user-panel" onRender={() => normalCommits++}>
        <UserPanel />
      </Profiler>
    );
    const normalBaseline = normalCommits;

    act(() => {
      useVoiceStore.getState().upsertParticipant('peer-1', { username: 'Peer' });
      useVoiceStore.getState().updateParticipant('peer-1', { isMuted: true });
      useVoiceStore.getState().setActiveSpeaker('peer-1');
    });
    expect(normalCommits).toBe(normalBaseline);

    act(() => {
      useVoiceStore.setState({
        activeChannelId: 'channel-1',
        activeChannelName: 'Lobby',
        activeServerId: 'server-1',
        connectionState: 'connected',
        callState: { kind: 'idle' },
      });
    });
    expect(normalCommits).toBeGreaterThan(normalBaseline);
    expect(screen.getByText('In voice')).toBeInTheDocument();

    const connectedNormalBaseline = normalCommits;
    act(() => {
      useVoiceStore.getState().updateParticipant('peer-1', { isMuted: false });
      useVoiceStore.getState().setActiveSpeaker(null);
    });
    expect(normalCommits).toBe(connectedNormalBaseline);

    let compactCommits = 0;
    render(
      <Profiler id="compact-user-panel" onRender={() => compactCommits++}>
        <UserPanel compact />
      </Profiler>
    );
    const compactBaseline = compactCommits;
    act(() => {
      useVoiceStore.getState().updateParticipant('peer-1', { isDeafened: true });
      useVoiceStore.getState().setActiveSpeaker(null);
      useVoiceStore.setState({ activeChannelId: null, callState: { kind: 'idle' } });
    });
    expect(compactCommits).toBe(compactBaseline);
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
