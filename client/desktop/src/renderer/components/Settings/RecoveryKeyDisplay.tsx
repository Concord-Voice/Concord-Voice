import React, { useState } from 'react';

interface RecoveryKeyDisplayProps {
  recoveryKey: string;
  onConfirm: () => void;
  onSkip?: () => void;
  disabled?: boolean;
}

const RecoveryKeyDisplay: React.FC<RecoveryKeyDisplayProps> = ({
  recoveryKey,
  onConfirm,
  onSkip,
  disabled,
}) => {
  const [saved, setSaved] = useState(false);
  const [showSkipWarning, setShowSkipWarning] = useState(false);

  const handleCopy = async () => {
    if (globalThis.electron?.writeClipboard) {
      globalThis.electron.writeClipboard(recoveryKey);
    } else {
      await navigator.clipboard.writeText(recoveryKey);
    }
  };

  const handleDownload = () => {
    const text = [
      'Concord Voice — Account Recovery Key',
      `Generated: ${new Date().toISOString()}`,
      '',
      'This is the ONLY way to recover your encrypted messages if you',
      'lose your password. Store this key in a safe place (paper, password manager).',
      '',
      'Recovery Key:',
      recoveryKey,
    ].join('\n');

    const blob = new Blob([text], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'concord-recovery-key.txt';
    a.click();
    URL.revokeObjectURL(url);
  };

  if (showSkipWarning) {
    return (
      <div className="backup-code-display">
        <div
          className="backup-code-warning"
          style={{ background: 'rgba(220, 38, 38, 0.15)', borderColor: '#dc2626' }}
        >
          <strong>Are you sure?</strong> Without a recovery key, losing your password means
          permanently losing all encrypted message history. This cannot be undone.
        </div>
        <div className="mfa-setup-actions">
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => setShowSkipWarning(false)}
          >
            Go Back
          </button>
          <button
            type="button"
            className="btn btn-primary"
            style={{ background: '#dc2626' }}
            onClick={onSkip}
          >
            Skip Without Recovery Key
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="backup-code-display">
      <div
        className="backup-code-warning"
        style={{ background: 'rgba(220, 38, 38, 0.15)', borderColor: '#dc2626' }}
      >
        This is your <strong>account recovery key</strong>. It is the only way to recover your
        encrypted messages if you lose your password. This key will not be shown again.
      </div>

      <div
        style={{
          background: 'var(--bg-tertiary, #0f3460)',
          borderRadius: 8,
          padding: '16px 24px',
          textAlign: 'center',
          margin: '12px 0',
        }}
      >
        <code
          style={{
            fontSize: 16,
            letterSpacing: 2,
            color: 'var(--text-primary, #e2e8f0)',
            wordBreak: 'break-all',
            lineHeight: 1.8,
          }}
        >
          {recoveryKey}
        </code>
      </div>

      <div className="backup-code-buttons">
        <button type="button" className="btn btn-secondary" onClick={handleCopy}>
          Copy
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
        I have saved my recovery key in a safe place
      </label>

      <button
        type="button"
        className="btn btn-primary"
        disabled={!saved || disabled}
        onClick={onConfirm}
      >
        Continue
      </button>

      {onSkip && (
        <button
          type="button"
          className="mfa-choose-another"
          onClick={() => setShowSkipWarning(true)}
          style={{ marginTop: 8 }}
        >
          Skip for now
        </button>
      )}
    </div>
  );
};

export default RecoveryKeyDisplay;
