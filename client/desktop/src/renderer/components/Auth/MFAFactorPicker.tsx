import React, { useId, useImperativeHandle, useRef } from 'react';
import type { StepUpMethod } from '../../hooks/auth/useStepUpFactor';
import './MFAFactorPicker.css';

export interface MFAFactorPickerHandle {
  /** Focuses the active panel: its code input, or its label when it has none. */
  focus: () => void;
}

export interface MFAFactorPickerProps {
  ref?: React.Ref<MFAFactorPickerHandle>;
  /** Every method on offer, strongest first, a backup code last (`StepUpFactor.methods`). */
  methods: readonly StepUpMethod[];
  /** The active panel. */
  method: StepUpMethod;
  code: string;
  /** The code input's `key`: a submit that may have spent the code remounts it. */
  attempt: number;
  onCodeChange: (code: string) => void;
  /** One switch link per other offered method; the owner clears the code and aborts a ceremony. */
  onSwitch: (method: StepUpMethod) => void;
  /** A TOTP code was accepted this period (S2a). Only the authenticator-app panel says so. */
  recentlyUsedCode: boolean;
  /** This panel's message, already worded; null for none. */
  error: string | null;
  /** A plain Enter in the code input (design §4.3): the owner presses its primary. */
  onEnter: () => void;
}

/**
 * A plain Enter: not one an input method is composing with, not one a
 * modifier changes, and not the auto-repeat of a held key: the field keeps
 * focus through a refusal, so a held Enter would press the primary again
 * against the answer it just got. The stage's password field uses it too, so
 * the two inputs submit on the same key.
 */
export function isPlainEnter(e: React.KeyboardEvent): boolean {
  return (
    e.key === 'Enter' &&
    !e.repeat &&
    !e.nativeEvent.isComposing &&
    !e.shiftKey &&
    !e.altKey &&
    !e.ctrlKey &&
    !e.metaKey
  );
}

interface StepUpFieldErrorProps {
  id: string;
  message: string;
}

/**
 * A rejected field's message. The text is `--text-primary` and the rejection
 * rides on the glyph, border and tint: `--danger` fails AA as text in 9 of the
 * 30 scheme x theme combinations, and nothing here is colour-only (WCAG 1.4.1).
 */
export const StepUpFieldError: React.FC<StepUpFieldErrorProps> = ({ id, message }) => (
  <p id={id} role="alert" className="step-up__error">
    <svg
      className="step-up__error-glyph"
      width="16"
      height="16"
      viewBox="0 0 24 24"
      fill="none"
      strokeWidth="2"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="10" />
      <path d="M12 8v5M12 16h.01" />
    </svg>
    <span>{message}</span>
  </p>
);

/** Per-method copy (design §4.2). The authenticator-app helper has a second wording (S2a). */
const PANEL_COPY: Record<StepUpMethod, { label: string; switchLabel: string; helper: string }> = {
  webauthn: {
    label: 'Passkey or security key',
    switchLabel: 'Use passkey or security key instead',
    helper: "You'll be asked for it when you continue.",
  },
  totp: {
    label: 'Authenticator app code',
    switchLabel: 'Use authenticator app instead',
    helper: 'Enter the 6-digit code from your authenticator app.',
  },
  backup: {
    label: 'Backup code',
    switchLabel: 'Use a backup code instead',
    helper: 'Enter one of your 8-character backup codes.',
  },
};

const RECENTLY_USED_HELPER =
  'You just used a code from your authenticator app. Enter the next one it shows.';

/**
 * The step-up picker's method region (design 2026-09-26-mfa-factor-picker §4.2):
 * the active panel and one switch link per other offered method. Presentational;
 * `StepUpCredentials` feeds it from `useStepUpFactor`.
 */
const MFAFactorPicker: React.FC<MFAFactorPickerProps> = ({
  ref,
  methods,
  method,
  code,
  attempt,
  onCodeChange,
  onSwitch,
  recentlyUsedCode,
  error,
  onEnter,
}) => {
  const baseId = useId();
  const labelId = `${baseId}-label`;
  const inputId = `${baseId}-input`;
  const helperId = `${baseId}-helper`;
  const errorId = `${baseId}-error`;
  const inputRef = useRef<HTMLInputElement>(null);
  const labelRef = useRef<HTMLParagraphElement>(null);

  useImperativeHandle(
    ref,
    () => ({ focus: () => (inputRef.current ?? labelRef.current)?.focus() }),
    []
  );

  const copy = PANEL_COPY[method];
  const helperText = method === 'totp' && recentlyUsedCode ? RECENTLY_USED_HELPER : copy.helper;
  const describedBy = error === null ? helperId : `${helperId} ${errorId}`;
  const helper = (
    <p id={helperId} className="step-up__helper">
      {helperText}
    </p>
  );
  const message = error === null ? null : <StepUpFieldError id={errorId} message={error} />;

  return (
    <div className="step-up__panel">
      {method === 'webauthn' ? (
        // A fieldset is the native group (Sonar S6819); it carries no form
        // control, so it disables nothing the outer stage does not.
        <fieldset
          className="step-up__field"
          aria-labelledby={labelId}
          aria-describedby={describedBy}
        >
          <p id={labelId} ref={labelRef} className="step-up__label" tabIndex={-1}>
            {copy.label}
          </p>
          {helper}
          {message}
        </fieldset>
      ) : (
        <div className="step-up__field">
          <label htmlFor={inputId} className="step-up__label">
            {copy.label}
          </label>
          <input
            // Remounts on a switch and after a spent code, so the field starts empty
            // and the owner's focus lands on a fresh element.
            key={`${method}-${attempt}`}
            id={inputId}
            ref={inputRef}
            className={`step-up__input step-up__input--${method}`}
            type="text"
            inputMode={method === 'totp' ? 'numeric' : undefined}
            autoComplete={method === 'totp' ? 'one-time-code' : 'off'}
            spellCheck={false}
            value={code}
            onChange={(e) => onCodeChange(e.target.value)}
            onKeyDown={(e) => {
              if (!isPlainEnter(e)) return;
              e.preventDefault();
              onEnter();
            }}
            aria-invalid={error !== null || undefined}
            aria-describedby={describedBy}
          />
          {helper}
          {message}
        </div>
      )}

      {methods.length > 1 && (
        <div className="step-up__switches">
          {methods
            .filter((other) => other !== method)
            .map((other) => (
              <button
                key={other}
                type="button"
                className="step-up__link"
                onClick={() => onSwitch(other)}
              >
                {PANEL_COPY[other].switchLabel}
              </button>
            ))}
        </div>
      )}
    </div>
  );
};

export default MFAFactorPicker;
