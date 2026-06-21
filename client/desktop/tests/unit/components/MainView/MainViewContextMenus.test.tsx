import { render as rtlRender, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { render } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import { resetAllStores } from '../../../helpers/store-helpers';
import MainViewContextMenus from '@/renderer/components/MainView/MainViewContextMenus';
import type { ServerWithRole } from '@/renderer/types/server';
import type { Channel, ChannelGroup } from '@/renderer/types/chat';

/**
 * Render the component at a specific route. Used by the DM-route gate test
 * — the default test-utils `render` wraps in `BrowserRouter` rooted at `/`,
 * which doesn't exercise the `useLocation().pathname.startsWith('/app/dms')`
 * branch added in issue #984.
 */
function renderAtRoute(ui: React.ReactElement, route: string) {
  return rtlRender(
    <MemoryRouter initialEntries={[route]}>
      <ModalProvider>{ui}</ModalProvider>
    </MemoryRouter>
  );
}

describe('MainViewContextMenus', () => {
  beforeEach(() => {
    resetAllStores();
  });

  const noopProps = {
    serverContextMenu: null,
    setServerContextMenu: vi.fn(),
    channelContextMenu: null,
    setChannelContextMenu: vi.fn(),
    categoryContextMenu: null,
    setCategoryContextMenu: vi.fn(),
    emptyContextMenu: null,
    setEmptyContextMenu: vi.fn(),
    activeServer: null,
    canManageChannels: false,
    onEditServer: vi.fn(),
    onDeleteServer: vi.fn(),
    onLeaveServer: vi.fn(),
    onInviteServer: vi.fn(),
    onEditChannel: vi.fn(),
    onDeleteChannel: vi.fn(),
    onChannelPermissions: vi.fn(),
    onEditCategory: vi.fn(),
    onDeleteCategory: vi.fn(),
    onCategoryPermissions: vi.fn(),
    onOpenCreateChannelModal: vi.fn(),
    onOpenCreateCategoryModal: vi.fn(),
  };

  it('renders without crashing when all menus are closed', () => {
    const { container } = render(<MainViewContextMenus {...noopProps} />);
    expect(container).toBeTruthy();
  });

  it('does not crash when all menu props are non-null (branch-coverage smoke)', () => {
    const mockServer: ServerWithRole = {
      id: 'server-1',
      name: 'Test Server',
      owner_id: 'user-1',
      allow_embedded_content: false,
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
      role: 'owner',
      member_count: 1,
      online_count: 1,
    };

    const mockChannel: Channel = {
      id: 'chan-1',
      server_id: 'server-1',
      name: 'general',
      type: 'text',
      position: 0,
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
    };

    const mockCategory: ChannelGroup = {
      id: 'cat-1',
      server_id: 'server-1',
      name: 'General',
      position: 0,
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
    };

    const allOpenProps = {
      ...noopProps,
      serverContextMenu: { server: mockServer, position: { x: 10, y: 10 } },
      channelContextMenu: { channel: mockChannel, position: { x: 10, y: 10 } },
      categoryContextMenu: { group: mockCategory, position: { x: 10, y: 10 } },
      emptyContextMenu: { position: { x: 10, y: 10 } },
      activeServer: mockServer,
      canManageChannels: true,
    };

    const { container } = render(<MainViewContextMenus {...allOpenProps} />);
    expect(container).toBeTruthy();
  });

  // Issue #984 — DM-route gate: empty-area menu must NOT render on /app/dms
  describe('empty-area menu DM-route gate (#984)', () => {
    const baseEmptyMenuProps = {
      ...noopProps,
      activeServer: {
        id: 'server-1',
        name: 'Test Server',
        owner_id: 'user-1',
        allow_embedded_content: false,
        created_at: '2024-01-01T00:00:00Z',
        updated_at: '2024-01-01T00:00:00Z',
        role: 'owner' as const,
        member_count: 1,
        online_count: 1,
      },
      canManageChannels: true,
      emptyContextMenu: { position: { x: 100, y: 100 } },
    };

    it('renders Create Channel menu when right-clicking empty space on a server route', () => {
      renderAtRoute(<MainViewContextMenus {...baseEmptyMenuProps} />, '/app/servers/server-1');
      // ChannelListContextMenu surfaces "Create Channel" — fail-loud if it's missing OR if it leaks.
      expect(screen.getByText(/Create Channel/i)).toBeTruthy();
    });

    it('does NOT render Create Channel menu when right-clicking empty space on /app/dms', () => {
      renderAtRoute(<MainViewContextMenus {...baseEmptyMenuProps} />, '/app/dms');
      expect(screen.queryByText(/Create Channel/i)).toBeNull();
    });

    it('does NOT render Create Channel menu when right-clicking empty space on a DM conversation route', () => {
      renderAtRoute(
        <MainViewContextMenus {...baseEmptyMenuProps} />,
        '/app/dms/some-conversation-id'
      );
      expect(screen.queryByText(/Create Channel/i)).toBeNull();
    });
  });
});
