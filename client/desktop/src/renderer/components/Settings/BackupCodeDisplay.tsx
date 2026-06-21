import React, { useState } from 'react';

interface BackupCodeDisplayProps {
  codes: string[];
  onConfirm: () => void;
  disabled?: boolean;
}

const BackupCodeDisplay: React.FC<BackupCodeDisplayProps> = ({ codes, onConfirm, disabled }) => {
  const [saved, setSaved] = useState(false);

  const handleCopy = async () => {
    const text = codes.map((c, i) => `${i + 1}. ${c}`).join('\n');
    if (globalThis.electron?.writeClipboard) {
      globalThis.electron.writeClipboard(text);
    } else {
      await navigator.clipboard.writeText(text);
    }
  };

  const handleDownload = () => {
    const text = [
      'Concord Voice — MFA Backup Codes',
      `Generated: ${new Date().toISOString()}`,
      '',
      'Each code can only be used once.',
      'Store these codes in a safe place.',
      '',
      ...codes.map((c, i) => `${i + 1}. ${c}`),
    ].join('\n');

    const blob = new Blob([text], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'concord-backup-codes.txt';
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <div className="backup-code-display">
      <div className="backup-code-warning">
        These codes will not be shown again. Each code can only be used once.
      </div>

      <div className="backup-code-grid">
        {codes.map((code, i) => (
          <div key={code} className="backup-code-item">
            <span className="backup-code-number">{i + 1}.</span>
            <code className="backup-code-value">{code}</code>
          </div>
        ))}
      </div>

      <div className="backup-code-buttons">
        <button type="button" className="btn btn-secondary" onClick={handleCopy}>
          Copy All
        </button>
        <button type="button" className="btn btn-secondary" onClick={handleDownload}>
          Download .txt
        </button>
      </div>

      <label className="backup-code-checkbox">
        <input
          type="checkbox"
          checked={saved}
          onChange={(e) => setSaved(e.target.checked)}
          disabled={disabled}
        />{' '}
        I have saved my backup codes in a safe place
      </label>

      <button
        type="button"
        className="btn btn-primary"
        disabled={!saved || disabled}
        onClick={onConfirm}
      >
        Activate MFA
      </button>
    </div>
  );
};

export default BackupCodeDisplay;
