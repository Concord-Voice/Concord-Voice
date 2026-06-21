import React, { useId, useState } from 'react';
import Modal from './Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import './ConfirmActionModal.css';

interface ConfirmActionModalProps {
  isOpen: boolean;
  onClose: () => void;
  title: string;
  message: React.ReactNode;
  confirmLabel: string;
  loadingLabel: string;
  onConfirm: () => Promise<void>;
  /** When provided, user must type expectedValue to enable the confirm button */
  confirmationInput?: {
    label: React.ReactNode;
    expectedValue: string;
    placeholder?: string;
  };
}

const WarningIcon: React.FC = () => (
  <svg width="24" height="24" viewBox="0 0 24 24" fill="none" className="warning-icon">
    <path
      d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
    <line
      x1="12"
      y1="9"
      x2="12"
      y2="13"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
    />
    <line
      x1="12"
      y1="17"
      x2="12.01"
      y2="17"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
    />
  </svg>
);

const ConfirmActionModal: React.FC<ConfirmActionModalProps> = ({
  isOpen,
  onClose,
  title,
  message,
  confirmLabel,
  loadingLabel,
  onConfirm,
  confirmationInput,
}) => {
  const inputId = useId();
  const [error, setError] = useState<string | null>(null);
  const [isProcessing, setIsProcessing] = useState(false);
  const [inputValue, setInputValue] = useState('');

  const resetAndClose = () => {
    setError(null);
    setInputValue('');
    setIsProcessing(false);
    onClose();
  };

  const handleClose = () => {
    if (!isProcessing) {
      resetAndClose();
    }
  };

  const isConfirmed = !confirmationInput || inputValue === confirmationInput.expectedValue;

  const handleConfirm = async () => {
    if (!isConfirmed) return;
    setIsProcessing(true);
    setError(null);

    try {
      await onConfirm();
      resetAndClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Something went wrong. Please try again.');
      setIsProcessing(false);
    }
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title={title} width="small">
      <div className="delete-server-content">
        <div className="delete-server-warning">
          <WarningIcon />
          <div className="confirm-action-message">{message}</div>
        </div>

        {confirmationInput && (
          <div className="delete-server-confirm">
            <label htmlFor={inputId} className="form-label">
              {confirmationInput.label}
            </label>
            <input
              id={inputId}
              type="text"
              className="form-input"
              placeholder={confirmationInput.placeholder ?? confirmationInput.expectedValue}
              value={inputValue}
              onChange={(e) => {
                setInputValue(e.target.value);
                if (error) setError(null);
              }}
              disabled={isProcessing}
              autoFocus
            />
          </div>
        )}

        {error && (
          <div className="form-error-banner">
            <span>{error}</span>
          </div>
        )}

        <div className="delete-server-actions">
          <button
            type="button"
            className="delete-server-cancel-btn"
            onClick={handleClose}
            disabled={isProcessing}
          >
            Cancel
          </button>
          <button
            type="button"
            className="delete-server-confirm-btn"
            onClick={handleConfirm}
            disabled={!isConfirmed || isProcessing}
          >
            {isProcessing ? (
              <>
                {loadingLabel}
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              confirmLabel
            )}
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default ConfirmActionModal;
