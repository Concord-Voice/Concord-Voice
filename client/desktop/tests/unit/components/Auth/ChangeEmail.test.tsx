import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { ChangeEmail } from '@/renderer/components/Auth/ChangeEmail';
import { usePendingRegistrationStore } from '@/renderer/stores/pendingRegistrationStore';
import { resetAllStores } from '../../../helpers/store-helpers';

const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

vi.mock('@/renderer/services/apiClient', () => ({
  API_BASE: 'http://localhost:8080',
}));

function seedStore() {
  usePendingRegistrationStore.getState().setPending({
    pending_id: 'test-pending-id',
    email: 'old@example.com',
    expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
    code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
  });
}

describe('ChangeEmail', () => {
  const mockOnDone = vi.fn();
  const mockOnCancel = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    seedStore();
  });

  it('renders the form', () => {
    render(<ChangeEmail onDone={mockOnDone} onCancel={mockOnCancel} />);
    expect(screen.getByText('Change email address')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send code to new email' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
  });

  it('calls onCancel when Cancel clicked', () => {
    render(<ChangeEmail onDone={mockOnDone} onCancel={mockOnCancel} />);
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(mockOnCancel).toHaveBeenCalledOnce();
  });

  it('on success: updates store.email and calls onDone', async () => {
    const newCodeExpiresAt = new Date(Date.now() + 2 * 60_000).toISOString();
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({ email: 'updated@example.com', code_expires_at: newCodeExpiresAt }),
    });

    render(<ChangeEmail onDone={mockOnDone} onCancel={mockOnCancel} />);
    fireEvent.change(screen.getByLabelText('New email'), {
      target: { value: 'updated@example.com' },
    });
    fireEvent.submit(
      screen.getByRole('button', { name: 'Send code to new email' }).closest('form')!
    );

    await waitFor(() => {
      expect(usePendingRegistrationStore.getState().email).toBe('updated@example.com');
      expect(mockOnDone).toHaveBeenCalledOnce();
    });
  });

  it('posts pending_id and new_email to change-email endpoint', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          email: 'new@example.com',
          code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
        }),
    });

    render(<ChangeEmail onDone={mockOnDone} onCancel={mockOnCancel} />);
    fireEvent.change(screen.getByLabelText('New email'), {
      target: { value: 'new@example.com' },
    });
    fireEvent.submit(
      screen.getByRole('button', { name: 'Send code to new email' }).closest('form')!
    );

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        'http://localhost:8080/api/v1/auth/register/change-email',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ pending_id: 'test-pending-id', new_email: 'new@example.com' }),
        })
      );
    });
  });

  it('shows error on email_already_registered', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 409,
      json: () => Promise.resolve({ code: 'email_already_registered' }),
    });

    render(<ChangeEmail onDone={mockOnDone} onCancel={mockOnCancel} />);
    fireEvent.change(screen.getByLabelText('New email'), {
      target: { value: 'taken@example.com' },
    });
    fireEvent.submit(
      screen.getByRole('button', { name: 'Send code to new email' }).closest('form')!
    );

    await waitFor(() => {
      expect(screen.getByText('An account with this email already exists.')).toBeInTheDocument();
    });
    expect(mockOnDone).not.toHaveBeenCalled();
  });

  it('shows generic error on 429', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 429,
      json: () => Promise.resolve({}),
    });

    render(<ChangeEmail onDone={mockOnDone} onCancel={mockOnCancel} />);
    fireEvent.change(screen.getByLabelText('New email'), { target: { value: 'x@example.com' } });
    fireEvent.submit(
      screen.getByRole('button', { name: 'Send code to new email' }).closest('form')!
    );

    await waitFor(() => {
      expect(screen.getByText('Too many email changes — wait 15 minutes.')).toBeInTheDocument();
    });
  });
});
