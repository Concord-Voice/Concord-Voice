import { render, screen, fireEvent } from '../../../test-utils';
import { useDMStore } from '@/renderer/stores/dmStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { vi } from 'vitest';

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

import EditGroupModal from '@/renderer/components/DirectMessages/EditGroupModal';

describe('EditGroupModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });
  });

  it('renders when open', () => {
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );
    expect(screen.getByText('Edit Group')).toBeInTheDocument();
  });

  it('does not render when closed', () => {
    const { container } = render(
      <EditGroupModal
        isOpen={false}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );
    expect(container.innerHTML).toBe('');
  });

  it('pre-fills current name', () => {
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );
    const input = screen.getByPlaceholderText('Group Name (optional)') as HTMLInputElement;
    expect(input.value).toBe('Test Group');
  });

  it('pre-fills empty string when currentName is null', () => {
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName={null}
      />
    );
    const input = screen.getByPlaceholderText('Group Name (optional)') as HTMLInputElement;
    expect(input.value).toBe('');
  });

  it('calls onClose on cancel', () => {
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onClose when close button clicked', () => {
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );
    const closeBtn = document.querySelector('.create-group-close-btn');
    fireEvent.click(closeBtn!);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('save button calls API and updates store', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });

    const mockUpdateConversation = vi.fn();
    useDMStore.setState({ updateConversation: mockUpdateConversation });

    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );

    // Change name
    const input = screen.getByPlaceholderText('Group Name (optional)');
    fireEvent.change(input, { target: { value: 'New Name' } });

    // Click save
    fireEvent.click(screen.getByText('Save'));

    await vi.waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/dm/conversations/group-1',
        expect.objectContaining({
          method: 'PATCH',
          body: JSON.stringify({ name: 'New Name' }),
        })
      );
      expect(mockUpdateConversation).toHaveBeenCalledWith('group-1', { name: 'New Name' });
      expect(mockOnClose).toHaveBeenCalled();
    });
  });

  it('shows error on API failure', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      json: () => Promise.resolve({ error: 'Name too long' }),
    });

    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );

    fireEvent.click(screen.getByText('Save'));

    await vi.waitFor(() => {
      expect(screen.getByText('Name too long')).toBeInTheDocument();
    });
  });

  it('shows Group Name label', () => {
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );
    expect(screen.getByText('Group Name')).toBeInTheDocument();
  });
});
