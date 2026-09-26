import React, { useCallback, useRef, useState, useEffect } from 'react';
import ToggleSwitch from './ToggleSwitch';
import MFAVerifyPrompt from '../Auth/MFAVerifyPrompt';
import RecoveryApprovalModal from '../Auth/RecoveryApprovalModal';
import RecoveryCircle from './RecoveryCircle';
import Modal from '../ui/Modal';
import ErrorBanner, { FieldError } from './ErrorBanner';
import { apiFetch } from '../../services/system/apiClient';
import {
  isStepUpLocked,
  stepUpBanner,
  stepUpCodeMayBeSpent,
  stepUpMfaError,
  stepUpPasswordError,
  stepUpPromptMethods,
  type MfaStepUpResult,
} from './mfaStepUp';

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
  | 'set-backup-email'
  | 'toggle-recovery-only'
  | 'toggle-hardened';

interface ActionModalState {
  type: ActionType;
  credentialId?: string;
  credentialName?: string;
  toggleMethod?: string;
  toggleValue?: boolean;
  /** `set-backup-email` only. Empty string means "remove the backup email". */
  pendingBackupEmail?: string;
}

/**
 * Derive the modal title from the current action state. Fixed the instant the
 * modal opens and never mutated afterward (WCAG 4.1.2 / 3.2.2 — Modal binds
 * this to aria-labelledby).
 */
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
      return 'Disable Email/SMS verification';
    case 'set-backup-email':
      return modal.pendingBackupEmail ? 'Save backup email' : 'Remove backup email';
  }
}

/**
 * Derive the modal description from the current action state, for the four
 * sibling action types plus `disable-emailsms`. `set-backup-email` needs the
 * live `backupEmail` prop and its own line breaks, so it is rendered by
 * {@link renderActionBody} instead — this case exists only to keep the switch
 * exhaustive over `ActionType`.
 */
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
      return "You'll no longer be able to use email or text codes to sign in.";
    case 'set-backup-email':
      return '';
  }
}

/** Derive the Confirm button's label. Only the two step-up-gated types get a
 * specific label (spec §4.6.2, T13) — the four siblings keep "Confirm". */
function getActionConfirmLabel(modal: ActionModalState | null): string {
  if (!modal) return 'Confirm';
  switch (modal.type) {
    case 'disable-emailsms':
      return 'Disable Email/SMS';
    case 'set-backup-email':
      return modal.pendingBackupEmail ? 'Save backup email' : 'Remove backup email';
    default:
      return 'Confirm';
  }
}

/** Derive the Confirm button's class. `set-backup-email` is primary when it
 * would save a value and danger when it would clear one. */
function getActionConfirmClass(modal: ActionModalState | null): string {
  if (!modal) return 'btn btn-sm btn-danger';
  switch (modal.type) {
    case 'toggle-recovery-only':
    case 'toggle-hardened':
      return 'btn btn-sm btn-primary';
    case 'set-backup-email':
      return modal.pendingBackupEmail ? 'btn btn-sm btn-primary' : 'btn btn-sm btn-danger';
    default:
      return 'btn btn-sm btn-danger';
  }
}

/**
 * Render the modal body for the current action. `set-backup-email` needs the
 * live `backupEmail` prop (to name the address being removed) and a two-line
 * body when saving a new one; every other type renders {@link getActionDesc}
 * as a single paragraph.
 */
function renderActionBody(
  modal: ActionModalState | null,
  backupEmail: string | undefined
): React.ReactNode {
  if (!modal) return null;
  if (modal.type === 'set-backup-email') {
    if (modal.pendingBackupEmail) {
      return (
        <>
          <p className="mfa-modal-desc">{modal.pendingBackupEmail}</p>
          <p className="mfa-modal-desc">This address becomes a way to recover your account.</p>
        </>
      );
    }
    return (
      <p className="mfa-modal-desc">
        {backupEmail || 'This address'} will no longer be able to recover your account.
      </p>
    );
  }
  return <p className="mfa-modal-desc">{getActionDesc(modal)}</p>;
}

/**
 * Whether Confirm waits for an MFA code. The four actions on the step-up seam
 * need one whenever the account holds an inline factor, up front, so a
 * password-only submit does not spend one of the five shared attempts on a
 * request that must fail with mfa_required (spec R-4). Reset TOTP always
 * does: its server binds `code` as required and the account necessarily holds
 * TOTP. Revoke takes the password alone. An mfa_required refusal overrides all
 * of it — the server has just said a code is needed, whatever the last status
 * fetch believed (F3).
 */
