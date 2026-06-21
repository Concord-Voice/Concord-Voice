import React, { useState } from 'react';
import {
  generateECDHKeyPair,
  exportECDHPublicKey,
  importECDHPublicKey,
  deriveSharedSecret,
  encryptWithSharedSecret,
  base64ToArrayBuffer,
} from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import { apiFetch } from '../../services/apiClient';

interface RecoveryApprovalModalProps {
  requestId: string;
  requesterEphemeralKey: string; // base64 ECDH public key
  createdAt: string;
  onClose: () => void;
}

const RecoveryApprovalModal: React.FC<RecoveryApprovalModalProps> = ({
  requestId,
  requesterEphemeralKey,
  createdAt,
  onClose,
}) => {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [completed, setCompleted] = useState(false);

  const handleApprove = async () => {
    setLoading(true);
    setError('');
    try {
      // 1. Generate our own ECDH keypair
      const ecdhKeyPair = await generateECDHKeyPair();
      const ourPublicKey = await exportECDHPublicKey(ecdhKeyPair.publicKey);

      // 2. Import requester's public key and derive shared secret
      const requesterKey = await importECDHPublicKey(requesterEphemeralKey);
      const sharedKey = await deriveSharedSecret(ecdhKeyPair.privateKey, requesterKey);

      // 3. Get the wrapped private key and wrapping key from e2eeService
      const wrappingKey = e2eeService.getWrappingKey();
      const wrappedPrivateKey = e2eeService.getWrappedPrivateKey();

      if (!wrappingKey || !wrappedPrivateKey) {
        throw new Error('E2EE keys not available. Please ensure you are logged in.');
      }

      // 4. Unwrap the password-wrapped private key to get raw PKCS8 bytes
      //    (wrappingKey only supports wrapKey/unwrapKey, not decrypt)
      const wrappedData = new Uint8Array(base64ToArrayBuffer(wrappedPrivateKey));
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

      // 5. Encrypt the raw PKCS8 with the ECDH shared secret
      const encryptedPayload = await encryptWithSharedSecret(sharedKey, rawPkcs8);

      // 6. Send approval to server
      const res = await apiFetch(`/api/v1/mfa/recovery-requests/${requestId}/respond`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          action: 'approve',
          encrypted_payload: encryptedPayload,
          responder_public_key: ourPublicKey,
        }),
      });

      if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error || 'Failed to send approval');
      }

      setCompleted(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to approve recovery');
    } finally {
      setLoading(false);
    }
  };

  const handleReject = async () => {
    setLoading(true);
    try {
      await apiFetch(`/api/v1/mfa/recovery-requests/${requestId}/respond`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'reject' }),
      });
      onClose();
    } catch {
      onClose();
    }
  };

  if (completed) {
    return (
      <div className="mfa-modal-overlay">
        <div className="mfa-modal">
          <h3>Recovery Approved</h3>
          <p>Your encrypted private key has been securely transferred to the recovering device.</p>
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
        <h3>Account Recovery Request</h3>
        <p style={{ color: 'var(--text-secondary)', marginBottom: 16 }}>
          A recovery request was created on {new Date(createdAt).toLocaleString()}. Approving will
          securely transfer your private key to the recovering device via an encrypted channel.
        </p>
        {error && (
          <div className="mfa-setup-error-banner">
            <span>{error}</span>
          </div>
        )}
        <div className="mfa-setup-actions">
          <button className="btn btn-primary" onClick={handleApprove} disabled={loading}>
            {loading ? 'Approving...' : 'Approve Recovery'}
          </button>
          <button className="btn btn-secondary" onClick={handleReject} disabled={loading}>
            Reject
          </button>
        </div>
      </div>
    </div>
  );
};

export default RecoveryApprovalModal;
