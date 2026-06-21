import React, { useState, useRef, useCallback } from 'react';

interface TOTPInputProps {
  onSubmit: (code: string) => void;
  onBackupCode?: () => void;
  disabled?: boolean;
  error?: string;
  autoFocus?: boolean;
}

const DIGIT_KEYS = ['d1', 'd2', 'd3', 'd4', 'd5', 'd6'] as const;

const TOTPInput: React.FC<TOTPInputProps> = ({
  onSubmit,
  onBackupCode,
  disabled = false,
  error,
  autoFocus = true,
}) => {
  // eslint-disable-next-line @eslint-react/use-state -- lazy initializer not needed for a fixed-size array literal; the fill('') call is idempotent and has no performance impact
  const [digits, setDigits] = useState<string[]>(new Array(6).fill(''));
  const inputsRef = useRef<(HTMLInputElement | null)[]>([]);

  const handleChange = useCallback(
    (index: number, value: string) => {
      if (disabled) return;
      const digit = value.replaceAll(/\D/g, '').slice(-1);
      const newDigits = [...digits];
      newDigits[index] = digit;
      setDigits(newDigits);

      if (digit && index < 5) {
        inputsRef.current[index + 1]?.focus();
      }

      // Auto-submit when all 6 digits are filled
      const code = newDigits.join('');
      if (code.length === 6 && newDigits.every((d) => d !== '')) {
        onSubmit(code);
      }
    },
    [digits, disabled, onSubmit]
  );

  const handleKeyDown = useCallback(
    (index: number, e: React.KeyboardEvent) => {
      if (e.key === 'Backspace' && !digits[index] && index > 0) {
        inputsRef.current[index - 1]?.focus();
      }
    },
    [digits]
  );

  const handlePaste = useCallback(
    (e: React.ClipboardEvent) => {
      e.preventDefault();
      const pasted = e.clipboardData.getData('text').replaceAll(/\D/g, '').slice(0, 6);
      if (!pasted) return;

      const newDigits = new Array(6).fill('');
      for (let i = 0; i < pasted.length; i++) {
        newDigits[i] = pasted[i];
      }
      setDigits(newDigits);

      if (pasted.length === 6) {
        onSubmit(pasted);
      } else {
        inputsRef.current[pasted.length]?.focus();
      }
    },
    [onSubmit]
  );

  return (
    <div className="totp-input-container">
      <div className="totp-digits" onPaste={handlePaste}>
        {digits.map((digit, i) => (
          <input
            key={DIGIT_KEYS[i]}
            ref={(el) => {
              inputsRef.current[i] = el;
            }}
            type="text"
            inputMode="numeric"
            maxLength={1}
            value={digit}
            onChange={(e) => handleChange(i, e.target.value)}
            onKeyDown={(e) => handleKeyDown(i, e)}
            disabled={disabled}
            autoFocus={autoFocus && i === 0}
            className={`totp-digit ${error ? 'totp-digit-error' : ''}`}
            aria-label={`Digit ${i + 1}`}
          />
        ))}
      </div>
      {error && <p className="totp-error">{error}</p>}
      {onBackupCode && (
        <button
          type="button"
          className="totp-backup-link"
          onClick={onBackupCode}
          disabled={disabled}
        >
          Use a backup code instead
        </button>
      )}
    </div>
  );
};

export default TOTPInput;
