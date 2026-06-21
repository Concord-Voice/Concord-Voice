import React, { useState, useCallback, useEffect, useMemo, useRef } from 'react';
import { useNavigate } from 'react-router-dom';
import { useAuthStore } from '../../stores/authStore';
import { useUserStore } from '../../stores/userStore';
import { usePrivacyStore, DMPrivacyLevel } from '../../stores/privacyStore';
import { apiFetch, API_BASE } from '../../services/apiClient';
import LoadingSpinner from '../Auth/LoadingSpinner';
import Modal from '../ui/Modal';
import MFATierSelector, { WebAuthnCredential } from './MFATierSelector';
import MFASetup from './MFASetup';
import MFAVerifyPrompt from '../Auth/MFAVerifyPrompt';
import BackupCodeDisplay from './BackupCodeDisplay';
import EmailSmsSetup from './EmailSmsSetup';
import CollapsibleSection from './CollapsibleSection';
import {
  useOsPermissionStore,
  type OsPermissionType,
  type OsPermissionStatus,
} from '../../stores/osPermissionStore';
import DMPrivacyControls from './DMPrivacyControls';
import ContentSafetyControls from './ContentSafetyControls';
import SearchVisibilityControls from './SearchVisibilityControls';
import LinkedAccountsList from './LinkedAccountsList';
import PresenceSettingsSection from './PresenceSettingsSection';
import './MFA.css';

interface Session {
  id: string;
  device_name: string;
  ip_address: string;
  user_agent: string;
  machine_id?: string;
  expires_at: string;
  created_at: string;
  last_used: string;
  is_current: boolean;
}

interface PastSession {
  id: string;
  device_name: string;
  ip_address: string;
  user_agent: string;
  created_at: string;
  last_used: string;
  revoked_at: string;
}

// ─── Data-fetch helpers (extracted to keep PrivacySecuritySection's cognitive complexity below threshold) ────────────────────

interface SessionsFetchResult {
  sessions: Session[];
  pastSessions: PastSession[];
  revocationMode: 'simple' | 'secure' | undefined;
}

async function fetchSessionsData(): Promise<SessionsFetchResult> {
  const response = await apiFetch('/api/v1/sessions');
  if (!response.ok) {
    const data = await response.json();
    throw new Error(data.error || 'Failed to fetch sessions');
  }
  const data = await response.json();
  return {
    sessions: data.sessions || [],
    pastSessions: data.past_sessions || [],
    revocationMode: data.revocation_mode,
  };
}

interface MFAStatusFetchResult {
  methods: string[];
  recoveryOnly: string[];
  recoveryHardened: boolean;
  backupRemaining: number | undefined;
  backupEmail: string;
  credentials: WebAuthnCredential[];
}

/**
 * Returns `null` when the status fetch fails — caller must skip its setter
 * dispatch in that case, otherwise transient HTTP failures would overwrite
 * the currently-displayed state with empty defaults. (Verified failure path
 * for the read-back-and-replace refetch.)
 */
async function fetchMFAStatusData(): Promise<MFAStatusFetchResult | null> {
  try {
    const res = await apiFetch('/api/v1/mfa/status');
    if (!res.ok) return null;
    const data = await res.json();
    const result: MFAStatusFetchResult = {
      methods: data.methods || [],
      recoveryOnly: data.recovery_only_methods || [],
      recoveryHardened: data.recovery_hardened || false,
      backupRemaining: data.backup_codes_remaining,
      backupEmail: data.backup_email || '',
      credentials: [],
    };
    const credRes = await apiFetch('/api/v1/mfa/webauthn/credentials');
    if (credRes.ok) {
      const credData = await credRes.json();
      result.credentials = credData.credentials || [];
    }
    return result;
  } catch {
    // Non-critical — preserve prior displayed state instead of clobbering it
    return null;
  }
}

interface SSOSecurityFetchResult {
  trustSSOSecurity: boolean;
  passwordLoginDisabled: boolean;
}

/**
 * Returns `null` when the security-flags fetch fails. Same rationale as
 * `fetchMFAStatusData` — on transient failure the toggle states should
 * remain as last successfully loaded, not snap back to `false`.
 */
async function fetchSSOSecurityData(): Promise<SSOSecurityFetchResult | null> {
  try {
    const res = await apiFetch('/api/v1/users/me/security');
    if (!res.ok) return null;
    const data = await res.json();
    return {
      trustSSOSecurity: data.trust_sso_security === true,
      passwordLoginDisabled: data.password_login_disabled === true,
    };
  } catch {
    // Non-critical — preserve prior displayed state instead of clobbering it
    return null;
  }
}

// ─── System Permissions Sub-section (#197) ─────────────────────────────

const PERMISSION_ROWS: {
  type: OsPermissionType;
  label: string;
  description: string;
  critical?: boolean;
}[] = [
  {
    type: 'secureStorage',
    label: 'Secure Storage (Keychain)',
    description: 'Required to safely store authentication tokens and encryption keys.',
    critical: true,
  },
  {
    type: 'microphone',
    label: 'Microphone',
    description: 'Used for voice channels and calls.',
  },
  {
    type: 'camera',
    label: 'Camera',
    description: 'Used for video in voice channels and calls.',
  },
  {
    type: 'screen',
    label: 'Screen Recording',
    description: 'Used for screen sharing in voice channels.',
  },
  {
    type: 'notifications',
    label: 'Notifications',
    description: 'Used for desktop notifications and incoming call alerts.',
  },
];

const PERMISSION_STATUS_BADGES: Record<OsPermissionStatus, { className: string; label: string }> = {
  granted: { className: 'os-perm-badge os-perm-badge--granted', label: 'Granted' },
  denied: { className: 'os-perm-badge os-perm-badge--denied', label: 'Denied' },
  restricted: { className: 'os-perm-badge os-perm-badge--denied', label: 'Restricted' },
  'not-determined': { className: 'os-perm-badge os-perm-badge--pending', label: 'Not Requested' },
  unavailable: { className: 'os-perm-badge os-perm-badge--unavailable', label: 'Unavailable' },
};

/** Get the description for the current revocation mode. */
function revocationModeDescription(mode: 'simple' | 'secure'): string {
  return mode === 'secure'
    ? 'Authentication via Password or MFA is required to revoke sessions under certain circumstances.'
    : 'Authenticate once to freely manage sessions for a short period.';
}

/** Resolve a human-readable message from an SSO security-toggle error_code. */
export function resolveSSOToggleError(errorCode: string | undefined): string {
  if (errorCode === 'invalid_credentials') return 'Incorrect passphrase.';
  if (errorCode === 'would_lock_out')
    return 'That change would lock you out. Link an SSO provider first.';
  return 'Failed to update security setting.';
}

