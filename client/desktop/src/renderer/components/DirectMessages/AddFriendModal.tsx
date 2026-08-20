import React, { useState, useRef, useCallback, useEffect } from 'react';
import { Copy, Check, UserPlus, Search, Trash2 } from 'lucide-react';
import {
  useFriendStore,
  type FriendCodePreview,
  type SearchResult,
} from '../../stores/friendStore';
import { usePrivacyStore } from '../../stores/privacyStore';
import { apiFetch } from '../../services/apiClient';
import Modal from '../ui/Modal';
import { errorMessage } from '../../utils/redactError';
import CustomSelect from '../ui/CustomSelect';
import './DirectMessages.css';

interface AddFriendModalProps {
  isOpen: boolean;
  onClose: () => void;
  /** Pre-fills the friend-code field, e.g. from a concord://friend/CODE deep link (#945). */
  initialCode?: string | null;
}

const EXPIRY_OPTIONS = [
  { label: '30 minutes', value: 1800 },
  { label: '1 hour', value: 3600 },
  { label: '6 hours', value: 21600 },
  { label: '12 hours', value: 43200 },
  { label: '1 day', value: 86400 },
];

const MAX_USES_OPTIONS = [
  { label: '1 use', value: 1 },
  { label: '3 uses', value: 3 },
  { label: '5 uses', value: 5 },
  { label: '10 uses', value: 10 },
];

const USER_SEARCH_MIN_CHARS = 2;

