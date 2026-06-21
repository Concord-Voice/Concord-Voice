import React from 'react';

/** MFA method categories in priority order (highest security first) */
export type MFAMethodCategory = 'webauthn' | 'totp' | 'email-sms' | 'backup';

interface MethodOption {
  category: MFAMethodCategory;
  label: string;
  description: string;
  icon: React.ReactNode;
}

const ALL_METHODS: MethodOption[] = [
  {
    category: 'webauthn',
    label: 'Security Key / Biometrics',
    description: 'Hardware key, fingerprint, or face recognition',
    icon: (
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
    ),
  },
  {
    category: 'totp',
    label: 'Authenticator App',
    description: '6-digit code from your authenticator',
    icon: (
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
    ),
  },
  {
    category: 'email-sms',
    label: 'Email / SMS Code',
    description: 'Verification code sent to your email or phone',
    icon: (
      <svg
        width="20"
        height="20"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
      >
        <path d="M3 8l7.89 5.26a2 2 0 002.22 0L21 8M5 19h14a2 2 0 002-2V7a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
      </svg>
    ),
  },
  {
    category: 'backup',
    label: 'Backup Code',
    description: 'One-time use recovery code',
    icon: (
      <svg
        width="20"
        height="20"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
      >
        <path d="M9 12h6m-3-3v6m-7 4h14a2 2 0 002-2V7a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
      </svg>
    ),
  },
];

/** Security priority: higher index = lower security */
const PRIORITY: MFAMethodCategory[] = ['webauthn', 'totp', 'email-sms', 'backup'];

/** Maps raw server method strings to our categories */
function methodToCategory(method: string): MFAMethodCategory | null {
  switch (method) {
    case 'webauthn':
      return 'webauthn';
    case 'totp':
      return 'totp';
    case 'email':
    case 'sms':
      return 'email-sms';
    default:
      return null;
  }
}

/** Given available server methods, returns available categories in priority order.
 *  excludeMethods: optional list of raw method strings to exclude (e.g. recovery-only methods) */
export function getAvailableCategories(
  methods: string[],
  excludeMethods?: string[]
): MFAMethodCategory[] {
  const excluded = excludeMethods ? new Set(excludeMethods) : null;
  const categories = new Set<MFAMethodCategory>();
  for (const m of methods) {
    if (excluded?.has(m)) continue;
    const cat = methodToCategory(m);
    if (cat) categories.add(cat);
  }
  // Always allow backup codes if any MFA method is available
  if (categories.size > 0) categories.add('backup');
  return PRIORITY.filter((p) => categories.has(p));
}

/** Returns the highest-security category available */
export function getDefaultMethod(methods: string[], excludeMethods?: string[]): MFAMethodCategory {
  const available = getAvailableCategories(methods, excludeMethods);
  return available[0] || 'totp';
}

interface MFAMethodPickerProps {
  methods: string[];
  currentMethod: MFAMethodCategory;
  onSelect: (method: MFAMethodCategory) => void;
  onCancel?: () => void;
  excludeMethods?: string[];
}

const MFAMethodPicker: React.FC<MFAMethodPickerProps> = ({
  methods,
  currentMethod,
  onSelect,
  onCancel,
  excludeMethods,
}) => {
  const available = getAvailableCategories(methods, excludeMethods);
  const options = ALL_METHODS.filter((m) => available.includes(m.category));

  return (
    <div className="mfa-method-picker">
      <h4 className="mfa-method-picker-title">Choose verification method</h4>
      <div className="mfa-method-picker-list">
        {options.map((opt) => (
          <button
            key={opt.category}
            type="button"
            className={`mfa-method-picker-option ${opt.category === currentMethod ? 'mfa-method-picker-active' : ''}`}
            onClick={() => onSelect(opt.category)}
          >
            <span className="mfa-method-picker-icon">{opt.icon}</span>
            <span className="mfa-method-picker-text">
              <span className="mfa-method-picker-label">{opt.label}</span>
              <span className="mfa-method-picker-desc">{opt.description}</span>
            </span>
          </button>
        ))}
      </div>
      {onCancel && (
        <button type="button" className="totp-backup-link" onClick={onCancel}>
          Cancel
        </button>
      )}
    </div>
  );
};

export default MFAMethodPicker;