/** Renders either an MFA verify prompt or a password input, depending on whether MFA is active. */
const AuthVerifyField: React.FC<{
  hasMFA: boolean;
  mfaMethods: string[];
  mfaRecoveryOnly: string[];
  password: string;
  onPasswordChange: (v: string) => void;
  onMfaVerify: (code: string) => void;
  error: string;
  onClearError: () => void;
  disabled: boolean;
  inputId: string;
  onEnterKey?: () => void;
  excludeBackupCodes?: boolean;
}> = ({
  hasMFA,
  mfaMethods,
  mfaRecoveryOnly,
  password,
  onPasswordChange,
  onMfaVerify,
  error,
  onClearError,
  disabled,
  inputId,
  onEnterKey,
  excludeBackupCodes,
}) => {
  if (hasMFA) {
    return (
      <MFAVerifyPrompt
        methods={mfaMethods}
        recoveryOnlyMethods={mfaRecoveryOnly}
        onVerify={(code) => {
          onMfaVerify(code);
          onClearError();
        }}
        disabled={disabled}
        error={error || undefined}
        excludeBackupCodes={excludeBackupCodes}
      />
    );
  }
  return (
    <div className="revoke-password-input">
      <label htmlFor={inputId}>Password</label>
      <input
        id={inputId}
        type="password"
        value={password}
        onChange={(e) => {
          onPasswordChange(e.target.value);
          onClearError();
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && password && onEnterKey) onEnterKey();
        }}
        placeholder="Enter your password"
        autoFocus
      />
      {error && <span className="revoke-password-error">{error}</span>}
    </div>
  );
};

/** Compute the disabled state for an auth-gated confirm button. */
function isAuthConfirmDisabled(hasMFA: boolean, mfaCode: string, password: string): boolean {
  return hasMFA ? !mfaCode : !password;
}

/**
 * Format an ISO timestamp as a short relative string ("Just now", "5m ago",
 * "Mar 6, 2026"). Pure helper hoisted to module scope so it does not inflate
 * PrivacySecuritySection's cognitive complexity.
 */
function formatRelativeTime(dateStr: string): string {
  const now = new Date();
  const date = new Date(dateStr);
  const diffMs = now.getTime() - date.getTime();
  const diffMin = Math.floor(diffMs / 60000);
  const diffHr = Math.floor(diffMin / 60);
  const diffDay = Math.floor(diffHr / 24);

  if (diffMin < 1) return 'Just now';
  if (diffMin < 60) return `${diffMin}m ago`;
  if (diffHr < 24) return `${diffHr}h ago`;
  if (diffDay < 30) return `${diffDay}d ago`;
  return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
}

/**
 * Derive a human-readable device label from a User-Agent string. Pure helper
 * hoisted to module scope so it does not inflate PrivacySecuritySection's
 * cognitive complexity.
 */
function parseUserAgent(ua: string): string {
  if (!ua) return 'Unknown Device';
  if (ua.includes('Electron')) return 'Concord Voice Desktop';
  if (ua.includes('Chrome')) return 'Chrome Browser';
  if (ua.includes('Firefox')) return 'Firefox Browser';
  if (ua.includes('Safari')) return 'Safari Browser';
  return 'Unknown Device';
}

/** Decode a base64url string into an ArrayBuffer. */
function base64UrlToBuffer(b64url: string): ArrayBuffer {
  const b64 = b64url.replaceAll('-', '+').replaceAll('_', '/');
  const pad = b64.length % 4 === 0 ? '' : '='.repeat(4 - (b64.length % 4));
  const bin = atob(b64 + pad);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.codePointAt(i) ?? 0;
  return bytes.buffer;
}

/**
 * Signal the authenticator (best-effort) that a WebAuthn credential was removed,
 * using the WebAuthn Signal API (signalAllAcceptedCredentialIds, Chrome 132+) so
 * platform/hardware authenticators can purge the deleted credential. Extracted
 * from PrivacySecuritySection so its deep nesting does not inflate the
 * component's cognitive complexity. Swallows failures by design — this is a
 * hint, not a hard requirement.
 */
async function signalRemovedWebAuthnCredential(data: {
  remaining_credential_ids?: string[];
  user_id?: string;
}): Promise<void> {
  try {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any -- WebAuthn Signal API (signalAllAcceptedCredentialIds) shipped in Chrome 132 and is not yet in the standard lib.dom.d.ts; runtime-gated by the `typeof PKC.signalAllAcceptedCredentialIds === 'function'` check below
    const PKC = PublicKeyCredential as any;
    if (
      typeof PublicKeyCredential !== 'undefined' &&
      typeof PKC.signalAllAcceptedCredentialIds === 'function' &&
      data.remaining_credential_ids &&
      data.user_id
    ) {
      const rpId = new URL(API_BASE).hostname;
      // WebAuthn user ID is the UUID string as raw bytes
      const userId = new TextEncoder().encode(data.user_id).buffer;
      const remaining = data.remaining_credential_ids.map(base64UrlToBuffer);
      await PKC.signalAllAcceptedCredentialIds({
        rpId,
        userId,
        allAcceptedCredentialIds: remaining,
      });
    }
  } catch {
    // Signal API is best-effort — don't block on failure
  }
}

/** Session card revoke/confirm action buttons. */
const SessionCardActions: React.FC<{
  sessionId: string;
  isConfirming: boolean;
  isRevoking: boolean;
  onRevoke: (id: string) => void;
  onCancelConfirm: () => void;
}> = ({ sessionId, isConfirming, isRevoking, onRevoke, onCancelConfirm }) => (
  <div className="session-card-actions">
    {isConfirming ? (
      <>
        <button
          className="session-revoke-btn confirm"
          onClick={() => onRevoke(sessionId)}
          disabled={isRevoking}
        >
          {isRevoking ? 'Revoking...' : 'Confirm'}
        </button>
        <button className="session-cancel-btn" onClick={onCancelConfirm}>
          Cancel
        </button>
      </>
    ) : (
      <button
        className="session-revoke-btn"
        onClick={() => onRevoke(sessionId)}
        disabled={isRevoking}
      >
        {isRevoking ? 'Revoking...' : 'Revoke'}
      </button>
    )}
  </div>
);

/** Inline display of backup code count with low-count warning styling. */
const BackupCodesCount: React.FC<{ remaining: number | undefined }> = ({ remaining }) => (
  <>
    <strong className={(remaining ?? 0) <= 2 ? 'mfa-status-warn' : ''}>{remaining}</strong> / 8
  </>
);

function permissionStatusBadge(status: OsPermissionStatus): {
  className: string;
  label: string;
} {
  return PERMISSION_STATUS_BADGES[status] ?? { className: 'os-perm-badge', label: 'Unknown' };
}

