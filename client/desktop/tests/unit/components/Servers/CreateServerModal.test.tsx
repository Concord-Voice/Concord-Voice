import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/serverStore';

// Mock apiFetch to avoid MSW timing issues in jsdom
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/apiClient';
import CreateServerModal from '@/renderer/components/Servers/CreateServerModal';

const mockedApiFetch = vi.mocked(apiFetch);

describe('CreateServerModal', () => {
  const mockOnClose = vi.fn();
  const mockOnSuccess = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <CreateServerModal isOpen={false} onClose={mockOnClose} onSuccess={mockOnSuccess} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders form when open', () => {
    render(<CreateServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('Create a Server')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('My Awesome Server')).toBeInTheDocument();
    expect(screen.getByText('Create Server')).toBeInTheDocument();
  });

  it('shows validation error for empty name', async () => {
    render(<CreateServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Create Server'));
    expect(screen.getByText('Server name is required')).toBeInTheDocument();
  });

  it('shows validation error for short name', () => {
    render(<CreateServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.change(screen.getByPlaceholderText('My Awesome Server'), {
      target: { value: 'ab' },
    });
    fireEvent.click(screen.getByText('Create Server'));
    expect(screen.getByText('Server name must be at least 3 characters')).toBeInTheDocument();
  });

  it('submits valid form and calls onSuccess', async () => {
    vi.useFakeTimers();

    mockedApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({
        server: {
          id: 'new-server',
          name: 'My Server',
          owner_id: 'user-1',
          created_at: '2025-01-01T00:00:00Z',
          updated_at: '2025-01-01T00:00:00Z',
        },
        role: 'owner',
      }),
    } as Response);

    render(<CreateServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('My Awesome Server'), {
      target: { value: 'My Server' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Server'));
    });

    // Flush microtasks
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(screen.getByText('Server created successfully!')).toBeInTheDocument();
    expect(useServerStore.getState().servers).toHaveLength(1);

    // #1647: the POST body must not carry the removed e2ee_default field.
    const [, requestInit] = mockedApiFetch.mock.calls[0];
    const sentBody = JSON.parse((requestInit?.body as string) ?? '{}');
    expect(sentBody).toEqual({ name: 'My Server' });
    expect(sentBody).not.toHaveProperty('e2ee_default');

    // The onSuccess is called after 800ms timeout
    await act(async () => {
      vi.advanceTimersByTime(800);
    });
    expect(mockOnSuccess).toHaveBeenCalled();

    vi.useRealTimers();
  });

  it('shows error on API failure', async () => {
    mockedApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Name taken' }),
    } as Response);

    render(<CreateServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('My Awesome Server'), {
      target: { value: 'My Server' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Server'));
    });

    await waitFor(() => {
      expect(screen.getByText('Name taken')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<CreateServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows character count', () => {
    render(<CreateServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('0/100 characters')).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText('My Awesome Server'), {
      target: { value: 'Test' },
    });
    expect(screen.getByText('4/100 characters')).toBeInTheDocument();
  });
});