function actionNeedsCode(
  type: ActionType,
  hasRealMFA: boolean,
  refusal: MfaStepUpResult | null
): boolean {
  if (refusal?.kind === 'mfaRequired') return true;
  switch (type) {
    case 'reset-totp':
      return true;
    case 'revoke-webauthn':
      return false;
    default:
      return hasRealMFA;
  }
}

/** Runs `toggle-recovery-only`, or reports it unavailable when no handler is
 * wired or the modal is missing its method/value. Extracted from
 * {@link MFATierSelector}'s `executeAction` to reduce its cognitive
 * complexity (SonarCloud typescript:S3776). */
function runToggleRecoveryOnly(
  modal: ActionModalState,
  handler: MFATierSelectorProps['onToggleRecoveryOnly'],
  password: string,
  mfaCode: string
): MfaStepUpResult | Promise<MfaStepUpResult> {
  return handler && modal.toggleMethod !== undefined && modal.toggleValue !== undefined
    ? handler(modal.toggleMethod, modal.toggleValue, password, mfaCode)
    : { kind: 'failed' };
}

/** Runs `toggle-hardened`, or reports it unavailable when no handler is wired
 * or the modal is missing its value. Sibling to {@link runToggleRecoveryOnly}. */
function runToggleHardened(
  modal: ActionModalState,
  handler: MFATierSelectorProps['onToggleRecoveryHardened'],
  password: string,
  mfaCode: string
): MfaStepUpResult | Promise<MfaStepUpResult> {
  return handler && modal.toggleValue !== undefined
    ? handler(modal.toggleValue, password, mfaCode)
    : { kind: 'failed' };
}

interface RecoveryCircleConfig {
  has_circle: boolean;
  threshold_k?: number;
  total_shares_n?: number;
  contacts?: Array<{ username: string }>;
}

/**
 * GETs `path` and returns its JSON only for a 2xx. A refusal, a non-JSON body
 * and a request that never completed all read as null, so a caller keeps its
 * current state instead of rendering an error body as the resource (F13).
 */
