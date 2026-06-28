import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

import EmailSmsSetup from '@/renderer/components/Settings/EmailSmsSetup';

describe('EmailSmsSetup', () => {
  const onComplete = vi.fn();
  const onCancel = vi.fn();

  const mockSetupSuccess = () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        message: 'Verification codes sent',
        methods: ['email'],
        expires_in: '10 minutes',
      }),
    });
  };

  const startEmailSetup = async () => {
    mockSetupSuccess();
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Send Code'));
    await waitFor(() => expect(screen.getByLabelText('Email code')).toBeInTheDocument());
  };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders email MFA setup without dev-mode SMS copy', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);

    expect(screen.getByText('Set Up Email MFA')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Your password')).toBeInTheDocument();
    expect(screen.getByText('Send Code')).toBeDisabled();
    expect(screen.queryByText(/DEV MODE/)).not.toBeInTheDocument();
    expect(screen.queryByText(/SMS code/i)).not.toBeInTheDocument();
  });

  it('does not expose dev-mode SMS setup in production', async () => {
    mockSetupSuccess();

    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);

    expect(screen.queryByText(/DEV MODE/)).not.toBeInTheDocument();

    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Send Code'));

    await waitFor(() => {
      const body = JSON.parse((mockApiFetch.mock.calls[0][1] as { body: string }).body);
      expect(body.methods).toEqual(['email']);
    });
  });

  it('requires password and calls onCancel', () => {
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);

    expect(screen.getByText('Send Code')).toBeDisabled();
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    expect(screen.getByText('Send Code')).not.toBeDisabled();

    fireEvent.click(screen.getByText('Cancel'));
    expect(onCancel).toHaveBeenCalled();
  });

  it('includes mfa_code when mfaActive', async () => {
    mockSetupSuccess();

    render(<EmailSmsSetup mfaActive={true} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.change(screen.getByPlaceholderText('MFA code from your authenticator'), {
      target: { value: '654321' },
    });
    fireEvent.click(screen.getByText('Send Code'));

    await waitFor(() => {
      const body = JSON.parse((mockApiFetch.mock.calls[0][1] as { body: string }).body);
      expect(body.mfa_code).toBe('654321');
      expect(body.methods).toEqual(['email']);
    });
  });

  it('shows loading while setup is pending', async () => {
    mockApiFetch.mockReturnValueOnce(new Promise(() => {}));
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Send Code'));

    await waitFor(() => expect(screen.getByText('Sending...')).toBeInTheDocument());
  });

  it('shows setup errors', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Incorrect password' }),
    });
    render(<EmailSmsSetup mfaActive={false} onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'wrong' },
    });
    fireEvent.click(screen.getByText('Send Code'));

    await waitFor(() => expect(screen.getByText('Incorrect password')).toBeInTheDocument());
  });

  it('shows only email verification after setup', async () => {
    await startEmailSetup();

    expect(screen.getByPlaceholderText('6-digit email code')).toBeInTheDocument();
    expect(screen.queryByPlaceholderText('6-digit SMS code')).not.toBeInTheDocument();
    expect(screen.getByText('Verify & Activate')).toBeDisabled();
  });

  it('sends verify request with the email code only', async () => {
    await startEmailSetup();

    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '111111' },
    });
    mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });
    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => {
      const verifyCall = mockApiFetch.mock.calls[1];
      expect(verifyCall[0]).toBe('/api/v1/mfa/email-sms/verify');
      const body = JSON.parse((verifyCall[1] as { body: string }).body);
      expect(body.codes).toEqual({ email: '111111' });
    });
  });

  it('shows verification loading while pending', async () => {
    await startEmailSetup();
    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '000000' },
    });

    mockApiFetch.mockReturnValueOnce(new Promise(() => {}));
    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => expect(screen.getByText('Verifying...')).toBeInTheDocument());
  });

  it('shows verification errors', async () => {
    await startEmailSetup();
    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '000000' },
    });
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Invalid code' }),
    });
    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => expect(screen.getByText('Invalid code')).toBeInTheDocument());
  });

  it('shows done step and calls onComplete', async () => {
    await startEmailSetup();
    fireEvent.change(screen.getByPlaceholderText('6-digit email code'), {
      target: { value: '111111' },
    });
    mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });
    fireEvent.click(screen.getByText('Verify & Activate'));

    await waitFor(() => expect(screen.getByText('Email MFA Activated!')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Done'));
    expect(onComplete).toHaveBeenCalled();
  });
});
