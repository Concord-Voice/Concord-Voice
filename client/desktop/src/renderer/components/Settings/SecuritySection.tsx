import React, { useState, useCallback, useEffect, useId, useMemo, useRef } from 'react';
import { useNavigate } from 'react-router';
import { useAuthStore } from '../../stores/auth/authStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useSettingsOverlayStore } from '../../stores/ui/settingsOverlayStore';
import { hasPendingDrafts, useDraftSettingsStore } from '../../stores/ui/draftSettingsStore';
import { apiFetch } from '../../services/system/apiClient';
import { getApiBase } from '../../services/system/runtimeServerBase';
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
import PasswordChangeForm from '../Profile/PasswordChangeForm';
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
import LinkedAccountsList from './LinkedAccountsList';
import ToggleSwitch from './ToggleSwitch';
import '../Profile/profileForms.css';
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

// ─── Data-fetch helpers (extracted to keep SecuritySection's cognitive complexity below threshold) ────────────────────

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
    if (!credRes.ok) return null;
    const credData = await credRes.json();
    result.credentials = credData.credentials || [];
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
 * SecuritySection's cognitive complexity.
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
 * hoisted to module scope so it does not inflate SecuritySection's
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
 * from SecuritySection so its deep nesting does not inflate the
 * component's cognitive complexity. Swallows failures by design — this is a
 * hint, not a hard requirement.
 */
