import { fireEvent, render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import FriendsFlexSpace from '@/renderer/components/DirectMessages/FriendsFlexSpace';
import { DockOverlayProvider } from '@/renderer/components/Layout/DockShell';

vi.mock('@/renderer/components/DirectMessages/FriendsList', () => ({
  default: ({
    onFriendClick,
    compact,
  }: {
    onFriendClick?: (userId: string) => void;
    compact?: boolean;
  }) => (
    <button
      type="button"
      data-testid="friends-list"
      data-compact={String(compact)}
      onClick={() => onFriendClick?.('user-1')}
    >
      Friends List
    </button>
  ),
}));

const renderFriends = (onFriendClick?: (userId: string) => void) =>
  render(
    <DockOverlayProvider>
      <FriendsFlexSpace onFriendClick={onFriendClick} />
    </DockOverlayProvider>
  );

describe('FriendsFlexSpace', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
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

  it('adapts the existing friends list into the DM right dock', () => {
    const { container } = renderFriends();

    expect(screen.getByTestId('friends-list')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Unpin Friends sidebar' })).toBeInTheDocument();
    expect(container.querySelector('.dock-shell__surface')).toHaveStyle({ width: '294px' });
  });

  it('commits right-side keyboard resizing to the DM profile', () => {
    renderFriends();
    fireEvent.keyDown(screen.getByRole('separator', { name: 'Resize Friends sidebar' }), {
      key: 'ArrowRight',
    });

    expect(useLayoutStore.getState().sidebarProfiles.dm.right.width).toBe(284);
    expect(useLayoutStore.getState().sidebarProfiles.server.right.width).toBe(306);
  });

  it('forwards friend activation through the unchanged list instance', () => {
    const onFriendClick = vi.fn();
    renderFriends(onFriendClick);
    fireEvent.click(screen.getByTestId('friends-list'));
    expect(onFriendClick).toHaveBeenCalledWith('user-1');
  });

  it('passes the dock compact presentation to the existing friends list', () => {
    useLayoutStore.setState((state) => ({
      sidebarProfiles: {
        ...state.sidebarProfiles,
        dm: {
          ...state.sidebarProfiles.dm,
          right: { ...state.sidebarProfiles.dm.right, width: 120 },
        },
      },
    }));

    renderFriends();
    expect(screen.getByTestId('friends-list')).toHaveAttribute('data-compact', 'true');
  });

  it('uses the shared right-edge lip when unpinned', () => {
    useLayoutStore.getState().setSidebarPinned('dm', 'right', false);
    renderFriends();
    expect(screen.getByRole('button', { name: 'Open Friends sidebar' })).toBeInTheDocument();
  });
});
