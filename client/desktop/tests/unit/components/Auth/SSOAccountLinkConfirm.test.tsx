import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import SSOAccountLinkConfirm from '@/renderer/components/Auth/SSOAccountLinkConfirm';
import { useSSOStore } from '@/renderer/stores/ssoStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch (matches Register.test.tsx / SSOPassphraseSetup.test.tsx pattern)
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

beforeEach(() => {
  vi.clearAllMocks();
  resetAllStores();
  useSSOStore.getState().setState({
    phase: 'link_required',
    provider: 'google',
    ssoToken: 'tok-link',
    maskedEmail: 'a***@example.test',
  });
});

describe('SSOAccountLinkConfirm', () => {
  it('renders masked email + password input', () => {
    render(<SSOAccountLinkConfirm />);
    expect(screen.getByText(/a\*\*\*@example\.test/)).toBeInTheDocument();
    expect(screen.getByLabelText(/password/i)).toBeInTheDocument();
  });

  it('posts complete-link with password', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ access_token: 'linked-token' }),
      text: async () => JSON.stringify({ access_token: 'linked-token' }),
    });

    render(<SSOAccountLinkConfirm />);
    fireEvent.change(screen.getByLabelText(/password/i), {
      target: { value: 'CorrectPW!' }, // pragma: allowlist secret
    });
    fireEvent.click(screen.getByRole('button', { name: /link account/i }));

    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

    const [url, init] = mockFetch.mock.calls[0];
    expect(String(url)).toContain('/api/v1/auth/sso/google/complete-link');
    expect(init?.method).toBe('POST');

    const body = JSON.parse(init.body as string);
    expect(body.sso_token).toBe('tok-link');
    expect(body.password).toBe('CorrectPW!'); // pragma: allowlist secret

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('linked-token');
    });
    expect(useSSOStore.getState().state.phase).toBe('idle');
  });

  it('shows lockout message on 423', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 423,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ error_code: 'account_locked' }),
      text: async () => JSON.stringify({ error_code: 'account_locked' }),
    });

    render(<SSOAccountLinkConfirm />);
    fireEvent.change(screen.getByLabelText(/password/i), {
      target: { value: 'whatever' }, // pragma: allowlist secret
    });
    fireEvent.click(screen.getByRole('button', { name: /link account/i }));

    const lockoutMessage = await screen.findByText(/too many failed attempts/i);
    expect(lockoutMessage).toBeInTheDocument();
    // Lockout must NOT mint an access token — verifies the 423 path
    // does not accidentally fall through to the success-token handler.
    expect(useAuthStore.getState().accessToken).toBeNull();
  });
});
