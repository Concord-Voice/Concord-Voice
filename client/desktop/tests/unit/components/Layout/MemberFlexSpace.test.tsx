import { render, screen } from '../../../test-utils';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { mockUser, mockServer, mockMember } from '../../../mocks/fixtures';

// Mock MemberList to prevent complex rendering
vi.mock('@/renderer/components/Members/MemberList', () => ({
  default: () => <div data-testid="member-list">Member List</div>,
}));

// Mock useResizablePanel
vi.mock('@/renderer/hooks/useResizablePanel', () => ({
  useResizablePanel: () => ({
    width: 260,
    onMouseDown: vi.fn(),
    onKeyDown: vi.fn(),
  }),
}));

// Mock apiFetch
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ members: [] }),
  }),
  API_BASE: 'http://localhost:8080',
}));

import MemberFlexSpace from '@/renderer/components/Layout/MemberFlexSpace';

describe('MemberFlexSpace', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    useUserStore.setState({ user: mockUser });
    useMemberStore.setState({
      members: [mockMember],
      onlineUserIds: new Set(['user-1']),
      userStatuses: new Map([['user-1', 'online']]),
      selfStatus: 'online',
    });
    useLayoutStore.setState({ memberPanelMode: 'expanded', interfaceLocked: false });
  });

  it('renders nothing when no active server', () => {
    useServerStore.setState({ activeServerId: null });
    const { container } = render(<MemberFlexSpace />);
    expect(container.innerHTML).toBe('');
  });

  it('renders member list when expanded', () => {
    render(<MemberFlexSpace />);
    expect(screen.getByTestId('member-list')).toBeInTheDocument();
  });

  it('renders toggle button when hidden', () => {
    useLayoutStore.setState({ memberPanelMode: 'hidden' });
    render(<MemberFlexSpace />);
    expect(screen.getByLabelText('Toggle member panel')).toBeInTheDocument();
  });

  it('renders collapsed avatar strip', () => {
    useLayoutStore.setState({ memberPanelMode: 'collapsed' });
    const { container } = render(<MemberFlexSpace />);
    expect(container.querySelector('.member-panel-collapsed')).toBeInTheDocument();
    expect(screen.getByLabelText('Expand member list')).toBeInTheDocument();
  });

  it('shows member avatar initial in collapsed mode', () => {
    useLayoutStore.setState({ memberPanelMode: 'collapsed' });
    render(<MemberFlexSpace />);
    // mockMember.username starts with 't'
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('resize handle is keyboard-accessible with aria-label', () => {
    useLayoutStore.setState({ memberPanelMode: 'expanded' });
    render(<MemberFlexSpace />);
    const handle = screen.getByLabelText('Resize member panel');
    expect(handle).toBeInTheDocument();
    expect(handle).toHaveAttribute('tabindex', '0');
  });

  // Interface lock (#188) — the member-panel resize handle is removed when
  // locked, freezing the current width.
  it('removes the resize handle when the interface is locked', () => {
    useLayoutStore.setState({ memberPanelMode: 'expanded', interfaceLocked: true });
    render(<MemberFlexSpace />);
    expect(screen.queryByLabelText('Resize member panel')).not.toBeInTheDocument();
  });
});
