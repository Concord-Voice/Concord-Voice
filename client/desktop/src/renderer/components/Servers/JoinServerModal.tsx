import React, { useState, useEffect, useRef } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useInviteStore } from '../../stores/inviteStore';
import { useServerStore } from '../../stores/serverStore';
import { apiFetch } from '../../services/apiClient';
import { ServerWithRole, InviteInfoResponse } from '../../types/server';
import './JoinServerModal.css';

interface JoinServerModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSuccess: (server: ServerWithRole) => void;
}

const CODE_LENGTH = 8;

const JoinServerModal: React.FC<JoinServerModalProps> = ({ isOpen, onClose, onSuccess }) => {
  const [code, setCode] = useState('');
  const [preview, setPreview] = useState<InviteInfoResponse | null>(null);
  const [isLoadingPreview, setIsLoadingPreview] = useState(false);
  const [isJoining, setIsJoining] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const previewTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const joinServer = useInviteStore((state) => state.joinServer);
  const getInviteInfo = useInviteStore((state) => state.getInviteInfo);

  // Auto-focus input when modal opens
  useEffect(() => {
    if (!isOpen) return;
    const focusTimer = setTimeout(() => inputRef.current?.focus(), 100);
    return () => clearTimeout(focusTimer);
  }, [isOpen]);

  // Reset form on close
  useEffect(() => {
    if (!isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets code when modal closes; not a render loop
      setCode('');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears preview when modal closes; not a render loop
      setPreview(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets loading state when modal closes; not a render loop
      setIsLoadingPreview(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets joining state when modal closes; not a render loop
      setIsJoining(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when modal closes; not a render loop
      setError(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears success message when modal closes; not a render loop
      setSuccessMessage(null);
    }
  }, [isOpen]);

  // Auto-preview when code reaches full length
  useEffect(() => {
    if (previewTimeoutRef.current) {
      clearTimeout(previewTimeoutRef.current);
    }

    if (code.length === CODE_LENGTH) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: shows loading state while fetching invite preview; not a render loop
      setIsLoadingPreview(true);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when starting preview fetch; not a render loop
      setError(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears preview when starting a new fetch; not a render loop
      setPreview(null);

      previewTimeoutRef.current = setTimeout(async () => {
        const info = await getInviteInfo(code);
        setIsLoadingPreview(false);
        if (info) {
          if (info.valid) {
            setPreview(info);
          } else {
            setError('This invite is no longer valid (expired, revoked, or used up)');
          }
        } else {
          // Check if this might be a friend code instead
          try {
            const fcRes = await apiFetch(`/api/v1/friends/codes/${code}`);
            if (fcRes.ok) {
              setError(
                'This looks like a friend code, not a server invite. Use the Add Friend button in Direct Messages to claim it.'
              );
            } else {
              setError('Invalid invite code');
            }
          } catch {
            setError('Invalid invite code');
          }
        }
      }, 300);
    } else {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears preview when code is incomplete; not a render loop
      setPreview(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when code is incomplete; not a render loop
      setError(null);
    }

    return () => {
      if (previewTimeoutRef.current) {
        clearTimeout(previewTimeoutRef.current);
      }
    };
  }, [code, getInviteInfo]);

  const handleCodeChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    // Only allow alphanumeric characters, strip spaces
    const value = e.target.value.replaceAll(/[^a-zA-Z0-9]/g, '').slice(0, CODE_LENGTH);
    setCode(value);
    setSuccessMessage(null);
  };

  const handleJoin = async () => {
    if (code.length !== CODE_LENGTH || !preview?.valid) return;

    setIsJoining(true);
    setError(null);

    const result = await joinServer(code);
    if (result) {
      const serverWithRole: ServerWithRole = {
        ...result.server,
        role: result.role as ServerWithRole['role'],
        member_count: 0,
        online_count: 0,
      };
      useServerStore.getState().addServer(serverWithRole);
      setSuccessMessage(`Joined ${result.server.name}!`);

      setTimeout(() => {
        onSuccess(serverWithRole);
        onClose();
      }, 800);
    } else {
      setError(useInviteStore.getState().error || 'Failed to join server');
      setIsJoining(false);
    }
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    handleJoin();
  };

  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Join a Server" width="medium">
      <form className="join-server-form" onSubmit={handleSubmit}>
        <div className="form-group">
          <label htmlFor="join-server-code" className="form-label">
            Invite Code
          </label>
          <input
            id="join-server-code"
            ref={inputRef}
            type="text"
            className={`form-input join-code-input ${error ? 'error' : ''}`}
            placeholder="AbCd1234"
            value={code}
            onChange={handleCodeChange}
            disabled={isJoining || !!successMessage}
            maxLength={CODE_LENGTH}
            autoComplete="off"
            spellCheck={false}
          />
          <span className="form-hint">
            {code.length}/{CODE_LENGTH} characters
            {code.length > 0 && code.length < CODE_LENGTH && ' — keep typing'}
          </span>
        </div>

        {/* Loading preview */}
        {isLoadingPreview && (
          <div className="join-preview-loading">
            <LoadingSpinner size="small" inline />
            <span>Looking up invite...</span>
          </div>
        )}

        {/* Server preview */}
        {preview?.valid && (
          <div className="join-server-preview">
            <div className="join-preview-icon">
              {resolveMediaUrl(preview.server_icon) ? (
                <img src={resolveMediaUrl(preview.server_icon)} alt={preview.server_name} />
              ) : (
                <span className="join-preview-initial">
                  {preview.server_name.charAt(0).toUpperCase()}
                </span>
              )}
            </div>
            <div className="join-preview-info">
              <span className="join-preview-name">{preview.server_name}</span>
              <span className="join-preview-members">
                {preview.member_count} {preview.member_count === 1 ? 'member' : 'members'}
              </span>
            </div>
          </div>
        )}

        {/* Error */}
        {error && (
          <div className="form-error-banner">
            <span>{error}</span>
          </div>
        )}

        {/* Success */}
        {successMessage && (
          <div className="form-success-banner">
            <span>{successMessage}</span>
          </div>
        )}

        {/* Actions */}
        <div className="join-server-actions">
          <button
            type="button"
            className="join-server-cancel-btn"
            onClick={onClose}
            disabled={isJoining}
          >
            Cancel
          </button>
          <button
            type="submit"
            className="join-server-submit-btn"
            disabled={
              code.length !== CODE_LENGTH || !preview?.valid || isJoining || !!successMessage
            }
          >
            {isJoining ? (
              <>
                Joining...
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              'Join Server'
            )}
          </button>
        </div>
      </form>
    </Modal>
  );
};

export default JoinServerModal;
