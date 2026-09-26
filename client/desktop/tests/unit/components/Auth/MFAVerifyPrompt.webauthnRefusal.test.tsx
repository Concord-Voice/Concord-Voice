import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';

// Mock apiFetch for the WebAuthn inline-verify begin/finish calls.
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

import MFAVerifyPrompt from '@/renderer/components/Auth/MFAVerifyPrompt';

// Regression for the #3467 review: in security-key mode the prompt never
// rendered its `error` prop. Every parent routes a code refusal ONLY to the
// prompt (MFASetup's ErrorBanner returns null for errorField 'mfa'; the action
// modal's banner returns null for mfa refusals), so a refused Confirm after a
// key ceremony was announced nowhere — and a refusal arriving while the prompt
// said "Security key verified" left that claim standing over a token the
// server may already have spent.
//
// Oracle: in security-key mode a refusal passed as `error` is announced as an
// alert, and one arriving after the key answered withdraws "Security key
// verified" and offers the key again.

const REFUSAL = 'That code is not correct, or it has expired. Try the next one.';

function stubCeremony(token: string) {
  mockApiFetch
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: { challenge: 'dGVzdC1jaGFsbGVuZ2U', rpId: 'localhost', allowCredentials: [] },
        challengeToken: 'test-challenge-token',
      }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ mfa_token: token }) });
  Object.defineProperty(navigator, 'credentials', {
    value: {
      get: vi.fn().mockResolvedValue({
        id: 'credential-id',
        rawId: new Uint8Array([1, 2, 3]).buffer,
        type: 'public-key',
        response: {
          authenticatorData: new Uint8Array([10, 20]).buffer,
          clientDataJSON: new Uint8Array([30, 40]).buffer,
          signature: new Uint8Array([50, 60]).buffer,
          userHandle: new Uint8Array([70, 80]).buffer,
        },
      }),
      create: vi.fn(),
    },
    writable: true,
    configurable: true,
  });
}

describe('MFAVerifyPrompt security-key refusal', () => {
  const onVerify = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // The parents remount the prompt after a refused Confirm, so this is the
  // state a user lands in: the refusal must be visible beside the key button.
  it('announces a refusal passed as error while offering the key', () => {
    render(
      <MFAVerifyPrompt
        purpose="sessions.revocation_mode_set"
        methods={['webauthn']}
        onVerify={onVerify}
        error={REFUSAL}
      />
    );

    expect(screen.getByRole('alert')).toHaveTextContent(REFUSAL);
    expect(screen.getByRole('button', { name: 'Verify with security key' })).toBeInTheDocument();
  });

  it('withdraws "Security key verified" when a refusal arrives after the key answered', async () => {
    stubCeremony('first-token');
    const { rerender } = render(
      <MFAVerifyPrompt
        purpose="sessions.revocation_mode_set"
        methods={['webauthn']}
        onVerify={onVerify}
      />
    );
    fireEvent.click(screen.getByText('Verify with security key'));
    expect(await screen.findByText('Security key verified')).toBeInTheDocument();

    rerender(
      <MFAVerifyPrompt
        purpose="sessions.revocation_mode_set"
        methods={['webauthn']}
        onVerify={onVerify}
        error={REFUSAL}
      />
    );

    expect(screen.queryByText('Security key verified')).not.toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(REFUSAL);
    expect(screen.getByRole('button', { name: 'Verify with security key' })).toBeInTheDocument();
  });

  // Guard, green before and after the fix: the parents keep a refusal's text
  // until the next submission, so it is still set when the user runs the key
  // again. That re-run must read as verified, not be withdrawn by a refusal
  // that predates it.
  it('reports a re-run key as verified even though the earlier refusal is still set', async () => {
    stubCeremony('second-token');
    render(
      <MFAVerifyPrompt
        purpose="sessions.revocation_mode_set"
        methods={['webauthn']}
        onVerify={onVerify}
        error={REFUSAL}
      />
    );
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => expect(onVerify).toHaveBeenCalledWith('second-token'));
    expect(await screen.findByRole('status')).toHaveTextContent('Security key verified');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
