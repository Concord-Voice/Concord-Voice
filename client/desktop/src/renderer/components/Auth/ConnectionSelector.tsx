import React, { useState } from 'react';
import { ConnectionMode } from '../../types/auth';
import './ConnectionSelector.css';

interface ConnectionSelectorProps {
  onSelect: (mode: ConnectionMode) => void;
}

const ConnectionSelector: React.FC<ConnectionSelectorProps> = ({ onSelect }) => {
  const [selectedMode, setSelectedMode] = useState<ConnectionMode | null>(null);

  const handleModeChange = (mode: ConnectionMode) => {
    setSelectedMode(mode);
  };

  const handleContinue = () => {
    if (selectedMode) {
      onSelect(selectedMode);
    }
  };

  return (
    <div className="connection-selector">
      <div className="connection-content">
        {/* Logo and Tagline */}
        <div className="connection-header">
          <img
            src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
            alt="Concord Voice"
            className="connection-logo"
          />
        </div>

        {/* Connection Options */}
        <div className="connection-options">
          <label
            className={`connection-option ${selectedMode === 'hosted-login' ? 'selected' : ''}`}
            aria-label="Sign In to Existing Account"
          >
            <input
              type="radio"
              name="connection-mode"
              value="hosted-login"
              checked={selectedMode === 'hosted-login'}
              onChange={() => handleModeChange('hosted-login')}
              className="visually-hidden"
            />
            <div className="option-radio">
              <div className="radio-outer">
                {selectedMode === 'hosted-login' && <div className="radio-inner" />}
              </div>
            </div>
            <div className="option-content">
              <div className="option-title">Sign In to Existing Account</div>
              <div className="option-description">
                <div className="option-feature">• Already have a Concord Voice account</div>
                <div className="option-feature">• Access your servers and communities</div>
                <div className="option-feature">• Secure E2EE authentication</div>
              </div>
            </div>
          </label>

          <label
            className={`connection-option ${selectedMode === 'hosted' ? 'selected' : ''}`}
            aria-label="Create New Account"
          >
            <input
              type="radio"
              name="connection-mode"
              value="hosted"
              checked={selectedMode === 'hosted'}
              onChange={() => handleModeChange('hosted')}
              className="visually-hidden"
            />
            <div className="option-radio">
              <div className="radio-outer">
                {selectedMode === 'hosted' && <div className="radio-inner" />}
              </div>
            </div>
            <div className="option-content">
              <div className="option-title">Create New Account</div>
              <div className="option-description">
                <div className="option-feature">• New to Concord Voice? Start here</div>
                <div className="option-feature">• Access public servers and communities</div>
                <div className="option-feature">• Add self-hosted servers later</div>
              </div>
            </div>
          </label>

          <label
            className={`connection-option ${selectedMode === 'self-hosted' ? 'selected' : ''}`}
            aria-label="Connect to Self-Hosted Server"
          >
            <input
              type="radio"
              name="connection-mode"
              value="self-hosted"
              checked={selectedMode === 'self-hosted'}
              onChange={() => handleModeChange('self-hosted')}
              className="visually-hidden"
            />
            <div className="option-radio">
              <div className="radio-outer">
                {selectedMode === 'self-hosted' && <div className="radio-inner" />}
              </div>
            </div>
            <div className="option-content">
              <div className="option-title">Connect to Self-Hosted Server</div>
              <div className="option-description">
                <div className="option-feature">• Private and independent</div>
                <div className="option-feature">• No hosted account required</div>
                <div className="option-feature">• Complete control over your data</div>
              </div>
            </div>
          </label>
        </div>

        {/* Continue Button */}
        <button className="connection-continue" disabled={!selectedMode} onClick={handleContinue}>
          Continue
        </button>

        {/* Footer Links */}
        <div className="connection-footer">
          <button type="button" className="footer-link" disabled aria-disabled="true">
            Learn about self-hosting
          </button>
        </div>
      </div>
    </div>
  );
};

export default ConnectionSelector;
