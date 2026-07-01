import React from 'react';

interface IconUploadAreaProps {
  preview: string | null;
  error?: string;
  onClick: () => void;
  onKeyDown: (e: React.KeyboardEvent) => void;
  onRemove: () => void;
  onFileChange: (e: React.ChangeEvent<HTMLInputElement>) => void;
  fileInputRef: React.RefObject<HTMLInputElement | null>;
  hint?: string;
}

const IconUploadArea: React.FC<IconUploadAreaProps> = ({
  preview,
  error,
  onClick,
  onKeyDown,
  onRemove,
  onFileChange,
  fileInputRef,
  hint,
}) => (
  <div className="server-icon-upload">
    <button
      type="button"
      className="icon-upload-area"
      onClick={onClick}
      onKeyDown={onKeyDown}
      aria-label="Upload server icon"
    >
      {preview ? (
        <img src={preview} alt="Server icon preview" className="icon-preview" />
      ) : (
        <div className="icon-placeholder">
          <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
            <path
              d="M23 19a2 2 0 01-2 2H3a2 2 0 01-2-2V8a2 2 0 012-2h4l2-3h6l2 3h4a2 2 0 012 2z"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
            <circle cx="12" cy="13" r="4" stroke="currentColor" strokeWidth="1.5" />
          </svg>
          <span className="icon-placeholder-text">Upload</span>
        </div>
      )}
    </button>
    <div className="banner-upload-actions">
      {hint && <span className="banner-upload-hint">{hint}</span>}
      {preview && (
        <button type="button" className="icon-remove-btn" onClick={onRemove}>
          Remove
        </button>
      )}
    </div>
    <input
      ref={fileInputRef}
      type="file"
      accept="image/png,image/jpeg,image/gif,image/webp"
      onChange={onFileChange}
      hidden
    />
    {error && <span className="form-error">{error}</span>}
  </div>
);

export default IconUploadArea;
