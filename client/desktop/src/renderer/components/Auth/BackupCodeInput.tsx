import React, { useState } from 'react';

interface BackupCodeInputProps {
  onSubmit: (code: string) => void;
  disabled?: boolean;
  error?: string;
}

const BackupCodeInput: React.FC<BackupCodeInputProps> = ({ onSubmit, disabled = false, error }) => {
  const [code, setCode] = useState('');

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = code.trim().toUpperCase();
    if (trimmed.length === 8) {
      onSubmit(trimmed);
    }
  };

  return (
    <form className="backup-code-input" onSubmit={handleSubmit}>
      <p className="backup-code-label">Enter one of your 8-character backup codes</p>
      <input
        type="text"
        value={code}
        onChange={(e) => setCode(e.target.value.toUpperCase())}
        placeholder="XXXXXXXX"
        maxLength={8}
        disabled={disabled}
        autoFocus
        className={`form-input backup-code-field ${error ? 'error' : ''}`}
        style={{
          fontFamily: 'var(--font-mono, monospace)',
          letterSpacing: '2px',
          textAlign: 'center',
        }}
      />
      {error && <p className="totp-error">{error}</p>}
    </form>
  );
};

export default BackupCodeInput;
