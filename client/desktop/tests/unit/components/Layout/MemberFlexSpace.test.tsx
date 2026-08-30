import { fireEvent, render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useLayoutStore } from '@/renderer/stores/ui/layoutStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { mockServer } from '../../../mocks/fixtures';
import MemberFlexSpace from '@/renderer/components/Layout/MemberFlexSpace';
import { DockOverlayProvider } from '@/renderer/components/Layout/DockShell';

vi.mock('@/renderer/components/Members/MemberList', () => ({
  default: ({ compact }: { compact?: boolean }) => (
    <div data-testid="member-list" data-compact={String(compact)}>
      Member List
    </div>
  ),
}));

const renderMembers = () =>
  render(
    <DockOverlayProvider>
      <MemberFlexSpace />
    </DockOverlayProvider>
  );

describe('MemberFlexSpace', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    useLayoutStore.setState({
      sidebarLayoutsDecoupled: true,
      sidebarProfiles: {
        dm: {
          left: { width: 212, pinned: true },
          right: { width: 294, pinned: true },
        },
        server: {
          left: { width: 318, pinned: true },
          right: { width: 306, pinned: true },
        },
      },
      interfaceLocked: false,
    });
  });

  it('renders nothing without an active server', () => {
    useServerStore.setState({ activeServerId: null });
    const { container } = renderMembers();
    expect(container).toBeEmptyDOMElement();
  });

  it('adapts the existing member list into the Server right dock', () => {
    const { container } = renderMembers();

    expect(screen.getByTestId('member-list')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Unpin Members sidebar' })).toBeInTheDocument();
    expect(container.querySelector('.dock-shell__surface')).toHaveStyle({ width: '306px' });
  });

  it('commits right-side keyboard resizing to the Server profile', () => {
    renderMembers();
    fireEvent.keyDown(screen.getByRole('separator', { name: 'Resize Members sidebar' }), {
      key: 'ArrowLeft',
    });

    expect(useLayoutStore.getState().sidebarProfiles.server.right.width).toBe(316);
    expect(useLayoutStore.getState().sidebarProfiles.dm.right.width).toBe(294);
  });

  it('passes the dock compact presentation to the existing member list', () => {
    useLayoutStore.setState((state) => ({
      sidebarProfiles: {
        ...state.sidebarProfiles,
        server: {
          ...state.sidebarProfiles.server,
          right: { ...state.sidebarProfiles.server.right, width: 120 },
        },
      },
    }));

    renderMembers();
    expect(screen.getByTestId('member-list')).toHaveAttribute('data-compact', 'true');
  });

  it('uses the shared right-edge lip when unpinned', () => {
    useLayoutStore.getState().setSidebarPinned('server', 'right', false);
    renderMembers();
    expect(screen.getByRole('button', { name: 'Open Members sidebar' })).toBeInTheDocument();
  });
});
