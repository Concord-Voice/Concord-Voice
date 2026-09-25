import React, { useId, useState } from 'react';

interface BackupCodeInputProps {
  onSubmit: (code: string) => void;
  /**
   * Fires on every edit: the complete code once eight characters are typed,
   * `''` while it is incomplete. A parent that stores the code for a later
   * Confirm reads this, so editing a complete code back down clears the stored
   * value instead of leaving Confirm enabled with the old one.
   */
  onCodeChange?: (code: string) => void;
  disabled?: boolean;
  error?: string;
}

const BACKUP_CODE_LENGTH = 8;

const BackupCodeInput: React.FC<BackupCodeInputProps> = ({
  onSubmit,
  onCodeChange,
  disabled = false,
  error,
}) => {
  const [code, setCode] = useState('');
  const labelId = useId();
  const hintId = useId();

  // A complete code is reported as soon as it is typed, as TOTPInput does at
  // six digits. A parent that reads the code only from onSubmit (a modal's
  // Confirm button) otherwise never receives it unless Enter is pressed. The
  // label says so in advance (WCAG 3.2.2).
  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    if (disabled) return;
    const next = e.target.value.toUpperCase();
    setCode(next);
    const trimmed = next.trim();
    const complete = trimmed.length === BACKUP_CODE_LENGTH;
    onCodeChange?.(complete ? trimmed : '');
    if (complete) onSubmit(trimmed);
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (disabled) return;
    const trimmed = code.trim();
    if (trimmed.length === BACKUP_CODE_LENGTH) {
      onSubmit(trimmed);
    }
  };

  return (
    <form className="backup-code-input" onSubmit={handleSubmit}>
      <p id={labelId} className="backup-code-label">
        Enter one of your 8-character backup codes
      </p>
      <p id={hintId} className="backup-code-hint">
        It is checked as soon as you type the 8th character.
      </p>
      <input
        type="text"
        value={code}
        onChange={handleChange}
        placeholder="XXXXXXXX"
        maxLength={BACKUP_CODE_LENGTH}
        disabled={disabled}
        autoFocus
        aria-labelledby={labelId}
        aria-describedby={hintId}
        className={`form-input backup-code-field ${error ? 'error' : ''}`}
        style={{
          fontFamily: 'var(--font-mono, monospace)',
          letterSpacing: '2px',
          textAlign: 'center',
        }}
      />
      {error && (
        <p className="totp-error" role="alert">
          {error}
        </p>
      )}
    </form>
  );
};

export default BackupCodeInput;
