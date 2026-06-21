import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import { mockServer, mockUser } from '../../../mocks/fixtures';
import LeaveServerModal from '@/renderer/components/Servers/LeaveServerModal';

const API_BASE = 'http://localhost:8080';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const getConfirmBtn = () =>
  document.querySelector<HTMLButtonElement>('button.delete-server-confirm-btn')!;

describe('LeaveServerModal', () => {
  const memberServer = { ...mockServer, role: 'member' as const };
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: mockUser });
    useServerStore.getState().addServer(memberServer);
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <LeaveServerModal isOpen={false} server={memberServer} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders warning about leaving', () => {
    render(<LeaveServerModal isOpen={true} server={memberServer} onClose={mockOnClose} />);
    expect(screen.getByText(/Test Server/)).toBeInTheDocument();
    expect(screen.getByText(/lose access/)).toBeInTheDocument();
  });

  it('leaves server on confirmation', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1/members/user-1`, () =>
        HttpResponse.json({ message: 'Left' })
      )
    );

    render(<LeaveServerModal isOpen={true} server={memberServer} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(useServerStore.getState().servers).toHaveLength(0);
  });

  it('shows error on API failure', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1/members/user-1`, () =>
        HttpResponse.json({ error: 'Cannot leave' }, { status: 400 })
      )
    );

    render(<LeaveServerModal isOpen={true} server={memberServer} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(screen.getByText('Cannot leave')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<LeaveServerModal isOpen={true} server={memberServer} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});