const SystemPermissionsSection: React.FC = () => {
  const fetchAll = useOsPermissionStore((s) => s.fetchAll);
  const requestOne = useOsPermissionStore((s) => s.requestOne);
  const openSettings = useOsPermissionStore((s) => s.openSettings);
  const isLoaded = useOsPermissionStore((s) => s.isLoaded);
  const [platform, setPlatform] = useState('');

  // Fetch fresh permission statuses when Settings is opened
  useEffect(() => {
    fetchAll();
    globalThis.electron?.getPlatform?.().then(setPlatform);
  }, [fetchAll]);

  // On non-macOS, mic/camera/screen are always 'granted' (Chromium handles them).
  // Hide request/fix buttons for those since there's no OS-level gate.
  const hasMacOsGate = (type: OsPermissionType): boolean => {
    if (platform !== 'darwin') return false;
    return type === 'microphone' || type === 'camera' || type === 'screen';
  };

  const showActionButton = (type: OsPermissionType, status: OsPermissionStatus): boolean => {
    // secureStorage: show "Fix" if unavailable (all platforms)
    if (type === 'secureStorage') return status === 'unavailable';
    // notifications: show "Request" if not-determined (all platforms)
    if (type === 'notifications') return status === 'not-determined';
    // mic/camera/screen: only actionable on macOS
    if (!hasMacOsGate(type)) return false;
    return status === 'not-determined' || status === 'denied' || status === 'restricted';
  };

  return (
    <CollapsibleSection id="section-system-permissions" title="System Permissions">
      <p className="settings-section-description">
        Concord requests system permissions only when needed. If a permission is denied, you can
        grant it in your operating system&apos;s settings.
      </p>

      {isLoaded ? (
        PERMISSION_ROWS.map((row) => (
          <PermissionRow
            key={row.type}
            type={row.type}
            label={row.label}
            description={row.description}
            critical={row.critical}
            showAction={showActionButton}
            onRequest={requestOne}
            onOpenSettings={openSettings}
          />
        ))
      ) : (
        <div className="settings-row">
          <span className="settings-row-label">Loading permission statuses...</span>
        </div>
      )}
    </CollapsibleSection>
  );
};

const PermissionRow: React.FC<{
  type: OsPermissionType;
  label: string;
  description: string;
  critical?: boolean;
  showAction: (type: OsPermissionType, status: OsPermissionStatus) => boolean;
  onRequest: (type: OsPermissionType) => Promise<OsPermissionStatus>;
  onOpenSettings: (type: OsPermissionType) => Promise<void>;
}> = ({ type, label, description, critical, showAction, onRequest, onOpenSettings }) => {
  const status = useOsPermissionStore((s) => s[type]);
  const badge = permissionStatusBadge(status);
  const [isRequesting, setIsRequesting] = useState(false);

  const handleRequest = async () => {
    setIsRequesting(true);
    try {
      await onRequest(type);
    } finally {
      setIsRequesting(false);
    }
  };

  const handleOpenSettings = () => {
    onOpenSettings(type);
  };

  return (
    <div
      className={`settings-row ${critical && status !== 'granted' ? 'settings-row--warning' : ''}`}
    >
      <div className="settings-row-info">
        <span className="settings-row-label">
          {label}
          {critical && <span className="os-perm-critical"> (Required)</span>}
        </span>
        <span className="settings-row-hint">{description}</span>
        {critical && status !== 'granted' && (
          <span className="settings-row-hint os-perm-warning">
            Secure storage is required for login. Please enable keychain / credential manager
            access.
          </span>
        )}
      </div>
      <div className="os-perm-actions">
        <span className={badge.className}>{badge.label}</span>
        {showAction(type, status) && (
          <>
            {status === 'not-determined' ? (
              <button
                className="btn btn-sm btn-primary"
                onClick={handleRequest}
                disabled={isRequesting}
              >
                {isRequesting ? 'Requesting...' : 'Request'}
              </button>
            ) : (
              <button className="btn btn-sm btn-secondary" onClick={handleOpenSettings}>
                Fix
              </button>
            )}
          </>
        )}
      </div>
    </div>
  );
};

type SSOField = 'trust_sso_security' | 'password_login_disabled';

interface SSOToggleRowProps {
  field: SSOField;
  checked: boolean;
  label: string;
  warning: React.ReactNode;
  confirmInputId: string;
  activeField: SSOField | null;
  passphrase: string;
  onPassphraseChange: (value: string) => void;
  loading: boolean;
  error: string;
  onToggle: (field: SSOField, checked: boolean) => void;
  onSubmit: () => void;
  onCancel: () => void;
}

/**
 * One SSO-security toggle row (checkbox + warning + inline passphrase-confirm).
 * Extracted from PrivacySecuritySection (SC-2) so the two near-identical rows
 * (trust-SSO-security, disable-password-login) share one implementation and stop
 * inflating the parent's cognitive complexity. Behavior is unchanged.
 */
const SSOToggleRow: React.FC<SSOToggleRowProps> = ({
  field,
  checked,
  label,
  warning,
  confirmInputId,
  activeField,
  passphrase,
  onPassphraseChange,
  loading,
  error,
  onToggle,
  onSubmit,
  onCancel,
}) => (
  <div className="sso-toggle-row">
    <label className="sso-toggle-label">
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onToggle(field, e.target.checked)}
        disabled={activeField !== null && activeField !== field}
      />
      <span>{label}</span>
    </label>
    <p className="sso-toggle-warning">{warning}</p>
    {activeField === field && (
      <div className="sso-toggle-confirm">
        <label htmlFor={confirmInputId}>Enter your passphrase to confirm</label>
        <input
          id={confirmInputId}
          type="password"
          value={passphrase}
          onChange={(e) => onPassphraseChange(e.target.value)}
          disabled={loading}
          autoComplete="current-password"
        />
        {error && <p className="sso-toggle-error">{error}</p>}
        <div className="sso-toggle-confirm-actions">
          <button
            type="button"
            className="btn btn-sm btn-primary"
            onClick={onSubmit}
            disabled={loading || !passphrase}
          >
            {loading ? 'Saving...' : 'Confirm'}
          </button>
          <button
            type="button"
            className="btn btn-sm btn-secondary"
            onClick={onCancel}
            disabled={loading}
          >
            Cancel
          </button>
        </div>
      </div>
    )}
  </div>
);

