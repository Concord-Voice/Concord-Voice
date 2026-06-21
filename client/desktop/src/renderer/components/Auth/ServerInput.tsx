import React, { useState } from 'react';
import './ServerInput.css';

interface ServerInputProps {
  onConnect: (serverUrl: string) => void;
  onBack: () => void;
}

const ServerInput: React.FC<ServerInputProps> = ({ onConnect, onBack }) => {
  const [serverUrl, setServerUrl] = useState('');
  const [isValidating, setIsValidating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const validateUrl = (url: string): boolean => {
    try {
      const parsed = new URL(url);
      return parsed.protocol === 'https:' || parsed.protocol === 'http:';
    } catch {
      return false;
    }
  };

  const handleConnect = async () => {
    setError(null);

    // Validate URL format
    if (!serverUrl) {
      setError('Please enter a server URL');
      return;
    }

    // Ensure URL has protocol
    let fullUrl = serverUrl;
    if (!serverUrl.startsWith('http://') && !serverUrl.startsWith('https://')) {
      fullUrl = `https://${serverUrl}`;
    }

    if (!validateUrl(fullUrl)) {
      setError('Invalid server URL');
      return;
    }

    // Check for HTTPS (warn if HTTP)
    if (fullUrl.startsWith('http://') && !fullUrl.includes('localhost')) {
      setError('HTTPS is required for security (except localhost)');
      return;
    }

    setIsValidating(true);

    try {
      // Test server connectivity (will implement full discovery later)
      // For now, just validate the URL format
      onConnect(fullUrl);
    } catch {
      setError('Could not connect to server. Please check the URL.');
      setIsValidating(false);
    }
  };

  const handleKeyPress = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && serverUrl) {
      handleConnect();
    }
  };

  return (
    <div className="server-input">
      <div className="server-content">
        {/* Header */}
        <div className="server-header">
          <img
            src="./branding/Concord-Voice/logos/symbol-transparent-vector.svg"
            alt="Concord Voice"
            className="server-icon"
          />
          <h2 className="server-title">Connect to Self-Hosted Server</h2>
          <p className="server-subtitle">Enter the address provided by your server administrator</p>
        </div>

        {/* URL Input */}
        <div className="server-form">
          <div className="input-group">
            <label htmlFor="server-url" className="input-label">
              Server URL
            </label>
            <input
              id="server-url"
              type="text"
              className={`server-url-input ${error ? 'error' : ''}`}
              placeholder="https://concord.myserver.com"
              value={serverUrl}
              onChange={(e) => setServerUrl(e.target.value)}
              onKeyPress={handleKeyPress}
              autoFocus
            />
            {error && <div className="input-error">{error}</div>}
          </div>

          {/* Security Info */}
          <div className="security-info">
            <div className="info-icon">🔒</div>
            <div className="info-text">
              <div className="info-title">Secure Connection</div>
              <div className="info-description">
                All connections are encrypted with HTTPS to protect your privacy
              </div>
            </div>
          </div>

          {/* Connect Button */}
          <button
            className="server-connect-btn"
            onClick={handleConnect}
            disabled={!serverUrl || isValidating}
          >
            {isValidating ? 'Connecting...' : 'Connect to Server'}
          </button>

          {/* Back Button */}
          <button className="server-back-btn" onClick={onBack}>
            ← Back to Connection Options
          </button>
        </div>

        {/* Help Link */}
        <div className="server-footer">
          <p className="footer-help">
            Don&apos;t have a server? <span className="help-link">Learn about self-hosting →</span>
          </p>
        </div>
      </div>
    </div>
  );
};

export default ServerInput;
