import React from 'react';
import Modal from '../ui/Modal';
import './DeleteMessageModal.css';

interface DeleteMessageModalProps {
  isOpen: boolean;
  onClose: () => void;
  onConfirm: () => void;
}

const DeleteMessageModal: React.FC<DeleteMessageModalProps> = ({ isOpen, onClose, onConfirm }) => {
  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Delete Message" width="small">
      <div className="delete-message-content">
        <div className="delete-message-warning">
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
          <p>Are you sure you want to delete this message? This cannot be undone.</p>
        </div>

        <span className="delete-message-hint">
          Tip: Hold <kbd>Shift</kbd> and click Delete to skip this prompt.
        </span>

        <div className="delete-message-actions">
          <button type="button" className="delete-message-cancel-btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            className="delete-message-confirm-btn"
            onClick={onConfirm}
            autoFocus
          >
            Delete
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default DeleteMessageModal;
