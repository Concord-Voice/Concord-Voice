import React from 'react';
import Modal from '../ui/Modal';
import './ServerActionModal.css';

interface ServerActionModalProps {
  isOpen: boolean;
  onClose: () => void;
  onCreateServer: () => void;
  onJoinServer: () => void;
}

const ServerActionModal: React.FC<ServerActionModalProps> = ({
  isOpen,
  onClose,
  onCreateServer,
  onJoinServer,
}) => {
  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Add a Server" width="small">
      <div className="server-action-choices">
        <button
          className="server-action-choice"
          onClick={() => {
            onClose();
            onCreateServer();
          }}
        >
          <div className="server-action-icon create">
            <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
              <path
                d="M12 5v14M5 12h14"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
              />
            </svg>
          </div>
          <div className="server-action-text">
            <span className="server-action-title">Create a Server</span>
            <span className="server-action-desc">Start a new community from scratch</span>
          </div>
          <svg
            className="server-action-arrow"
            width="16"
            height="16"
            viewBox="0 0 16 16"
            fill="none"
          >
            <path
              d="M6 4l4 4-4 4"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </button>

        <button
          className="server-action-choice"
          onClick={() => {
            onClose();
            onJoinServer();
          }}
        >
          <div className="server-action-icon join">
            <svg width="24" height="24" viewBox="0 0 24 24" fill="none">
              <path
                d="M15 3h4a2 2 0 012 2v14a2 2 0 01-2 2h-4M10 17l5-5-5-5M15 12H3"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </div>
          <div className="server-action-text">
            <span className="server-action-title">Join a Server</span>
            <span className="server-action-desc">
              Enter an invite code to join an existing server
            </span>
          </div>
          <svg
            className="server-action-arrow"
            width="16"
            height="16"
            viewBox="0 0 16 16"
            fill="none"
          >
            <path
              d="M6 4l4 4-4 4"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </button>
      </div>
    </Modal>
  );
};

export default ServerActionModal;
