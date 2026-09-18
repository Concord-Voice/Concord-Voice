import React, { useState, useEffect, useRef } from 'react';
import { resolveMediaUrl } from '../../utils/ui/resolveMediaUrl';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useInviteStore } from '../../stores/chat/inviteStore';
import { useIsServerMember } from '../../hooks/messaging/useIsServerMember';
import { apiFetch } from '../../services/system/apiClient';
import { ServerWithRole, InviteInfoResponse } from '../../types/server';
import './JoinServerModal.css';

interface JoinServerModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSuccess: (server: ServerWithRole) => void;
  initialCode?: string | null;
}

const CODE_LENGTH = 8;

/**
 * A code that is not a valid invite may still be a FRIEND code — a different
 * feature with a different entry point — so the message names that rather than
 * saying "invalid" and leaving the user to guess.
 *
 * Lives at module scope rather than inside the preview effect deliberately.
 * Cognitive Complexity aggregates a nested function's branches into its
 * enclosing one, so the try/catch and its two arms counted against the effect
 * and pushed it to 16 against a limit of 15 (`typescript:S3776`). Hoisting is
 * the fix the rule is actually asking for; the alternatives were suppressing it
 * or tuning the threshold, both of which this repo forbids on AI-authored code.
 *
 * Returns the message instead of setting it, so it owns no React state and the
 * caller keeps sole responsibility for the currentness fence.
 */
async function describeUnknownCode(code: string): Promise<string> {
  try {
    const fcRes = await apiFetch(`/api/v1/friends/codes/${encodeURIComponent(code)}`);
    return fcRes.ok
      ? 'This looks like a friend code, not a server invite. Use the Add Friend button in Direct Messages to claim it.'
      : 'Invalid invite code';
  } catch {
    return 'Invalid invite code';
  }
}

