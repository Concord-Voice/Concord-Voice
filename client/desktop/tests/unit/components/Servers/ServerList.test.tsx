import { render, screen, fireEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { mockServer, mockServer2 } from '../../../mocks/fixtures';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

import ServerList from '@/renderer/components/Servers/ServerList';

describe('ServerList', () => {
  const mockOnOpenActionModal = vi.fn();
  const mockOnContextMenu = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // Return empty server list from API so manual addServer calls don't
    // collide with the component's fetchServers() useEffect.
    mswServer.use(
      http.get('http://localhost:8080/api/v1/servers', () => {
        return HttpResponse.json({ servers: [] });
      })
    );
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('renders add server button', () => {
    render(
      <ServerList onOpenActionModal={mockOnOpenActionModal} onContextMenu={mockOnContextMenu} />
    );
    expect(screen.getByLabelText('Add a Server')).toBeInTheDocument();
  });

  it('calls onOpenActionModal when add button clicked', () => {
    render(
      <ServerList onOpenActionModal={mockOnOpenActionModal} onContextMenu={mockOnContextMenu} />
    );
    fireEvent.click(screen.getByLabelText('Add a Server'));
    expect(mockOnOpenActionModal).toHaveBeenCalled();
  });

  it('renders server icons', () => {
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().addServer(mockServer2);
    render(
      <ServerList onOpenActionModal={mockOnOpenActionModal} onContextMenu={mockOnContextMenu} />
    );
    expect(screen.getByLabelText('Test Server server')).toBeInTheDocument();
    expect(screen.getByLabelText('Second Server server')).toBeInTheDocument();
  });

  it('renders server initials when no icon', () => {
    useServerStore.getState().addServer(mockServer);
    render(
      <ServerList onOpenActionModal={mockOnOpenActionModal} onContextMenu={mockOnContextMenu} />
    );
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('marks active server', () => {
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
    render(
      <ServerList onOpenActionModal={mockOnOpenActionModal} onContextMenu={mockOnContextMenu} />
    );
    expect(screen.getByLabelText('Test Server server').closest('.server-icon-btn')).toHaveClass(
      'active'
    );
  });

  it('shows unread indicator for non-active servers', () => {
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().addServer(mockServer2);
    useServerStore.getState().setActiveServer(mockServer.id);
    useUnreadStore.getState().markServerUnread(mockServer2.id);
    render(
      <ServerList onOpenActionModal={mockOnOpenActionModal} onContextMenu={mockOnContextMenu} />
    );
    const server2Wrapper = screen
      .getByLabelText('Second Server server')
      .closest('.server-icon-wrapper');
    expect(server2Wrapper?.querySelector('.server-unread-pill')).toBeInTheDocument();
  });

  it('sets active server on click', () => {
    useServerStore.getState().addServer(mockServer);
    render(
      <ServerList onOpenActionModal={mockOnOpenActionModal} onContextMenu={mockOnContextMenu} />
    );
    fireEvent.click(screen.getByLabelText('Test Server server'));
    expect(useServerStore.getState().activeServerId).toBe('server-1');
  });
});
