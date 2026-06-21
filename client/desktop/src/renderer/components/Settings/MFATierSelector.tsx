import React, { useState, useEffect } from 'react';
import ToggleSwitch from './ToggleSwitch';
import MFAVerifyPrompt from '../Auth/MFAVerifyPrompt';
import RecoveryApprovalModal from '../Auth/RecoveryApprovalModal';
import RecoveryCircle from './RecoveryCircle';
import { apiFetch } from '../../services/apiClient';

export interface WebAuthnCredential {
  id: string;
  credential_name: string;
  credential_type: string; // 'hardware' | 'platform'
  created_at: string;
  last_used_at?: string | null;
}

interface MFATier {
  level: string;
  name: string;
  methodKey?: string;
  methods: string[];
  description: string;
  locked?: boolean;
  lockReason?: string;
  recoveryOnlyEligible?: boolean;
}

const tiers: MFATier[] = [
  {
    level: 'maximum',
    name: 'Maximum — Hardware Keys',
    methodKey: 'webauthn',
    methods: ['YubiKey', 'Google Titan', 'SoloKeys', 'Nitrokey', 'Any FIDO2 security key'],
    description:
      'Fort Knox mode. A dedicated physical device you carry separately. Hackers need to literally steal it from you.',
  },
  {
    level: 'strong',
    name: 'Strong — Platform Authenticator',
    methodKey: 'webauthn',
    methods: ['Windows Hello', 'Apple Touch ID / Face ID', 'Android biometrics', 'Chrome OS'],
    description:
      'Your device IS the key. Built-in biometrics + secure hardware. Very solid, and nothing extra to carry.',
  },
  {
    level: 'standard',
    name: 'Standard — Authenticator App',
    methodKey: 'totp',
    methods: [
      'Google Authenticator',
      'Microsoft Authenticator',
      'Authy',
      'Proton Pass',
      'Bitwarden',
      '1Password',
      'Any TOTP app',
    ],
    description:
      'The classic. 6 digits, 30 seconds. Works with any app that supports TOTP. A tried-and-true workhorse.',
  },
  {
    level: 'last-resort',
    name: 'Last Resort — Email / SMS',
    methodKey: 'email',
    methods: ['Email code', 'SMS code'],
    description:
      'Better than nothing, but phone numbers get SIM-swapped and emails get phished. Requires a real MFA method first.',
    locked: true,
    lockReason: 'Enable a Standard or higher MFA method first',
    recoveryOnlyEligible: true,
  },
];

type ActionType =
  | 'reset-totp'
  | 'revoke-webauthn'
  | 'disable-emailsms'
  | 'toggle-recovery-only'
  | 'toggle-hardened';

interface ActionModalState {
  type: ActionType;
  credentialId?: string;
  credentialName?: string;
  toggleMethod?: string;
  toggleValue?: boolean;
}

/** Derive the modal title from the current action state */
function getActionTitle(modal: ActionModalState | null): string {
  if (!modal) return '';
  switch (modal.type) {
    case 'reset-totp':
      return 'Reset TOTP';
    case 'revoke-webauthn':
      return `Revoke "${modal.credentialName || 'Key'}"`;
    case 'toggle-recovery-only':
      return modal.toggleValue ? 'Enable Recovery Only' : 'Disable Recovery Only';
    case 'toggle-hardened':
      return modal.toggleValue ? 'Enable Hardened Mode' : 'Disable Hardened Mode';
    case 'disable-emailsms':
      return 'Disable Email/SMS';
  }
}

