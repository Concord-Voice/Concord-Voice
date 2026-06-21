import { render, screen, fireEvent } from '../../../test-utils';
import MFAMethodPicker, {
  getAvailableCategories,
  getDefaultMethod,
} from '@/renderer/components/Auth/MFAMethodPicker';

describe('getAvailableCategories', () => {
  it('maps webauthn to webauthn category', () => {
    expect(getAvailableCategories(['webauthn'])).toEqual(['webauthn', 'backup']);
  });

  it('maps totp to totp category', () => {
    expect(getAvailableCategories(['totp'])).toEqual(['totp', 'backup']);
  });

  it('maps email and sms to email-sms category', () => {
    const result = getAvailableCategories(['email', 'sms']);
    expect(result).toContain('email-sms');
    expect(result).toContain('backup');
  });

  it('deduplicates email-sms from email and sms', () => {
    const result = getAvailableCategories(['email', 'sms']);
    expect(result.filter((c) => c === 'email-sms')).toHaveLength(1);
  });

  it('returns categories in priority order (webauthn > totp > email-sms > backup)', () => {
    const result = getAvailableCategories(['email', 'totp', 'webauthn']);
    expect(result).toEqual(['webauthn', 'totp', 'email-sms', 'backup']);
  });

  it('always includes backup when at least one MFA method exists', () => {
    expect(getAvailableCategories(['totp'])).toContain('backup');
  });

  it('returns empty array when no methods provided', () => {
    expect(getAvailableCategories([])).toEqual([]);
  });

  it('ignores unknown method strings', () => {
    expect(getAvailableCategories(['unknown', 'magic'])).toEqual([]);
  });

  it('excludes methods in excludeMethods', () => {
    const result = getAvailableCategories(['totp', 'email'], ['email']);
    expect(result).toEqual(['totp', 'backup']);
    expect(result).not.toContain('email-sms');
  });

  it('excludes multiple methods', () => {
    const result = getAvailableCategories(['totp', 'webauthn', 'email'], ['email', 'webauthn']);
    expect(result).toEqual(['totp', 'backup']);
  });
});

describe('getDefaultMethod', () => {
  it('returns highest priority method (webauthn)', () => {
    expect(getDefaultMethod(['totp', 'webauthn'])).toBe('webauthn');
  });

  it('returns totp when webauthn is not available', () => {
    expect(getDefaultMethod(['totp', 'email'])).toBe('totp');
  });

  it('returns email-sms when only email/sms available', () => {
    expect(getDefaultMethod(['email'])).toBe('email-sms');
  });

  it('falls back to totp when no methods available', () => {
    expect(getDefaultMethod([])).toBe('totp');
  });

  it('respects excludeMethods', () => {
    expect(getDefaultMethod(['webauthn', 'totp'], ['webauthn'])).toBe('totp');
  });
});

describe('MFAMethodPicker', () => {
  const onSelect = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders title', () => {
    render(<MFAMethodPicker methods={['totp']} currentMethod="totp" onSelect={onSelect} />);
    expect(screen.getByText('Choose verification method')).toBeInTheDocument();
  });

  it('renders available method options', () => {
    render(
      <MFAMethodPicker methods={['totp', 'webauthn']} currentMethod="totp" onSelect={onSelect} />
    );
    expect(screen.getByText('Authenticator App')).toBeInTheDocument();
    expect(screen.getByText('Security Key / Biometrics')).toBeInTheDocument();
    expect(screen.getByText('Backup Code')).toBeInTheDocument();
  });

  it('does not render unavailable methods', () => {
    render(<MFAMethodPicker methods={['totp']} currentMethod="totp" onSelect={onSelect} />);
    expect(screen.queryByText('Security Key / Biometrics')).not.toBeInTheDocument();
    expect(screen.queryByText('Email / SMS Code')).not.toBeInTheDocument();
  });

  it('highlights current method with active class', () => {
    render(<MFAMethodPicker methods={['totp']} currentMethod="totp" onSelect={onSelect} />);
    const totpBtn = screen.getByText('Authenticator App').closest('button')!;
    expect(totpBtn).toHaveClass('mfa-method-picker-active');
  });

  it('calls onSelect when a method is clicked', () => {
    render(
      <MFAMethodPicker methods={['totp', 'webauthn']} currentMethod="totp" onSelect={onSelect} />
    );
    fireEvent.click(screen.getByText('Security Key / Biometrics'));
    expect(onSelect).toHaveBeenCalledWith('webauthn');
  });

  it('renders cancel button when onCancel is provided', () => {
    const onCancel = vi.fn();
    render(
      <MFAMethodPicker
        methods={['totp']}
        currentMethod="totp"
        onSelect={onSelect}
        onCancel={onCancel}
      />
    );
    const cancelBtn = screen.getByText('Cancel');
    fireEvent.click(cancelBtn);
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it('does not render cancel button when onCancel is not provided', () => {
    render(<MFAMethodPicker methods={['totp']} currentMethod="totp" onSelect={onSelect} />);
    expect(screen.queryByText('Cancel')).not.toBeInTheDocument();
  });

  it('respects excludeMethods', () => {
    render(
      <MFAMethodPicker
        methods={['totp', 'email']}
        currentMethod="totp"
        onSelect={onSelect}
        excludeMethods={['email']}
      />
    );
    expect(screen.queryByText('Email / SMS Code')).not.toBeInTheDocument();
  });
});
