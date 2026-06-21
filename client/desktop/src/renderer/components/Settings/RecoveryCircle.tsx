import React, { useState, useEffect } from 'react';
import { split } from '../../utils/shamir';
import { base64ToArrayBuffer, arrayBufferToBase64 } from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import { apiFetch } from '../../services/apiClient';
import { useFriendStore, Friend } from '../../stores/friendStore';

interface RecoveryCircleProps {
  onComplete: () => void;
  onCancel: () => void;
}

const RecoveryCircle: React.FC<RecoveryCircleProps> = ({ onComplete, onCancel }) => {
  const friends = useFriendStore((s) => s.friends);
  const fetchFriends = useFriendStore((s) => s.fetchFriends);

  const [selectedContacts, setSelectedContacts] = useState<string[]>([]);
  const [threshold, setThreshold] = useState(3);
  const [password, setPassword] = useState('');
  const [mfaCode, setMfaCode] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [step, setStep] = useState<'select' | 'confirm' | 'done'>('select');

  useEffect(() => {
    fetchFriends();
  }, [fetchFriends]);

  const toggleContact = (userId: string) => {
    setSelectedContacts((prev) => {
      if (prev.includes(userId)) return prev.filter((id) => id !== userId);
      if (prev.length < 7) return [...prev, userId];
      return prev;
    });
  };

  const handleSetup = async () => {
    setLoading(true);
    setError('');
    try {
      // 1. Get the wrapped private key and wrapping key
      const wrappingKey = e2eeService.getWrappingKey();
      const wrappedPrivateKeyBase64 = e2eeService.getWrappedPrivateKey();
      if (!wrappingKey || !wrappedPrivateKeyBase64) {
        throw new Error('E2EE keys not available');
      }

      // 2. Unwrap to get raw PKCS8 bytes (wrappingKey only supports wrapKey/unwrapKey, not decrypt)
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
      const rawPkcs8 = await crypto.subtle.exportKey('pkcs8', privateKeyForExport);

      // 3. Split using Shamir SSS
      const secretBytes = new Uint8Array(rawPkcs8);
      const shares = split(secretBytes, selectedContacts.length, threshold);

      // 4. For each contact, fetch their public key and encrypt the share
      const encryptedShares: Array<{
        contact_id: string;
        share_index: number;
        encrypted_share: string;
      }> = [];

      for (let i = 0; i < selectedContacts.length; i++) {
        const contactId = selectedContacts[i];
        const share = shares[i];

        // Fetch contact's public key
        const pkRes = await apiFetch(`/api/v1/users/${contactId}/public-key`);
        if (!pkRes.ok) throw new Error('Failed to fetch public key for contact');
        const pkData = await pkRes.json();

        // Import the public key
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
        const encryptedAesKey = await crypto.subtle.encrypt(
          { name: 'RSA-OAEP' },
          publicKey,
          exportedAesKey
        );
        const hybridPayload = JSON.stringify({
          k: arrayBufferToBase64(encryptedAesKey),
          iv: arrayBufferToBase64(shareIv.buffer),
          c: arrayBufferToBase64(shareCiphertext),
        });
        const encodedPayload = new TextEncoder().encode(hybridPayload);

        encryptedShares.push({
          contact_id: contactId,
          share_index: share.index,
          encrypted_share: arrayBufferToBase64(encodedPayload.buffer),
        });
      }

      // 5. Upload to server
      const res = await apiFetch('/api/v1/mfa/recovery-circle', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          password,
          mfa_code: mfaCode || undefined,
          threshold_k: threshold,
          total_shares_n: selectedContacts.length,
          shares: encryptedShares,
        }),
      });
      if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error || 'Failed to configure recovery circle');
      }

      setStep('done');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Setup failed');
    } finally {
      setLoading(false);
    }
  };

  if (step === 'done') {
    return (
      <div className="mfa-setup-wizard">
        <h3>Recovery Circle Configured</h3>
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
      <div className="mfa-setup-wizard">
        <h3>Confirm Recovery Circle</h3>
        <div className="mfa-setup-step">
          <p>
            <strong>
              {threshold} of {selectedContacts.length}
            </strong>{' '}
            contacts will be needed to recover your account. Verify your identity to create the
            recovery circle.
          </p>
          <input
            type="password"
            className="form-input"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Your password"
            disabled={loading}
          />
          <input
            type="text"
            className="form-input"
            value={mfaCode}
            onChange={(e) => setMfaCode(e.target.value)}
            placeholder="MFA code (if enabled)"
            disabled={loading}
            style={{ marginTop: 8 }}
          />
          {error && <p className="mfa-setup-error">{error}</p>}
          <div className="mfa-setup-actions">
            <button
              className="btn btn-primary"
              onClick={handleSetup}
              disabled={loading || !password}
            >
              {loading ? 'Setting up...' : 'Create Recovery Circle'}
            </button>
            <button className="btn btn-secondary" onClick={() => setStep('select')}>
              Back
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="mfa-setup-wizard">
      <h3>Set Up Recovery Circle</h3>
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
          <p style={{ color: 'var(--text-tertiary, #718096)', fontSize: 12, margin: '4px 0 0' }}>
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
            <p style={{ color: 'var(--text-tertiary)', textAlign: 'center', padding: 16 }}>
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
                <span style={{ color: 'var(--text-tertiary)', fontSize: 12 }}>
                  @{friend.username}
                </span>
              </label>
            ))
          )}
        </div>

        {error && <p className="mfa-setup-error">{error}</p>}
        <div className="mfa-setup-actions">
          <button
            className="btn btn-primary"
            disabled={
              selectedContacts.length < 2 || threshold < 2 || threshold > selectedContacts.length
            }
            onClick={() => setStep('confirm')}
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
