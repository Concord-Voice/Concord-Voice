import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import { mockServer } from '../../../mocks/fixtures';
import DeleteServerModal from '@/renderer/components/Servers/DeleteServerModal';

const API_BASE = 'http://localhost:8080';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const getConfirmBtn = () =>
  document.querySelector<HTMLButtonElement>('button.delete-server-confirm-btn')!;

describe('DeleteServerModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useServerStore.getState().addServer(mockServer);
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <DeleteServerModal isOpen={false} server={mockServer} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders warning message with server name', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    expect(screen.getByText(/cannot be undone/)).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Test Server')).toBeInTheDocument();
  });

  it('disables delete button until name is confirmed', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    expect(getConfirmBtn()).toBeDisabled();

    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    expect(getConfirmBtn()).not.toBeDisabled();
  });

  it('does not delete with wrong confirmation name', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Wrong Name' },
    });
    expect(getConfirmBtn()).toBeDisabled();
  });

  it('deletes server on confirmation', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1`, () =>
        HttpResponse.json({ message: 'Deleted' })
      )
    );

    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(useServerStore.getState().servers).toHaveLength(0);
  });

  it('shows error on API failure', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1`, () =>
        HttpResponse.json({ error: 'Forbidden' }, { status: 403 })
      )
    );

    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(screen.getByText('Forbidden')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});
