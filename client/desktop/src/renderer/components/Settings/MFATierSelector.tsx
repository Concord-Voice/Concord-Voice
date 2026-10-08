import React, { useCallback, useRef, useState, useEffect } from 'react';
import ToggleSwitch from './ToggleSwitch';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import { isPlainEnter } from '../Auth/MFAFactorPicker';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import RecoveryApprovalModal from '../Auth/RecoveryApprovalModal';
import RecoveryCircle from './RecoveryCircle';
import Modal from '../ui/Modal';
import ErrorBanner, { FieldError } from './ErrorBanner';
import { useUserStore } from '../../stores/auth/userStore';
import { useAuthStore } from '../../stores/auth/authStore';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
  onRuntimeServerSelectionChange,
} from '../../services/system/runtimeServerBase';
import {
  listDeviceRecoveryRequests,
  type ReviewableDeviceRecoveryRequest,
} from '../../services/system/deviceRecoveryService';
import { apiFetch } from '../../services/system/apiClient';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import {
  useStepUpFactor,
  type StepUpPhase,
  type StepUpSubmit,
} from '../../hooks/auth/useStepUpFactor';
import {
  isStepUpLocked,
  passwordOnlyBanner,
  stepUpBanner,
  stepUpPasswordError,
  toStepUpSubmitOutcome,
  type MfaSeamHandler,
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
  /** Methods listed but not yet offered; each renders a "Coming soon" tag. */
  comingSoon?: readonly string[];
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
    // The server refuses SMS enrolment outside development and test until a
    // provider is integrated (mfa validateEmailSmsMethods).
    comingSoon: ['SMS code'],
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
 * The step-up purpose of the request an action's code is sent with, which is
 * the one route a WebAuthn inline token minted in its prompt can be spent on.
 * Revoking a key sends the password alone, so it has none.
 */
function actionPurpose(type: ActionType): StepUpPurpose | null {
  switch (type) {
    case 'reset-totp':
      return 'mfa_settings.totp_disable';
    case 'revoke-webauthn':
      return null;
    case 'disable-emailsms':
      return 'mfa_settings.email_sms_disable';
    case 'set-backup-email':
      return 'mfa_settings.backup_email_set';
    case 'toggle-recovery-only':
      return 'mfa_settings.recovery_only_set';
    case 'toggle-hardened':
      return 'mfa_settings.recovery_hardened_set';
  }
}

/** The six action handlers, each a step-up seam handler (mfaStepUp.ts). */
interface ActionHandlers {
  onToggleRecoveryOnly?: MfaSeamHandler<[method: string, recoveryOnly: boolean]>;
  onToggleRecoveryHardened?: MfaSeamHandler<[enabled: boolean]>;
  onResetTOTP?: MfaSeamHandler;
  onRevokeWebAuthnKey?: MfaSeamHandler<[credentialId: string]>;
  onDisableEmailSms?: MfaSeamHandler;
  onSetBackupEmail?: MfaSeamHandler<[email: string]>;
}

/** What an action reports when its handler is not wired or its modal state is incomplete. */
const UNAVAILABLE: MfaStepUpResult = { kind: 'failed' };

/** Runs `toggle-recovery-only`, or reports it unavailable when no handler is
 * wired or the modal is missing its method/value. Extracted from
 * {@link executeAction} to reduce its cognitive complexity (SonarCloud
 * typescript:S3776). */
function runToggleRecoveryOnly(
  modal: ActionModalState,
  handler: ActionHandlers['onToggleRecoveryOnly'],
  password: string,
  mfaCode: string | undefined,
  context: ApiRequestContext
): MfaStepUpResult | Promise<MfaStepUpResult> {
  return handler && modal.toggleMethod !== undefined && modal.toggleValue !== undefined
    ? handler(modal.toggleMethod, modal.toggleValue, password, mfaCode, context)
    : UNAVAILABLE;
}

/** Runs `toggle-hardened`, or reports it unavailable when no handler is wired
 * or the modal is missing its value. Sibling to {@link runToggleRecoveryOnly}. */
function runToggleHardened(
  modal: ActionModalState,
  handler: ActionHandlers['onToggleRecoveryHardened'],
  password: string,
  mfaCode: string | undefined,
  context: ApiRequestContext
): MfaStepUpResult | Promise<MfaStepUpResult> {
  return handler && modal.toggleValue !== undefined
    ? handler(modal.toggleValue, password, mfaCode, context)
    : UNAVAILABLE;
}

/** Runs the action behind the modal, under the capture of the activation that
 * called it (C82). Every one of the six answers with the step-up result shape,
 * so there is one routing path and no action can surface a refusal the others
 * would route differently. */
async function executeAction(
  modal: ActionModalState,
  handlers: ActionHandlers,
  password: string,
  mfaCode: string | undefined,
  context: ApiRequestContext
): Promise<MfaStepUpResult> {
  switch (modal.type) {
    case 'reset-totp':
      return handlers.onResetTOTP ? handlers.onResetTOTP(password, mfaCode, context) : UNAVAILABLE;
    case 'revoke-webauthn':
      return handlers.onRevokeWebAuthnKey && modal.credentialId
        ? handlers.onRevokeWebAuthnKey(modal.credentialId, password, mfaCode, context)
        : UNAVAILABLE;
    case 'toggle-recovery-only':
      return runToggleRecoveryOnly(
        modal,
        handlers.onToggleRecoveryOnly,
        password,
        mfaCode,
        context
      );
    case 'toggle-hardened':
      return runToggleHardened(
        modal,
        handlers.onToggleRecoveryHardened,
        password,
        mfaCode,
        context
      );
    case 'disable-emailsms':
      return handlers.onDisableEmailSms
        ? handlers.onDisableEmailSms(password, mfaCode, context)
        : UNAVAILABLE;
    case 'set-backup-email':
      return handlers.onSetBackupEmail
        ? handlers.onSetBackupEmail(modal.pendingBackupEmail ?? '', password, mfaCode, context)
        : UNAVAILABLE;
  }
}

/**
 * Reset TOTP binds a TOTP `code` as required, so TOTP is this route's floor
 * (Q3): a failed read leaves it offered and a read can only add to it.
 */
const RESET_TOTP_FLOOR = ['totp'] as const;

/** The primary's label by phase; the in-flight ones replace the action's own. */
function primaryLabel(modal: ActionModalState, phase: StepUpPhase, busy: boolean): string {
  if (phase === 'ceremony') return 'Waiting…';
  return busy ? 'Processing...' : getActionConfirmLabel(modal);
}

interface ActionPasswordFieldProps {
  value: string;
  onChange: (value: string) => void;
  error: string | undefined;
  disabled: boolean;
  inputRef: React.RefObject<HTMLInputElement | null>;
  /** A plain Enter presses the host's primary, as in `StepUpCredentials`. */
  onEnter: () => void;
}

/**
 * The password field of an action that verifies the password alone, where the
 * factor hook has no leg to show (revoke-webauthn, D10).
 */
const ActionPasswordField: React.FC<ActionPasswordFieldProps> = ({
  value,
  onChange,
  error,
  disabled,
  inputRef,
  onEnter,
}) => (
  <div className="mfa-verify-field">
    <label htmlFor="mfa-action-password">Password</label>
    <input
      id="mfa-action-password"
      ref={inputRef}
      type="password"
      autoComplete="current-password"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      onKeyDown={(e) => {
        if (!isPlainEnter(e)) return;
        e.preventDefault();
        onEnter();
      }}
      placeholder="Enter your password"
      disabled={disabled}
      aria-invalid={error !== undefined}
      aria-describedby={error ? 'mfa-action-password-error' : undefined}
    />
    {error && <FieldError id="mfa-action-password-error">{error}</FieldError>}
  </div>
);

interface ActionModalProps {
  modal: ActionModalState;
  backupEmail: string | undefined;
  handlers: ActionHandlers;
  onClose: () => void;
  /** The action was accepted; the host closes the modal. */
  onAccepted: (modal: ActionModalState) => void;
}

/**
 * The confirmation modal of one action (handoff §1.1). Mounted only while an
 * action is open, so every open starts from a fresh password, factor and
 * result. The title is fixed at open and never mutated (WCAG 4.1.2 / 3.2.2).
 *
 * Five actions take the password and a code through `StepUpCredentials`.
 * Revoking a key takes the password alone, so its factor hook is disabled and
 * the password is collected and sent here (D10).
 */
const ActionModal: React.FC<ActionModalProps> = ({
  modal,
  backupEmail,
  handlers,
  onClose,
  onAccepted,
}) => {
  const [password, setPassword] = useState('');
  // The last answer. The banner and the lock derive from it (mfaStepUp.ts), so
  // they cannot disagree. The lock clears only when the modal is reopened
  // (handoff §1.1 / §5).
  const [result, setResult] = useState<MfaStepUpResult | null>(null);
  const [revoking, setRevoking] = useState(false);
  // The account and server this modal opened under. The password typed here
  // belongs to them, so a switch before the click sends nothing anywhere:
  // revoke-webauthn runs no factor hook, whose own open-time capture covers
  // the other actions (picker PR 3 review).
  const [openedContext] = useState(captureApiRequestContext);
  // The password-only revoke's single-flight latch. `revoking` is state, so two
  // activations in one tick (a double click, Enter then a click) both read it
  // false; the ref is set before the first await.
  const revokeInFlightRef = useRef(false);
  const passwordRef = useRef<HTMLInputElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);

  const passwordOnly = modal.type === 'revoke-webauthn';
  const factor = useStepUpFactor({
    enabled: !passwordOnly,
    purpose: actionPurpose(modal.type),
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'passwordOnly',
    allowBackup: true,
    floorMethods: modal.type === 'reset-totp' ? RESET_TOTP_FLOOR : undefined,
  });
  const submitting = revoking || factor.phase === 'submitting';
  const locked = isStepUpLocked(result);
  const passwordError = passwordOnly ? stepUpPasswordError(result) : undefined;

  // Focus the password field once a password refusal has rendered AND the
  // field is enabled again. Focusing inside the submit handler ran while the
  // input was still `disabled` for loading, so the call was a no-op (F4). The
  // factor hook's credentials place their own focus.
  useEffect(() => {
    if (passwordError !== undefined && !revoking) passwordRef.current?.focus();
  }, [passwordError, revoking, result]);

  /** Applies an answer, unless the account or server changed since `context`:
   * an answer for the old one is neither shown nor acted on. Only a refused
   * password is dropped (handoff §1.1); the hook decides about the code. */
  const settle = (answer: MfaStepUpResult, context: ApiRequestContext) => {
    if (!apiRequestContextIsCurrent(context)) return;
    setResult(answer);
    if (stepUpPasswordError(answer) !== undefined) setPassword('');
    if (answer.kind === 'accepted') onAccepted(modal);
  };

  const submit: StepUpSubmit = async (mfa, context) => {
    const answer = await executeAction(modal, handlers, password, mfa, context);
    settle(answer, context);
    return toStepUpSubmitOutcome(answer);
  };

  const revoke = async () => {
    if (revokeInFlightRef.current) return;
    if (password === '') {
      setResult({ kind: 'passwordRequired' });
      return;
    }
    if (!apiRequestContextIsCurrent(openedContext)) {
      setPassword('');
      setResult({ kind: 'sessionExpired' });
      return;
    }
    revokeInFlightRef.current = true;
    const context = openedContext;
    setRevoking(true);
    try {
      settle(await executeAction(modal, handlers, password, undefined, context), context);
    } finally {
      revokeInFlightRef.current = false;
      setRevoking(false);
    }
  };

  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit);
  const onPrimary = () => {
    if (locked || submitting) return;
    if (passwordOnly) void revoke();
    else activate();
  };
  const primaryDisabled = locked || (passwordOnly ? submitting || password === '' : ariaDisabled);

  return (
    <Modal
      isOpen
      onClose={onClose}
      title={getActionTitle(modal)}
      width="small"
      dismissable={!submitting}
      initialFocusRef={passwordRef}
    >
      <div className="mfa-action-body">
        {renderActionBody(modal, backupEmail)}

        {passwordOnly ? (
          <ActionPasswordField
            value={password}
            onChange={setPassword}
            error={passwordError}
            disabled={revoking}
            inputRef={passwordRef}
            onEnter={() => primaryRef.current?.click()}
          />
        ) : (
          <StepUpCredentials
            factor={factor}
            password={password}
            onPasswordChange={setPassword}
            primaryRef={primaryRef}
            passwordRef={passwordRef}
          />
        )}

        <ErrorBanner error={(passwordOnly ? passwordOnlyBanner : stepUpBanner)(result) ?? ''} />

        <div className="mfa-setup-actions">
          <button
            ref={primaryRef}
            type="button"
            className={getActionConfirmClass(modal)}
            aria-disabled={primaryDisabled || undefined}
            onClick={onPrimary}
          >
            {primaryLabel(modal, factor.phase, submitting)}
          </button>
          <button
            type="button"
            className="btn btn-sm btn-secondary"
            onClick={onClose}
            disabled={submitting}
          >
            Cancel
          </button>
        </div>
      </div>
    </Modal>
  );
};

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

