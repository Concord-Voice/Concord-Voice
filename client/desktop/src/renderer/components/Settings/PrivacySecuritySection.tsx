import React, { useState, useCallback, useEffect, useId, useMemo, useRef } from 'react';
import { useNavigate } from 'react-router';
import { useAuthStore } from '../../stores/auth/authStore';
import { useUserStore } from '../../stores/auth/userStore';
import {
  usePrivacyStore,
  DMPrivacyLevel,
  type FriendRequestPrivacyMode,
  type PrivacySettings,
} from '../../stores/ui/privacyStore';
import { useClientConfigStore } from '../../stores/ui/clientConfigStore';
import { useSettingsOverlayStore } from '../../stores/ui/settingsOverlayStore';
import { hasPendingDrafts, useDraftSettingsStore } from '../../stores/ui/draftSettingsStore';
import { apiFetch, API_BASE } from '../../services/system/apiClient';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
} from '../../services/system/requestContext';
import { adaptSessionsRefusal, serverErrorText } from '../../services/system/stepUpRouteAdapters';
import {
  returnFromVerificationSetup,
  verificationReturnLabel,
} from '../../utils/ui/openVerificationSetup';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import LoadingSpinner from '../Auth/LoadingSpinner';
import MFATierSelector, { WebAuthnCredential } from './MFATierSelector';
import MFASetup from './MFASetup';
import { submitMfaStepUp, type MfaSeamHandler } from './mfaStepUp';
import ErrorBanner from './ErrorBanner';
import BackupCodeDisplay from './BackupCodeDisplay';
import BackupCodeRegenerateStage from './BackupCodeRegenerateStage';
import SessionStepUpDialog, {
  sessionStepUpKey,
  type SessionStepUpAction,
} from './SessionStepUpDialog';
import EmailSmsSetup from './EmailSmsSetup';
import CollapsibleSection from './CollapsibleSection';
import {
  useOsPermissionStore,
  type OsPermissionType,
  type OsPermissionStatus,
} from '../../stores/voice/osPermissionStore';
import DMPrivacyControls from './DMPrivacyControls';
import FriendRequestPrivacyControls from './FriendRequestPrivacyControls';
import ContentSafetyControls from './ContentSafetyControls';
import SearchVisibilityControls from './SearchVisibilityControls';
import LinkedAccountsList from './LinkedAccountsList';
import PresenceSettingsSection from './PresenceSettingsSection';
import ActivityHistoryCard from './ActivityHistoryCard';
import PresenceHistorySection from '../Profile/PresenceHistorySection';
import ToggleSwitch from './ToggleSwitch';
import PurgeFenceStepUpDialog from './PurgeFenceStepUpDialog';
import {
  setDraftContentProtection,
  useDraftContentProtection,
  useDraftContentProtectionApplying,
  useDraftContentProtectionLoaded,
} from '../../hooks/ui/useDraftSettings';
import './MFA.css';

// #1354: the purge dead-end card navigates here by control id
// (`settingsNavStore.requestFocus('privacy', 'requireAuthBeforePurge')`),
// so the switch id and the label it points at are a cross-component contract.
const PURGE_AUTH_LABEL_ID = 'requireAuthBeforePurge-label';
const CONTENT_PROTECTION_LABEL_ID = 'contentProtection-label';
const CONTENT_PROTECTION_HINT_ID = 'contentProtection-hint';

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

const REVOKE_FAILED = 'Failed to revoke session';

/** What a single revoke's credential-free DELETE came to (#7). */
type RevokeAnswer =
  | { kind: 'revoked' }
  | { kind: 'stepUp'; seed: StepUpFactorRefusal }
  | { kind: 'failed'; message: string };

/**
 * Reads a single revoke's answer through the route adapter. A body that is not
 * JSON (a proxy's error page) reads as no body, so it reaches the banner as
 * the generic sentence instead of as a parse error.
 */
async function readRevokeAnswer(response: Response): Promise<RevokeAnswer> {
  if (response.ok) return { kind: 'revoked' };
  const body: unknown = await response.json().catch(() => null);
  const refusal = adaptSessionsRefusal(response.status, body);
  if (refusal?.kind === 'mfaRequired' || refusal?.kind === 'passwordRequired') {
    return { kind: 'stepUp', seed: refusal };
  }
  return { kind: 'failed', message: serverErrorText(body) ?? REVOKE_FAILED };
}

