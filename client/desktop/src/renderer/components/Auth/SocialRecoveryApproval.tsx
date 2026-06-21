import React, { useState } from 'react';
import {
  base64ToArrayBuffer,
  arrayBufferToBase64,
  importECDHPublicKey,
  generateECDHKeyPair,
  deriveSharedSecret,
  encryptWithSharedSecret,
} from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import { apiFetch } from '../../services/apiClient';

interface SocialRecoveryApprovalProps {
  requestId: string;
  requesterUsername: string;
  requesterDisplayName?: string;
  requesterEphemeralKey: string; // base64
  myEncryptedShare: string; // base64 — my share encrypted with MY public key (hybrid-encrypted)
  shareIndex: number; // original Shamir share index (from server's GetMyRecoveryShares)
  onClose: () => void;
}

const SocialRecoveryApproval: React.FC<SocialRecoveryApprovalProps> = ({
  requestId,
  requesterUsername,
  requesterDisplayName,
  requesterEphemeralKey,
  myEncryptedShare,
  shareIndex,
  onClose,
}) => {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [completed, setCompleted] = useState(false);

  const handleApprove = async () => {
    setLoading(true);
    setError('');
    try {
      // 1. Get our private key to decrypt the share
      const wrappingKey = e2eeService.getWrappingKey();
      const wrappedPrivateKeyBase64 = e2eeService.getWrappedPrivateKey();
      if (!wrappingKey || !wrappedPrivateKeyBase64) {
        throw new Error('E2EE keys not available');
      }

      // Unwrap our private key (wrappingKey only supports wrapKey/unwrapKey, not decrypt)
      const wrappedData = new Uint8Array(base64ToArrayBuffer(wrappedPrivateKeyBase64));
      const iv = wrappedData.slice(0, 12);
      const ciphertext = wrappedData.slice(12);
      const privateKey = await crypto.subtle.unwrapKey(
        'pkcs8',
        ciphertext,
        wrappingKey,
        { name: 'AES-GCM', iv },
        { name: 'RSA-OAEP', hash: 'SHA-256' },
        false,
        ['decrypt']
      );

      // 2. Hybrid decryption: parse JSON payload, RSA-OAEP decrypt AES key, AES-GCM decrypt share
      const payloadStr = new TextDecoder().decode(base64ToArrayBuffer(myEncryptedShare));
      const hybridPayload = JSON.parse(payloadStr);

      // RSA-OAEP decrypt the AES key
      const encryptedAesKeyBytes = base64ToArrayBuffer(hybridPayload.k);
      const aesKeyRaw = await crypto.subtle.decrypt(
        { name: 'RSA-OAEP' },
        privateKey,
        encryptedAesKeyBytes
      );
      const aesKey = await crypto.subtle.importKey('raw', aesKeyRaw, { name: 'AES-GCM' }, false, [
        'decrypt',
      ]);

      // AES-GCM decrypt the share
      const shareIv = new Uint8Array(base64ToArrayBuffer(hybridPayload.iv));
      const shareCiphertext = base64ToArrayBuffer(hybridPayload.c);
      const decryptedShare = await crypto.subtle.decrypt(
        { name: 'AES-GCM', iv: shareIv },
        aesKey,
        shareCiphertext
      );

      // 3. Re-encrypt with requester's ephemeral ECDH public key
      const requesterKey = await importECDHPublicKey(requesterEphemeralKey);
      const ecdhKeyPair = await generateECDHKeyPair();
      const sharedKey = await deriveSharedSecret(ecdhKeyPair.privateKey, requesterKey);
      const reEncryptedShare = await encryptWithSharedSecret(sharedKey, decryptedShare);

      // Include our ECDH public key so requester can derive the same shared secret
      const ourPubKey = await crypto.subtle.exportKey('raw', ecdhKeyPair.publicKey);
      const ourPubKeyBase64 = arrayBufferToBase64(ourPubKey);

      // 4. Send to server
      // The encrypted_share field contains: JSON { ephemeral_public_key, encrypted_data, share_index }
      const payload = JSON.stringify({
        ephemeral_public_key: ourPubKeyBase64,
        encrypted_data: reEncryptedShare,
        share_index: shareIndex,
      });

      const res = await apiFetch(`/api/v1/mfa/recovery-requests/social/${requestId}/respond`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          encrypted_share: btoa(payload),
        }),
      });
      if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error || 'Failed to submit share');
      }

      setCompleted(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to approve');
    } finally {
      setLoading(false);
    }
  };

  if (completed) {
    return (
      <div className="mfa-modal-overlay">
        <div className="mfa-modal">
          <h3>Share Submitted</h3>
          <p>
            Your recovery share has been securely sent. The requesting user will be able to recover
            their account once enough contacts approve.
          </p>
          <div className="mfa-setup-actions">
            <button className="btn btn-primary" onClick={onClose}>
              Close
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="mfa-modal-overlay">
      <div className="mfa-modal">
        <h3>Social Recovery Request</h3>
        <p style={{ color: 'var(--text-secondary)', marginBottom: 16 }}>
          <strong>{requesterDisplayName || requesterUsername}</strong> (@{requesterUsername}) is
          requesting account recovery from their Recovery Circle. You hold one of the shares needed
          to recover their account.
        </p>
        <p style={{ color: 'var(--text-tertiary, #718096)', fontSize: 13, marginBottom: 16 }}>
          Approving will securely transfer your share via an encrypted channel. You cannot see or
          use the share contents.
        </p>
        {error && (
          <div className="mfa-setup-error-banner">
            <span>{error}</span>
          </div>
        )}
        <div className="mfa-setup-actions">
          <button className="btn btn-primary" onClick={handleApprove} disabled={loading}>
            {loading ? 'Submitting...' : 'Approve'}
          </button>
          <button className="btn btn-secondary" onClick={onClose} disabled={loading}>
            Decline
          </button>
        </div>
      </div>
    </div>
  );
};

export default SocialRecoveryApproval;