const JoinServerModal: React.FC<JoinServerModalProps> = ({
  isOpen,
  onClose,
  onSuccess,
  initialCode,
}) => {
  const [code, setCode] = useState('');
  const [preview, setPreview] = useState<InviteInfoResponse | null>(null);
  const [isLoadingPreview, setIsLoadingPreview] = useState(false);
  const [isJoining, setIsJoining] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const previewTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Monotonic preview generation. Clearing the timer below only stops a request
  // that has not STARTED; `getInviteInfo` is a plain await with no cancellation,
  // so a request already in flight for a superseded code still resolves and its
  // continuation must ask whether it is still the current one (CodeRabbit, #3353).
  const previewGenerationRef = useRef(0);

  const joinServer = useInviteStore((state) => state.joinServer);
  const getInviteInfo = useInviteStore((state) => state.getInviteInfo);

  // This modal is also where `concord://invite/<code>` deep links land —
  // App.tsx feeds them in as `initialCode` — so this guard covers the reporter's
  // "shouldn't be allowed to join a server they're already a member of ... via
  // the app:// link" half as well as manual code entry (#2372).
  //
  // Strictly `=== true`: `undefined` means the control plane sent no
  // `server_id` and we cannot tell, which must leave Join offered.
  const alreadyMember = useIsServerMember(preview?.server_id) === true;

  // Auto-focus input when modal opens
  useEffect(() => {
    if (!isOpen) return;
    const focusTimer = setTimeout(() => inputRef.current?.focus(), 100);
    return () => clearTimeout(focusTimer);
  }, [isOpen]);

  useEffect(() => {
    if (!isOpen || !initialCode) return;
    // Filtered like typed input is. A deep-link code is validated in the MAIN
    // process (`deepLink.ts`), but that regex is one of three independent copies
    // this repo's own comment says #1557's vanity slugs must relax together — so
    // the renderer does not restate the trust, it re-applies the filter
    // (security review, PR #3353).
    //
    // The disable directive sits DIRECTLY above the call on purpose: it is
    // positional, so prose between it and `setCode` silently disarms it and
    // ESLint reports that only as a warning.
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: seeds the modal from a deep-link invite code when opened; not a render loop
    setCode(initialCode.replaceAll(/[^a-zA-Z0-9]/g, '').slice(0, CODE_LENGTH));
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears stale join success when a new deep-link invite is loaded; not a render loop
    setSuccessMessage(null);
  }, [isOpen, initialCode]);

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
    // Every run of this effect supersedes whatever preview work preceded it.
    previewGenerationRef.current += 1;
    const generation = previewGenerationRef.current;
    const isCurrentPreview = () => previewGenerationRef.current === generation;

    if (code.length === CODE_LENGTH) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: shows loading state while fetching invite preview; not a render loop
      setIsLoadingPreview(true);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when starting preview fetch; not a render loop
      setError(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears preview when starting a new fetch; not a render loop
      setPreview(null);

      previewTimeoutRef.current = setTimeout(async () => {
        const info = await getInviteInfo(code);
        // A superseded lookup writes NOTHING. Letting it through swapped the card
        // to the previous code's server while the input read the new one — and
        // since `alreadyMember` is derived from `preview.server_id`, the stale row
        // also aimed this modal's membership guard at the wrong server.
        if (!isCurrentPreview()) return;
        setIsLoadingPreview(false);
        if (info) {
          if (info.valid) {
            setPreview(info);
          } else {
            setError('This invite is no longer valid (expired, revoked, or used up)');
          }
        } else {
          const message = await describeUnknownCode(code);
          // Second await, so the fence is re-asked rather than assumed to hold
          // across it. It is asked HERE, once, rather than inside the helper:
          // the helper returns a value and touches no state, so there is exactly
          // one place a stale answer could be written and exactly one guard.
          if (!isCurrentPreview()) return;
          setError(message);
        }
      }, 300);
    } else {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears preview when code is incomplete; not a render loop
      setPreview(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when code is incomplete; not a render loop
      setError(null);
      // Required BY the fence above, not incidental to it: an in-flight lookup for
      // the previous complete code used to be what turned the spinner off on its
      // way past. Now that it is fenced out, nothing else would, and the modal sat
      // on "Looking up invite..." forever after a single backspace.
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: stops the spinner when the code is incomplete; not a render loop
      setIsLoadingPreview(false);
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
    // The disabled button is the visible half; this is the half that survives a
    // form submit reaching here another way (Enter in the code input).
    if (code.length !== CODE_LENGTH || !preview?.valid || alreadyMember) return;

    setIsJoining(true);
    setError(null);

    const outcome = await joinServer(code);
    if (outcome.status === 'joined') {
      const serverWithRole: ServerWithRole = {
        ...outcome.response.server,
        role: outcome.response.role as ServerWithRole['role'],
        member_count: 0,
        online_count: 0,
      };
      setSuccessMessage(`Joined ${outcome.response.server.name}!`);

      setTimeout(() => {
        onSuccess(serverWithRole);
        onClose();
      }, 800);
      return;
    }
    setIsJoining(false);
    // `abandoned` means a different account owns the session now — the join
    // happened for the ORIGINAL user, so say nothing to whoever is sitting here.
    //
    // The reason comes from the OUTCOME rather than `inviteStore.error`, which
    // this modal used to read back after its await. That was safe here only
    // because one modal exists at a time, which is an argument about the caller
    // rather than the store — and `InviteEmbed`, which renders one card per
    // invite link, broke it. Reading per call makes the safety structural
    // instead of circumstantial (Gitar, PR #3353).
    if (outcome.status === 'failed') {
      setError(outcome.reason);
      return;
    }
    if (outcome.status === 'abandoned') return;
    // Exhaustiveness sink. Without it a fourth JoinServerOutcome member compiles
    // cleanly here and produces a dead button press — spinner cleared, no error,
    // no success (code review, PR #3353).
    const unreachable: never = outcome;
    return unreachable;
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

        {/* Already a member — a statement of fact, not a failure, so it is a
            neutral note rather than the error banner. */}
        {alreadyMember && preview && !successMessage && (
          <div className="join-preview-note">
            You&apos;re already a member of {preview.server_name}.
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
              code.length !== CODE_LENGTH ||
              !preview?.valid ||
              isJoining ||
              !!successMessage ||
              alreadyMember
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
