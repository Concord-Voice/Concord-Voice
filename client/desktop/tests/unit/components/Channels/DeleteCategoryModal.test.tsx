import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { mockServer } from '../../../mocks/fixtures';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import type { ChannelGroup } from '@/renderer/types/chat';
import DeleteCategoryModal from '@/renderer/components/Channels/DeleteCategoryModal';

const API_BASE = 'http://localhost:8080';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const mockGroup: ChannelGroup = {
  id: 'group-1',
  server_id: 'server-1',
  name: 'Voice Channels',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

const getConfirmBtn = () =>
  document.querySelector<HTMLButtonElement>('button.delete-server-confirm-btn')!;

describe('DeleteCategoryModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
    useChannelStore.setState({
      channelGroups: [mockGroup],
    });
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <DeleteCategoryModal isOpen={false} group={mockGroup} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders warning with category name', () => {
    render(<DeleteCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    expect(screen.getByText(/Voice Channels/)).toBeInTheDocument();
    expect(screen.getByText(/become uncategorized/)).toBeInTheDocument();
  });

  it('renders Delete Category confirmation button', () => {
    render(<DeleteCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    expect(getConfirmBtn()).toBeInTheDocument();
    expect(getConfirmBtn().textContent).toContain('Delete Category');
  });

  it('deletes category on confirmation', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1/channel-groups/group-1`, () =>
        HttpResponse.json({ message: 'Deleted' })
      )
    );

    render(<DeleteCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(useChannelStore.getState().channelGroups).toHaveLength(0);
  });

  it('shows error on API failure', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1/channel-groups/group-1`, () =>
        HttpResponse.json({ error: 'Forbidden' }, { status: 403 })
      )
    );

    render(<DeleteCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(screen.getByText('Forbidden')).toBeInTheDocument();
    });
  });

  it('shows generic error when API returns no error message', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1/channel-groups/group-1`, () =>
        HttpResponse.json({}, { status: 500 })
      )
    );

    render(<DeleteCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(screen.getByText('Failed to delete category')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<DeleteCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});