const PrivacySecuritySection: React.FC = () => {
  const accessToken = useAuthStore((s) => s.accessToken);
  const logout = useUserStore((s) => s.logout);
  const navigate = useNavigate();
  const privacySettings = usePrivacyStore((s) => s.settings);
  const fetchPrivacy = usePrivacyStore((s) => s.fetchPrivacy);
  const updatePrivacy = usePrivacyStore((s) => s.updatePrivacy);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [pastSessions, setPastSessions] = useState<PastSession[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [revokingId, setRevokingId] = useState<string | null>(null);
  const [isRevokingAll, setIsRevokingAll] = useState(false);
  const [confirmRevoke, setConfirmRevoke] = useState<string | null>(null); // session id for individual confirm
  const [showRevokeAllModal, setShowRevokeAllModal] = useState(false);
  const [revokePassword, setRevokePassword] = useState('');
  const [revokeMfaCode, setRevokeMfaCode] = useState('');
  const [revokePasswordError, setRevokePasswordError] = useState('');

  // Revocation mode (Simple / Secure toggle)
  const [revocationMode, setRevocationMode] = useState<'simple' | 'secure'>('secure');
  const [showModeChangeModal, setShowModeChangeModal] = useState(false);
  const [pendingMode, setPendingMode] = useState<'simple' | 'secure' | null>(null);
  const [modeChangePassword, setModeChangePassword] = useState('');
  const [modeChangeMfaCode, setModeChangeMfaCode] = useState('');
  const [modeChangeError, setModeChangeError] = useState('');
  const [isChangingMode, setIsChangingMode] = useState(false);

  // Individual session revoke auth modal
  const [showSessionPasswordModal, setShowSessionPasswordModal] = useState(false);
  const [sessionPasswordTarget, setSessionPasswordTarget] = useState<string | null>(null);
  const [sessionMfaCode, setSessionMfaCode] = useState('');

  // MFA state
  const [mfaMethods, setMfaMethods] = useState<string[]>([]);
  const [mfaRecoveryOnly, setMfaRecoveryOnly] = useState<string[]>([]);
  const [mfaRecoveryHardened, setMfaRecoveryHardened] = useState(false);
  const [mfaBackupRemaining, setMfaBackupRemaining] = useState<number | undefined>();
  const [mfaWebauthnCredentials, setMfaWebauthnCredentials] = useState<WebAuthnCredential[]>([]);
  const [mfaBackupEmail, setMfaBackupEmail] = useState('');
  const [mfaSetupMethod, setMfaSetupMethod] = useState<'totp' | 'webauthn' | 'email-sms' | null>(
    null
  );
  const [webauthnCredentialType, setWebauthnCredentialType] = useState<'hardware' | 'platform'>(
    'hardware'
  );
  const [sessionPassword, setSessionPassword] = useState('');
  const [sessionPasswordError, setSessionPasswordError] = useState('');

  // Backup code reset modal
  const [showBackupReset, setShowBackupReset] = useState(false);
  const [backupResetPassword, setBackupResetPassword] = useState('');
  const [backupResetMfaCode, setBackupResetMfaCode] = useState('');
  const [backupResetError, setBackupResetError] = useState('');
  const [backupResetLoading, setBackupResetLoading] = useState(false);
  const [backupResetCodes, setBackupResetCodes] = useState<string[] | null>(null);

  // SSO Security flags (issue #270). Hydrated on mount from
  // GET /users/me/security so the toggles reflect the actual server state
  // instead of the user's most-recent local intent. PATCH updates the local
  // mirror on success.
  const [trustSSOSecurity, setTrustSSOSecurity] = useState<boolean>(false);
  const [passwordLoginDisabled, setPasswordLoginDisabled] = useState<boolean>(false);
  // Inline-passphrase confirm panels — one per toggle. `null` = collapsed.
  const [ssoConfirmField, setSsoConfirmField] = useState<
    'trust_sso_security' | 'password_login_disabled' | null
  >(null);
  const [ssoConfirmPassphrase, setSsoConfirmPassphrase] = useState<string>('');
  const [ssoConfirmDesiredValue, setSsoConfirmDesiredValue] = useState<boolean>(false);
  const [ssoConfirmError, setSsoConfirmError] = useState<string>('');
  const [ssoConfirmLoading, setSsoConfirmLoading] = useState<boolean>(false);

  // Local DM privacy level for responsive slider (debounces API calls)
  const [localDmLevel, setLocalDmLevel] = useState<DMPrivacyLevel>(privacySettings.dmPrivacyLevel);
  const dmDebounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Sync local state when store updates (e.g., after fetch)
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: syncs localDmLevel from store when dmPrivacyLevel changes (e.g., after settings fetch); not a render loop
    setLocalDmLevel(privacySettings.dmPrivacyLevel);
  }, [privacySettings.dmPrivacyLevel]);

  const setDmPrivacyLevel = useCallback(
    (level: DMPrivacyLevel) => {
      setLocalDmLevel(level);
      if (dmDebounceRef.current) clearTimeout(dmDebounceRef.current);
      dmDebounceRef.current = setTimeout(() => {
        updatePrivacy({ dmPrivacyLevel: level });
      }, 300);
    },
    [updatePrivacy]
  );

  // Cleanup debounce timer
  useEffect(() => {
    return () => {
      if (dmDebounceRef.current) clearTimeout(dmDebounceRef.current);
    };
  }, []);

  const fetchSessions = useCallback(async () => {
    if (!accessToken) return;
    setIsLoading(true);
    setError(null);
    try {
      const data = await fetchSessionsData();
      setSessions(data.sessions);
      setPastSessions(data.pastSessions);
      if (data.revocationMode) setRevocationMode(data.revocationMode);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch sessions');
    } finally {
      setIsLoading(false);
    }
  }, [accessToken]);

  const fetchMFAStatus = useCallback(async () => {
    if (!accessToken) return;
    const data = await fetchMFAStatusData();
    if (!data) return; // helper returned null on transient failure; keep prior state
    setMfaMethods(data.methods);
    setMfaRecoveryOnly(data.recoveryOnly);
    setMfaRecoveryHardened(data.recoveryHardened);
    setMfaBackupRemaining(data.backupRemaining);
    setMfaBackupEmail(data.backupEmail);
    setMfaWebauthnCredentials(data.credentials);
  }, [accessToken]);

  // Hydrate the SSO Security toggle states from GET /users/me/security so the
  // checkboxes reflect actual server state on mount. Preserves last-known state
  // on transient failure (helper returns `null`) rather than snapping back to
  // false, which would silently override user changes during a refetch.
  const fetchSSOSecurity = useCallback(async () => {
    if (!accessToken) return;
    const data = await fetchSSOSecurityData();
    if (!data) return; // helper returned null on transient failure; keep prior state
    setTrustSSOSecurity(data.trustSSOSecurity);
    setPasswordLoginDisabled(data.passwordLoginDisabled);
  }, [accessToken]);

  useEffect(() => {
    fetchSessions();
    fetchPrivacy();
    fetchMFAStatus();
    fetchSSOSecurity();
  }, [fetchSessions, fetchPrivacy, fetchMFAStatus, fetchSSOSecurity]);

  const handleBackupReset = async () => {
    setBackupResetError('');
    setBackupResetLoading(true);
    try {
      const res = await apiFetch('/api/v1/mfa/backup-codes/regenerate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: backupResetPassword, mfa_code: backupResetMfaCode }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Failed to regenerate backup codes');
      setBackupResetCodes(data.backup_codes || []);
      fetchMFAStatus();
    } catch (err) {
      setBackupResetError(err instanceof Error ? err.message : 'Failed to regenerate codes');
    } finally {
      setBackupResetLoading(false);
    }
  };

  // Sort sessions: current session always first, then by last_used descending
  const sortedSessions = useMemo(() => {
    return [...sessions].sort((a, b) => {
      if (a.is_current && !b.is_current) return -1;
      if (!a.is_current && b.is_current) return 1;
      return new Date(b.last_used).getTime() - new Date(a.last_used).getTime();
    });
  }, [sessions]);

  const handleRevoke = async (sessionId: string, password?: string, mfaCode?: string) => {
    if (!accessToken) return;

    // Check if this is the current session — requires confirmation
    const session = sessions.find((s) => s.id === sessionId);
    if (session?.is_current && confirmRevoke !== sessionId) {
      setConfirmRevoke(sessionId);
      return;
    }

    setConfirmRevoke(null);
    setRevokingId(sessionId);

    try {
      const fetchOpts = buildAuthFetchOpts('DELETE', password, mfaCode);
      const response = await apiFetch(`/api/v1/sessions/${sessionId}`, fetchOpts);

      if (!response.ok) {
        const data = await response.json();
        if (response.status === 403) {
          const handled = handleRevoke403(data.error, sessionId);
          if (handled) {
            setRevokingId(null);
            return;
          }
        }
        throw new Error(data.error || 'Failed to revoke session');
      }

      // Success — close password modal if open
      setShowSessionPasswordModal(false);
      setSessionPasswordTarget(null);
      setSessionPassword('');
      setSessionPasswordError('');

      // If we revoked the current session, log out
      if (session?.is_current) {
        await logout();
        navigate('/');
        return;
      }

      setSessions((prev) => prev.filter((s) => s.id !== sessionId));
      fetchSessions();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to revoke session');
    } finally {
      setRevokingId(null);
    }
  };

  /** Build fetch options with optional auth body. */
  const buildAuthFetchOpts = (method: string, password?: string, mfaCode?: string): RequestInit => {
    const opts: RequestInit = { method };
    if (!password && !mfaCode) return opts;
    opts.headers = { 'Content-Type': 'application/json' };
    const body: Record<string, string> = {};
    if (password) body.password = password;
    if (mfaCode) body.mfa_code = mfaCode;
    opts.body = JSON.stringify(body);
    return opts;
  };

  /** Handle 403 errors from session revocation. Returns true if handled (should abort). */
  const handleRevoke403 = (errorCode: string, sessionId: string): boolean => {
    if (errorCode === 'password_required' || errorCode === 'auth_required') {
      setSessionPasswordTarget(sessionId);
      setSessionPassword('');
      setSessionMfaCode('');
      setSessionPasswordError('');
      setShowSessionPasswordModal(true);
      return true;
    }
    if (errorCode === 'Incorrect password' || errorCode === 'Invalid MFA code') {
      setSessionPasswordError(errorCode);
      return true;
    }
    return false;
  };

  const openRevokeAllModal = () => {
    setRevokePassword('');
    setRevokeMfaCode('');
    setRevokePasswordError('');
    setShowRevokeAllModal(true);
  };

  const closeRevokeAllModal = () => {
    setShowRevokeAllModal(false);
    setRevokePassword('');
    setRevokeMfaCode('');
    setRevokePasswordError('');
  };

  const openModeChangeModal = (mode: 'simple' | 'secure') => {
    setPendingMode(mode);
    setModeChangePassword('');
    setModeChangeMfaCode('');
    setModeChangeError('');
    setShowModeChangeModal(true);
  };

  const closeModeChangeModal = () => {
    setShowModeChangeModal(false);
    setPendingMode(null);
    setModeChangePassword('');
    setModeChangeMfaCode('');
    setModeChangeError('');
  };

  const handleModeChange = async () => {
    if (!accessToken || !pendingMode) return;
    if (hasMFA ? !modeChangeMfaCode : !modeChangePassword) return;

    setIsChangingMode(true);

    try {
      const body: Record<string, string> = { mode: pendingMode };
      if (hasMFA) body.mfa_code = modeChangeMfaCode;
      else body.password = modeChangePassword;

      const response = await apiFetch('/api/v1/sessions/revocation-mode', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        const data = await response.json();
        if (response.status === 403) {
          setModeChangeError(data.error || 'Authentication failed');
          setIsChangingMode(false);
          return;
        }
        throw new Error(data.error || 'Failed to change revocation mode');
      }

      const data = await response.json();
      setRevocationMode(data.revocation_mode);
      closeModeChangeModal();
    } catch (err) {
      setModeChangeError(err instanceof Error ? err.message : 'Failed to change revocation mode');
    } finally {
      setIsChangingMode(false);
    }
  };

  const closeSessionPasswordModal = () => {
    setShowSessionPasswordModal(false);
    setSessionPasswordTarget(null);
    setSessionPassword('');
    setSessionMfaCode('');
    setSessionPasswordError('');
  };

  const hasMFA = mfaMethods.length > 0;

  const handleRevokeAll = async () => {
    if (!accessToken) return;

    const requiredField = hasMFA ? revokeMfaCode : revokePassword;
    if (!requiredField) {
      setRevokePasswordError(hasMFA ? 'MFA verification is required' : 'Password is required');
      return;
    }

    setIsRevokingAll(true);

    try {
      const body: Record<string, unknown> = { include_current: true };
      if (hasMFA) body.mfa_code = revokeMfaCode;
      else body.password = revokePassword;

      const response = await apiFetch('/api/v1/sessions/revoke-all', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        const data = await response.json();
        if (response.status === 403) {
          setRevokePasswordError(data.error || 'Authentication failed');
          setIsRevokingAll(false);
          return;
        }
        throw new Error(data.error || 'Failed to revoke sessions');
      }

      setRevokePassword('');
      setRevokeMfaCode('');
      setShowRevokeAllModal(false);
      // All sessions revoked including current — log out
      await logout();
      navigate('/');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to revoke sessions');
      setShowRevokeAllModal(false);
    } finally {
      setIsRevokingAll(false);
    }
  };

  // ─── SSO Security toggle handlers (issue #270) ───────────────────────
  // Each click on a toggle reveals an inline passphrase confirm. Submit
  // PATCHes /users/me/security with the desired flag value. `would_lock_out`
  // (returned by the backend if e.g. disabling password login while no SSO
  // is linked) is surfaced inline.
  const requestSSOToggle = (
    field: 'trust_sso_security' | 'password_login_disabled',
    desired: boolean
  ): void => {
    setSsoConfirmField(field);
    setSsoConfirmDesiredValue(desired);
    setSsoConfirmPassphrase('');
    setSsoConfirmError('');
  };

  const cancelSSOToggle = (): void => {
    setSsoConfirmField(null);
    setSsoConfirmPassphrase('');
    setSsoConfirmError('');
  };

  const submitSSOToggle = async (): Promise<void> => {
    if (!ssoConfirmField || !ssoConfirmPassphrase) return;
    setSsoConfirmLoading(true);
    setSsoConfirmError('');
    try {
      const body: Record<string, unknown> = { current_passphrase: ssoConfirmPassphrase };
      body[ssoConfirmField] = ssoConfirmDesiredValue;
      const res = await apiFetch('/api/v1/users/me/security', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        const errBody = (await res.json().catch(() => ({}))) as { error_code?: string };
        setSsoConfirmError(resolveSSOToggleError(errBody.error_code));
        return;
      }
      // Success — update local mirror and close confirm.
      const applyToggle =
        ssoConfirmField === 'trust_sso_security' ? setTrustSSOSecurity : setPasswordLoginDisabled;
      applyToggle(ssoConfirmDesiredValue);
      setSsoConfirmField(null);
      setSsoConfirmPassphrase('');
    } catch {
      setSsoConfirmError('Network error. Please try again.');
    } finally {
      setSsoConfirmLoading(false);
    }
  };

  // ─── MFA action handlers (extracted from JSX props so their branching does
  // not inflate this component's cognitive complexity) ─────────────────────
  const handleResetTOTP = async (password: string, code: string): Promise<boolean> => {
    const res = await apiFetch('/api/v1/mfa/totp/disable', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password, code }),
    });
    if (res.ok) {
      fetchMFAStatus();
      return true;
    }
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || 'Failed to disable TOTP');
  };

  const handleRevokeWebAuthnKey = async (
    credentialId: string,
    password: string
  ): Promise<boolean> => {
    const res = await apiFetch(`/api/v1/mfa/webauthn/credentials/${credentialId}`, {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    });
    if (res.ok) {
      const data = await res.json();
      // Signal the authenticator to clean up the deleted credential (best-effort).
      await signalRemovedWebAuthnCredential(data);
      fetchMFAStatus();
      return true;
    }
    const errData = await res.json().catch(() => ({}));
    throw new Error(errData.error || 'Failed to revoke credential');
  };

  const handleDisableEmailSms = async (): Promise<boolean> => {
    const res = await apiFetch('/api/v1/mfa/email-sms/disable', { method: 'POST' });
    if (res.ok) {
      fetchMFAStatus();
      return true;
    }
    const errData = await res.json().catch(() => ({}));
    throw new Error(errData.error || 'Failed to disable Email/SMS');
  };

  const handleSetBackupEmail = async (email: string): Promise<boolean> => {
    const res = await apiFetch('/api/v1/mfa/backup-email', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email }),
    });
    if (res.ok) {
      const data = await res.json();
      setMfaBackupEmail(data.backup_email || '');
      return true;
    }
    return false;
  };

  const handleToggleRecoveryHardened = async (
    enabled: boolean,
    password: string,
    mfaCode?: string
  ): Promise<boolean> => {
    try {
      const res = await apiFetch('/api/v1/mfa/recovery-hardened', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled, password, mfa_code: mfaCode || undefined }),
      });
      if (res.ok) {
        const data = await res.json();
        setMfaRecoveryHardened(data.recovery_hardened);
        return true;
      }
      return false;
    } catch {
      return false;
    }
  };

  const handleToggleRecoveryOnly = async (
    method: string,
    recoveryOnly: boolean,
    password: string,
    mfaCode?: string
  ): Promise<boolean> => {
    const newList = recoveryOnly
      ? [...mfaRecoveryOnly, method]
      : mfaRecoveryOnly.filter((m) => m !== method);
    try {
      const res = await apiFetch('/api/v1/mfa/recovery-only', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          methods: newList,
          password,
          mfa_code: mfaCode || undefined,
        }),
      });
      if (res.ok) {
        const data = await res.json();
        setMfaRecoveryOnly(data.recovery_only_methods || []);
        if (data.recovery_hardened !== undefined) {
          setMfaRecoveryHardened(data.recovery_hardened);
        }
        return true;
      }
      return false;
    } catch {
      return false;
    }
  };

  return (
    <>
      <CollapsibleSection id="section-privacy-settings" title="Privacy">
        <p className="settings-section-description">
          Control who can message you and how others can find you.
        </p>

        <DMPrivacyControls localDmLevel={localDmLevel} setDmPrivacyLevel={setDmPrivacyLevel} />
        <ContentSafetyControls />
        <SearchVisibilityControls />
      </CollapsibleSection>

      <PresenceSettingsSection />

      <SystemPermissionsSection />

      <CollapsibleSection id="section-mfa" title="Multi-Factor Authentication">
        <p className="settings-section-description">
          Add an extra layer of security to your account. When enabled, you&apos;ll need both your
          password and a second factor to sign in.
        </p>

        <div className="mfa-status-bar">
          <div className="mfa-status-item">
            <svg
              width="16"
              height="16"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              style={{ flexShrink: 0 }}
            >
              <path d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z" />
            </svg>
            <span>
              Security Keys: <strong>{mfaWebauthnCredentials.length}</strong> / 10
            </span>
          </div>
          <div className="mfa-status-item mfa-status-item-right">
            <svg
              width="16"
              height="16"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              style={{ flexShrink: 0 }}
            >
              <path d="M9 12h6m-3-3v6m-7 4h14a2 2 0 002-2V7a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z" />
            </svg>
            <span>
              Backup Codes:{' '}
              {mfaMethods.length === 0 ? (
                <em className="mfa-status-muted">Requires MFA</em>
              ) : (
                <BackupCodesCount remaining={mfaBackupRemaining} />
              )}
            </span>
            <button
              type="button"
              className="btn btn-xs btn-reset-danger"
              disabled={!mfaBackupRemaining || mfaMethods.length === 0}
              onClick={() => {
                setShowBackupReset(true);
                setBackupResetPassword('');
                setBackupResetMfaCode('');
                setBackupResetError('');
                setBackupResetCodes(null);
              }}
            >
              Reset
              <br />
              Codes
            </button>
          </div>
        </div>

        {mfaSetupMethod === 'email-sms' && (
          <EmailSmsSetup
            mfaActive={mfaMethods.length > 0}
            onComplete={() => {
              setMfaSetupMethod(null);
              fetchMFAStatus();
            }}
            onCancel={() => setMfaSetupMethod(null)}
          />
        )}
        {mfaSetupMethod && mfaSetupMethod !== 'email-sms' && (
          <MFASetup
            method={mfaSetupMethod}
            credentialType={mfaSetupMethod === 'webauthn' ? webauthnCredentialType : undefined}
            mfaActive={mfaMethods.length > 0}
            activeMethods={mfaMethods}
            recoveryOnlyMethods={mfaRecoveryOnly}
            onComplete={() => {
              setMfaSetupMethod(null);
              fetchMFAStatus();
            }}
            onCancel={() => setMfaSetupMethod(null)}
          />
        )}
        {!mfaSetupMethod && (
          <MFATierSelector
            activeMethods={mfaMethods}
            recoveryOnlyMethods={mfaRecoveryOnly}
            recoveryHardened={mfaRecoveryHardened}
            backupCodesRemaining={mfaBackupRemaining}
            webauthnCredentials={mfaWebauthnCredentials}
            backupEmail={mfaBackupEmail}
            onSetupTOTP={() => setMfaSetupMethod('totp')}
            onSetupWebAuthn={(credType) => {
              setWebauthnCredentialType(credType);
              setMfaSetupMethod('webauthn');
            }}
            onSetupEmailSms={() => setMfaSetupMethod('email-sms')}
            onResetTOTP={handleResetTOTP}
            onRevokeWebAuthnKey={handleRevokeWebAuthnKey}
            onDisableEmailSms={handleDisableEmailSms}
            onSetBackupEmail={handleSetBackupEmail}
            onToggleRecoveryHardened={handleToggleRecoveryHardened}
            onToggleRecoveryOnly={handleToggleRecoveryOnly}
          />
        )}

        {/* Backup code reset modal */}
        {showBackupReset && (
          <div className="mfa-modal-overlay">
            <div className="mfa-modal">
              <h3>Reset Backup Codes</h3>
              <p className="mfa-modal-desc">
                This will invalidate all existing backup codes and generate new ones.
              </p>

              {backupResetCodes ? (
                <BackupCodeDisplay
                  codes={backupResetCodes}
                  onConfirm={() => {
                    setShowBackupReset(false);
                    setBackupResetCodes(null);
                  }}
                  disabled={false}
                />
              ) : (
                <>
                  <div className="mfa-verify-field">
                    <label htmlFor="backup-reset-password">Password</label>
                    <input
                      id="backup-reset-password"
                      type="password"
                      value={backupResetPassword}
                      onChange={(e) => setBackupResetPassword(e.target.value)}
                      placeholder="Enter your password"
                      disabled={backupResetLoading}
                    />
                  </div>

                  <MFAVerifyPrompt
                    methods={mfaMethods}
                    recoveryOnlyMethods={mfaRecoveryOnly}
                    onVerify={(code) => setBackupResetMfaCode(code)}
                    disabled={backupResetLoading}
                    error={backupResetError || undefined}
                  />

                  <div className="mfa-setup-actions">
                    <button
                      type="button"
                      className="btn btn-sm btn-primary"
                      onClick={handleBackupReset}
                      disabled={backupResetLoading || !backupResetPassword || !backupResetMfaCode}
                    >
                      {backupResetLoading ? 'Regenerating...' : 'Regenerate Codes'}
                    </button>
                    <button
                      type="button"
                      className="btn btn-sm btn-secondary"
                      onClick={() => setShowBackupReset(false)}
                      disabled={backupResetLoading}
                    >
                      Cancel
                    </button>
                  </div>
                </>
              )}
            </div>
          </div>
        )}
      </CollapsibleSection>

      <CollapsibleSection id="section-sso-security" title="SSO Security">
        <p className="settings-section-description">
          Manage your linked single sign-on providers and how they interact with your account.
        </p>

        <LinkedAccountsList />

        <SSOToggleRow
          field="trust_sso_security"
          checked={trustSSOSecurity}
          label="Trust SSO provider security (skip Concord MFA on SSO login)"
          warning="Only enable this if your Google account has multi-factor authentication enabled. If your Google account is compromised, an attacker could access your Concord account without additional verification."
          confirmInputId="sso-confirm-passphrase-trust"
          activeField={ssoConfirmField}
          passphrase={ssoConfirmPassphrase}
          onPassphraseChange={setSsoConfirmPassphrase}
          loading={ssoConfirmLoading}
          error={ssoConfirmError}
          onToggle={requestSSOToggle}
          onSubmit={() => void submitSSOToggle()}
          onCancel={cancelSSOToggle}
        />

        <SSOToggleRow
          field="password_login_disabled"
          checked={passwordLoginDisabled}
          label="Disable password login (require SSO every sign-in)"
          warning={
            passwordLoginDisabled
              ? 'You can only sign in with your linked SSO providers. Make sure Social Recovery trustees are configured.'
              : 'Your account will be vulnerable to password phishing.'
          }
          confirmInputId="sso-confirm-passphrase-pwlogin"
          activeField={ssoConfirmField}
          passphrase={ssoConfirmPassphrase}
          onPassphraseChange={setSsoConfirmPassphrase}
          loading={ssoConfirmLoading}
          error={ssoConfirmError}
          onToggle={requestSSOToggle}
          onSubmit={() => void submitSSOToggle()}
          onCancel={cancelSSOToggle}
        />
      </CollapsibleSection>

      <CollapsibleSection id="section-active-sessions" title="Active Sessions">
        <p className="settings-section-description">
          These are the devices currently logged into your account. Revoke any session you
          don&apos;t recognize.
        </p>

        {!isLoading && (
          <div className="session-revocation-mode">
            <div className="session-revocation-mode-info">
              <span className="session-revocation-mode-label">Session Revocation</span>
              <span className="session-revocation-mode-description">
                {revocationModeDescription(revocationMode)}
              </span>
            </div>
            <div className="session-revocation-mode-toggle">
              <button
                className={`revocation-mode-btn ${revocationMode === 'secure' ? 'active' : ''}`}
                onClick={() => {
                  if (revocationMode !== 'secure') openModeChangeModal('secure');
                }}
              >
                Secure
              </button>
              <button
                className={`revocation-mode-btn ${revocationMode === 'simple' ? 'active' : ''}`}
                onClick={() => {
                  if (revocationMode !== 'simple') openModeChangeModal('simple');
                }}
              >
                Simple
              </button>
            </div>
          </div>
        )}

        {error && <div className="settings-error">{error}</div>}

        {isLoading ? (
          <div className="settings-loading">
            <LoadingSpinner size="small" inline />
          </div>
        ) : (
          <>
            {sessions.length > 0 && (
              <div className="sessions-actions-top">
                <button
                  className="sessions-revoke-all-btn"
                  onClick={openRevokeAllModal}
                  disabled={isRevokingAll}
                >
                  Revoke All Sessions
                </button>
              </div>
            )}

            <div className="sessions-list">
              {sortedSessions.map((session) => (
                <div
                  key={session.id}
                  className={`session-card ${session.is_current ? 'current' : ''}`}
                >
                  <div className="session-card-icon">
                    <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
                      <rect
                        x="2"
                        y="3"
                        width="16"
                        height="12"
                        rx="2"
                        stroke="currentColor"
                        strokeWidth="1.5"
                      />
                      <path
                        d="M7 19h6M10 15v4"
                        stroke="currentColor"
                        strokeWidth="1.5"
                        strokeLinecap="round"
                      />
                    </svg>
                  </div>
                  <div className="session-card-info">
                    <div className="session-card-title">
                      {parseUserAgent(session.user_agent)}
                      {session.is_current && (
                        <span className="session-card-badge">This Device</span>
                      )}
                    </div>
                    <div className="session-card-details">
                      <span>{session.ip_address}</span>
                      <span>Active {formatRelativeTime(session.last_used)}</span>
                      <span>Created {formatRelativeTime(session.created_at)}</span>
                    </div>
                    {confirmRevoke === session.id && session.is_current && (
                      <div className="session-confirm-warning">
                        This is your current active session. Revoking it will log you out and you
                        must sign back in.
                      </div>
                    )}
                  </div>
                  <SessionCardActions
                    sessionId={session.id}
                    isConfirming={confirmRevoke === session.id}
                    isRevoking={revokingId === session.id}
                    onRevoke={handleRevoke}
                    onCancelConfirm={() => setConfirmRevoke(null)}
                  />
                </div>
              ))}
            </div>
          </>
        )}

        <Modal
          isOpen={showRevokeAllModal}
          onClose={closeRevokeAllModal}
          title="Revoke All Sessions"
          width="small"
        >
          <div className="revoke-all-modal-content">
            <p className="revoke-all-modal-description">
              You&apos;re about to revoke all of your active session tokens, which will log you out
              of all sessions, including this one.{' '}
              {hasMFA
                ? 'Verify your identity to continue.'
                : 'In order to continue, please input your password.'}
            </p>
            <AuthVerifyField
              hasMFA={hasMFA}
              mfaMethods={mfaMethods}
              mfaRecoveryOnly={mfaRecoveryOnly}
              password={revokePassword}
              onPasswordChange={setRevokePassword}
              onMfaVerify={setRevokeMfaCode}
              error={revokePasswordError}
              onClearError={() => setRevokePasswordError('')}
              disabled={isRevokingAll}
              inputId="revoke-all-password"
              onEnterKey={handleRevokeAll}
            />
            <div className="revoke-all-modal-actions">
              <button className="revoke-all-modal-cancel-btn" onClick={closeRevokeAllModal}>
                No, Cancel
              </button>
              <button
                className="revoke-all-modal-confirm-btn"
                onClick={handleRevokeAll}
                disabled={
                  isRevokingAll || isAuthConfirmDisabled(hasMFA, revokeMfaCode, revokePassword)
                }
              >
                {isRevokingAll ? 'Revoking...' : 'Yes, Revoke All Sessions'}
              </button>
            </div>
          </div>
        </Modal>

        {/* Individual session revoke auth modal */}
        <Modal
          isOpen={showSessionPasswordModal}
          onClose={closeSessionPasswordModal}
          title="Verify Your Identity"
          width="small"
        >
          <div className="revoke-all-modal-content">
            <p className="revoke-all-modal-description">
              For your security, {hasMFA ? 'verify your identity' : 'please enter your password'} to
              revoke this session.
            </p>
            <AuthVerifyField
              hasMFA={hasMFA}
              mfaMethods={mfaMethods}
              mfaRecoveryOnly={mfaRecoveryOnly}
              password={sessionPassword}
              onPasswordChange={setSessionPassword}
              onMfaVerify={setSessionMfaCode}
              error={sessionPasswordError}
              onClearError={() => setSessionPasswordError('')}
              disabled={revokingId === sessionPasswordTarget}
              inputId="session-revoke-password"
              onEnterKey={
                sessionPasswordTarget
                  ? () => handleRevoke(sessionPasswordTarget, sessionPassword)
                  : undefined
              }
            />
            <div className="revoke-all-modal-actions">
              <button className="revoke-all-modal-cancel-btn" onClick={closeSessionPasswordModal}>
                Cancel
              </button>
              <button
                className="revoke-all-modal-confirm-btn"
                onClick={() =>
                  sessionPasswordTarget &&
                  handleRevoke(
                    sessionPasswordTarget,
                    hasMFA ? undefined : sessionPassword,
                    hasMFA ? sessionMfaCode : undefined
                  )
                }
                disabled={
                  isAuthConfirmDisabled(hasMFA, sessionMfaCode, sessionPassword) ||
                  revokingId === sessionPasswordTarget
                }
              >
                {revokingId === sessionPasswordTarget ? 'Revoking...' : 'Confirm & Revoke'}
              </button>
            </div>
          </div>
        </Modal>

        {/* Revocation mode change modal */}
        <Modal
          isOpen={showModeChangeModal}
          onClose={closeModeChangeModal}
          title="Change Revocation Mode"
          width="small"
        >
          <div className="revoke-all-modal-content">
            <p className="revoke-all-modal-description">
              {pendingMode === 'simple'
                ? 'Switching to Simple Revocation. You will be able to authenticate once and freely manage sessions for a short period.'
                : 'Switching to Secure Revocation. Authentication will be required to revoke sessions under certain circumstances.'}
            </p>
            <AuthVerifyField
              hasMFA={hasMFA}
              mfaMethods={mfaMethods}
              mfaRecoveryOnly={mfaRecoveryOnly}
              password={modeChangePassword}
              onPasswordChange={setModeChangePassword}
              onMfaVerify={setModeChangeMfaCode}
              error={modeChangeError}
              onClearError={() => setModeChangeError('')}
              disabled={isChangingMode}
              inputId="mode-change-password"
              onEnterKey={handleModeChange}
              excludeBackupCodes
            />
            <div className="revoke-all-modal-actions">
              <button className="revoke-all-modal-cancel-btn" onClick={closeModeChangeModal}>
                Cancel
              </button>
              <button
                className="revoke-all-modal-confirm-btn"
                onClick={handleModeChange}
                disabled={
                  isChangingMode ||
                  isAuthConfirmDisabled(hasMFA, modeChangeMfaCode, modeChangePassword)
                }
              >
                {isChangingMode ? 'Changing...' : 'Confirm'}
              </button>
            </div>
          </div>
        </Modal>
      </CollapsibleSection>

      {pastSessions.length > 0 && (
        <CollapsibleSection id="section-past-sessions" title="Past Sessions">
          <p className="settings-section-description">
            Sessions that were logged out or revoked in the last 30 days.
          </p>

          <div className="sessions-list">
            {pastSessions.map((session) => (
              <div key={session.id} className="session-card past">
                <div className="session-card-icon past">
                  <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
                    <rect
                      x="2"
                      y="3"
                      width="16"
                      height="12"
                      rx="2"
                      stroke="currentColor"
                      strokeWidth="1.5"
                    />
                    <path
                      d="M7 19h6M10 15v4"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                    />
                  </svg>
                </div>
                <div className="session-card-info">
                  <div className="session-card-title">
                    {parseUserAgent(session.user_agent)}
                    <span className="session-card-badge past">Revoked</span>
                  </div>
                  <div className="session-card-details">
                    <span>{session.ip_address}</span>
                    <span>Last active {formatRelativeTime(session.last_used)}</span>
                    <span>Revoked {formatRelativeTime(session.revoked_at)}</span>
                  </div>
                </div>
              </div>
            ))}
          </div>
        </CollapsibleSection>
      )}
    </>
  );
};

export default PrivacySecuritySection;
