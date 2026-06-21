import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useMemberStore, ServerMember } from '@/renderer/stores/memberStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { mockUser, mockServer, mockMember, mockMember2 } from '../../../mocks/fixtures';

import MemberList from '@/renderer/components/Members/MemberList';

// Mock apiFetch to control member loading
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
  API_BASE: 'http://localhost:8080',
}));

// Mock MemberProfileCard to avoid complex rendering
vi.mock('@/renderer/components/Members/MemberProfileCard', () => ({
  default: ({ member, onClose }: { member: { username: string }; onClose: () => void }) => (
    <div data-testid="profile-card">
      {member.username}
      <button onClick={onClose}>Close</button>
    </div>
  ),
}));

// Mock MemberContextMenu — expose onBan/onKick so we can trigger the modals
vi.mock('@/renderer/components/Members/MemberContextMenu', () => ({
  default: ({
    member,
    onClose,
    onBan,
    onKick,
  }: {
    member: ServerMember;
    onClose: () => void;
    onBan: (m: ServerMember) => void;
    onKick: (m: ServerMember) => void;
  }) => (
    <div data-testid="context-menu">
      {member.username}
      <button onClick={onClose}>Close</button>
      <button data-testid="ctx-ban" onClick={() => onBan(member)}>
        Ban
      </button>
      <button data-testid="ctx-kick" onClick={() => onKick(member)}>
        Kick
      </button>
    </div>
  ),
}));

describe('MemberList', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: mockUser });
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
    // No RBAC roles configured — members fall into Online/Offline groups
    usePermissionStore.setState({ serverPermissions: {}, serverRoles: {} });
    // Default: return members successfully
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember, mockMember2] }),
    });
  });

  it('renders Members header', () => {
    render(<MemberList />);
    expect(screen.getByText('Members')).toBeInTheDocument();
  });

  it('shows empty state when no members', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [] }),
    });
    render(<MemberList />);
    expect(await screen.findByText('No members')).toBeInTheDocument();
  });

  it('shows loading skeletons when loading', () => {
    // Make fetch never resolve so isLoading stays true
    mockApiFetch.mockReturnValue(new Promise(() => {}));
    render(<MemberList />);
    const skeletons = document.querySelectorAll('.member-skeleton');
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it('shows error state with retry button', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Network error' }),
    });
    render(<MemberList />);
    expect(await screen.findByText('Network error')).toBeInTheDocument();
    expect(screen.getByText('Retry')).toBeInTheDocument();
  });

  it('renders members grouped by role', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember, mockMember2] }),
    });
    render(<MemberList />);
    // Without display_separately RBAC roles, members fall into Online/Offline groups
    expect(await screen.findByText(/Offline/)).toBeInTheDocument();
  });

  it('shows member display name', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    expect(await screen.findByText('Test User')).toBeInTheDocument();
  });

  it('renders avatar initial when no avatar URL', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    await screen.findByText('Test User');
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('opens profile card on member click', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    await screen.findByText('Test User');
    fireEvent.click(screen.getByText('Test User'));
    expect(screen.getByTestId('profile-card')).toBeInTheDocument();
  });

  it('opens context menu on right click', async () => {
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ members: [mockMember] }),
    });
    render(<MemberList />);
    await screen.findByText('Test User');
    fireEvent.contextMenu(screen.getByText('Test User'));
    expect(screen.getByTestId('context-menu')).toBeInTheDocument();
  });

  describe('ban confirmation modal', () => {
    it('opens ban modal when onBan is called from context menu', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      // Open context menu
      fireEvent.contextMenu(screen.getByText('Test User'));
      expect(screen.getByTestId('context-menu')).toBeInTheDocument();
      // Click ban in context menu mock
      fireEvent.click(screen.getByTestId('ctx-ban'));
      // Context menu should close, ban modal should open
      expect(screen.queryByTestId('context-menu')).not.toBeInTheDocument();
      expect(screen.getByText(/permanently remove them/)).toBeInTheDocument();
    });

    it('calls ban API and removes member on confirm', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');

      // Set up ban API response after mount fetches have consumed their mocks
      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-ban'));

      const confirmBtn = screen.getByRole('button', { name: 'Ban' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      // Verify ban API was called
      expect(mockApiFetch).toHaveBeenCalledWith(
        `/api/v1/servers/${mockServer.id}/bans/${mockMember.user_id}`,
        { method: 'POST' }
      );
      // Member should be removed from store
      const members = useMemberStore.getState().members;
      expect(members.find((m) => m.user_id === mockMember.user_id)).toBeUndefined();
    });

    it('shows error when ban API fails', async () => {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: async () => ({ members: [mockMember, mockMember2] }),
      });

      render(<MemberList />);
      await screen.findByText('Test User');

      // Now set up the ban to fail
      mockApiFetch.mockResolvedValueOnce({ ok: false });
      mockSafeJson.mockResolvedValueOnce({ error: 'Hierarchy violation' });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-ban'));

      const confirmBtn = screen.getByRole('button', { name: 'Ban' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      // Error should be displayed in the modal
      expect(screen.getByText('Hierarchy violation')).toBeInTheDocument();
    });

    it('closes ban modal on cancel', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-ban'));
      expect(screen.getByText(/permanently remove them/)).toBeInTheDocument();

      // Click cancel
      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByText(/permanently remove them/)).not.toBeInTheDocument();
    });
  });

  describe('kick confirmation modal', () => {
    it('opens kick modal when onKick is called from context menu', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));
      expect(screen.queryByTestId('context-menu')).not.toBeInTheDocument();
      expect(screen.getByText(/can rejoin with a new invite/)).toBeInTheDocument();
    });

    it('calls kick API and removes member on confirm', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');

      // Set up kick API response after mount fetches have consumed their mocks
      mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));

      const confirmBtn = screen.getByRole('button', { name: 'Kick' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      expect(mockApiFetch).toHaveBeenCalledWith(
        `/api/v1/servers/${mockServer.id}/members/${mockMember.user_id}`,
        { method: 'DELETE' }
      );
      const members = useMemberStore.getState().members;
      expect(members.find((m) => m.user_id === mockMember.user_id)).toBeUndefined();
    });

    it('shows error when kick API fails', async () => {
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: async () => ({ members: [mockMember, mockMember2] }),
      });

      render(<MemberList />);
      await screen.findByText('Test User');

      // Now set up the kick to fail
      mockApiFetch.mockResolvedValueOnce({ ok: false });
      mockSafeJson.mockResolvedValueOnce({ error: 'Cannot kick owner' });

      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));

      const confirmBtn = screen.getByRole('button', { name: 'Kick' });
      await act(async () => {
        fireEvent.click(confirmBtn);
      });

      expect(screen.getByText('Cannot kick owner')).toBeInTheDocument();
    });

    it('closes kick modal on cancel', async () => {
      render(<MemberList />);
      await screen.findByText('Test User');
      fireEvent.contextMenu(screen.getByText('Test User'));
      fireEvent.click(screen.getByTestId('ctx-kick'));
      expect(screen.getByText(/can rejoin with a new invite/)).toBeInTheDocument();

      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByText(/can rejoin with a new invite/)).not.toBeInTheDocument();
    });
  });
});