interface MFAStatusFetchResult {
  methods: string[];
  recoveryOnly: string[];
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

/**
 * The one "Back to …" button (#3456 §3.6a): shown when verification setup was
 * opened from somewhere (the pending return) and a TOTP or key setup has just
 * finished here. Nothing else renders; the pending return is taken on use, so
 * the button goes with it.
 *
 * Leaving App Settings drops a change the user has not applied, so while one
 * is pending the button stays focusable but does nothing, and says why; the
 * Apply/Revert prompt is already on screen, as for "Back to app".
 */
const VerificationReturnButton: React.FC<{ setupCompleted: boolean }> = ({ setupCompleted }) => {
  const target = useSettingsOverlayStore((s) => s.verificationReturn);
  const pendingChanges = useDraftSettingsStore((s) => hasPendingDrafts(s.drafts));
  const hintId = useId();
  if (!setupCompleted || target === null) return null;
  return (
    <>
      <button
        type="button"
        className="btn btn-sm btn-secondary"
        aria-disabled={pendingChanges || undefined}
        aria-describedby={pendingChanges ? hintId : undefined}
        onClick={() => {
          if (!pendingChanges) void returnFromVerificationSetup();
        }}
      >
        {verificationReturnLabel(target)}
      </button>
      {pendingChanges && (
        <span className="settings-row-hint" id={hintId}>
          Apply or revert your settings changes first.
        </span>
      )}
    </>
  );
};

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

  // Fetch fresh permission statuses when Settings is opened.
  useEffect(() => {
    fetchAll();
  }, [fetchAll]);

