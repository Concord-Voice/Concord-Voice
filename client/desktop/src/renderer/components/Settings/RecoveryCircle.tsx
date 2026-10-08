import React, { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { split } from '../../utils/crypto/shamir';
import { base64ToArrayBuffer, arrayBufferToBase64 } from '../../utils/crypto/crypto';
import { e2eeService } from '../../services/e2ee/e2eeService';
import {
  apiFetchInContext,
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { useFriendStore, Friend } from '../../stores/chat/friendStore';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import {
  useStepUpFactor,
  type StepUpPhase,
  type StepUpSubmit,
} from '../../hooks/auth/useStepUpFactor';
import ErrorBanner from './ErrorBanner';
import { stepUpBanner, submitMfaStepUp, toStepUpSubmitOutcome } from './mfaStepUp';

interface RecoveryCircleProps {
  onComplete: () => void;
  onCancel: () => void;
}

interface EncryptedShare {
  contact_id: string;
  share_index: number;
  encrypted_share: string;
}

/** The raw PKCS8 bytes of the account's private key, unwrapped from the key held in memory. */
async function exportPrivateKeyBytes(): Promise<Uint8Array> {
  const wrappingKey = e2eeService.getWrappingKey();
  const wrappedPrivateKeyBase64 = e2eeService.getWrappedPrivateKey();
  if (!wrappingKey || !wrappedPrivateKeyBase64) {
    throw new Error('E2EE keys not available');
  }

  // wrappingKey only supports wrapKey/unwrapKey, not decrypt
  const wrappedData = new Uint8Array(base64ToArrayBuffer(wrappedPrivateKeyBase64));
  const iv = wrappedData.slice(0, 12);
  const ciphertext = wrappedData.slice(12);
  const privateKeyForExport = await crypto.subtle.unwrapKey(
    'pkcs8',
    ciphertext,
    wrappingKey,
    { name: 'AES-GCM', iv },
    { name: 'RSA-OAEP', hash: 'SHA-256' },
    true, // extractable so we can re-export
    ['decrypt']
  );
  return new Uint8Array(await crypto.subtle.exportKey('pkcs8', privateKeyForExport));
}

/**
 * RSA-OAEP-wraps a raw AES key, then zeroes the raw bytes whether or not the
 * wrap succeeded. WebCrypto copies its input when called, so nothing still
 * reads them.
 */
async function wrapRawKey(publicKey: CryptoKey, rawKey: ArrayBuffer): Promise<ArrayBuffer> {
  try {
    return await crypto.subtle.encrypt({ name: 'RSA-OAEP' }, publicKey, rawKey);
  } finally {
    new Uint8Array(rawKey).fill(0);
  }
}

/**
 * Fetches `contactId`'s public key under `context` and encrypts `share` to it.
 * The plaintext share is zeroed once this settles, sent or not, its raw AES key
 * once wrapped (`wrapRawKey`), and nothing past the fetch runs once `signal`
 * has aborted.
 */
async function encryptShareFor(
  contactId: string,
  share: { index: number; data: Uint8Array },
  context: ApiRequestContext,
  signal: AbortSignal
): Promise<EncryptedShare> {
  try {
    const pkRes = await apiFetchInContext(
      `/api/v1/users/${contactId}/public-key`,
      { signal },
      context
    );
    if (!pkRes.ok) throw new Error('Failed to fetch public key for contact');
    const pkData = await pkRes.json();
    signal.throwIfAborted();

    const publicKeyBytes = base64ToArrayBuffer(pkData.public_key);
    const publicKey = await crypto.subtle.importKey(
      'spki',
      publicKeyBytes,
      { name: 'RSA-OAEP', hash: 'SHA-256' },
      false,
      ['encrypt']
    );

    // Hybrid encryption: AES-GCM encrypt share, RSA-OAEP encrypt the AES key
    // (RSA-OAEP with 4096-bit key can only encrypt ~446 bytes, but shares are ~3.4KB)
    const aesKey = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, [
      'encrypt',
    ]);
    const shareIv = crypto.getRandomValues(new Uint8Array(12));
    const shareCiphertext = await crypto.subtle.encrypt(
      { name: 'AES-GCM', iv: shareIv },
      aesKey,
      share.data.buffer as ArrayBuffer
    );
    const exportedAesKey = await crypto.subtle.exportKey('raw', aesKey);
    const encryptedAesKey = await wrapRawKey(publicKey, exportedAesKey);
    const hybridPayload = JSON.stringify({
      k: arrayBufferToBase64(encryptedAesKey),
      iv: arrayBufferToBase64(shareIv.buffer),
      c: arrayBufferToBase64(shareCiphertext),
    });
    const encodedPayload = new TextEncoder().encode(hybridPayload);

    return {
      contact_id: contactId,
      share_index: share.index,
      encrypted_share: arrayBufferToBase64(encodedPayload.buffer),
    };
  } finally {
    share.data.fill(0);
  }
}

/**
 * Everything the save sends except the credentials: the Shamir split of the
 * private key, one share per contact, each encrypted to that contact. It runs
 * when the confirm step opens (D11), so the PUT is the only thing left to do
 * once the user has proven who they are (C9). Leaving the step aborts
 * `signal`, which stops it between steps and cancels its fetches.
 */
async function prepareShares(
  contactIds: readonly string[],
  threshold: number,
  context: ApiRequestContext,
  signal: AbortSignal
): Promise<EncryptedShare[]> {
  const secret = await exportPrivateKeyBytes();
  let shares: ReturnType<typeof split>;
  try {
    signal.throwIfAborted();
    shares = split(secret, contactIds.length, threshold);
  } finally {
    // The shares carry everything these bytes did.
    secret.fill(0);
  }
  return Promise.all(
    contactIds.map((contactId, i) => encryptShareFor(contactId, shares[i], context, signal))
  );
}

/** The primary's label by phase. */
const PRIMARY_LABEL: Record<StepUpPhase, string> = {
  idle: 'Create Recovery Circle',
  ceremony: 'Waiting…',
  submitting: 'Setting up...',
};

interface RecoveryCircleConfirmProps {
  contactIds: readonly string[];
  threshold: number;
  onBack: () => void;
  /** Preparation failed; nothing was sent. Stable, because preparation restarts if it changes. */
  onPrepareFailed: (message: string) => void;
  onDone: () => void;
}

/**
 * The confirm step (#10). Mounting is the step opening: it takes the capture
 * `run` works against (C82) and starts preparing at once, and the primary stays
 * down until preparation resolves. A reopened step is a new mount, so it
 * prepares again. The click handler awaits nothing before `run` (Q6), and
 * `submit` only transmits.
 *
 * The password is local state and the code lives in the factor hook, so both
 * are dropped with the step.
 */
const RecoveryCircleConfirm: React.FC<RecoveryCircleConfirmProps> = ({
  contactIds,
  threshold,
  onBack,
  onPrepareFailed,
  onDone,
}) => {
  const [capture] = useState(captureApiRequestContext);
  const [shares, setShares] = useState<EncryptedShare[] | null>(null);
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const headingRef = useRef<HTMLHeadingElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);

  // The select step's Continue went with that step, so focus starts on the
  // password, as on the other inline credential steps.
  useEffect(() => {
    passwordRef.current?.focus();
  }, []);

  useEffect(() => {
    const preparation = new AbortController();
    const { signal } = preparation;
    prepareShares(contactIds, threshold, capture, signal).then(
      (prepared) => {
        if (!signal.aborted) setShares(prepared);
      },
      (err: unknown) => {
        if (!signal.aborted) onPrepareFailed(err instanceof Error ? err.message : 'Setup failed');
      }
    );
    return () => preparation.abort();
  }, [contactIds, threshold, capture, onPrepareFailed]);

  const factor = useStepUpFactor({
    enabled: true,
    purpose: 'mfa_settings.recovery_circle_upsert',
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'passwordOnly',
    allowBackup: true,
    preparing: shares === null,
  });

  const submit: StepUpSubmit = async (mfa, context) => {
    // Unreachable: the primary is down until `shares` is set. Nothing is sent.
    if (shares === null) return { kind: 'aborted' };
    const result = await submitMfaStepUp(
      '/api/v1/mfa/recovery-circle',
      'PUT',
      { threshold_k: threshold, total_shares_n: contactIds.length, shares },
      { password, mfaCode: mfa },
      { context }
    );
    // An answer for an account or server that is no longer current is not this step's.
    if (apiRequestContextIsCurrent(context)) {
      setError(stepUpBanner(result) ?? '');
      // Only the password the server refused is dropped. It answers
      // `password_required` to an empty one, which this step never sends.
      if (result.kind === 'invalidPassword') setPassword('');
      if (result.kind === 'accepted') onDone();
    }
    return toStepUpSubmitOutcome(result);
  };

  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit, { capture });

  return (
    <div className="mfa-setup-wizard">
      <h3 tabIndex={-1} ref={headingRef}>
        Confirm Recovery Circle
      </h3>
      <div className="mfa-setup-step">
        <p>
          <strong>
            {threshold} of {contactIds.length}
          </strong>{' '}
          contacts will be needed to recover your account. Verify your identity to create the
          recovery circle.
        </p>
        <StepUpCredentials
          factor={factor}
          password={password}
          onPasswordChange={setPassword}
          primaryRef={primaryRef}
          passwordRef={passwordRef}
          headingRef={headingRef}
        />
        <ErrorBanner error={error} />
        <div className="mfa-setup-actions">
          <button
            ref={primaryRef}
            type="button"
            className="btn btn-primary"
            aria-disabled={ariaDisabled || undefined}
            onClick={activate}
          >
            {PRIMARY_LABEL[factor.phase]}
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            disabled={factor.phase === 'submitting'}
            onClick={onBack}
          >
            Back
          </button>
        </div>
      </div>
    </div>
  );
};