async function readOkJson<T>(path: string): Promise<T | null> {
  try {
    const res = await apiFetch(path);
    if (!res.ok) return null;
    return (await res.json()) as T;
  } catch {
    return null;
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
  ) => Promise<MfaStepUpResult>;
  onToggleRecoveryHardened?: (
    enabled: boolean,
    password: string,
    mfaCode: string
  ) => Promise<MfaStepUpResult>;
  onResetTOTP?: (password: string, code: string) => Promise<MfaStepUpResult>;
  onRevokeWebAuthnKey?: (credentialId: string, password: string) => Promise<MfaStepUpResult>;
  onDisableEmailSms?: (password: string, mfaCode: string) => Promise<MfaStepUpResult>;
  onSetBackupEmail?: (email: string, password: string, mfaCode: string) => Promise<MfaStepUpResult>;
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
  // The last refusal. The field errors, the banner and the lock are all
  // derived from it (mfaStepUp.ts), so they cannot disagree with each other.
  // The lock clears only when the modal is reopened (handoff §1.1 / §5).
  const [actionRefusal, setActionRefusal] = useState<MfaStepUpResult | null>(null);
  const [actionLoading, setActionLoading] = useState(false);
  // Bumped on an invalid-code refusal so MFAVerifyPrompt remounts empty
  // (handoff §1.1) rather than showing a stale rejected code.
  const [mfaPromptKey, setMfaPromptKey] = useState(0);
  const passwordRef = useRef<HTMLInputElement>(null);

  // Recovery key state
  const [hasRecoveryKey, setHasRecoveryKey] = useState(false);
  const [recoveryKeyCreatedAt, setRecoveryKeyCreatedAt] = useState<string | null>(null);

  // Recovery circle state
  const [circleConfig, setCircleConfig] = useState<RecoveryCircleConfig | null>(null);
  const [showCircleSetup, setShowCircleSetup] = useState(false);
  const closeCircleSetup = useCallback(() => setShowCircleSetup(false), []);
  const refreshCircleConfig = useCallback(() => {
    readOkJson<RecoveryCircleConfig>('/api/v1/mfa/recovery-circle').then((data) => {
      if (data) setCircleConfig(data);
    });
  }, []);

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
    // A non-2xx body is an error object, not the resource: reading it as one
    // would report "no recovery key" / "no circle" for a read that failed.
    // Each section keeps its initial state instead.
    readOkJson<{ has_recovery_key?: unknown; created_at?: unknown }>(
      '/api/v1/mfa/recovery-key'
    ).then((data) => {
      if (!data) return;
      setHasRecoveryKey(data.has_recovery_key === true);
      setRecoveryKeyCreatedAt(typeof data.created_at === 'string' ? data.created_at : null);
    });

    readOkJson<{ devices?: unknown }>('/api/v1/mfa/trusted-devices').then((data) => {
      if (data) setTrustedDevices(Array.isArray(data.devices) ? data.devices : []);
    });

    refreshCircleConfig();

    readOkJson<{ requests?: unknown }>('/api/v1/mfa/recovery-requests').then((data) => {
      if (data) setPendingRecoveryRequests(Array.isArray(data.requests) ? data.requests : []);
    });
  }, [refreshCircleConfig]);

  // Backup email state
  const [editingBackupEmail, setEditingBackupEmail] = useState(false);
  const [backupEmailDraft, setBackupEmailDraft] = useState(backupEmail || '');
  const [backupEmailError, setBackupEmailError] = useState('');

  // Stable identity: Modal re-registers its Escape listener whenever onClose
  // changes, which an inline arrow would do on every render.
  const clearActionModal = useCallback(() => {
    setActionModal(null);
    setActionPassword('');
    setActionMfaCode('');
    setActionRefusal(null);
  }, []);

  const openActionModal = (state: ActionModalState) => {
    setActionModal(state);
    setActionPassword('');
    setActionMfaCode('');
    setActionRefusal(null);
  };

  // Focus the password field once a password refusal has rendered AND the
  // field is enabled again. Focusing inside the submit handler ran while the
  // input was still `disabled` for loading, so the call was a no-op (F4).
  useEffect(() => {
    if (actionLoading) return;
    if (actionRefusal?.kind === 'passwordRequired' || actionRefusal?.kind === 'invalidPassword') {
      passwordRef.current?.focus();
    }
  }, [actionLoading, actionRefusal]);

  /** Runs the action behind the modal. Every one of the six answers with the
   * step-up result shape, so there is one routing path and no action can
   * surface a refusal the others would route differently. */
  const executeAction = async (modal: ActionModalState): Promise<MfaStepUpResult> => {
    const unavailable: MfaStepUpResult = { kind: 'failed' };
    switch (modal.type) {
      case 'reset-totp':
        return onResetTOTP ? onResetTOTP(actionPassword, actionMfaCode) : unavailable;
      case 'revoke-webauthn':
        return onRevokeWebAuthnKey && modal.credentialId
          ? onRevokeWebAuthnKey(modal.credentialId, actionPassword)
          : unavailable;
      case 'toggle-recovery-only':
        return runToggleRecoveryOnly(modal, onToggleRecoveryOnly, actionPassword, actionMfaCode);
      case 'toggle-hardened':
        return runToggleHardened(modal, onToggleRecoveryHardened, actionPassword, actionMfaCode);
      case 'disable-emailsms':
        return onDisableEmailSms ? onDisableEmailSms(actionPassword, actionMfaCode) : unavailable;
      case 'set-backup-email':
        return onSetBackupEmail
          ? onSetBackupEmail(modal.pendingBackupEmail ?? '', actionPassword, actionMfaCode)
          : unavailable;
    }
  };

  /** Records a refusal, clears the password only when it was refused (handoff
   * §1.1), and clears the code whenever the server may have used it up — the
   * prompt remounts empty so Confirm waits for a fresh one. Where the refusal
   * is SHOWN is derived at render. */
  const applyRefusal = (result: MfaStepUpResult) => {
    setActionRefusal(result);
    if (result.kind === 'passwordRequired' || result.kind === 'invalidPassword') {
      setActionPassword('');
    }
    if (stepUpCodeMayBeSpent(result)) {
      setMfaPromptKey((k) => k + 1);
      setActionMfaCode('');
    }
  };

  const handleAction = async () => {
    if (!actionModal) return;
    setActionLoading(true);

    try {
      const result = await executeAction(actionModal);
      if (result.kind === 'accepted') {
        // The draft-email reset is scoped to a saved/removed backup email —
        // cancelling or failing the modal leaves the draft untouched
        // (handoff §1.2).
        if (actionModal.type === 'set-backup-email') setEditingBackupEmail(false);
        clearActionModal();
        return;
      }
      applyRefusal(result);
    } finally {
      setActionLoading(false);
    }
  };

  /**
   * Trim and validate locally (no credentials needed for a format error),
   * then hand off to the shared step-up modal. Save no longer writes
   * directly — the request happens on Confirm (handoff §1.2).
   */
  const handleSaveBackupEmail = () => {
    const trimmed = backupEmailDraft.trim();
    if (trimmed && !/^[^\s@]+@[^\s@.]+(?:\.[^\s@.]+)+$/.test(trimmed)) {
      setBackupEmailError('Please enter a valid email address.');
      return;
    }
    setBackupEmailError('');
    if (trimmed === (backupEmail || '')) {
      setEditingBackupEmail(false);
      return;
    }
    openActionModal({ type: 'set-backup-email', pendingBackupEmail: trimmed });
  };

  const actionTitle = getActionTitle(actionModal);
  const needsCode = actionModal
    ? actionNeedsCode(actionModal.type, hasRealMFA, actionRefusal)
    : false;
  const confirmEnabled = actionModal
    ? !actionLoading &&
      actionPassword !== '' &&
      (!needsCode || actionMfaCode !== '') &&
      !isStepUpLocked(actionRefusal)
    : false;
  // The prompt renders whenever a code could be asked for: the account's
  // known factors, or the server's own list on an mfa_required refusal (F3).
  const showActionPrompt = hasRealMFA || actionRefusal?.kind === 'mfaRequired';
  const actionPromptMethods = stepUpPromptMethods(actionRefusal, activeMethods);
  const actionPasswordError = stepUpPasswordError(actionRefusal);

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
                      aria-invalid={backupEmailError !== ''}
                      aria-describedby={
                        backupEmailError
                          ? 'mfa-backup-email-error'
                          : 'mfa-backup-email-confirm-hint'
                      }
                    />
                    <span id="mfa-backup-email-confirm-hint" className="mfa-backup-email-hint">
                      You&apos;ll confirm with your password before this is saved.
                    </span>
                    {backupEmailError && (
                      <FieldError id="mfa-backup-email-error">{backupEmailError}</FieldError>
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
                  <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
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

      {/* No-rot (handoff §3, X6): this overlay had the same missing-dialog
          defect (no role, no trap, no Escape) as the action modal below. */}
      <Modal
        isOpen={showCircleSetup}
        onClose={closeCircleSetup}
        title="Recovery Circle"
        width="medium"
        dismissable
      >
        <RecoveryCircle
          onComplete={() => {
            setShowCircleSetup(false);
            refreshCircleConfig();
          }}
          onCancel={closeCircleSetup}
        />
      </Modal>

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

      {/* Action confirmation modal — reused by all eight action types (handoff
          §1.1). Moved onto ui/Modal for dialog semantics (role, Tab trap,
          Escape, focus return); the title is fixed at open and never mutated. */}
      <Modal
        isOpen={actionModal !== null}
        onClose={clearActionModal}
        title={actionTitle}
        width="small"
        dismissable={!actionLoading}
        initialFocusRef={passwordRef}
      >
        {actionModal && (
          <div className="mfa-action-body">
            {renderActionBody(actionModal, backupEmail)}

            <div className="mfa-verify-field">
              <label htmlFor="mfa-action-password">Password</label>
              <input
                id="mfa-action-password"
                ref={passwordRef}
                type="password"
                autoComplete="current-password"
                value={actionPassword}
                onChange={(e) => setActionPassword(e.target.value)}
                placeholder="Enter your password"
                disabled={actionLoading}
                aria-invalid={actionPasswordError !== undefined}
                aria-describedby={actionPasswordError ? 'mfa-action-password-error' : undefined}
              />
              {actionPasswordError && (
                <FieldError id="mfa-action-password-error">{actionPasswordError}</FieldError>
              )}
            </div>

            {showActionPrompt && (
              <MFAVerifyPrompt
                key={mfaPromptKey}
                methods={actionPromptMethods}
                recoveryOnlyMethods={recoveryOnlyMethods}
                onVerify={setActionMfaCode}
                onCodeChange={setActionMfaCode}
                disabled={actionLoading}
                error={stepUpMfaError(actionRefusal)}
                excludeBackupCodes={!actionPromptMethods.includes('totp')}
              />
            )}

            <ErrorBanner error={stepUpBanner(actionRefusal) ?? ''} />

            <div className="mfa-setup-actions">
              <button
                type="button"
                className={getActionConfirmClass(actionModal)}
                onClick={() => void handleAction()}
                disabled={!confirmEnabled}
              >
                {actionLoading ? 'Processing...' : getActionConfirmLabel(actionModal)}
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
        )}
      </Modal>
    </div>
  );
};

export default MFATierSelector;