  return (
    <CollapsibleSection id="section-system-permissions" title="System Permissions">
      <p className="settings-section-description">
        These show the status of permissions managed by your operating system — they are not in-app
        switches. Concord requests each permission only when it&apos;s needed. Use &ldquo;Open
        System Settings&rdquo; to review or change a permission in your OS.
      </p>

      {isLoaded ? (
        PERMISSION_ROWS.map((row) => (
          <PermissionRow
            key={row.type}
            type={row.type}
            label={row.label}
            description={row.description}
            critical={row.critical}
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
  onRequest: (type: OsPermissionType) => Promise<OsPermissionStatus>;
  onOpenSettings: (type: OsPermissionType) => Promise<void>;
}> = ({ type, label, description, critical, onRequest, onOpenSettings }) => {
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
        <span className="settings-row-hint os-perm-managed">Managed by your operating system.</span>
        {critical && status !== 'granted' && (
          <span className="settings-row-hint os-perm-warning">
            Secure storage is required for login. Please enable keychain / credential manager
            access.
          </span>
        )}
      </div>
      <div className="os-perm-actions">
        <span className={badge.className}>{badge.label}</span>
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
            Open System Settings
          </button>
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
 * One SSO-security toggle row (switch + warning + inline passphrase-confirm).
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
}) => {
  const labelId = `sso-toggle-${field}-label`;
  const switchId = `sso-toggle-${field}`;

  return (
    <div className="sso-toggle-row">
      <div className="sso-toggle-label">
        <ToggleSwitch
          id={switchId}
          checked={checked}
          onChange={(nextChecked) => onToggle(field, nextChecked)}
          disabled={activeField !== null && activeField !== field}
          ariaLabelledBy={labelId}
          inputRole="switch"
        />
        <label id={labelId} htmlFor={switchId}>
          {label}
        </label>
      </div>
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
};

interface SessionCardProps {
  session: Session;
  confirmRevoke: string | null;
  revokingId: string | null;
  onRevoke: React.ComponentProps<typeof SessionCardActions>['onRevoke'];
  onCancelConfirm: () => void;
}

/**
 * One row in the active-sessions list. Extracted from PrivacySecuritySection to
 * keep that component's cognitive complexity within the SonarCloud S3776 budget
 * (the per-row is_current / confirm-warning conditionals were its deepest-nested
 * branches). Behaviour is identical — same markup, props threaded straight through.
 */
const SessionCard: React.FC<SessionCardProps> = ({
  session,
  confirmRevoke,
  revokingId,
  onRevoke,
  onCancelConfirm,
}) => (
  <div className={`session-card ${session.is_current ? 'current' : ''}`}>
    <div className="session-card-icon">
      <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
        <rect x="2" y="3" width="16" height="12" rx="2" stroke="currentColor" strokeWidth="1.5" />
        <path d="M7 19h6M10 15v4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
    </div>
    <div className="session-card-info">
      <div className="session-card-title">
        {parseUserAgent(session.user_agent)}
        {session.is_current && <span className="session-card-badge">This Device</span>}
      </div>
      <div className="session-card-details">
        <span>{session.ip_address}</span>
        <span>Active {formatRelativeTime(session.last_used)}</span>
        <span>Created {formatRelativeTime(session.created_at)}</span>
      </div>
      {confirmRevoke === session.id && session.is_current && (
        <div className="session-confirm-warning">
          This is your current active session. Revoking it will log you out and you must sign back
          in.
        </div>
      )}
    </div>
    <SessionCardActions
      sessionId={session.id}
      isConfirming={confirmRevoke === session.id}
      isRevoking={revokingId === session.id}
      onRevoke={onRevoke}
      onCancelConfirm={onCancelConfirm}
    />
  </div>
);

const PrivacySecuritySection: React.FC = () => {
  const accessToken = useAuthStore((s) => s.accessToken);
  const logout = useUserStore((s) => s.logout);
  const userId = useUserStore((s) => s.user?.id ?? null);
  const activityHistoryCapability = useClientConfigStore(
    (state) => state.activityHistoryCapability
  );
  const navigate = useNavigate();
  const privacySettings = usePrivacyStore((s) => s.settings);
  const fetchPrivacy = usePrivacyStore((s) => s.fetchPrivacy);
  const updatePrivacy = usePrivacyStore((s) => s.updatePrivacy);
  const privacyLoaded = usePrivacyStore((s) => s.loaded);
  const contentProtection = useDraftContentProtection();
  const contentProtectionLoaded = useDraftContentProtectionLoaded();
  const contentProtectionApplying = useDraftContentProtectionApplying();
  const [contentProtectionPlatform, setContentProtectionPlatform] = useState<string | null>(null);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [pastSessions, setPastSessions] = useState<PastSession[]>([]);
  const [activityHistoryControlsVisible, setActivityHistoryControlsVisible] = useState(false);
  const [presenceHistoryVisible, setPresenceHistoryVisible] = useState(false);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [revokingId, setRevokingId] = useState<string | null>(null);
  const [confirmRevoke, setConfirmRevoke] = useState<string | null>(null); // session id for individual confirm

  // Revocation mode (Simple / Secure toggle)
  const [revocationMode, setRevocationMode] = useState<'simple' | 'secure'>('secure');

  // The one step-up dialog for revoke, revoke-all and the mode change (#7).
  // `seed` is the refusal that opened a refusal-triggered one (single revoke).
  const [sessionStepUp, setSessionStepUp] = useState<{
    action: SessionStepUpAction;
    seed: StepUpFactorRefusal | null;
  } | null>(null);

  // MFA state
  const [mfaMethods, setMfaMethods] = useState<string[]>([]);
  const [mfaRecoveryOnly, setMfaRecoveryOnly] = useState<string[]>([]);
  const [mfaBackupRemaining, setMfaBackupRemaining] = useState<number | undefined>();
  const [mfaWebauthnCredentials, setMfaWebauthnCredentials] = useState<WebAuthnCredential[]>([]);
  const [mfaBackupEmail, setMfaBackupEmail] = useState('');
  // Whether the MFA state above is the server's (F8). Until it is, the tier
  // controls stay hidden: the empty defaults would read as "MFA is off",
  // undoing the server's fail-closed 500 on a status read it could not do.
  const [mfaStatusLoad, setMfaStatusLoad] = useState<'loading' | 'ready' | 'error'>('loading');
  const [mfaSetupMethod, setMfaSetupMethod] = useState<'totp' | 'webauthn' | 'email-sms' | null>(
    null
  );
  const [webauthnCredentialType, setWebauthnCredentialType] = useState<'hardware' | 'platform'>(
    'hardware'
  );
  // A TOTP or key setup finished in this visit: what lets "Back to …" appear.
  const [mfaSetupCompleted, setMfaSetupCompleted] = useState(false);

  // Backup code reset modal
  const [showBackupReset, setShowBackupReset] = useState(false);
  const [backupResetCodes, setBackupResetCodes] = useState<string[] | null>(null);
  const backupResetHeadingRef = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    let mounted = true;
    void globalThis.electron
      ?.getPlatform?.()
      .then((platform) => {
        if (mounted) setContentProtectionPlatform(platform);
      })
      .catch(() => {
        if (mounted) setContentProtectionPlatform(null);
      });
    return () => {
      mounted = false;
    };
  }, []);

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

  // #1241 / AC-19: one revert mechanism for BOTH tier sliders. Each optimistic
  // local value must fall back to the last value the server confirmed — leaving
  // a control showing a mode the server rejected misrepresents the user's actual
  // protection, which on a privacy control is worse than showing an error.
  // Implementing it once and applying it to both is less code than two
  // mechanisms, so the spec's lockstep-companion-PR escape hatch is not needed.
  // Monotonic tier-mutation counters, keyed PER FIELD. The debounce coalesces
  // rapid clicks, but two selections more than 300 ms apart put two PATCHes in
  // flight at once and their responses can land out of order.
  //
  // The key is load-bearing. A single shared counter let one control supersede
  // the OTHER: both tier controls route through commitPrivacyTier, so changing
  // the friend-request mode while a DM PATCH was in flight bumped the counter,
  // the DM rejection was judged superseded, and its error was swallowed and its
  // revert skipped — reintroducing, from an unrelated control, exactly the
  // silent failure this guard exists to prevent. Found by Gitar and CodeRabbit
  // independently on PR #2888.
  const tierMutationRef = useRef<Record<string, number>>({});

  const commitPrivacyTier = useCallback(
    (
      updates: Partial<PrivacySettings>,
      revert: (confirmed: PrivacySettings) => void,
      setError: (message: string | null) => void
    ) => {
      // Supersession is per FIELD, not per field-set. Keying on the joined set
      // would mean a `{a, b}` write neither supersedes nor is superseded by an
      // in-flight `{a}` — the same cross-write staleness this fence exists to
      // stop, reappearing the moment anyone passes two fields. Every caller
      // passes one today; the signature is `Partial<PrivacySettings>`.
      const stamps = (Object.keys(updates) as (keyof PrivacySettings)[]).map((field) => {
        const next = (tierMutationRef.current[field] ?? 0) + 1;
        tierMutationRef.current[field] = next;
        return [field, next] as const;
      });
      setError(null);
      // Previously the DM path called updatePrivacy() bare, so a rejection was
      // an unhandled promise AND the slider kept the value the server refused.
      void updatePrivacy(updates).catch((err: unknown) => {
        // A superseded request must not act on its own outcome. Without this,
        // a slow rejection of an ALREADY-REPLACED selection reverts the user's
        // newer choice and reports an error for a request they abandoned.
        if (stamps.some(([field, stamp]) => tierMutationRef.current[field] !== stamp)) return;
        setError(err instanceof Error ? err.message : 'Failed to update privacy settings');
        try {
          revert(usePrivacyStore.getState().settings);
        } catch {
          // The error is already surfaced. A failed revert must not mask it —
          // guarded rather than relying on statement order, so a future reorder
          // cannot reintroduce the silent failure.
        }
      });
    },
    [updatePrivacy]
  );

  const [dmSaveError, setDmSaveError] = useState<string | null>(null);

  const setDmPrivacyLevel = useCallback(
    (level: DMPrivacyLevel) => {
      setLocalDmLevel(level);
      if (dmDebounceRef.current) clearTimeout(dmDebounceRef.current);
      dmDebounceRef.current = setTimeout(() => {
        commitPrivacyTier(
          { dmPrivacyLevel: level },
          (confirmed) => setLocalDmLevel(confirmed.dmPrivacyLevel),
          setDmSaveError
        );
      }, 300);
    },
    [commitPrivacyTier]
  );

  // Cleanup debounce timer
  useEffect(() => {
    return () => {
      if (dmDebounceRef.current) clearTimeout(dmDebounceRef.current);
    };
  }, []);

  // #1241: local mode for a responsive slider, debounced like the DM tier.
  const [localFriendRequestMode, setLocalFriendRequestMode] = useState<FriendRequestPrivacyMode>(
    privacySettings.allowFriendRequestsFrom
  );
  const [friendRequestError, setFriendRequestError] = useState<string | null>(null);
  const friendRequestDebounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: syncs localFriendRequestMode from the store after the settings fetch; not a render loop
    setLocalFriendRequestMode(privacySettings.allowFriendRequestsFrom);
  }, [privacySettings.allowFriendRequestsFrom]);

  const setFriendRequestMode = useCallback(
    (mode: FriendRequestPrivacyMode) => {
      setLocalFriendRequestMode(mode);
      setFriendRequestError(null);
      if (friendRequestDebounceRef.current) clearTimeout(friendRequestDebounceRef.current);
      friendRequestDebounceRef.current = setTimeout(() => {
        commitPrivacyTier(
          { allowFriendRequestsFrom: mode },
          (confirmed) => setLocalFriendRequestMode(confirmed.allowFriendRequestsFrom),
          setFriendRequestError
        );
      }, 300);
    },
    [commitPrivacyTier]
  );

  useEffect(() => {
    return () => {
      if (friendRequestDebounceRef.current) clearTimeout(friendRequestDebounceRef.current);
    };
  }, []);

  // #1354: a toggle-only PATCH against a control-plane that predates the field
  // comes back 400. The store maps that one shape to the skew copy; surfacing it
  // here is what stops the flip from failing silently.
  const [purgeAuthError, setPurgeAuthError] = useState<string | null>(null);
  const [purgeFenceStepUpOpen, setPurgeFenceStepUpOpen] = useState(false);

  const setRequireAuthBeforePurge = useCallback(
    async (next: boolean) => {
      setPurgeAuthError(null);
      // #2765: turning the fence OFF is step-up gated, so the dialog owns that
      // transition end-to-end — it collects the factors and sends the PATCH.
      // Nothing goes out from here, and the switch stays bound to the store, so
      // it does not flip until the server accepts. Turning it ON is ungated.
      if (!next) {
        setPurgeFenceStepUpOpen(true);
        return;
      }
      try {
        await updatePrivacy({ requireAuthBeforePurge: true });
      } catch (err) {
        setPurgeAuthError(err instanceof Error ? err.message : 'Failed to update privacy settings');
      }
    },
    [updatePrivacy]
  );

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
    // A failed read keeps the prior values (never clobbered with defaults) but
    // is reported, and the controls built on them are hidden until a read
    // succeeds — after a change, the prior values are known to be stale.
    if (!data) {
      setMfaStatusLoad('error');
      return;
    }
    setMfaStatusLoad('ready');
    setMfaMethods(data.methods);
    setMfaRecoveryOnly(data.recoveryOnly);
    setMfaBackupRemaining(data.backupRemaining);
    setMfaBackupEmail(data.backupEmail);
    setMfaWebauthnCredentials(data.credentials);
  }, [accessToken]);

  // Hydrate the SSO Security toggle states from GET /users/me/security so the
  // switches reflect actual server state on mount. Preserves last-known state
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

  // Sort sessions: current session always first, then by last_used descending
  const sortedSessions = useMemo(() => {
    return [...sessions].sort((a, b) => {
      if (a.is_current && !b.is_current) return -1;
      if (!a.is_current && b.is_current) return 1;
      return new Date(b.last_used).getTime() - new Date(a.last_used).getTime();
    });
  }, [sessions]);

  /** Signs out of the session this client holds (revoked itself, or revoked with every other). */
  const endCurrentSession = async () => {
    await logout();
    navigate('/');
  };

  /** A session the server revoked: sign out if it was this one, else drop it from the list. */
  const applyRevoked = async (sessionId: string) => {
    if (sessions.find((s) => s.id === sessionId)?.is_current) {
      await endCurrentSession();
      return;
    }
    setSessions((prev) => prev.filter((s) => s.id !== sessionId));
    void fetchSessions();
  };

  // A dialog that is already open keeps its action: a second Revoke's refusal
  // landing while it is up is dropped, never re-pointing the open dialog at
  // another session.
  const openStepUp = (action: SessionStepUpAction, seed: StepUpFactorRefusal | null = null) =>
    setSessionStepUp((open) => open ?? { action, seed });
  const closeStepUp = () => setSessionStepUp(null);

  // Single revoke is refusal-triggered: the first DELETE carries no credential,
  // and the server's `auth_required` / `password_required` opens the step-up
  // dialog seeded with that refusal (#7). Revoke-all and the mode change open
  // the dialog up front instead.
  const handleRevoke = async (sessionId: string) => {
    if (!accessToken) return;

    // Check if this is the current session — requires confirmation
    const session = sessions.find((s) => s.id === sessionId);
    if (session?.is_current && confirmRevoke !== sessionId) {
      setConfirmRevoke(sessionId);
      return;
    }

    setConfirmRevoke(null);
    setRevokingId(sessionId);
    // Captured before the request and sent with it: an answer that lands after
    // an account or server change belongs to the old one, so it neither opens
    // a dialog nor touches the list.
    const context = captureApiRequestContext();

    try {
      const response = await apiFetch(
        `/api/v1/sessions/${sessionId}`,
        { method: 'DELETE' },
        { context }
      );
      const answer = await readRevokeAnswer(response);
      if (!apiRequestContextIsCurrent(context)) return;
      if (answer.kind === 'stepUp') openStepUp({ kind: 'revoke', sessionId }, answer.seed);
      else if (answer.kind === 'failed') setError(answer.message);
      else await applyRevoked(sessionId);
    } catch (err) {
      if (apiRequestContextIsCurrent(context)) {
        setError(err instanceof Error ? err.message : REVOKE_FAILED);
      }
    } finally {
      setRevokingId(null);
    }
  };

  const handleStepUpAccepted = (action: SessionStepUpAction) => {
    closeStepUp();
    switch (action.kind) {
      case 'revoke':
        void applyRevoked(action.sessionId);
        break;
      case 'revokeAll':
        void endCurrentSession();
        break;
      case 'modeChange':
        setRevocationMode(action.mode);
        break;
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
  // Every MFA settings action resolves an MfaStepUpResult and never throws:
  // the modal routes all six through one path (mfaStepUp.ts). Each is an
  // `MfaSeamHandler`: the modal's `run` hands it the capture its request is
  // sent under (C82), and its own side effects run only while that capture is
  // current — an answer that lands after an account or server change belongs
  // to the old one. The status is refetched only after an accepted change — a
  // refusal changed nothing.
  const handleResetTOTP: MfaSeamHandler = async (password, mfaCode, context) => {
    // TOTPDisable predates the seam and binds the code as `code`.
    const result = await submitMfaStepUp(
      '/api/v1/mfa/totp/disable',
      'POST',
      {},
      { password, mfaCode },
      { codeField: 'code', context }
    );
    if (result.kind === 'accepted' && apiRequestContextIsCurrent(context)) fetchMFAStatus();
    return result;
  };

  const handleRevokeWebAuthnKey: MfaSeamHandler<[credentialId: string]> = async (
    credentialId,
    password,
    _mfaCode,
    context
  ) => {
    // The route verifies the password alone; no code is sent.
    const result = await submitMfaStepUp(
      `/api/v1/mfa/webauthn/credentials/${credentialId}`,
      'DELETE',
      {},
      { password, mfaCode: undefined },
      { context }
    );
    if (result.kind === 'accepted' && apiRequestContextIsCurrent(context)) {
      // Signal the authenticator to clean up the deleted credential (best-effort).
      const data = (result.data ?? {}) as { remaining_credential_ids?: string[]; user_id?: string };
      await signalRemovedWebAuthnCredential(data);
      fetchMFAStatus();
    }
    return result;
  };

  const handleDisableEmailSms: MfaSeamHandler = async (password, mfaCode, context) => {
    const result = await submitMfaStepUp(
      '/api/v1/mfa/email-sms/disable',
      'POST',
      {},
      { password, mfaCode },
      { context }
    );
    if (result.kind === 'accepted' && apiRequestContextIsCurrent(context)) fetchMFAStatus();
    return result;
  };

  const handleSetBackupEmail: MfaSeamHandler<[email: string]> = async (
    email,
    password,
    mfaCode,
    context
  ) => {
    const result = await submitMfaStepUp(
      '/api/v1/mfa/backup-email',
      'PUT',
      { email },
      { password, mfaCode },
      { context }
    );
    if (result.kind === 'accepted' && apiRequestContextIsCurrent(context)) {
      const data = result.data as { backup_email?: string } | null;
      setMfaBackupEmail(data?.backup_email ?? '');
    }
    return result;
  };

  // Nothing shows the flag while the hardened toggle is dormant, so an
  // accepted change has no state to update.
  const handleToggleRecoveryHardened: MfaSeamHandler<[enabled: boolean]> = (
    enabled,
    password,
    mfaCode,
    context
  ) =>
    submitMfaStepUp(
      '/api/v1/mfa/recovery-hardened',
      'PUT',
      { enabled },
      { password, mfaCode },
      { context }
    );

  const handleToggleRecoveryOnly: MfaSeamHandler<[method: string, recoveryOnly: boolean]> = async (
    method,
    recoveryOnly,
    password,
    mfaCode,
    context
  ) => {
    const newList = recoveryOnly
      ? [...mfaRecoveryOnly, method]
      : mfaRecoveryOnly.filter((m) => m !== method);
    const result = await submitMfaStepUp(
      '/api/v1/mfa/recovery-only',
      'PUT',
      { methods: newList },
      { password, mfaCode },
      { context }
    );
    if (result.kind === 'accepted' && apiRequestContextIsCurrent(context)) {
      const data = result.data as { recovery_only_methods?: unknown } | null;
      const methods = data?.recovery_only_methods;
      if (!Array.isArray(methods)) {
        fetchMFAStatus();
        return result;
      }
      setMfaRecoveryOnly(methods.filter((m): m is string => typeof m === 'string'));
    }
    return result;
  };

  // MFA setup area — three-way branch extracted from the render tree so its
  // conditionals do not inflate this component's cognitive complexity (S3776).
  const renderMfaSetupArea = () => (
    <>
      {mfaSetupMethod === 'email-sms' && (
        <EmailSmsSetup
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
          onComplete={() => {
            setMfaSetupMethod(null);
            setMfaSetupCompleted(true);
            fetchMFAStatus();
          }}
          onCancel={() => setMfaSetupMethod(null)}
        />
      )}
      {!mfaSetupMethod && mfaStatusLoad === 'loading' && (
        <p className="settings-section-description">Loading your MFA settings…</p>
      )}
      {!mfaSetupMethod && mfaStatusLoad === 'error' && (
        <div className="mfa-status-error">
          <ErrorBanner error="We couldn't load your MFA settings, so they're hidden until they load. Nothing has changed." />
          <button
            type="button"
            className="btn btn-sm btn-secondary"
            onClick={() => {
              setMfaStatusLoad('loading');
              void fetchMFAStatus();
            }}
          >
            Reload MFA settings
          </button>
        </div>
      )}
      {!mfaSetupMethod && mfaStatusLoad === 'ready' && (
        <MFATierSelector
          activeMethods={mfaMethods}
          recoveryOnlyMethods={mfaRecoveryOnly}
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
    </>
  );

  const closeBackupReset = () => {
    setShowBackupReset(false);
    setBackupResetCodes(null);
  };

  // Backup-code reset modal — extracted so its nested conditional does not
  // inflate this component's cognitive complexity (S3776).
  const renderBackupResetModal = () =>
    showBackupReset && (
      <div className="mfa-modal-overlay">
        <div className="mfa-modal">
          <h3 tabIndex={-1} ref={backupResetHeadingRef}>
            Reset Backup Codes
          </h3>
          <p className="mfa-modal-desc">
            This will invalidate all existing backup codes and generate new ones.
          </p>

          {backupResetCodes ? (
            <BackupCodeDisplay
              codes={backupResetCodes}
              onConfirm={closeBackupReset}
              disabled={false}
            />
          ) : (
            <BackupCodeRegenerateStage
              headingRef={backupResetHeadingRef}
              onRegenerated={(codes) => {
                setBackupResetCodes(codes);
                void fetchMFAStatus();
              }}
              onTotpRemoved={() => void fetchMFAStatus()}
              onCancel={closeBackupReset}
            />
          )}
        </div>
      </div>
    );

  const renderContentProtectionControl = () =>
    contentProtectionLoaded &&
    (contentProtectionPlatform === 'darwin' || contentProtectionPlatform === 'win32') && (
      <div className="settings-row">
        <div className="settings-row-info">
          <label
            className="settings-row-label"
            id={CONTENT_PROTECTION_LABEL_ID}
            htmlFor="contentProtection"
          >
            Protect Concord windows from screen capture
          </label>
          <span className="settings-row-hint" id={CONTENT_PROTECTION_HINT_ID}>
            When applied, Concord asks macOS or Windows to prevent its main and picture-in-picture
            call windows from being captured. Your operating system may not block every capture
            method.
          </span>
        </div>
        <ToggleSwitch
          id="contentProtection"
          checked={contentProtection}
          onChange={setDraftContentProtection}
          disabled={contentProtectionApplying}
          ariaLabelledBy={CONTENT_PROTECTION_LABEL_ID}
          aria-describedby={CONTENT_PROTECTION_HINT_ID}
          inputRole="switch"
        />
      </div>
    );

  return (
    <>
      <CollapsibleSection id="section-privacy-settings" title="Privacy">
        <p className="settings-section-description">
          Control who can message you and how others can find you.
        </p>

        {renderContentProtectionControl()}

        <DMPrivacyControls
          localDmLevel={localDmLevel}
          setDmPrivacyLevel={setDmPrivacyLevel}
          saveError={dmSaveError}
        />

        <FriendRequestPrivacyControls
          localMode={localFriendRequestMode}
          setMode={setFriendRequestMode}
          saveError={friendRequestError}
          isLoaded={privacyLoaded}
        />

        <div className="settings-row">
          <div className="settings-row-info">
            <label
              className="settings-row-label"
              id={PURGE_AUTH_LABEL_ID}
              htmlFor="requireAuthBeforePurge"
            >
              Require authentication before purging
            </label>
            <span className="settings-row-hint">
              Ask for your password before purging messages in a direct message or group chat.
            </span>
            {!privacySettings.requireAuthBeforePurge && (
              <span className="settings-row-hint" role="alert">
                Without this, anyone with access to your unlocked account can permanently purge your
                message history.
              </span>
            )}
            {purgeAuthError && (
              <span className="settings-row-hint" role="alert">
                {purgeAuthError}
              </span>
            )}
          </div>
          <ToggleSwitch
            id="requireAuthBeforePurge"
            checked={privacySettings.requireAuthBeforePurge}
            onChange={(v) => void setRequireAuthBeforePurge(v)}
            ariaLabelledBy={PURGE_AUTH_LABEL_ID}
            inputRole="switch"
          />
        </div>

        <PurgeFenceStepUpDialog
          open={purgeFenceStepUpOpen}
          onClose={() => setPurgeFenceStepUpOpen(false)}
        />

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
                setBackupResetCodes(null);
              }}
            >
              Reset
              <br />
              Codes
            </button>
          </div>
        </div>

        {renderMfaSetupArea()}

        <VerificationReturnButton setupCompleted={mfaSetupCompleted} />

        {renderBackupResetModal()}
      </CollapsibleSection>

      <CollapsibleSection id="section-sso-security" title="SSO Security">
        <p className="settings-section-description">
          Linked SSO providers can change how Concord verifies future sign-ins. Keep these settings
          aligned with the MFA and recovery protections on your SSO provider.
        </p>

        <LinkedAccountsList />

        <SSOToggleRow
          field="trust_sso_security"
          checked={trustSSOSecurity}
          label="Trust SSO provider verification"
          warning="Concord will skip its own MFA after a successful SSO sign-in. Only enable this if your SSO provider enforces MFA."
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
          label="Require SSO for sign-in"
          warning={
            passwordLoginDisabled
              ? 'You can only sign in with your linked SSO providers. Keep recovery options configured before enabling this.'
              : 'Password login remains available for this account.'
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

      <div
        hidden={
          activityHistoryCapability.status === 'confirmed-unsupported' &&
          !activityHistoryControlsVisible &&
          !presenceHistoryVisible
        }
      >
        <CollapsibleSection id="section-presence-history" title="Activity History">
          <ActivityHistoryCard onVisibilityChange={setActivityHistoryControlsVisible} />
          <PresenceHistorySection userId={userId} onVisibilityChange={setPresenceHistoryVisible} />
        </CollapsibleSection>
      </div>

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
                  if (revocationMode !== 'secure')
                    openStepUp({ kind: 'modeChange', mode: 'secure' });
                }}
              >
                Secure
              </button>
              <button
                className={`revocation-mode-btn ${revocationMode === 'simple' ? 'active' : ''}`}
                onClick={() => {
                  if (revocationMode !== 'simple')
                    openStepUp({ kind: 'modeChange', mode: 'simple' });
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
                  onClick={() => openStepUp({ kind: 'revokeAll' })}
                >
                  Revoke All Sessions
                </button>
              </div>
            )}

            <div className="sessions-list">
              {sortedSessions.map((session) => (
                <SessionCard
                  key={session.id}
                  session={session}
                  confirmRevoke={confirmRevoke}
                  revokingId={revokingId}
                  onRevoke={handleRevoke}
                  onCancelConfirm={() => setConfirmRevoke(null)}
                />
              ))}
            </div>
          </>
        )}

        {sessionStepUp && (
          <SessionStepUpDialog
            key={sessionStepUpKey(sessionStepUp.action)}
            action={sessionStepUp.action}
            seed={sessionStepUp.seed}
            onClose={closeStepUp}
            onAccepted={handleStepUpAccepted}
          />
        )}
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
