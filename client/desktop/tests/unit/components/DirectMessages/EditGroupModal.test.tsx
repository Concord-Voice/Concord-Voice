import { render, screen, fireEvent, act } from '../../../test-utils';
import { StrictMode } from 'react';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { deferred } from '../../../helpers/deferred';
import { vi } from 'vitest';

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
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
      <StrictMode>
        <EditGroupModal
          isOpen={true}
          onClose={mockOnClose}
          conversationId="group-1"
          currentName="Test Group"
        />
      </StrictMode>
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
      <StrictMode>
        <EditGroupModal
          isOpen={true}
          onClose={mockOnClose}
          conversationId="group-1"
          currentName="Test Group"
        />
      </StrictMode>
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

  it('focuses the group name field through the shared modal focus target', async () => {
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );

    await vi.waitFor(() =>
      expect(screen.getByPlaceholderText('Group Name (optional)')).toHaveFocus()
    );
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
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('save button calls API and updates store', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });

    const mockUpdateConversation = vi.fn();
    useDMStore.setState({ updateConversation: mockUpdateConversation });

    render(
      <StrictMode>
        <EditGroupModal
          isOpen={true}
          onClose={mockOnClose}
          conversationId="group-1"
          currentName="Test Group"
        />
      </StrictMode>
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

  it('resets the draft for a new conversation while an old save is held', async () => {
    const oldSave = deferred<Response>();
    mockApiFetch.mockReturnValueOnce(oldSave.promise);
    const mockUpdateConversation = vi.fn();
    useDMStore.setState({ updateConversation: mockUpdateConversation });
    const { rerender } = render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );

    fireEvent.change(screen.getByPlaceholderText('Group Name (optional)'), {
      target: { value: 'old-name' },
    });
    fireEvent.click(screen.getByText('Save'));
    await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));

    rerender(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-2"
        currentName="Test Group"
      />
    );
    await vi.waitFor(() =>
      expect(screen.getByPlaceholderText('Group Name (optional)')).toHaveValue('Test Group')
    );

    try {
      await act(async () => {
        oldSave.resolve({ ok: true, json: async () => ({}) } as Response);
        await oldSave.promise;
      });
    } finally {
      oldSave.resolve({ ok: true, json: async () => ({}) } as Response);
      await oldSave.promise;
    }

    expect(screen.getByPlaceholderText('Group Name (optional)')).toHaveValue('Test Group');
    expect(mockUpdateConversation).not.toHaveBeenCalled();
    expect(mockOnClose).not.toHaveBeenCalled();
    expect(screen.queryByText('Name too long')).not.toBeInTheDocument();
  });

  it('clears a pending save when auth lifecycle changes and ignores its response', async () => {
    const oldSave = deferred<Response>();
    mockApiFetch.mockReturnValueOnce(oldSave.promise);
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

    fireEvent.change(screen.getByPlaceholderText('Group Name (optional)'), {
      target: { value: 'old-name' },
    });
    fireEvent.click(screen.getByText('Save'));
    await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));

    act(() => {
      useAuthStore.getState().beginAuthLifecycle('successor-token', 'successor-session');
    });
    await vi.waitFor(() => {
      expect(screen.getByPlaceholderText('Group Name (optional)')).toHaveValue('Test Group');
      expect(screen.getByText('Save')).toBeEnabled();
    });

    try {
      await act(async () => {
        oldSave.resolve({
          ok: false,
          json: async () => ({ error: 'stale failure' }),
        } as Response);
        await oldSave.promise;
      });
    } finally {
      oldSave.resolve({ ok: true, json: async () => ({}) } as Response);
      await oldSave.promise;
    }

    expect(screen.getByPlaceholderText('Group Name (optional)')).toHaveValue('Test Group');
    expect(screen.getByText('Save')).toBeEnabled();
    expect(screen.queryByText('stale failure')).not.toBeInTheDocument();
    expect(mockUpdateConversation).not.toHaveBeenCalled();
    expect(mockOnClose).not.toHaveBeenCalled();
  });

  it('locks expiration controls while a group save is held', async () => {
    const save = deferred<Response>();
    useUserStore.getState().setUser({ id: 'user-1', username: 'alice' });
    useDMStore.getState().addConversation({
      id: 'group-1',
      isGroup: true,
      isPersonal: false,
      name: 'Test Group',
      participants: [{ userId: 'user-1', username: 'alice', role: 'admin' }],
      lastMessage: null,
      unreadCount: 0,
      createdAt: '2026-09-08T05:00:00Z',
      expirationPolicy: {
        windowSeconds: 86400,
        updatedAt: '2026-09-08T05:00:00Z',
        revision: 4,
        backfillPending: false,
      },
    });
    mockApiFetch.mockImplementation(async (url, init) => {
      if (init?.method === 'PATCH') return save.promise;
      if (String(url).endsWith('/api/v1/dm/conversations')) {
        return {
          ok: true,
          json: async () => ({
            conversations: [
              {
                id: 'group-1',
                is_group: true,
                name: 'Test Group',
                participants: [{ user_id: 'user-1', username: 'alice', role: 'admin' }],
                last_message: null,
                unread_count: 0,
                created_at: '2026-09-08T05:00:00Z',
                expiration_window_seconds: 86400,
                expiration_updated_at: '2026-09-08T05:00:00Z',
                expiration_revision: 4,
                expiration_backfill_pending: false,
              },
            ],
          }),
        } as Response;
      }
      return { ok: true, json: async () => ({}) } as Response;
    });
    render(
      <EditGroupModal
        isOpen={true}
        onClose={mockOnClose}
        conversationId="group-1"
        currentName="Test Group"
      />
    );
    await vi.waitFor(() =>
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'false'
      )
    );
    fireEvent.change(screen.getByPlaceholderText('Group Name (optional)'), {
      target: { value: 'Held Group' },
    });
    fireEvent.click(screen.getByText('Save'));
    await vi.waitFor(() =>
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'true'
      )
    );
    save.resolve({ ok: true, json: async () => ({}) } as Response);
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
