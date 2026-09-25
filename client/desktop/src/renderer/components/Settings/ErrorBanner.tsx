import React from 'react';

/**
 * The "!" glyph shared by the banner and the field errors. Decorative — the
 * text beside it carries the meaning — so it is hidden from assistive tech and
 * kept out of the tab order (old Edge focused inline SVG). The glyph, not the
 * text, takes `--error-color`: a non-text mark only needs the 3:1 UI floor,
 * while `--danger` as label text fails AA in 9 of 30 scheme×theme pairs.
 */
export const AlertGlyph: React.FC<{ size?: number }> = ({ size = 16 }) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    aria-hidden="true"
    focusable="false"
    style={{ flexShrink: 0 }}
  >
    <circle cx="12" cy="12" r="10" />
    <line x1="12" y1="8" x2="12" y2="12" />
    <line x1="12" y1="16" x2="12.01" y2="16" />
  </svg>
);

interface FieldErrorProps {
  /** Referenced by the field's `aria-describedby`. */
  id: string;
  children: React.ReactNode;
}

/**
 * An error that belongs to one field: glyph plus `--text-primary` copy, so it
 * is never signalled by colour alone (SC 1.4.1), and `role="alert"` so it is
 * announced when it appears. The field names it through `aria-describedby`
 * and marks itself `aria-invalid`.
 */
export const FieldError: React.FC<FieldErrorProps> = ({ id, children }) => (
  <p id={id} role="alert" className="mfa-field-error">
    <AlertGlyph size={14} />
    <span>{children}</span>
  </p>
);

interface ErrorBannerProps {
  error: string;
  /** When `'mfa'`, the banner is suppressed — the MFA-specific error already
   * renders inline inside `MFAVerifyPrompt`, and duplicating it here would
   * show the same refusal twice. Callers with no field concept (the action
   * modal) simply never pass `'mfa'`. */
  errorField?: string;
  size?: number;
}

/**
 * Shared general-error banner (handoff §1.1: "the general banner slot").
 * Used by both `MFASetup`'s wizard and `MFATierSelector`'s action modal so
 * the two render identically — icon + `role="alert"` + `--text-primary`
 * copy, never `--danger`/`--error-color` text (spec §4.6.5). Originally a
 * private component inside `MFASetup.tsx`; extracted so both surfaces reuse
 * the exact same markup instead of drifting apart.
 */
const ErrorBanner: React.FC<ErrorBannerProps> = ({ error, errorField = '', size = 16 }) => {
  if (!error || errorField === 'mfa') return null;
  return (
    <div className="mfa-setup-error-banner" role="alert">
      <AlertGlyph size={size} />
      <span>{error}</span>
    </div>
  );
};

export default ErrorBanner;