async function signalRemovedWebAuthnCredential(data: {
  remaining_credential_ids?: string[];
  user_id?: string;
}): Promise<void> {
  try {
    if (typeof PublicKeyCredential === 'undefined') return;
    const PKC = PublicKeyCredential as typeof PublicKeyCredential & {
      signalAllAcceptedCredentialIds?: (options: {
        rpId: string;
        userId: ArrayBuffer;
        allAcceptedCredentialIds: ArrayBuffer[];
      }) => Promise<void>;
    };
    if (
      typeof PKC.signalAllAcceptedCredentialIds === 'function' &&
      data.remaining_credential_ids &&
      data.user_id
    ) {
      const rpId = new URL(getApiBase()).hostname;
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
 * Extracted from SecuritySection (SC-2) so the two near-identical rows
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
 * One row in the active-sessions list. Extracted from SecuritySection to
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
      <svg aria-hidden="true" width="20" height="20" viewBox="0 0 20 20" fill="none">
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

interface ActiveSessionsContentProps extends Omit<SessionCardProps, 'session'> {
  isLoading: boolean;
  readError: string | null;
  sessions: Session[];
  onRevokeAll: () => void;
}

const ActiveSessionsContent: React.FC<ActiveSessionsContentProps> = ({
  isLoading,
  readError,
  sessions,
  confirmRevoke,
  revokingId,
  onRevoke,
  onCancelConfirm,
  onRevokeAll,
}) => {
  if (isLoading) {
    return (
      <div className="settings-loading">
        <LoadingSpinner size="small" inline />
      </div>
    );
  }
  if (readError) return null;

  return (
    <>
      {sessions.length > 0 && (
        <div className="sessions-actions-top">
          <button className="sessions-revoke-all-btn" onClick={onRevokeAll}>
            Revoke All Sessions
          </button>
        </div>
      )}
      <div className="sessions-list">
        {sessions.map((session) => (
          <SessionCard
            key={session.id}
            session={session}
            confirmRevoke={confirmRevoke}
            revokingId={revokingId}
            onRevoke={onRevoke}
            onCancelConfirm={onCancelConfirm}
          />
        ))}
      </div>
    </>
  );
};

interface PastSessionsContentProps {
  isLoading: boolean;
  readError: string | null;
  sessions: PastSession[];
  onReload: () => void;
}

const PastSessionsContent: React.FC<PastSessionsContentProps> = ({
  isLoading,
  readError,
  sessions,
  onReload,
}) => {
  if (isLoading) {
    return (
      <output
        className="settings-section-description"
        aria-live="polite"
        style={{ display: 'block' }}
      >
        Loading past sessions…
      </output>
    );
  }
  if (readError) {
    return (
      <div>
        <p className="settings-error" role="alert">
          We couldn&apos;t load your past sessions.
        </p>
        <button type="button" className="btn btn-sm btn-secondary" onClick={onReload}>
          Reload past sessions
        </button>
      </div>
    );
  }
  if (sessions.length === 0) {
    return <p className="settings-section-description">No past sessions in the last 30 days.</p>;
  }

  return (
    <div className="sessions-list">
      {sessions.map((session) => (
        <div key={session.id} className="session-card past">
          <div className="session-card-icon past">
            <svg aria-hidden="true" width="20" height="20" viewBox="0 0 20 20" fill="none">
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
  );
};

const SecuritySection: React.FC = () => {
  const accessToken = useAuthStore((s) => s.accessToken);
  const logout = useUserStore((s) => s.logout);
  const navigate = useNavigate();
  const [sessions, setSessions] = useState<Session[]>([]);
  const [pastSessions, setPastSessions] = useState<PastSession[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [sessionsReadError, setSessionsReadError] = useState<string | null>(null);
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

  // SSO Security flags (issue #270). Hydrated on mount from
  // GET /users/me/security so the toggles reflect the actual server state
  // instead of the user's most-recent local intent. PATCH updates the local
  // mirror on success.
  const [trustSSOSecurity, setTrustSSOSecurity] = useState<boolean>(false);
  const [passwordLoginDisabled, setPasswordLoginDisabled] = useState<boolean>(false);
  const [ssoStatusLoad, setSsoStatusLoad] = useState<'loading' | 'ready' | 'error'>('loading');
  // Inline-passphrase confirm panels — one per toggle. `null` = collapsed.
  const [ssoConfirmField, setSsoConfirmField] = useState<
    'trust_sso_security' | 'password_login_disabled' | null
  >(null);
  const [ssoConfirmPassphrase, setSsoConfirmPassphrase] = useState<string>('');
  const [ssoConfirmDesiredValue, setSsoConfirmDesiredValue] = useState<boolean>(false);
  const [ssoConfirmError, setSsoConfirmError] = useState<string>('');
  const [ssoConfirmLoading, setSsoConfirmLoading] = useState<boolean>(false);

  const fetchSessions = useCallback(async () => {
    if (!accessToken) return;
    setIsLoading(true);
    setError(null);
    setSessionsReadError(null);
    try {
      const data = await fetchSessionsData();
      setSessions(data.sessions);
      setPastSessions(data.pastSessions);
      if (data.revocationMode) setRevocationMode(data.revocationMode);
    } catch (err) {
      setSessionsReadError(err instanceof Error ? err.message : 'Failed to fetch sessions');
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
    if (!data) {
      setSsoStatusLoad('error');
      return;
    }
    setTrustSSOSecurity(data.trustSSOSecurity);
    setPasswordLoginDisabled(data.passwordLoginDisabled);
    setSsoStatusLoad('ready');
  }, [accessToken]);

  useEffect(() => {
    fetchSessions();
    fetchMFAStatus();
    fetchSSOSecurity();
  }, [fetchSessions, fetchMFAStatus, fetchSSOSecurity]);

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

  return (
    <>
      <CollapsibleSection id="section-password" title="Change Password">
        <PasswordChangeForm />
      </CollapsibleSection>

      <CollapsibleSection id="section-mfa" title="Multi-Factor Authentication">
        <p className="settings-section-description">
          Add an extra layer of security to your account. When enabled, you&apos;ll need both your
          password and a second factor to sign in.
        </p>

        {mfaStatusLoad === 'ready' && (
          <div className="mfa-status-bar">
            <div className="mfa-status-item">
              <svg
                aria-hidden="true"
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
                aria-hidden="true"
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
        )}

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

        {ssoStatusLoad === 'loading' && (
          <p className="settings-section-description">Loading your SSO security settings…</p>
        )}
        {ssoStatusLoad === 'error' && (
          <div className="sso-status-error">
            <ErrorBanner error="We couldn't load your SSO security settings. Reload them before making changes." />
            <button
              type="button"
              className="btn btn-sm btn-secondary"
              onClick={() => {
                setSsoStatusLoad('loading');
                void fetchSSOSecurity();
              }}
            >
              Reload SSO security settings
            </button>
          </div>
        )}
        {ssoStatusLoad === 'ready' && (
          <>
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
          </>
        )}
      </CollapsibleSection>

      <CollapsibleSection id="section-active-sessions" title="Active Sessions">
        <p className="settings-section-description">
          These are the devices currently logged into your account. Revoke any session you
          don&apos;t recognize.
        </p>

        {!isLoading && !sessionsReadError && (
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

        {(error || sessionsReadError) && (
          <div className="settings-error" role="alert">
            {error || sessionsReadError}
          </div>
        )}
        {sessionsReadError && (
          <button
            type="button"
            className="btn btn-sm btn-secondary"
            onClick={() => void fetchSessions()}
          >
            Reload sessions
          </button>
        )}

        <ActiveSessionsContent
          isLoading={isLoading}
          readError={sessionsReadError}
          sessions={sortedSessions}
          confirmRevoke={confirmRevoke}
          revokingId={revokingId}
          onRevoke={handleRevoke}
          onCancelConfirm={() => setConfirmRevoke(null)}
          onRevokeAll={() => openStepUp({ kind: 'revokeAll' })}
        />

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

      <CollapsibleSection id="section-past-sessions" title="Past Sessions">
        <p className="settings-section-description">
          Sessions that were logged out or revoked in the last 30 days.
        </p>

        <PastSessionsContent
          isLoading={isLoading}
          readError={sessionsReadError}
          sessions={pastSessions}
          onReload={() => void fetchSessions()}
        />
      </CollapsibleSection>
    </>
  );
};

export default SecuritySection;
