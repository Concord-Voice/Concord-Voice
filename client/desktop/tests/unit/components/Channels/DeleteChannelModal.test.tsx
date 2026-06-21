import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import { mockChannel } from '../../../mocks/fixtures';
import DeleteChannelModal from '@/renderer/components/Channels/DeleteChannelModal';

const API_BASE = 'http://localhost:8080';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const getConfirmBtn = () =>
  document.querySelector<HTMLButtonElement>('button.delete-server-confirm-btn')!;

describe('DeleteChannelModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useChannelStore.getState().addChannel(mockChannel);
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <DeleteChannelModal isOpen={false} channel={mockChannel} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders warning with channel name', () => {
    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);
    expect(screen.getByText(/#general/)).toBeInTheDocument();
    expect(screen.getByText(/cannot be undone/)).toBeInTheDocument();
  });

  it('deletes channel on confirmation', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/channels/channel-1`, () =>
        HttpResponse.json({ message: 'Deleted' })
      )
    );

    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(useChannelStore.getState().channels).toHaveLength(0);
  });

  it('shows error on API failure', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/channels/channel-1`, () =>
        HttpResponse.json({ error: 'Forbidden' }, { status: 403 })
      )
    );

    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(screen.getByText('Forbidden')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});
