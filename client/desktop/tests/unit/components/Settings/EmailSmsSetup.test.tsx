import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

import EmailSmsSetup from '@/renderer/components/Settings/EmailSmsSetup';

describe('EmailSmsSetup', () => {
  const onComplete = vi.fn();
  const onCancel = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // ── Initial Rendering (Password Step) ──────────────────────────────────

  it('renders setup wizard title', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Set Up Email / SMS MFA')).toBeInTheDocument();
  });

  it('renders DEV MODE banner', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText(/DEV MODE/)).toBeInTheDocument();
  });

  it('renders password input', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByPlaceholderText('Your password')).toBeInTheDocument();
  });

  it('renders Generate Codes and Cancel buttons', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Generate Codes')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  it('disables Generate Codes when password is empty', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Generate Codes')).toBeDisabled();
  });

  it('enables Generate Codes when password is entered', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    expect(screen.getByText('Generate Codes')).not.toBeDisabled();
  });

  it('calls onCancel when Cancel is clicked', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(onCancel).toHaveBeenCalled();
  });

  // ── MFA Active State ───────────────────────────────────────────────────

  it('shows MFA code input when mfaActive is true', () => {
    render(<EmailSmsSetup mfaActive={true} onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByPlaceholderText('MFA code from your authenticator')).toBeInTheDocument();
  });

  it('hides MFA code input when mfaActive is false', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    expect(
      screen.queryByPlaceholderText('MFA code from your authenticator')
    ).not.toBeInTheDocument();
  });

  it('disables Generate Codes when mfaActive and no MFA code', () => {
    render(<EmailSmsSetup mfaActive={true} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    expect(screen.getByText('Generate Codes')).toBeDisabled();
  });

  it('enables Generate Codes when mfaActive and both fields filled', () => {
    render(<EmailSmsSetup mfaActive={true} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.change(screen.getByPlaceholderText('MFA code from your authenticator'), {
      target: { value: '123456' },
    });
    expect(screen.getByText('Generate Codes')).not.toBeDisabled();
  });

  // ── Setup API Call ─────────────────────────────────────────────────────

  it('sends setup request with password and methods', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/mfa/email-sms/setup',
        expect.objectContaining({
          method: 'POST',
        })
      );
      const body = JSON.parse((mockApiFetch.mock.calls[0][1] as { body: string }).body);
      expect(body.password).toBe('mypassword');
      expect(body.methods).toEqual(['email', 'sms']);
    });
  });

  it('includes mfa_code when mfaActive', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={true} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.change(screen.getByPlaceholderText('MFA code from your authenticator'), {
      target: { value: '654321' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      const body = JSON.parse((mockApiFetch.mock.calls[0][1] as { body: string }).body);
      expect(body.mfa_code).toBe('654321');
    });
  });

  it('shows loading state during setup', async () => {
    mockApiFetch.mockReturnValue(new Promise(() => {}));

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      expect(screen.getByText('Generating...')).toBeInTheDocument();
    });
  });

  it('shows error on setup failure', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Incorrect password' }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'wrong' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      expect(screen.getByText('Incorrect password')).toBeInTheDocument();
    });
  });

  // ── Codes Step ─────────────────────────────────────────────────────────

  it('displays dev codes after successful setup', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      expect(screen.getByText('111111')).toBeInTheDocument();
      expect(screen.getByText('222222')).toBeInTheDocument();
    });
  });

  it('shows Email code and SMS code labels', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      expect(screen.getByText('Email code:')).toBeInTheDocument();
      expect(screen.getByText('SMS code:')).toBeInTheDocument();
    });
  });

  it('shows Enter Codes button on codes step', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      expect(screen.getByText('Enter Codes')).toBeInTheDocument();
    });
  });

  // ── Verify Step ────────────────────────────────────────────────────────

  it('transitions to verify step when Enter Codes is clicked', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => {
      expect(screen.getByText('Enter Codes')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('Enter Codes'));

    expect(screen.getByLabelText('Email code')).toBeInTheDocument();
    expect(screen.getByLabelText('SMS code')).toBeInTheDocument();
  });

  it('shows email and SMS code input fields on verify step', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    expect(screen.getByPlaceholderText('6-digit email code')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('6-digit SMS code')).toBeInTheDocument();
  });

  it('disables Verify & Activate when both codes are empty', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    expect(screen.getByText('Verify & Activate')).toBeDisabled();
  });

  it('enables Verify & Activate when at least one code is entered', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '111111' },
    });
    expect(screen.getByText('Verify & Activate')).not.toBeDisabled();
  });

  it('sends verify request with entered codes', async () => {
    // Setup call
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '111111' },
    });
    fireEvent.change(screen.getByPlaceholderText('6-digit SMS code'), {
      target: { value: '222222' },
    });

    // Verify call
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => {
      const verifyCall = mockApiFetch.mock.calls[1];
      expect(verifyCall[0]).toBe('/api/v1/mfa/email-sms/verify');
      const body = JSON.parse((verifyCall[1] as { body: string }).body);
      expect(body.codes.email).toBe('111111');
      expect(body.codes.sms).toBe('222222');
    });
  });

  it('shows loading state during verification', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '111111' },
    });

    mockApiFetch.mockReturnValue(new Promise(() => {}));
    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => {
      expect(screen.getByText('Verifying...')).toBeInTheDocument();
    });
  });

  it('shows error on verification failure', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '000000' },
    });

    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Invalid code' }),
    });

    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => {
      expect(screen.getByText('Invalid code')).toBeInTheDocument();
    });
  });

  it('navigates back to codes step from verify step', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111', sms: '222222' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    fireEvent.click(screen.getByText('Back'));
    expect(screen.getByText('111111')).toBeInTheDocument();
  });

  // ── Done Step ──────────────────────────────────────────────────────────

  it('shows done step after successful verification', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '111111' },
    });

    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => {
      expect(screen.getByText('Email / SMS MFA Activated!')).toBeInTheDocument();
    });
  });

  it('calls onComplete when Done is clicked', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        dev_codes: { email: '111111' },
      }),
    });

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Generate Codes'));

    await waitFor(() => expect(screen.getByText('Enter Codes')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Enter Codes'));

    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '111111' },
    });

    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => expect(screen.getByText('Done')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Done'));
    expect(onComplete).toHaveBeenCalled();
  });
});
