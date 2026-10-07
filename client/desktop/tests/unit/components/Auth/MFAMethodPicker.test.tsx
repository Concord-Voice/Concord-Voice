import type { ReactElement } from 'react';
import { render, screen, fireEvent } from '../../../test-utils';
import MFAMethodPicker, {
  getAvailableCategories,
  getDefaultMethod,
} from '@/renderer/components/Auth/MFAMethodPicker';
import KeyIcon from '@/renderer/components/Auth/icons/KeyIcon';
import PhoneIcon from '@/renderer/components/Auth/icons/PhoneIcon';

describe('getAvailableCategories', () => {
  it('maps webauthn to webauthn category', () => {
    expect(getAvailableCategories(['webauthn'])).toEqual(['webauthn']);
  });

  it('maps totp to totp category', () => {
    expect(getAvailableCategories(['totp'])).toEqual(['totp', 'backup']);
  });

  it('maps email and sms to email-sms category', () => {
    const result = getAvailableCategories(['email', 'sms']);
    expect(result).toContain('email-sms');
  });

  it('deduplicates email-sms from email and sms', () => {
    const result = getAvailableCategories(['email', 'sms']);
    expect(result.filter((c) => c === 'email-sms')).toHaveLength(1);
  });

  it('returns categories in priority order (webauthn > totp > email-sms > backup)', () => {
    const result = getAvailableCategories(['email', 'totp', 'webauthn']);
    expect(result).toEqual(['webauthn', 'totp', 'email-sms', 'backup']);
  });

  // Backup codes belong to the authenticator app (picker spec C16, C50).
  it('offers backup codes only when totp is listed', () => {
    expect(getAvailableCategories(['totp'])).toContain('backup');
    expect(getAvailableCategories(['webauthn'])).not.toContain('backup');
    expect(getAvailableCategories(['email', 'sms'])).not.toContain('backup');
    expect(getAvailableCategories(['webauthn', 'email'])).toEqual(['webauthn', 'email-sms']);
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

  // A sign-in challenge leaves a recovery-only TOTP out of `methods`, so its
  // backup codes go with it (#3563)...
  it('offers no backup codes when a sign-in challenge omits a recovery-only totp', () => {
    expect(getAvailableCategories(['email'], ['totp'])).toEqual(['email-sms']);
  });

  // ...while a step-up list still names it, and step-up accepts its backup codes.
  it('keeps backup codes when totp is listed but excluded', () => {
    expect(getAvailableCategories(['totp', 'email'], ['totp'])).toEqual(['email-sms', 'backup']);
  });

  // Excluding every listed factor would hide the app code step-up accepts.
  it('keeps totp when every listed method is excluded', () => {
    expect(getAvailableCategories(['totp'], ['totp'])).toEqual(['totp', 'backup']);
    expect(getDefaultMethod(['totp'], ['totp'])).toBe('totp');
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

// The two glyphs moved out of this file into `icons/` so the step-up picker can
// share them. The extraction must be invisible: the markup below is the JSX the
// picker carried before it, copied verbatim, and the picker's own rendering must
// still equal it.
describe('MFAMethodPicker extracted icons', () => {
  const ORIGINAL_KEY = (
    <svg
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
    >
      <path d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z" />
    </svg>
  );
  const ORIGINAL_PHONE = (
    <svg
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
    >
      <rect x="5" y="2" width="14" height="20" rx="2" />
      <line x1="12" y1="18" x2="12" y2="18.01" strokeWidth="2" strokeLinecap="round" />
    </svg>
  );

  function markupOf(node: ReactElement): string {
    return render(node).container.innerHTML;
  }

  function pickerIconHtml(label: string): string {
    const view = render(
      <MFAMethodPicker methods={['webauthn', 'totp']} currentMethod="totp" onSelect={vi.fn()} />
    );
    const button = view.getByText(label).closest('button');
    return button?.querySelector('.mfa-method-picker-icon')?.innerHTML ?? '';
  }

  it('KeyIcon renders the markup the picker carried before the extraction', () => {
    expect(markupOf(<KeyIcon />)).toBe(markupOf(ORIGINAL_KEY));
  });

  it('PhoneIcon renders the markup the picker carried before the extraction', () => {
    expect(markupOf(<PhoneIcon />)).toBe(markupOf(ORIGINAL_PHONE));
  });

  it('the picker still shows the security-key glyph on the Security Key option', () => {
    expect(pickerIconHtml('Security Key / Biometrics')).toBe(markupOf(ORIGINAL_KEY));
  });

  it('the picker still shows the phone glyph on the Authenticator App option', () => {
    expect(pickerIconHtml('Authenticator App')).toBe(markupOf(ORIGINAL_PHONE));
  });

  it('draws two different glyphs', () => {
    expect(markupOf(<KeyIcon />)).not.toBe(markupOf(<PhoneIcon />));
  });
});