/** Derive the modal description from the current action state */
function getActionDesc(modal: ActionModalState | null): string {
  if (!modal) return '';
  switch (modal.type) {
    case 'reset-totp':
      return 'This will remove your authenticator app enrollment. You will need to set it up again.';
    case 'revoke-webauthn':
      return 'This will permanently remove this security key. It will no longer work for authentication.';
    case 'toggle-recovery-only':
      return modal.toggleValue
        ? 'This method will only be usable for account recovery, not for login or sensitive actions.'
        : 'This method will be usable for login and sensitive actions again.';
    case 'toggle-hardened':
      return modal.toggleValue
        ? 'Recovery will require BOTH email and SMS codes simultaneously.'
        : 'Recovery will accept either an email code or an SMS code individually.';
    case 'disable-emailsms':
      return 'This will disable email and SMS verification methods.';
  }
}

/** Determine whether a tier is currently active based on active methods and credentials */
function isTierActive(
  tier: MFATier,
  hasTotp: boolean,
  hasWebauthn: boolean,
  hasEmailOrSms: boolean,
  hasHardwareKeys: boolean,
  hasPlatformKeys: boolean
): boolean {
  switch (tier.level) {
    case 'standard':
      return hasTotp;
    case 'maximum':
      return hasWebauthn && hasHardwareKeys;
    case 'strong':
      return hasWebauthn && hasPlatformKeys;
    case 'last-resort':
      return hasEmailOrSms;
    default:
      return false;
  }
}

/** Determine whether a tier is in recovery-only mode */
function isTierRecoveryOnly(tier: MFATier, recoveryOnlyMethods: string[]): boolean {
  if (tier.level === 'last-resort') {
    return recoveryOnlyMethods.includes('email') || recoveryOnlyMethods.includes('sms');
  }
  return tier.methodKey ? recoveryOnlyMethods.includes(tier.methodKey) : false;
}

/** Get the filtered WebAuthn credentials for a tier */
function getTierCredentials(
  tier: MFATier,
  isActive: boolean,
  webauthnCredentials: WebAuthnCredential[]
): WebAuthnCredential[] {
  if (tier.methodKey !== 'webauthn' || !isActive) return [];
  return webauthnCredentials.filter((c) =>
    tier.level === 'maximum' ? c.credential_type === 'hardware' : c.credential_type === 'platform'
  );
}

interface MFATierSelectorProps {
  activeMethods: string[];
  recoveryOnlyMethods?: string[];
  recoveryHardened?: boolean;
  backupCodesRemaining?: number;
  webauthnCredentials?: WebAuthnCredential[];
  backupEmail?: string;
  onSetupTOTP: () => void;
  onSetupWebAuthn: (credentialType: 'hardware' | 'platform') => void;
  onSetupEmailSms?: () => void;
  onToggleRecoveryOnly?: (
    method: string,
    recoveryOnly: boolean,
    password: string,
    mfaCode: string
  ) => Promise<boolean>;
  onToggleRecoveryHardened?: (
    enabled: boolean,
    password: string,
    mfaCode: string
  ) => Promise<boolean>;
  onResetTOTP?: (password: string, code: string) => Promise<boolean>;
  onRevokeWebAuthnKey?: (credentialId: string, password: string) => Promise<boolean>;
  onDisableEmailSms?: (password: string) => Promise<boolean>;
  onSetBackupEmail?: (email: string) => Promise<boolean>;
}

