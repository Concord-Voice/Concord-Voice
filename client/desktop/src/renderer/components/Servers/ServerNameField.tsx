import React from 'react';
import { NAME_MAX } from './serverConstants';

export interface ServerNameFieldProps {
  inputId: string;
  name: string;
  error?: string;
  disabled: boolean;
  autoFocus?: boolean;
  onChange: (value: string) => void;
}

/**
 * Server-name form group shared by CreateServerModal and ServerSettingsPage.
 * Validation lives in validateServerName (serverConstants.ts); this renders
 * the input, its inline error, and the character-count hint.
 */
export const ServerNameField: React.FC<ServerNameFieldProps> = ({
  inputId,
  name,
  error,
  disabled,
  autoFocus = false,
  onChange,
}) => (
  <div className="form-group">
    <label htmlFor={inputId} className="form-label">
      Server Name
    </label>
    <input
      id={inputId}
      type="text"
      className={`form-input ${error ? 'error' : ''}`}
      placeholder="My Awesome Server"
      value={name}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
      autoFocus={autoFocus}
      maxLength={NAME_MAX}
    />
    {error && <span className="form-error">{error}</span>}
    <span className="form-hint">
      {name.trim().length}/{NAME_MAX} characters
    </span>
  </div>
);

export interface ServerFormBannersProps {
  generalError?: string;
  successMessage?: string | null;
}

/** General-error and success banners shared by the two server forms. */
export const ServerFormBanners: React.FC<ServerFormBannersProps> = ({
  generalError,
  successMessage,
}) => (
  <>
    {generalError && (
      <div className="form-error-banner">
        <span>{generalError}</span>
      </div>
    )}
    {successMessage && (
      <div className="form-success-banner">
        <span>{successMessage}</span>
      </div>
    )}
  </>
);