const AddFriendModal: React.FC<AddFriendModalProps> = ({ isOpen, onClose, initialCode }) => {
  // Friend code claim
  const [codeInput, setCodeInput] = useState('');
  const [codePreview, setCodePreview] = useState<FriendCodePreview | null>(null);
  const [codeError, setCodeError] = useState('');
  const [revokeError, setRevokeError] = useState('');
  const codeInputRef = useRef<HTMLInputElement | null>(null);
  const [claimStatus, setClaimStatus] = useState('');
  const [isClaiming, setIsClaiming] = useState(false);

  // User search
  const [searchQuery, setSearchQuery] = useState('');
  const [searchResults, setSearchResults] = useState<SearchResult[]>([]);
  const [searchError, setSearchError] = useState('');
  const [hasSearchedUsers, setHasSearchedUsers] = useState(false);
  const [sendingTo, setSendingTo] = useState<string | null>(null);
  const searchTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const latestSearchQueryRef = useRef('');

  // Code generation
  const [expiresIn, setExpiresIn] = useState(3600);
  const [maxUses, setMaxUses] = useState(1);
  const [autoAccept, setAutoAccept] = useState(false);
  const [generatedCode, setGeneratedCode] = useState('');
  const [isGenerating, setIsGenerating] = useState(false);
  const [copied, setCopied] = useState(false);

  const friendCodes = useFriendStore((s) => s.friendCodes);
  const previewFriendCode = useFriendStore((s) => s.previewFriendCode);
  const claimFriendCode = useFriendStore((s) => s.claimFriendCode);
  const generateFriendCode = useFriendStore((s) => s.generateFriendCode);
  const revokeFriendCode = useFriendStore((s) => s.revokeFriendCode);
  const fetchFriendCodes = useFriendStore((s) => s.fetchFriendCodes);
  const searchUsers = useFriendStore((s) => s.searchUsers);
  const sendRequest = useFriendStore((s) => s.sendRequest);
  const privacyAutoAccept = usePrivacyStore((s) => s.settings.autoAcceptFriendCodes);

  // Fetch codes when modal opens
  React.useEffect(() => {
    if (isOpen) {
      fetchFriendCodes();
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: syncs autoAccept from privacy store when modal opens; not a render loop
      setAutoAccept(privacyAutoAccept);
    }
  }, [isOpen, fetchFriendCodes, privacyAutoAccept]);

  // Check whether a code is actually a server invite (not a friend code)
  const checkIfServerInvite = useCallback(async (code: string): Promise<boolean> => {
    try {
      const invRes = await apiFetch(`/api/v1/invites/${code}`);
      return invRes.ok;
    } catch {
      return false;
    }
  }, []);

  // Preview friend code on input
  const handleCodeInput = useCallback(
    async (value: string) => {
      setCodeInput(value);
      setCodeError('');
      setCodePreview(null);
      setClaimStatus('');

      const trimmed = value.trim();
      if (trimmed.length !== 8) return;

      try {
        const preview = await previewFriendCode(trimmed);
        setCodePreview(preview);
        if (!preview.valid) setCodeError('This code is expired or has been used');
      } catch (err) {
        const isServerInvite = await checkIfServerInvite(trimmed);
        if (isServerInvite) {
          setCodeError(
            'This looks like a server invite code, not a friend code. Use the Join Server button to use it.'
          );
        } else {
          setCodeError(err instanceof Error ? err.message : 'Invalid code');
        }
      }
    },
    [previewFriendCode, checkIfServerInvite]
  );

  // Prefill from a deep link. Never auto-claims: the user still clicks
  // Send Friend Request, so opening a link stays a preview, not an action (#945).
  useEffect(() => {
    if (!isOpen || !initialCode) return;
    void handleCodeInput(initialCode);
    const input = codeInputRef.current;
    if (input) {
      input.focus();
      input.setSelectionRange(initialCode.length, initialCode.length);
    }
  }, [isOpen, initialCode, handleCodeInput]);

  // Claim friend code
  const handleClaim = useCallback(async () => {
    if (!codeInput.trim() || isClaiming) return;
    setIsClaiming(true);
    setClaimStatus('');
    try {
      const result = await claimFriendCode(codeInput.trim());
      setClaimStatus(
        result.status === 'accepted'
          ? `You are now friends with ${result.user.username}!`
          : `Friend request sent to ${result.user.username}`
      );
      setCodeInput('');
      setCodePreview(null);
    } catch (err) {
      setCodeError(err instanceof Error ? err.message : 'Failed to claim code');
    } finally {
      setIsClaiming(false);
    }
  }, [codeInput, isClaiming, claimFriendCode]);

  // Search users
  const handleSearch = useCallback(
    (value: string) => {
      setSearchQuery(value);
      setSearchError('');
      setHasSearchedUsers(false);

      if (searchTimerRef.current) clearTimeout(searchTimerRef.current);

      const trimmed = value.trim();
      latestSearchQueryRef.current = trimmed;
      if (trimmed.length < USER_SEARCH_MIN_CHARS) {
        setSearchResults([]);
        return;
      }

      searchTimerRef.current = setTimeout(async () => {
        try {
          const results = await searchUsers(trimmed);
          if (latestSearchQueryRef.current !== trimmed) return;
          setSearchResults(results.slice(0, 8));
          setHasSearchedUsers(true);
        } catch {
          if (latestSearchQueryRef.current === trimmed) setSearchError('Search failed');
        }
      }, 300);
    },
    [searchUsers]
  );

  // Send friend request from search
  const handleSendRequest = useCallback(
    async (userId: string) => {
      setSendingTo(userId);
      try {
        await sendRequest(userId);
        setSearchResults((prev) => prev.filter((r) => r.id !== userId));
      } catch (err) {
        setSearchError(err instanceof Error ? err.message : 'Failed to send request');
      } finally {
        setSendingTo(null);
      }
    },
    [sendRequest]
  );

  // Generate friend code
  const handleGenerate = useCallback(async () => {
    if (isGenerating) return;
    setIsGenerating(true);
    try {
      const code = await generateFriendCode({ maxUses, expiresIn, autoAccept });
      setGeneratedCode(code.code);
    } catch (err) {
      console.error('Failed to generate code:', errorMessage(err));
    } finally {
      setIsGenerating(false);
    }
  }, [isGenerating, generateFriendCode, maxUses, expiresIn, autoAccept]);

  // Copy code to clipboard
  const handleCopy = useCallback(
    async (code?: string) => {
      const text = code || generatedCode;
      if (!text) return;
      try {
        await navigator.clipboard.writeText(text);
        setCopied(true);
        setTimeout(() => setCopied(false), 2000);
      } catch {
        // Clipboard unavailable — user can select the code manually
      }
    },
    [generatedCode]
  );

  // Revoke code
  const handleRevoke = useCallback(
    async (id: string) => {
      setRevokeError('');
      try {
        await revokeFriendCode(id);
      } catch (err) {
        // A friend code is a PUBLIC URL that anonymously resolves the owner's
        // username, display name and avatar (#945). Revoke is the only control a
        // user has to take that disclosure offline, so swallowing the failure
        // leaves them believing a live code is dead — 401 after token expiry,
        // 403, 404, 429, 500 and offline were all invisible before this.
        setRevokeError(
          err instanceof Error
            ? `Failed to revoke: ${err.message}. The code may still be active.`
            : 'Failed to revoke. The code may still be active.'
        );
      }
    },
    [revokeFriendCode]
  );

  return (
    <Modal isOpen={isOpen} onClose={onClose} title="Add Friend" width="large">
      <div className="add-friend-modal">
        {/* Section 1: Add a Friend */}
        <div className="add-friend-section">
          <h4 className="add-friend-section-title">Add by Friend Code</h4>
          <label className="sr-only" htmlFor="add-friend-code-input">
            Friend code
          </label>
          <div className="add-friend-code-input-row">
            <input
              id="add-friend-code-input"
              ref={codeInputRef}
              type="text"
              className="add-friend-input"
              placeholder="Enter 8-character code..."
              value={codeInput}
              onChange={(e) => handleCodeInput(e.target.value)}
              maxLength={8}
            />
          </div>

          {codeError && (
            <div className="add-friend-error" role="alert">
              {codeError}
            </div>
          )}
          {/* <output> is the native live region for the result of an operation and
              carries role="status" implicitly — preferred over an ARIA role on a div
              (sonar typescript:S6819). */}
          {claimStatus && <output className="add-friend-success">{claimStatus}</output>}

          {codePreview?.valid && (
            <output className="friend-code-preview">
              <div className="member-avatar">
                <span className="member-avatar-initial">
                  {(codePreview.displayName || codePreview.username).charAt(0).toUpperCase()}
                </span>
              </div>
              <span className="friend-code-preview-name">
                {codePreview.displayName || codePreview.username}
              </span>
              <button className="add-friend-action-btn" onClick={handleClaim} disabled={isClaiming}>
                <UserPlus size={14} />
                {isClaiming ? 'Sending...' : 'Send Friend Request'}
              </button>
            </output>
          )}

          <div className="add-friend-divider" />

          <h4 className="add-friend-section-title">
            <Search size={14} /> Search for a User
          </h4>
          <input
            type="text"
            className="add-friend-input"
            placeholder="Search by name or username (2+ characters)..."
            value={searchQuery}
            onChange={(e) => handleSearch(e.target.value)}
          />
          {searchError && (
            <div className="add-friend-error" role="alert">
              {searchError}
            </div>
          )}
          {hasSearchedUsers && !searchError && searchResults.length === 0 && (
            <div className="add-friend-empty">No users found</div>
          )}

          {searchResults.map((user) => (
            <div key={user.id} className="search-result-item">
              <div className="member-avatar">
                <span className="member-avatar-initial">
                  {(user.displayName || user.username).charAt(0).toUpperCase()}
                </span>
              </div>
              <span className="search-result-name">
                {user.displayName || user.username}
                {user.displayName && (
                  <span className="search-result-username">@{user.username}</span>
                )}
              </span>
              <button
                className="add-friend-action-btn"
                onClick={() => handleSendRequest(user.id)}
                disabled={sendingTo === user.id}
              >
                <UserPlus size={14} />
                {sendingTo === user.id ? 'Sending...' : 'Add Friend'}
              </button>
            </div>
          ))}
        </div>

        <div className="add-friend-divider" />

        {/* Section 2: Share Your Friend Code */}
        <div className="add-friend-section">
          <h4 className="add-friend-section-title">Share Your Friend Code</h4>

          <div className="add-friend-options">
            <div className="add-friend-option-group">
              <label htmlFor="friend-code-expires">Expires after</label>
              <CustomSelect
                id="friend-code-expires"
                options={EXPIRY_OPTIONS.map((opt) => ({
                  value: String(opt.value),
                  label: opt.label,
                }))}
                value={String(expiresIn)}
                onChange={(v) => setExpiresIn(Number(v))}
              />
            </div>
            <div className="add-friend-option-group">
              <label htmlFor="friend-code-max-uses">Max uses</label>
              <CustomSelect
                id="friend-code-max-uses"
                options={MAX_USES_OPTIONS.map((opt) => ({
                  value: String(opt.value),
                  label: opt.label,
                }))}
                value={String(maxUses)}
                onChange={(v) => setMaxUses(Number(v))}
              />
            </div>
            <div className="add-friend-option-group">
              <label htmlFor="auto-accept-toggle">Auto-accept</label>
              <span className="add-friend-toggle">
                <input
                  id="auto-accept-toggle"
                  type="checkbox"
                  checked={autoAccept}
                  onChange={(e) => setAutoAccept(e.target.checked)}
                />
                <span className="add-friend-toggle-track" />
                <span className="add-friend-toggle-thumb" />
              </span>
            </div>
          </div>

          <div className="add-friend-code-display">
            {generatedCode ? (
              <>
                <span className="invite-code-value">{generatedCode}</span>
                <button className="invite-code-copy-btn" onClick={() => handleCopy()}>
                  {copied ? <Check size={14} /> : <Copy size={14} />}
                  {copied ? 'Copied!' : 'Copy'}
                </button>
              </>
            ) : (
              <button
                className="invite-generate-btn"
                onClick={handleGenerate}
                disabled={isGenerating}
              >
                {isGenerating ? 'Generating...' : 'Generate Code'}
              </button>
            )}
          </div>

          {friendCodes.length > 0 && (
            <div className="add-friend-active-codes">
              <h5>Active Codes</h5>
              {revokeError && (
                <div className="add-friend-error" role="alert">
                  {revokeError}
                </div>
              )}
              {friendCodes.map((fc) => {
                const expired = fc.expiresAt && new Date(fc.expiresAt) < new Date();
                const maxed = fc.maxUses !== null && fc.useCount >= fc.maxUses;
                return (
                  <div
                    key={fc.id}
                    className={`add-friend-code-item${expired || maxed ? ' expired' : ''}`}
                  >
                    <span className="invite-code-value" style={{ fontSize: 13 }}>
                      {fc.code}
                    </span>
                    <span className="add-friend-code-meta">
                      {fc.useCount}/{fc.maxUses ?? '\u221e'} uses
                      {fc.autoAccept && ' \u00b7 auto-accept'}
                    </span>
                    <button
                      className="add-friend-revoke-btn"
                      onClick={() => handleRevoke(fc.id)}
                      title="Revoke"
                    >
                      <Trash2 size={12} />
                    </button>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>
    </Modal>
  );
};

export default AddFriendModal;
