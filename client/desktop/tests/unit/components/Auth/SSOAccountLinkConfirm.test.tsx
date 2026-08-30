import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import SSOAccountLinkConfirm from '@/renderer/components/Auth/SSOAccountLinkConfirm';
import { abandonSSOReservation } from '@/renderer/services/ssoService';
import { useSSOStore } from '@/renderer/stores/auth/ssoStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/services/apiClient', async (orig) => ({
  ...(await orig<typeof import('@/renderer/services/apiClient')>()),
  revokeAbortedSession: vi.fn().mockResolvedValue(undefined),
}));

// #2394: replace only abandonSSOReservation. completeSSOLink stays real so the
// existing tests keep asserting against the electron sso.completeLink bridge.
vi.mock('@/renderer/services/ssoService', async (orig) => ({
  ...(await orig<typeof import('@/renderer/services/ssoService')>()),
  abandonSSOReservation: vi.fn().mockResolvedValue(true),
}));

const storeRefreshToken = vi.fn().mockResolvedValue(41);
const clearTokensIfOwner = vi.fn().mockResolvedValue(true);
const originalElectron = globalThis.electron;

const linkedSessionResponse = {
  kind: 'tokens' as const,
  accessToken: 'linked-token',
  sessionId: 'linked-session',
  credentialOwner: 41,
};
const completeLink = vi.fn().mockResolvedValue(linkedSessionResponse);

beforeEach(() => {
  vi.clearAllMocks();
  resetAllStores();
  resetRuntimeServerBase();
  completeLink.mockResolvedValue(linkedSessionResponse);
  Object.defineProperty(globalThis, 'electron', {
    value: {
      ...originalElectron,
      storeRefreshToken,
      clearTokensIfOwner,
      sso: { ...originalElectron?.sso, completeLink },
    },
    writable: true,
  });
  useSSOStore.getState().setState({
    phase: 'link_required',
    provider: 'google',
    ssoToken: 'tok-link',
    maskedEmail: 'a***@example.test',
  });
});

afterEach(() => {
  resetRuntimeServerBase();
  Object.defineProperty(globalThis, 'electron', {
    value: originalElectron,
    writable: true,
  });
});

describe('SSOAccountLinkConfirm', () => {
  it('renders masked email + password input', () => {
    render(<SSOAccountLinkConfirm />);
    expect(screen.getByText(/a\*\*\*@example\.test/)).toBeInTheDocument();
    expect(screen.getByLabelText(/password/i)).toBeInTheDocument();
  });

  it('delegates complete-link to main without renderer refresh-token custody', async () => {
    let gateWasArmedAtAdmission = false;
    const unsubscribe = useAuthStore.subscribe((auth) => {
      if (auth.accessToken === 'linked-token') {
        const e2ee = useE2EEStore.getState();
        gateWasArmedAtAdmission =
          e2ee.needsSSOUnlock && e2ee.ssoCredentialOwner === linkedSessionResponse.credentialOwner;
      }
    });
    render(<SSOAccountLinkConfirm />);
    fireEvent.change(screen.getByLabelText(/password/i), {
      target: { value: 'CorrectPW!' }, // pragma: allowlist secret
    });
    fireEvent.click(screen.getByRole('button', { name: /link account/i }));

    await waitFor(() => expect(completeLink).toHaveBeenCalledTimes(1));
    expect(completeLink).toHaveBeenCalledWith('http://localhost:8080', {
      provider: 'google',
      ssoToken: 'tok-link',
      password: 'CorrectPW!', // pragma: allowlist secret
    });

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('linked-token');
    });
    unsubscribe();
    expect(gateWasArmedAtAdmission).toBe(true);
    expect(useAuthStore.getState().sessionId).toBe('linked-session');
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(useE2EEStore.getState()).toMatchObject({
      needsSSOUnlock: true,
      ssoCredentialOwner: 41,
    });
    expect(useSSOStore.getState().state.phase).toBe('idle');
  });

  it('rejects an in-flight response after an A → B → A server switch', async () => {
    let resolveResponse: (response: typeof linkedSessionResponse) => void = () => {};
    completeLink.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveResponse = resolve;
      })
    );

    render(<SSOAccountLinkConfirm />);
    fireEvent.change(screen.getByLabelText(/password/i), {
      target: { value: 'CorrectPW!' }, // pragma: allowlist secret
    });
    fireEvent.click(screen.getByRole('button', { name: /link account/i }));
    await waitFor(() => expect(completeLink).toHaveBeenCalledTimes(1));

    setRuntimeServerBase('https://server-b.example');
    setRuntimeServerBase('http://localhost:8080');
    resolveResponse(linkedSessionResponse);

    const { revokeAbortedSession } = await import('@/renderer/services/apiClient');
    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalledTimes(1));
    expect(revokeAbortedSession).toHaveBeenCalledWith({
      accessToken: 'linked-token',
      sessionId: 'linked-session',
      apiBase: 'http://localhost:8080',
    });
    expect(clearTokensIfOwner).toHaveBeenCalledWith(41);
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useSSOStore.getState().state.phase).toBe('link_required');
  });

  it('clears only its main-process owner when a successor wins during completion', async () => {
    completeLink.mockImplementationOnce(async () => {
      useAuthStore.getState().beginAuthLifecycle('successor-token', 'successor-session');
      return linkedSessionResponse;
    });

    render(<SSOAccountLinkConfirm />);
    fireEvent.change(screen.getByLabelText(/password/i), {
      target: { value: 'CorrectPW!' }, // pragma: allowlist secret
    });
    fireEvent.click(screen.getByRole('button', { name: /link account/i }));

    await waitFor(() => expect(clearTokensIfOwner).toHaveBeenCalledWith(41));
    expect(useAuthStore.getState().accessToken).toBe('successor-token');
    expect(useAuthStore.getState().sessionId).toBe('successor-session');
    expect(useSSOStore.getState().state.phase).toBe('link_required');
  });

  it('does not admit and re-enables retry when main rejects completion', async () => {
    completeLink.mockResolvedValueOnce({
      kind: 'error',
      status: 500,
      code: 'session_store_failed',
    });

    render(<SSOAccountLinkConfirm />);
    fireEvent.change(screen.getByLabelText(/password/i), {
      target: { value: 'CorrectPW!' }, // pragma: allowlist secret
    });
    fireEvent.click(screen.getByRole('button', { name: /link account/i }));

    expect(await screen.findByText(/wrong password/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /link account/i })).toBeEnabled();
    expect(useAuthStore.getState().accessToken).toBeNull();
  });

  it('shows lockout message on 423', async () => {
    completeLink.mockResolvedValueOnce({
      kind: 'error',
      status: 423,
      code: 'account_locked',
      body: { error_code: 'account_locked' },
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

  it('Cancel abandons the SSO reservation and returns the store to idle (#2394)', async () => {
    // Backing out of account linking abandons the SSO flow, so the orphaned
    // main-process reservation is retired eagerly rather than left to block a
    // later password registration's E2EE key staging.
    render(<SSOAccountLinkConfirm />);
    fireEvent.click(screen.getByRole('button', { name: /cancel/i }));

    await waitFor(() => expect(useSSOStore.getState().state.phase).toBe('idle'));
    expect(abandonSSOReservation).toHaveBeenCalledTimes(1);
    // Cancelling never touches credentials — it only releases the reservation.
    expect(clearTokensIfOwner).not.toHaveBeenCalled();
    expect(useAuthStore.getState().accessToken).toBeNull();
  });
});