const MFATierSelector: React.FC<MFATierSelectorProps> = ({
  activeMethods,
  recoveryOnlyMethods = [],
  recoveryHardened = true,
  backupCodesRemaining: _backupCodesRemaining,
  webauthnCredentials = [],
  backupEmail,
  onSetupTOTP,
  onSetupWebAuthn,
  onSetupEmailSms,
  onToggleRecoveryOnly,
  onToggleRecoveryHardened,
  onResetTOTP,
  onRevokeWebAuthnKey,
  onDisableEmailSms,
  onSetBackupEmail,
}) => {
  const hasTotp = activeMethods.includes('totp');
  const hasWebauthn = activeMethods.includes('webauthn');
  const hasEmail = activeMethods.includes('email');
  const hasSms = activeMethods.includes('sms');
  const hasEmailOrSms = hasEmail || hasSms;
  const hasRealMFA = hasTotp || hasWebauthn;
  // Count distinct "real" MFA types (not email/sms)
  const realMFATypeCount = (hasTotp ? 1 : 0) + (hasWebauthn ? 1 : 0);
  // Sole-MFA protection: if only one real MFA type and Email/SMS is also on,
  // disabling that sole MFA type would leave only Email/SMS (which isn't real MFA).
  const soleRealMFAWithEmailSms = realMFATypeCount === 1 && hasEmailOrSms;

  // Action modal state
  const [actionModal, setActionModal] = useState<ActionModalState | null>(null);
  const [actionPassword, setActionPassword] = useState('');
  const [actionMfaCode, setActionMfaCode] = useState('');
  const [actionError, setActionError] = useState('');
  const [actionLoading, setActionLoading] = useState(false);

  // Recovery key state
  const [hasRecoveryKey, setHasRecoveryKey] = useState(false);
  const [recoveryKeyCreatedAt, setRecoveryKeyCreatedAt] = useState<string | null>(null);

  // Recovery circle state
  const [circleConfig, setCircleConfig] = useState<{
    has_circle: boolean;
    threshold_k?: number;
    total_shares_n?: number;
    contacts?: Array<{ username: string }>;
  } | null>(null);
  const [showCircleSetup, setShowCircleSetup] = useState(false);

  // Trusted devices state
  const [trustedDevices, setTrustedDevices] = useState<
    Array<{ id: string; device_name: string; machine_id: string; designated_at: string }>
  >([]);
  const [pendingRecoveryRequests, setPendingRecoveryRequests] = useState<
    Array<{ id: string; ephemeral_public_key: string; created_at: string }>
  >([]);
  const [activeRecoveryRequest, setActiveRecoveryRequest] = useState<{
    id: string;
    ephemeral_public_key: string;
    created_at: string;
  } | null>(null);

  useEffect(() => {
    apiFetch('/api/v1/mfa/recovery-key')
      .then((res) => res.json())
      .then((data) => {
        setHasRecoveryKey(data.has_recovery_key || false);
        setRecoveryKeyCreatedAt(data.created_at || null);
      })
      .catch(() => {});

    apiFetch('/api/v1/mfa/trusted-devices')
      .then((res) => res.json())
      .then((data) => setTrustedDevices(Array.isArray(data.devices) ? data.devices : []))
      .catch(() => {});

    apiFetch('/api/v1/mfa/recovery-circle')
      .then((res) => res.json())
      .then((data) => setCircleConfig(data))
      .catch(() => {});

    apiFetch('/api/v1/mfa/recovery-requests')
      .then((res) => res.json())
      .then((data) => setPendingRecoveryRequests(Array.isArray(data.requests) ? data.requests : []))
      .catch(() => {});
  }, []);

  // Backup email state
  const [editingBackupEmail, setEditingBackupEmail] = useState(false);
  const [backupEmailDraft, setBackupEmailDraft] = useState(backupEmail || '');
  const [backupEmailError, setBackupEmailError] = useState('');

  const clearActionModal = () => {
    setActionModal(null);
    setActionPassword('');
    setActionMfaCode('');
    setActionError('');
  };

  const openActionModal = (state: ActionModalState) => {
    setActionModal(state);
    setActionPassword('');
    setActionMfaCode('');
    setActionError('');
  };

  const executeAction = async (modal: ActionModalState): Promise<boolean> => {
    switch (modal.type) {
      case 'reset-totp':
        return onResetTOTP ? onResetTOTP(actionPassword, actionMfaCode) : false;
      case 'revoke-webauthn':
        return onRevokeWebAuthnKey && modal.credentialId
          ? onRevokeWebAuthnKey(modal.credentialId, actionPassword)
          : false;
      case 'disable-emailsms':
        return onDisableEmailSms ? onDisableEmailSms(actionPassword) : false;
      case 'toggle-recovery-only':
        return onToggleRecoveryOnly &&
          modal.toggleMethod !== undefined &&
          modal.toggleValue !== undefined
          ? onToggleRecoveryOnly(
              modal.toggleMethod,
              modal.toggleValue,
              actionPassword,
              actionMfaCode
            )
          : false;
      case 'toggle-hardened':
        return onToggleRecoveryHardened && modal.toggleValue !== undefined
          ? onToggleRecoveryHardened(modal.toggleValue, actionPassword, actionMfaCode)
          : false;
    }
  };

  const handleAction = async () => {
    if (!actionModal) return;
    setActionError('');
    setActionLoading(true);

    try {
      const success = await executeAction(actionModal);
      if (success) {
        clearActionModal();
      } else {
        setActionError('Verification failed. Check your password and try again.');
      }
    } catch (err) {
      setActionError(err instanceof Error ? err.message : 'An error occurred. Please try again.');
    } finally {
      setActionLoading(false);
    }
  };

  const handleSaveBackupEmail = async () => {
    const trimmed = backupEmailDraft.trim();
    if (trimmed && !/^[^\s@]+@[^\s@.]+(?:\.[^\s@.]+)+$/.test(trimmed)) {
      setBackupEmailError('Please enter a valid email address.');
      return;
    }
    setBackupEmailError('');
    if (onSetBackupEmail) {
      const ok = await onSetBackupEmail(trimmed);
      if (ok) setEditingBackupEmail(false);
      else setBackupEmailError('Failed to save. Please try again.');
    }
  };

  const actionTitle = getActionTitle(actionModal);
  const actionDesc = getActionDesc(actionModal);

  return (
    <div className="mfa-tier-selector">
      {tiers.map((tier) => {
        const isLocked = tier.locked && !hasRealMFA;
        const hasHardwareKeys = webauthnCredentials.some((c) => c.credential_type === 'hardware');
        const hasPlatformKeys = webauthnCredentials.some((c) => c.credential_type === 'platform');
        const isActive = isTierActive(
          tier,
          hasTotp,
          hasWebauthn,
          hasEmailOrSms,
          hasHardwareKeys,
          hasPlatformKeys
        );
        const isRecoveryOnly = isTierRecoveryOnly(tier, recoveryOnlyMethods);
        const tierCredentials = getTierCredentials(tier, isActive, webauthnCredentials);

        return (
          <div
            key={tier.level}
            className={`mfa-tier-card ${isLocked ? 'mfa-tier-locked' : ''} ${isActive ? 'mfa-tier-active' : ''}`}
          >
            <div className="mfa-tier-header">
              <h4 className="mfa-tier-name">{tier.name}</h4>
              {isActive && !isRecoveryOnly && <span className="mfa-tier-badge">Configured</span>}
              {isActive && isRecoveryOnly && (
                <span className="mfa-tier-badge mfa-tier-badge-recovery">Recovery Only</span>
              )}
            </div>
            <p className="mfa-tier-desc">{tier.description}</p>
            <div className="mfa-tier-methods-row">
              <div className="mfa-tier-methods">
                {tier.methods.map((m) => (
                  <span key={m} className="mfa-tier-method">
                    {m}
                  </span>
                ))}
              </div>

              {/* TOTP Reset button — inline with methods */}
              {tier.level === 'standard' && isActive && onResetTOTP && (
                <div className="mfa-tier-actions-inline">
                  {soleRealMFAWithEmailSms && hasTotp && !hasWebauthn && (
                    <span className="mfa-action-hint">
                      Disable Email/SMS before resetting your only MFA method
                    </span>
                  )}
                  <button
                    type="button"
                    className="btn btn-uniform btn-danger"
                    disabled={soleRealMFAWithEmailSms && hasTotp && !hasWebauthn}
                    title={
                      soleRealMFAWithEmailSms && hasTotp && !hasWebauthn
                        ? 'Disable Email/SMS first — this is your only real MFA method'
                        : 'Reset TOTP enrollment'
                    }
                    onClick={() => openActionModal({ type: 'reset-totp' })}
                  >
                    Reset
                  </button>
                </div>
              )}
            </div>

            {/* WebAuthn credential list with per-key Revoke + Add Key */}
            {tier.methodKey === 'webauthn' && isActive && tierCredentials.length > 0 && (
              <div className="mfa-credential-list">
                {tierCredentials.map((cred) => (
                  <div key={cred.id} className="mfa-credential-row">
                    <div className="mfa-credential-info">
                      <span className="mfa-credential-name">{cred.credential_name}</span>
                      <span className="mfa-credential-meta">
                        Added {new Date(cred.created_at).toLocaleDateString()}
                        {cred.last_used_at &&
                          ` · Last used ${new Date(cred.last_used_at).toLocaleDateString()}`}
                      </span>
                    </div>
                    {onRevokeWebAuthnKey && (
                      <button
                        type="button"
                        className="btn btn-uniform btn-danger"
                        disabled={
                          soleRealMFAWithEmailSms &&
                          hasWebauthn &&
                          !hasTotp &&
                          webauthnCredentials.length === 1
                        }
                        title={
                          soleRealMFAWithEmailSms &&
                          hasWebauthn &&
                          !hasTotp &&
                          webauthnCredentials.length === 1
                            ? 'Disable Email/SMS first — this is your only real MFA method'
                            : 'Revoke this key'
                        }
                        onClick={() =>
                          openActionModal({
                            type: 'revoke-webauthn',
                            credentialId: cred.id,
                            credentialName: cred.credential_name,
                          })
                        }
                      >
                        Revoke
                      </button>
                    )}
                  </div>
                ))}
                {soleRealMFAWithEmailSms &&
                  hasWebauthn &&
                  !hasTotp &&
                  webauthnCredentials.length === 1 && (
                    <span className="mfa-action-hint">
                      Disable Email/SMS before revoking your only MFA method
                    </span>
                  )}
                {webauthnCredentials.length < 10 && (
                  <button
                    type="button"
                    className="btn btn-uniform btn-secondary mfa-add-key-btn"
                    onClick={() =>
                      onSetupWebAuthn(tier.level === 'maximum' ? 'hardware' : 'platform')
                    }
                  >
                    + Add Another Key
                  </button>
                )}
              </div>
            )}

            {/* Email/SMS Disable button */}
            {tier.level === 'last-resort' && isActive && onDisableEmailSms && (
              <div className="mfa-tier-actions">
                <button
                  type="button"
                  className="btn btn-uniform btn-danger"
                  onClick={() => openActionModal({ type: 'disable-emailsms' })}
                >
                  Disable
                </button>
              </div>
            )}

            {/* Recovery-Only toggle */}
            {tier.recoveryOnlyEligible && isActive && onToggleRecoveryOnly && (
              <div className="mfa-toggle-row">
                <div className="mfa-toggle-text">
                  <span className="mfa-recovery-only-label">Recovery only</span>
                  <span className="mfa-recovery-only-hint">
                    Can verify your identity for account recovery, but won&apos;t be accepted for
                    login or sensitive actions. Like a spare key that unlocks the door but
                    doesn&apos;t start the engine.
                  </span>
                </div>
                <ToggleSwitch
                  checked={isRecoveryOnly}
                  onChange={(checked) => {
                    let method: string;
                    if (tier.level === 'last-resort') {
                      method = hasEmail ? 'email' : 'sms';
                    } else {
                      method = tier.methodKey || '';
                    }
                    openActionModal({
                      type: 'toggle-recovery-only',
                      toggleMethod: method,
                      toggleValue: checked,
                    });
                  }}
                />
              </div>
            )}

            {/* Hardened mode toggle — shown whenever Email/SMS is active */}
            {tier.level === 'last-resort' && isActive && onToggleRecoveryHardened && (
              <div className="mfa-toggle-row mfa-hardened-toggle">
                <div className="mfa-toggle-text">
                  <span className="mfa-recovery-only-label">Hardened mode</span>
                  <span className="mfa-recovery-only-hint">
                    Require BOTH an email code AND an SMS code for recovery. An attacker must
                    compromise both your email and your phone — neither alone is sufficient.
                  </span>
                </div>
                <ToggleSwitch
                  checked={recoveryHardened}
                  onChange={(checked) => {
                    openActionModal({
                      type: 'toggle-hardened',
                      toggleValue: checked,
                    });
                  }}
                />
              </div>
            )}

            {/* Backup Email — under Email/SMS tier */}
            {tier.level === 'last-resort' && isActive && onSetBackupEmail && (
              <div className="mfa-backup-email-section">
                <label htmlFor="mfa-backup-email" className="mfa-backup-email-label">
                  Backup Email
                </label>
                <span className="mfa-backup-email-hint">
                  A secondary email for recovery if your primary email is compromised.
                </span>
                {editingBackupEmail ? (
                  <div className="mfa-backup-email-edit">
                    <input
                      id="mfa-backup-email"
                      type="email"
                      className="mfa-backup-email-input"
                      value={backupEmailDraft}
                      onChange={(e) => {
                        setBackupEmailDraft(e.target.value);
                        setBackupEmailError('');
                      }}
                      placeholder="backup@example.com"
                    />
                    {backupEmailError && (
                      <span className="mfa-backup-email-error">{backupEmailError}</span>
                    )}
                    <div className="mfa-backup-email-actions">
                      <button
                        type="button"
                        className="btn btn-xs btn-primary"
                        onClick={handleSaveBackupEmail}
                      >
                        Save
                      </button>
                      <button
                        type="button"
                        className="btn btn-xs btn-secondary"
                        onClick={() => {
                          setEditingBackupEmail(false);
                          setBackupEmailDraft(backupEmail || '');
                          setBackupEmailError('');
                        }}
                      >
                        Cancel
                      </button>
                    </div>
                  </div>
                ) : (
                  <div className="mfa-backup-email-display">
                    <button
                      type="button"
                      className="btn btn-xs btn-secondary"
                      onClick={() => {
                        setEditingBackupEmail(true);
                        setBackupEmailDraft(backupEmail || '');
                      }}
                    >
                      {backupEmail ? 'Change' : 'Add'}
                    </button>
                    <span className="mfa-backup-email-value">{backupEmail || 'Not set'}</span>
                  </div>
                )}
              </div>
            )}

            {/* Recovery-Only description for eligible but inactive tiers */}
            {tier.recoveryOnlyEligible && !isActive && !isLocked && (
              <p className="mfa-recovery-only-preview">
                Once set up, this method can be restricted to account recovery only.
              </p>
            )}

            {isLocked && (
              <div className="mfa-tier-lock-overlay">
                <span>{tier.lockReason}</span>
              </div>
            )}
            {!isLocked && !isActive && (
              <div className="mfa-tier-actions mfa-tier-actions-left">
                {tier.level === 'standard' && (
                  <button
                    type="button"
                    className="btn btn-uniform btn-primary"
                    onClick={onSetupTOTP}
                  >
                    Set Up
                  </button>
                )}
                {(tier.level === 'maximum' || tier.level === 'strong') && (
                  <button
                    type="button"
                    className="btn btn-uniform btn-primary"
                    onClick={() =>
                      onSetupWebAuthn(tier.level === 'maximum' ? 'hardware' : 'platform')
                    }
                  >
                    Set Up
                  </button>
                )}
                {tier.level === 'last-resort' && onSetupEmailSms && (
                  <button
                    type="button"
                    className="btn btn-uniform btn-primary"
                    onClick={onSetupEmailSms}
                  >
                    Set Up
                  </button>
                )}
              </div>
            )}
          </div>
        );
      })}

      {/* Recovery Key Section */}
      {hasRealMFA && (
        <div
          className="mfa-tier-section"
          style={{
            marginTop: 24,
            borderTop: '1px solid var(--border-color, #2d3748)',
            paddingTop: 24,
          }}
        >
          <h4 style={{ color: 'var(--text-primary)', margin: '0 0 8px' }}>Recovery Key</h4>
          <p style={{ color: 'var(--text-secondary)', fontSize: 14, margin: '0 0 12px' }}>
            {(() => {
              if (!hasRecoveryKey) {
                return 'No recovery key configured. Without one, losing your password means losing all encrypted message history.';
              }
              const dateStr = recoveryKeyCreatedAt
                ? ` on ${new Date(recoveryKeyCreatedAt).toLocaleDateString()}`
                : '';
              return `Recovery key configured${dateStr}.`;
            })()}
          </p>
          <button
            className="btn btn-secondary"
            disabled
            title="Recovery key management will be available in a future update"
          >
            {hasRecoveryKey ? 'Regenerate Recovery Key' : 'Generate Recovery Key'}
          </button>
        </div>
      )}

      {/* Trusted Devices Section */}
      {hasRealMFA && (
        <div
          className="mfa-tier-section"
          style={{
            marginTop: 24,
            borderTop: '1px solid var(--border-color, #2d3748)',
            paddingTop: 24,
          }}
        >
          <h4 style={{ color: 'var(--text-primary)', margin: '0 0 8px' }}>Trusted Devices</h4>
          <p style={{ color: 'var(--text-secondary)', fontSize: 14, margin: '0 0 12px' }}>
            Designate this device as trusted to allow account recovery from it.
          </p>
          {trustedDevices.length > 0 && (
            <div style={{ marginBottom: 12 }}>
              {trustedDevices.map((device) => (
                <div
                  key={device.id}
                  style={{
                    display: 'flex',
                    justifyContent: 'space-between',
                    alignItems: 'center',
                    padding: '8px 0',
                    borderBottom: '1px solid var(--border-color, #2d3748)',
                  }}
                >
                  <span style={{ color: 'var(--text-primary)', fontSize: 14 }}>
                    {device.device_name}
                  </span>
                  <span style={{ color: 'var(--text-tertiary, #718096)', fontSize: 12 }}>
                    {new Date(device.designated_at).toLocaleDateString()}
                  </span>
                </div>
              ))}
            </div>
          )}
          <button
            className="btn btn-secondary"
            onClick={() => {
              // Designate-device flow not yet implemented
            }}
          >
            Designate This Device
          </button>

          {/* Pending Recovery Requests */}
          {pendingRecoveryRequests.length > 0 && (
            <div style={{ marginTop: 16 }}>
              <h5 style={{ color: 'var(--text-primary)', margin: '0 0 8px' }}>
                Pending Recovery Requests
              </h5>
              {pendingRecoveryRequests.map((req) => (
                <div
                  key={req.id}
                  style={{
                    display: 'flex',
                    justifyContent: 'space-between',
                    alignItems: 'center',
                    padding: '8px 0',
                  }}
                >
                  <span style={{ color: 'var(--text-secondary)', fontSize: 14 }}>
                    Request from {new Date(req.created_at).toLocaleString()}
                  </span>
                  <button
                    className="btn btn-secondary"
                    style={{ fontSize: 12, padding: '4px 12px' }}
                    onClick={() => setActiveRecoveryRequest(req)}
                  >
                    Review
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Recovery Circle Section */}
      {hasRealMFA && (
        <div
          className="mfa-tier-section"
          style={{
            marginTop: 24,
            borderTop: '1px solid var(--border-color, #2d3748)',
            paddingTop: 24,
          }}
        >
          <h4 style={{ color: 'var(--text-primary)', margin: '0 0 8px' }}>Recovery Circle</h4>
          <p style={{ color: 'var(--text-secondary)', fontSize: 14, margin: '0 0 12px' }}>
            {circleConfig?.has_circle
              ? `${circleConfig.threshold_k}-of-${circleConfig.total_shares_n} contacts can recover your account.`
              : "Distribute your recovery among trusted contacts using Shamir's Secret Sharing."}
          </p>
          {circleConfig?.has_circle && circleConfig.contacts && (
            <div style={{ marginBottom: 12 }}>
              {circleConfig.contacts.map((c) => (
                <span
                  key={c.username}
                  style={{ color: 'var(--text-primary)', fontSize: 13, marginRight: 8 }}
                >
                  @{c.username}
                </span>
              ))}
            </div>
          )}
          <button className="btn btn-secondary" onClick={() => setShowCircleSetup(true)}>
            {circleConfig?.has_circle ? 'Reconfigure' : 'Set Up Recovery Circle'}
          </button>
        </div>
      )}

      {showCircleSetup && (
        <div className="mfa-modal-overlay">
          <div className="mfa-modal" style={{ maxWidth: 500, maxHeight: '80vh', overflow: 'auto' }}>
            <RecoveryCircle
              onComplete={() => {
                setShowCircleSetup(false);
                // Refresh circle status
                apiFetch('/api/v1/mfa/recovery-circle')
                  .then((res) => res.json())
                  .then((data) => setCircleConfig(data))
                  .catch(() => {});
              }}
              onCancel={() => setShowCircleSetup(false)}
            />
          </div>
        </div>
      )}

      {/* Recovery Approval Modal */}
      {activeRecoveryRequest && (
        <RecoveryApprovalModal
          requestId={activeRecoveryRequest.id}
          requesterEphemeralKey={activeRecoveryRequest.ephemeral_public_key}
          createdAt={activeRecoveryRequest.created_at}
          onClose={() => {
            setActiveRecoveryRequest(null);
            // Remove from pending list after handling
            setPendingRecoveryRequests((prev) =>
              prev.filter((r) => r.id !== activeRecoveryRequest.id)
            );
          }}
        />
      )}

      {/* Action confirmation modal */}
      {actionModal && (
        <div className="mfa-modal-overlay">
          <div className="mfa-modal">
            <h3>{actionTitle}</h3>
            <p className="mfa-modal-desc">{actionDesc}</p>

            <div className="mfa-verify-field">
              <label htmlFor="mfa-action-password">Password</label>
              <input
                id="mfa-action-password"
                type="password"
                value={actionPassword}
                onChange={(e) => setActionPassword(e.target.value)}
                placeholder="Enter your password"
                disabled={actionLoading}
              />
            </div>

            {hasRealMFA && (
              <MFAVerifyPrompt
                methods={activeMethods}
                recoveryOnlyMethods={recoveryOnlyMethods}
                onVerify={(code) => setActionMfaCode(code)}
                disabled={actionLoading}
                error={actionError || undefined}
              />
            )}

            {actionError && !hasRealMFA && <p className="mfa-setup-error">{actionError}</p>}

            <div className="mfa-setup-actions">
              <button
                type="button"
                className={
                  actionModal.type === 'toggle-recovery-only' ||
                  actionModal.type === 'toggle-hardened'
                    ? 'btn btn-sm btn-primary'
                    : 'btn btn-sm btn-danger'
                }
                onClick={handleAction}
                disabled={actionLoading || !actionPassword}
              >
                {actionLoading ? 'Processing...' : 'Confirm'}
              </button>
              <button
                type="button"
                className="btn btn-sm btn-secondary"
                onClick={clearActionModal}
                disabled={actionLoading}
              >
                Cancel
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default MFATierSelector;
