import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import LinkedAccountsList from '@/renderer/components/Settings/LinkedAccountsList';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch (matches SSOAccountLinkConfirm.test.tsx / SSOPassphraseSetup.test.tsx pattern)
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

beforeEach(() => {
  vi.clearAllMocks();
  resetAllStores();
  // Auth context — apiFetch attaches Authorization header from this token
  useAuthStore.getState().setAccessToken('test-token-123');
});

describe('LinkedAccountsList', () => {
  it('renders provider rows with last_used_at', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({
        identities: [
          {
            provider: 'google',
            provider_email: 'me@example.test',
            is_relay_email: false,
            linked_at: '2026-01-01T00:00:00Z',
            last_used_at: '2026-04-26T00:00:00Z',
          },
        ],
      }),
      text: async () =>
        JSON.stringify({
          identities: [
            {
              provider: 'google',
              provider_email: 'me@example.test',
              is_relay_email: false,
              linked_at: '2026-01-01T00:00:00Z',
              last_used_at: '2026-04-26T00:00:00Z',
            },
          ],
        }),
    });

    render(<LinkedAccountsList />);
    await screen.findByText(/google/i);
    expect(screen.getByText(/me@example\.test/)).toBeInTheDocument();
    expect(screen.getByText(/last used/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /unlink/i })).toBeInTheDocument();
  });

  it('renders empty state when no identities are linked', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ identities: [] }),
      text: async () => JSON.stringify({ identities: [] }),
    });

    render(<LinkedAccountsList />);
    await screen.findByText(/no linked accounts/i);
    // The unlink button must NOT be present in the empty state.
    expect(screen.queryByRole('button', { name: /unlink/i })).not.toBeInTheDocument();
  });

  it('renders Apple privacy-relay placeholder instead of provider_email', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({
        identities: [
          {
            provider: 'apple',
            provider_email: 'abc123@privaterelay.appleid.com',
            is_relay_email: true,
            linked_at: '2026-04-01T00:00:00Z',
            last_used_at: null,
          },
        ],
      }),
      text: async () =>
        JSON.stringify({
          identities: [
            {
              provider: 'apple',
              provider_email: 'abc123@privaterelay.appleid.com',
              is_relay_email: true,
              linked_at: '2026-04-01T00:00:00Z',
              last_used_at: null,
            },
          ],
        }),
    });

    render(<LinkedAccountsList />);
    await screen.findByText(/Hidden via Apple Privacy/);
    // The raw relay address must NOT be rendered.
    expect(screen.queryByText(/privaterelay\.appleid\.com/)).not.toBeInTheDocument();
    // last_used_at: null → "Never used" is the canonical never-used label.
    expect(screen.getByText(/never used/i)).toBeInTheDocument();
  });

  it('shows a generic load-failed error when the list endpoint returns non-2xx', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ error_code: 'db_unavailable' }),
      text: async () => JSON.stringify({ error_code: 'db_unavailable' }),
    });

    render(<LinkedAccountsList />);
    await screen.findByText(/failed to load linked accounts/i);
    // Even on load failure, the empty-state message renders so the user
    // is not stuck on the loading spinner.
    expect(screen.getByText(/no linked accounts/i)).toBeInTheDocument();
  });

  it('successfully unlinks a provider and refreshes the list', async () => {
    // Initial load: one identity present.
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({
        identities: [
          {
            provider: 'google',
            provider_email: 'me@example.test',
            is_relay_email: false,
            linked_at: '2026-01-01T00:00:00Z',
            last_used_at: '2026-04-26T00:00:00Z',
          },
        ],
      }),
      text: async () =>
        JSON.stringify({
          identities: [
            {
              provider: 'google',
              provider_email: 'me@example.test',
              is_relay_email: false,
              linked_at: '2026-01-01T00:00:00Z',
              last_used_at: '2026-04-26T00:00:00Z',
            },
          ],
        }),
    });
    // DELETE succeeds.
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 204,
      headers: new Headers(),
      json: async () => ({}),
      text: async () => '',
    });
    // Refetch after unlink: empty list.
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ identities: [] }),
      text: async () => JSON.stringify({ identities: [] }),
    });

    render(<LinkedAccountsList />);
    const unlinkBtn = await screen.findByRole('button', { name: /unlink/i });
    fireEvent.click(unlinkBtn);

    // After the refetch, the empty state replaces the row.
    await screen.findByText(/no linked accounts/i);
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(3));
  });

  it('shows a generic unlink-failed error for non-lockout errors', async () => {
    // Initial load: one identity.
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({
        identities: [
          {
            provider: 'google',
            provider_email: 'me@example.test',
            is_relay_email: false,
            linked_at: '2026-01-01T00:00:00Z',
            last_used_at: null,
          },
        ],
      }),
      text: async () =>
        JSON.stringify({
          identities: [
            {
              provider: 'google',
              provider_email: 'me@example.test',
              is_relay_email: false,
              linked_at: '2026-01-01T00:00:00Z',
              last_used_at: null,
            },
          ],
        }),
    });
    // DELETE fails with a non-lockout server error.
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ error_code: 'db_unavailable' }),
      text: async () => JSON.stringify({ error_code: 'db_unavailable' }),
    });

    render(<LinkedAccountsList />);
    const unlinkBtn = await screen.findByRole('button', { name: /unlink/i });
    fireEvent.click(unlinkBtn);

    // Non-lockout errors fall through to the generic message.
    await screen.findByText(/failed to unlink account/i);
    // Lock-out copy must NOT appear for generic failures.
    expect(screen.queryByText(/lock you out/i)).not.toBeInTheDocument();
  });

  it('blocks unlink when it would lock the user out', async () => {
    // First call: list identities
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({
        identities: [
          {
            provider: 'google',
            provider_email: 'me@example.test',
            is_relay_email: false,
            linked_at: '2026-01-01T00:00:00Z',
            last_used_at: null,
          },
        ],
      }),
      text: async () =>
        JSON.stringify({
          identities: [
            {
              provider: 'google',
              provider_email: 'me@example.test',
              is_relay_email: false,
              linked_at: '2026-01-01T00:00:00Z',
              last_used_at: null,
            },
          ],
        }),
    });
    // Second call: unlink — server refuses with would_lock_out
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 400,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({
        error_code: 'would_lock_out',
        detail: 'Set a passphrase first or link another provider before unlinking this one.',
      }),
      text: async () =>
        JSON.stringify({
          error_code: 'would_lock_out',
          detail: 'Set a passphrase first or link another provider before unlinking this one.',
        }),
    });

    render(<LinkedAccountsList />);
    const unlinkBtn = await screen.findByRole('button', { name: /unlink/i });
    fireEvent.click(unlinkBtn);

    await screen.findByText(/lock you out/i);
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
    // Confirm second call was the DELETE
    const [, deleteInit] = mockFetch.mock.calls[1];
    expect(deleteInit?.method).toBe('DELETE');
    expect(String(mockFetch.mock.calls[1][0])).toContain('/api/v1/users/me/sso-identities/google');
  });
});