const RecoveryCircle: React.FC<RecoveryCircleProps> = ({ onComplete, onCancel }) => {
  const friends = useFriendStore((s) => s.friends);
  const fetchFriends = useFriendStore((s) => s.fetchFriends);

  const [selectedContacts, setSelectedContacts] = useState<string[]>([]);
  const [threshold, setThreshold] = useState(3);
  const [error, setError] = useState('');
  const [step, setStep] = useState<'select' | 'confirm' | 'done'>('select');
  // Leaving the confirm step unmounts whatever held focus there, so the step
  // it lands on, select or done, gives its heading focus instead of <body>. A
  // layout effect, so focus moves in the commit that removes the old step.
  const headingRef = useRef<HTMLHeadingElement>(null);
  const previousStepRef = useRef(step);

  useEffect(() => {
    fetchFriends();
  }, [fetchFriends]);

  useLayoutEffect(() => {
    const left = previousStepRef.current;
    previousStepRef.current = step;
    if (left === 'confirm') headingRef.current?.focus();
  }, [step]);

  const toggleContact = (userId: string) => {
    setSelectedContacts((prev) => {
      if (prev.includes(userId)) return prev.filter((id) => id !== userId);
      if (prev.length < 7) return [...prev, userId];
      return prev;
    });
  };

  const openConfirm = () => {
    setError('');
    setStep('confirm');
  };
  const backToSelect = useCallback(() => setStep('select'), []);
  // A failed contact lookup is mended on the select step, where the contacts
  // are chosen; Continue prepares again.
  const failPreparation = useCallback((message: string) => {
    setError(message);
    setStep('select');
  }, []);
  const finish = useCallback(() => setStep('done'), []);

  if (step === 'done') {
    return (
      <div className="mfa-setup-wizard">
        <h3 tabIndex={-1} ref={headingRef}>
          Recovery Circle Configured
        </h3>
        <div className="mfa-setup-step mfa-setup-success">
          <h4>Recovery Circle Active</h4>
          <p>
            {threshold} of {selectedContacts.length} trusted contacts can help you recover your
            account. No single contact can access your data.
          </p>
          <button className="btn btn-primary" onClick={onComplete}>
            Done
          </button>
        </div>
      </div>
    );
  }

  if (step === 'confirm') {
    return (
      <RecoveryCircleConfirm
        contactIds={selectedContacts}
        threshold={threshold}
        onBack={backToSelect}
        onPrepareFailed={failPreparation}
        onDone={finish}
      />
    );
  }

  return (
    <div className="mfa-setup-wizard">
      <h3 tabIndex={-1} ref={headingRef}>
        Set Up Recovery Circle
      </h3>
      <div className="mfa-setup-step">
        <p>
          Select trusted contacts who can help you recover your account. Your private key will be
          split using Shamir&apos;s Secret Sharing — no single contact can access your data.
        </p>

        <div style={{ marginBottom: 16 }}>
          <label htmlFor="recovery-threshold" className="form-label">
            Recovery threshold: {threshold} of {selectedContacts.length || '?'}
          </label>
          <input
            id="recovery-threshold"
            type="range"
            min={2}
            max={Math.max(2, selectedContacts.length)}
            value={threshold}
            onChange={(e) => setThreshold(Number(e.target.value))}
            style={{ width: '100%' }}
            disabled={selectedContacts.length < 2}
          />
          <p style={{ color: 'var(--text-secondary)', fontSize: 12, margin: '4px 0 0' }}>
            Select at least 2 contacts (max 7). Threshold must be at least 2.
          </p>
        </div>

        <div
          style={{
            maxHeight: 300,
            overflowY: 'auto',
            border: '1px solid var(--border-color, #2d3748)',
            borderRadius: 8,
            padding: 8,
          }}
        >
          {friends.length === 0 ? (
            <p style={{ color: 'var(--text-secondary)', textAlign: 'center', padding: 16 }}>
              No friends found. Add friends to set up a recovery circle.
            </p>
          ) : (
            friends.map((friend: Friend) => (
              <label
                key={friend.userId}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 8,
                  padding: '8px 12px',
                  cursor: 'pointer',
                  borderRadius: 6,
                  background: selectedContacts.includes(friend.userId)
                    ? 'rgba(59, 130, 246, 0.15)'
                    : 'transparent',
                }}
              >
                <input
                  type="checkbox"
                  checked={selectedContacts.includes(friend.userId)}
                  onChange={() => toggleContact(friend.userId)}
                />
                <span style={{ color: 'var(--text-primary)' }}>
                  {friend.displayName || friend.username}
                </span>
                <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
                  @{friend.username}
                </span>
              </label>
            ))
          )}
        </div>

        <ErrorBanner error={error} />
        <div className="mfa-setup-actions">
          <button
            className="btn btn-primary"
            disabled={
              selectedContacts.length < 2 || threshold < 2 || threshold > selectedContacts.length
            }
            onClick={openConfirm}
          >
            Continue
          </button>
          <button className="btn btn-secondary" onClick={onCancel}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
};

export default RecoveryCircle;
