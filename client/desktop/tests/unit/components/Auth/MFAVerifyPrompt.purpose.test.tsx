import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';

// Mock apiFetch for the WebAuthn inline-verify begin call.
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

import MFAVerifyPrompt from '@/renderer/components/Auth/MFAVerifyPrompt';

// Reproduction: the WebAuthn inline-verify begin request must carry the
// purpose the caller is verifying for, so the server can bind the minted
// token to that one action. Today MFAVerifyPrompt.tsx sends
// `apiFetch(url, { method: 'POST' })` with no body at all — `purpose` is not
// a real prop yet, so vitest (no type-check) still runs this and the request
// body assertion fails against today's component.
describe('MFAVerifyPrompt purpose binding', () => {
  const onVerify = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('sends the purpose in the WebAuthn inline verify-inline/begin request body', async () => {
    // Begin never resolves the ceremony; we only need to inspect the request
    // apiFetch was called with.
    mockApiFetch.mockReturnValueOnce(new Promise(() => {}));

    render(
      <MFAVerifyPrompt
        methods={['webauthn']}
        onVerify={onVerify}
        purpose="mfa_settings.backup_email_set"
      />
    );
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalled();
    });

    const [url, options] = mockApiFetch.mock.calls[0];
    expect(url).toBe('/api/v1/mfa/webauthn/verify-inline/begin');
    expect(options?.body).toBe(JSON.stringify({ purpose: 'mfa_settings.backup_email_set' }));
  });
});
