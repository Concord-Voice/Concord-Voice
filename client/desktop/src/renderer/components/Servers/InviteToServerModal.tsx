import React, { useState, useEffect, useRef, useCallback } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import Modal from '../ui/Modal';
import CustomSelect from '../ui/CustomSelect';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useInviteStore } from '../../stores/inviteStore';
import { ServerWithRole, ServerInviteWithCreator } from '../../types/server';
import { errorMessage } from '../../utils/redactError';
import './InviteToServerModal.css';

const EMPTY_INVITES: ServerInviteWithCreator[] = [];

interface InviteToServerModalProps {
  isOpen: boolean;
  server: ServerWithRole;
  onClose: () => void;
}

const EXPIRY_OPTIONS = [
  { label: '30 minutes', value: 1800 },
  { label: '1 hour', value: 3600 },
  { label: '6 hours', value: 21600 },
  { label: '12 hours', value: 43200 },
  { label: '1 day', value: 86400 },
  { label: '7 days', value: 604800 },
];

const MAX_USES_OPTIONS = [
  { label: 'No limit', value: 0 },
  { label: '1 use', value: 1 },
  { label: '5 uses', value: 5 },
  { label: '10 uses', value: 10 },
  { label: '25 uses', value: 25 },
  { label: '100 uses', value: 100 },
];