interface MFATierSelectorProps extends ActionHandlers {
  activeMethods: string[];
  recoveryOnlyMethods?: string[];
  backupCodesRemaining?: number;
  webauthnCredentials?: WebAuthnCredential[];
  backupEmail?: string;
  onSetupTOTP: () => void;
  onSetupWebAuthn: (credentialType: 'hardware' | 'platform') => void;
  onSetupEmailSms?: () => void;
}

const MFATierSelector: React.FC<MFATierSelectorProps> = ({
  activeMethods,
  recoveryOnlyMethods = [],
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

  // The open action; the modal itself owns the rest of its state.
  const [actionModal, setActionModal] = useState<ActionModalState | null>(null);

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
  const userId = useUserStore((state) => state.user?.id);
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const [serverGeneration, setServerGeneration] = useState(0);
  const [pendingRecoveryRequests, setPendingRecoveryRequests] = useState<
    ReviewableDeviceRecoveryRequest[]
  >([]);
  const [activeRecoveryRequest, setActiveRecoveryRequest] =
    useState<ReviewableDeviceRecoveryRequest | null>(null);
  const [recoveryRequestError, setRecoveryRequestError] = useState('');
  const [recoveryRefresh, setRecoveryRefresh] = useState(0);
  const recoveryRefreshGuardRef = useRef({ busy: false, retryAt: 0 });
  const resolvedRecoveryRequestsRef = useRef(new Set<string>());
  useEffect(
    () => onRuntimeServerSelectionChange(() => setServerGeneration((generation) => generation + 1)),
    []
  );
  useEffect(() => {
    resolvedRecoveryRequestsRef.current.clear();
    let current = true;
    void Promise.resolve().then(() => {
      if (current) {
        setPendingRecoveryRequests([]);
        setActiveRecoveryRequest(null);
      }
    });
    return () => {
      current = false;
    };
  }, [userId, authGeneration, serverGeneration]);
  useEffect(() => {
    let mounted = true;
    let refreshing = false;
    const refreshGuard = { busy: false, retryAt: 0 };
    recoveryRefreshGuardRef.current = refreshGuard;
    let timer: ReturnType<typeof setTimeout>;
    const auth = captureAuthLifecycle();
    const server = captureRuntimeServerSelection();
    const assertCurrent = () => {
      if (
        !mounted ||
        !isSameAuthLifecycle(auth) ||
        !runtimeServerSelectionIsCurrent(server) ||
        useUserStore.getState().user?.id !== userId
      )
        throw new Error('Recovery context changed.');
    };

    const refresh = async () => {
      if (!userId || refreshing) return;
      refreshing = true;
      refreshGuard.busy = true;
      let delay = 15_000;
      try {
        const requests = await listDeviceRecoveryRequests(userId, assertCurrent);
        assertCurrent();
        setPendingRecoveryRequests(
          requests.filter((request) => !resolvedRecoveryRequestsRef.current.has(request.request_id))
        );
        setRecoveryRequestError('');
      } catch (error) {
        try {
          assertCurrent();
          setRecoveryRequestError(
            error instanceof Error ? error.message : 'Recovery requests could not be refreshed.'
          );
          if (
            error &&
            typeof error === 'object' &&
            'retryAfterMs' in error &&
            typeof error.retryAfterMs === 'number'
          )
            delay = Math.max(delay, error.retryAfterMs);
          refreshGuard.retryAt = Date.now() + delay;
        } catch {
          /* A stale list never replaces the current account's rows. */
        }
      } finally {
        refreshing = false;
        refreshGuard.busy = false;
        if (mounted)
          timer = setTimeout(() => {
            void refresh();
          }, delay);
      }
    };
    void refresh();
    return () => {
      mounted = false;
      clearTimeout(timer);
    };
  }, [userId, authGeneration, serverGeneration, recoveryRefresh]);

  const closeRecoveryRequest = useCallback(() => setActiveRecoveryRequest(null), []);
  const resolveRecoveryRequest = useCallback((requestId: string) => {
    // Terminal acknowledgements must survive list reads begun before the write.
    resolvedRecoveryRequestsRef.current.add(requestId);
    setPendingRecoveryRequests((rows) => rows.filter((row) => row.request_id !== requestId));
  }, []);

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
  }, [refreshCircleConfig]);

  // Backup email state
  const [editingBackupEmail, setEditingBackupEmail] = useState(false);
  const [backupEmailDraft, setBackupEmailDraft] = useState(backupEmail || '');
  const [backupEmailError, setBackupEmailError] = useState('');

  // Stable identity: Modal re-registers its Escape listener whenever onClose
  // changes, which an inline arrow would do on every render.
  const clearActionModal = useCallback(() => setActionModal(null), []);

  // The draft-email reset is scoped to a saved/removed backup email —
  // cancelling or failing the modal leaves the draft untouched (handoff §1.2).
  const closeAcceptedAction = (accepted: ActionModalState) => {
    if (accepted.type === 'set-backup-email') setEditingBackupEmail(false);
    clearActionModal();
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
    setActionModal({ type: 'set-backup-email', pendingBackupEmail: trimmed });
  };

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
                    {tier.comingSoon?.includes(m) && (
                      <>
                        {' '}
                        <span className="mfa-coming-soon">Coming soon</span>
                      </>
                    )}
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
                    onClick={() => setActionModal({ type: 'reset-totp' })}
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
                          setActionModal({
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
                  onClick={() => setActionModal({ type: 'disable-emailsms' })}
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
                    setActionModal({
                      type: 'toggle-recovery-only',
                      toggleMethod: method,
                      toggleValue: checked,
                    });
                  }}
                />
              </div>
            )}

            {/* Hardened mode needs an SMS code, and SMS is not offered yet, so the
                toggle is dormant: focusable, aria-disabled, and described by its
                "Coming soon" tag (the PremiumGate a11y rule). When SMS ships, restore
                its onChange (it opened the 'toggle-hardened' action modal, which
                runToggleHardened still serves) and pass the server's
                recovery_hardened back in for checked. */}
            {tier.level === 'last-resort' && isActive && onToggleRecoveryHardened && (
              <div className="mfa-toggle-row mfa-hardened-toggle">
                <div className="mfa-toggle-text">
                  <span className="mfa-recovery-only-label">
                    Hardened mode{' '}
                    <span id="mfa-hardened-coming-soon" className="mfa-coming-soon">
                      Coming soon
                    </span>
                  </span>
                  <span className="mfa-recovery-only-hint">
                    Require BOTH an email code AND an SMS code for recovery. Available once SMS
                    verification launches.
                  </span>
                </div>
                <ToggleSwitch
                  checked={false}
                  onChange={() => {
                    // Dormant until SMS verification ships; see the comment above.
                  }}
                  label="Hardened mode"
                  aria-disabled
                  aria-describedby="mfa-hardened-coming-soon"
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

          {recoveryRequestError && <p role="alert">{recoveryRequestError}</p>}
          <button
            className="btn btn-secondary"
            style={{ minHeight: 44 }}
            onClick={() => {
              const guard = recoveryRefreshGuardRef.current;
              if (!guard.busy && Date.now() >= guard.retryAt)
                setRecoveryRefresh((generation) => generation + 1);
            }}
          >
            Refresh recovery requests
          </button>
          {/* Pending Recovery Requests */}
          {pendingRecoveryRequests.length > 0 && (
            <div style={{ marginTop: 16 }}>
              <h5 style={{ color: 'var(--text-primary)', margin: '0 0 8px' }}>
                Pending Recovery Requests
              </h5>
              {pendingRecoveryRequests.map((req) => (
                <div
                  key={req.request_id}
                  style={{
                    display: 'flex',
                    justifyContent: 'space-between',
                    alignItems: 'center',
                    padding: '8px 0',
                  }}
                >
                  <span style={{ color: 'var(--text-secondary)', fontSize: 14 }}>
                    Request expires {new Date(req.expires_at).toLocaleTimeString()}
                  </span>
                  <button
                    className="btn btn-secondary"
                    style={{ fontSize: 12, padding: '4px 12px', minHeight: 44 }}
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
          key={activeRecoveryRequest.request_id}
          request={activeRecoveryRequest}
          onClose={closeRecoveryRequest}
          onResolved={resolveRecoveryRequest}
        />
      )}

      {/* Action confirmation modal — reused by all six action types (handoff
          §1.1), on ui/Modal for dialog semantics (role, Tab trap, Escape, focus
          return). Mounted per open, so each starts from empty credentials. */}
      {actionModal && (
        <ActionModal
          modal={actionModal}
          backupEmail={backupEmail}
          handlers={{
            onToggleRecoveryOnly,
            onToggleRecoveryHardened,
            onResetTOTP,
            onRevokeWebAuthnKey,
            onDisableEmailSms,
            onSetBackupEmail,
          }}
          onClose={clearActionModal}
          onAccepted={closeAcceptedAction}
        />
      )}
    </div>
  );
};

export default MFATierSelector;