const InviteToServerModal: React.FC<InviteToServerModalProps> = ({ isOpen, server, onClose }) => {
  const [generatedCode, setGeneratedCode] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [isGenerating, setIsGenerating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expiresIn, setExpiresIn] = useState(86400);
  const [maxUses, setMaxUses] = useState(0);
  const [existingInvites, setExistingInvites] = useState<ServerInviteWithCreator[]>([]);
  const codeRef = useRef<HTMLInputElement>(null);

  const createInvite = useInviteStore((state) => state.createInvite);
  const fetchInvites = useInviteStore((state) => state.fetchInvites);
  const invites = useInviteStore((state) => state.invites[server.id] ?? EMPTY_INVITES);

  // Load existing invites when modal opens
  useEffect(() => {
    if (isOpen) {
      fetchInvites(server.id);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets generatedCode when modal opens; not a render loop
      setGeneratedCode(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets copied flag when modal opens; not a render loop
      setCopied(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when modal opens; not a render loop
      setError(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets expiresIn to default when modal opens; not a render loop
      setExpiresIn(86400);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets maxUses to default when modal opens; not a render loop
      setMaxUses(0);
    }
  }, [isOpen, server.id, fetchInvites]);

  // Filter active invites (recalculated on each tick)
  const filterActive = useCallback(() => {
    const now = Date.now();
    return invites.filter((inv) => {
      if (inv.is_revoked) return false;
      if (inv.expires_at && new Date(inv.expires_at).getTime() <= now) return false;
      if (inv.max_uses !== null && inv.max_uses > 0 && inv.use_count >= inv.max_uses) return false;
      return true;
    });
  }, [invites]);

  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: refreshes existingInvites when invite list or filter criteria change; not a render loop
    setExistingInvites(filterActive());
  }, [filterActive]);

  // Tick timer so expiry countdowns update live
  useEffect(() => {
    if (!isOpen || existingInvites.length === 0) return;
    // Tick every second when any invite is under 1 hour, otherwise every 30s
    const hasSubHour = existingInvites.some((inv) => {
      if (!inv.expires_at) return false;
      const diff = new Date(inv.expires_at).getTime() - Date.now();
      return diff > 0 && diff < 3600000;
    });
    const interval = setInterval(
      () => {
        setExistingInvites(filterActive());
      },
      hasSubHour ? 1000 : 30000
    );
    return () => clearInterval(interval);
  }, [isOpen, existingInvites, filterActive]);

  const handleGenerateCode = async () => {
    setIsGenerating(true);
    setError(null);
    setCopied(false);

    try {
      const opts: { max_uses: number; expires_in: number } = {
        expires_in: expiresIn,
        max_uses: maxUses, // 0 = unlimited (backend stores NULL)
      };

      const invite = await createInvite(server.id, opts);
      if (invite) {
        setGeneratedCode(invite.code);
        // Auto-copy to clipboard (non-fatal if clipboard access fails)
        try {
          await navigator.clipboard.writeText(invite.code);
          setCopied(true);
          setTimeout(() => setCopied(false), 2000);
        } catch {
          // Clipboard unavailable — user can copy manually via the copy button
        }
      } else {
        setError('Failed to create invite');
      }
    } catch (err: unknown) {
      console.error('Failed to create invite:', errorMessage(err));
      setError('Failed to create invite');
    } finally {
      setIsGenerating(false);
    }
  };

  const handleCopyCode = async (code: string) => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard unavailable — user can select the code manually
    }
  };

  const handleSelectCode = () => {
    codeRef.current?.select();
  };

  const formatExpiry = (expiresAt: string | null) => {
    if (!expiresAt) return 'Never';
    const diff = new Date(expiresAt).getTime() - Date.now();
    if (diff <= 0) return 'Expired';
    const hours = Math.floor(diff / 3600000);
    const minutes = Math.floor((diff % 3600000) / 60000);
    const seconds = Math.floor((diff % 60000) / 1000);
    if (hours >= 24) return `${Math.floor(hours / 24)}d remaining`;
    if (hours >= 1) return `${hours}h remaining`;
    return `${minutes}m ${seconds}s remaining`;
  };

  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Invite to Server" width="medium">
      <div className="invite-modal">
        {/* Server header */}
        <div className="invite-modal-server">
          <div className="invite-modal-server-icon">
            {resolveMediaUrl(server.icon_url) ? (
              <img src={resolveMediaUrl(server.icon_url)} alt={server.name} />
            ) : (
              <span>{server.name.charAt(0).toUpperCase()}</span>
            )}
          </div>
          <span className="invite-modal-server-name">{server.name}</span>
        </div>

        {/* Generate new invite */}
        <div className="invite-modal-section">
          <h4 className="invite-modal-section-title">Generate Invite Code</h4>
          <p className="invite-modal-section-desc">
            Create a code that others can use to join this server.
          </p>

          <div className="invite-modal-options">
            <div className="invite-option-group">
              <label htmlFor="invite-expires" className="invite-option-label">
                Expires after
              </label>
              <CustomSelect
                id="invite-expires"
                options={EXPIRY_OPTIONS.map((opt) => ({
                  value: String(opt.value),
                  label: opt.label,
                }))}
                value={String(expiresIn)}
                onChange={(v) => setExpiresIn(Number(v))}
                disabled={isGenerating}
                className="invite-option-select"
              />
            </div>

            <div className="invite-option-group">
              <label htmlFor="invite-max-uses" className="invite-option-label">
                Max uses
              </label>
              <CustomSelect
                id="invite-max-uses"
                options={MAX_USES_OPTIONS.map((opt) => ({
                  value: String(opt.value),
                  label: opt.label,
                }))}
                value={String(maxUses)}
                onChange={(v) => setMaxUses(Number(v))}
                disabled={isGenerating}
                className="invite-option-select"
              />
            </div>
          </div>

          {/* Generated code display */}
          {generatedCode && (
            <div className="invite-code-display">
              <input
                ref={codeRef}
                className="invite-code-value"
                value={generatedCode}
                readOnly
                onClick={handleSelectCode}
              />
              <button
                className="invite-code-copy-btn"
                onClick={() => handleCopyCode(generatedCode)}
              >
                {copied ? (
                  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                    <path
                      d="M2 8.5l3.5 3.5L14 3"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                ) : (
                  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                    <rect
                      x="5"
                      y="5"
                      width="9"
                      height="9"
                      rx="1.5"
                      stroke="currentColor"
                      strokeWidth="1.5"
                    />
                    <path
                      d="M11 5V3.5A1.5 1.5 0 009.5 2h-6A1.5 1.5 0 002 3.5v6A1.5 1.5 0 003.5 11H5"
                      stroke="currentColor"
                      strokeWidth="1.5"
                    />
                  </svg>
                )}
                {copied ? 'Copied!' : 'Copy'}
              </button>
            </div>
          )}

          {error && (
            <div className="form-error-banner">
              <span>{error}</span>
            </div>
          )}

          <button
            className="invite-generate-btn"
            onClick={handleGenerateCode}
            disabled={isGenerating}
          >
            {(() => {
              if (isGenerating) {
                return (
                  <>
                    Generating...
                    <LoadingSpinner size="small" inline />
                  </>
                );
              }
              return generatedCode ? 'Generate New Code' : 'Generate Invite Code';
            })()}
          </button>
        </div>

        {/* Existing active invites */}
        {existingInvites.length > 0 && (
          <div className="invite-modal-section">
            <h4 className="invite-modal-section-title">Active Invites</h4>
            <div className="invite-existing-list">
              {existingInvites.map((inv) => (
                <div key={inv.id} className="invite-existing-item">
                  <div className="invite-existing-info">
                    <span className="invite-existing-code">{inv.code}</span>
                    <span className="invite-existing-meta">
                      {inv.use_count} /{' '}
                      {inv.max_uses !== null && inv.max_uses > 0 ? inv.max_uses : '\u221E'} Uses
                      {' \u00B7 '}
                      {formatExpiry(inv.expires_at)}
                    </span>
                  </div>
                  <button
                    className="invite-existing-copy"
                    onClick={() => handleCopyCode(inv.code)}
                    title="Copy code"
                  >
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="none">
                      <rect
                        x="5"
                        y="5"
                        width="9"
                        height="9"
                        rx="1.5"
                        stroke="currentColor"
                        strokeWidth="1.5"
                      />
                      <path
                        d="M11 5V3.5A1.5 1.5 0 009.5 2h-6A1.5 1.5 0 002 3.5v6A1.5 1.5 0 003.5 11H5"
                        stroke="currentColor"
                        strokeWidth="1.5"
                      />
                    </svg>
                  </button>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </Modal>
  );
};

export default InviteToServerModal;
