import React, { useState, useMemo, useEffect } from 'react';
import { z } from 'zod';
import { unwrapLoginKeys, generateRegistrationKeys, exportPublicKey } from '../../utils/crypto';
import {
  e2eeService,
  type E2EEInitializationGuard,
  type E2EEInitializationReceipt,
} from '../../services/e2ee/e2eeService';
import { E2EEInitTeardownError } from '../../services/e2ee/e2eeErrors';
import { errorMessage } from '../../utils/redactError';
import { persistE2EESessionKeys } from '../../utils/persistE2EESessionKeys';
import { parseContinuationPair, type ContinuationPair } from '../../utils/continuationPair';
import { hydratePostLogin } from '../../services/system/postLoginHydration';
import { beginPostLoginHydrationGuard } from '../../services/system/postLoginHydrationLifecycle';
import { useAuthStore } from '../../stores/auth/authStore';
import { useClientConfigStore } from '../../stores/ui/clientConfigStore';
import {
  apiFetch,
  ensureMachineId,
  revokeAbortedSession,
  type AbortedSessionRef,
} from '../../services/system/apiClient';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
  type RuntimeServerSelection,
} from '../../services/system/runtimeServerBase';
import type { CredentialOwner } from '../../../main/ipcContract';
import type { UserProfile } from '../../stores/auth/userStore';
import TOTPInput from './TOTPInput';
import BackupCodeInput from './BackupCodeInput';
import WebAuthnPrompt from './WebAuthnPrompt';
import MFAMethodPicker, {
  getDefaultMethod,
  getAvailableCategories,
  MFAMethodCategory,
} from './MFAMethodPicker';
import LoadingSpinner from './LoadingSpinner';
import { SSOButton } from './SSOButton';
import { useSSOFlow } from '../../hooks/ui/useSSOFlow';
import { useSSOStore } from '../../stores/auth/ssoStore';
import KeyRecoveryPrompt from './KeyRecoveryPrompt';
import { Eye, EyeOff } from 'lucide-react';
import { base64urlToBuffer } from '../../utils/base64url';
import './Login.css';
import './TOTPInput.css';

const AuthenticatorTransportSchema = z.enum([
  'ble',
  'hybrid',
  'internal',
  'nfc',
  'smart-card',
  'usb',
]);
const WebAuthnRequestSchema = z.object({
  challenge: z.string().min(1),
  timeout: z.number().nonnegative().optional(),
  rpId: z.string().min(1).optional(),
  allowCredentials: z
    .array(
      z.object({
        type: z.literal('public-key'),
        id: z.string().min(1),
        transports: z.array(AuthenticatorTransportSchema).optional(),
      })
    )
    .optional(),
  userVerification: z.enum(['discouraged', 'preferred', 'required']).optional(),
});
const WebAuthnServerOptionsSchema = z.union([
  WebAuthnRequestSchema,
  z.object({ publicKey: WebAuthnRequestSchema }).transform(({ publicKey }) => publicKey),
]);

const UserProfileSchema = z
  .object({
    id: z.string().min(1),
    username: z.string(),
    email: z.string().optional(),
    email_verified: z.boolean().optional(),
    display_name: z.string().optional(),
    bio: z.string().optional(),
    avatar_url: z.string().optional(),
    header_image_url: z.string().optional(),
    links: z.array(z.string()).optional(),
    created_at: z.string().optional(),
    username_changed_at: z.string().optional(),
    username_change_eligible_at: z.string().optional(),
  })
  .passthrough();
const LoginSuccessResponseSchema = z.object({
  access_token: z.string().min(1),
  refresh_token: z.string().min(1),
  session_id: z.string().min(1),
  remember_me: z.boolean().optional(),
  user: UserProfileSchema,
  e2ee_keys: z.object({
    wrapped_private_key: z.string().min(1),
    key_derivation_salt: z.string().min(1),
    key_derivation_alg: z.enum(['argon2id', 'pbkdf2']).optional(),
    key_version: z.number().int().positive().optional(),
  }),
});
const MFARequiredResponseSchema = z.object({
  mfa_required: z.literal(true),
  mfa_challenge_token: z.string().min(1),
  methods: z.array(z.string()),
  recovery_only_methods: z.array(z.string()).optional(),
  webauthn_options: z.unknown().optional(),
});
const LoginErrorResponseSchema = z
  .object({
    error: z.string().optional(),
    error_code: z.string().optional(),
    providers: z.array(z.string()).optional(),
  })
  .passthrough();

type LoginSuccessResponse = z.infer<typeof LoginSuccessResponseSchema>;
type MFARequiredResponse = z.infer<typeof MFARequiredResponseSchema>;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function responseError(data: unknown, fallback: string): string {
  const parsed = LoginErrorResponseSchema.safeParse(data);
  return parsed.success ? (parsed.data.error ?? fallback) : fallback;
}

function malformedLoginSession(data: unknown, apiBase: string) {
  const record = isRecord(data) ? data : {};
  return {
    accessToken: typeof record.access_token === 'string' ? record.access_token : null,
    refreshToken: typeof record.refresh_token === 'string' ? record.refresh_token : null,
    sessionId: typeof record.session_id === 'string' ? record.session_id : null,
    apiBase,
  };
}

function responseIssuedSessionID(response: Response): string | null {
  return response.headers?.get('X-Concord-Session-ID')?.trim() || null;
}

async function revokeMalformedLoginSession(
  data: unknown,
  apiBase: string,
  issuedSessionID: string | null
): Promise<void> {
  const session = malformedLoginSession(data, apiBase);
  // When present, the response header is the backend's authoritative refresh
  // row ID. Prefer it to any partially decoded body value; this is precisely
  // the malformed-success path the header exists to recover.
  if (issuedSessionID !== null) session.sessionId = issuedSessionID;
  if (
    session.refreshToken !== null ||
    (session.accessToken !== null && session.sessionId !== null)
  ) {
    await revokeAbortedSession(session);
  } else if (issuedSessionID !== null) {
    await revokeAbortedSession({
      accessToken: null,
      sessionId: issuedSessionID,
      cookieBound: true,
      apiBase,
    });
  }
}

async function parseLoginResponseJson(response: Response, apiBase: string): Promise<unknown> {
  try {
    return await response.json();
  } catch (error) {
    if (!response.ok) throw error;

    // Only CompleteLogin responses carry a server-issued session ID. An
    // undecodable 2xx initial MFA challenge has no new session and must not log
    // out an unrelated remembered cookie. The backend binds this ID to the
    // ambient cookie hash atomically, so a successor cookie cannot be revoked.
    const sessionID = responseIssuedSessionID(response);
    if (sessionID !== null) {
      await revokeAbortedSession({
        accessToken: null,
        sessionId: sessionID,
        cookieBound: true,
        apiBase,
      });
    }
    throw new Error('Server returned an invalid login response.');
  }
}

async function parseLoginSuccessResponse(
  data: unknown,
  apiBase: string,
  issuedSessionID: string | null
): Promise<LoginSuccessResponse> {
  const parsed = LoginSuccessResponseSchema.safeParse(data);
  if (!parsed.success) {
    // The backend commits the refresh cookie before serializing the success
    // body. Revoke when the issuance marker or safely extracted access/session
    // evidence proves this is a completion — never for an unmarked malformed
    // initial MFA challenge, which did not replace the remembered cookie.
    await revokeMalformedLoginSession(data, apiBase, issuedSessionID);
    throw new Error('Server returned an invalid login response.');
  }
  return parsed.data;
}

function parseWebAuthnOptions(serverOptions: unknown): PublicKeyCredentialRequestOptions {
  const parsed = WebAuthnServerOptionsSchema.safeParse(serverOptions);
  if (!parsed.success) throw new Error('Server returned invalid WebAuthn options.');
  const pk = parsed.data;
  const opts: PublicKeyCredentialRequestOptions = {
    challenge: base64urlToBuffer(pk.challenge),
    timeout: pk.timeout,
    rpId: pk.rpId,
  };
  if (pk.allowCredentials) {
    opts.allowCredentials = pk.allowCredentials.map((cred) => ({
      type: cred.type,
      id: base64urlToBuffer(cred.id),
      // TypeScript's lib.dom omits WebAuthn Level 3's smart-card value. The
      // closed Zod enum above matches the backend's go-webauthn v0.17.4 contract.
      transports: cred.transports?.map((transport) => transport as AuthenticatorTransport),
    }));
  }
  if (pk.userVerification) {
    opts.userVerification = pk.userVerification;
  }
  return opts;
}

/** Check that Electron safeStorage is available, returning an error message or null */
async function checkSafeStorage(): Promise<string | null> {
  if (!globalThis.electron?.checkPermission) return null;
  try {
    const status = await globalThis.electron.checkPermission('secureStorage');
    if (status !== 'granted') {
      return 'Secure storage is unavailable. Concord requires keychain / credential manager access to safely store authentication tokens and encryption keys. Please enable it and restart the app.';
    }
    return null;
  } catch {
    return 'Secure storage could not be verified. Please try again, and if the problem persists, restart the app.';
  }
}

/**
 * User-facing copy for a login aborted by a mid-login session teardown
 * (E2EEInitTeardownError). Direction-neutral: the session died under the
 * login (authoritative 401 → reset), not through anything the user did.
 */
const TEARDOWN_ABORT_NOTICE =
  'Your session ended before sign-in could finish. Please sign in again.';

/**
 * User-facing copy for a consented key reset that COMMITTED but whose
 * continuation session the server deliberately withheld (#2415, §4.2). The
 * trailing clause is load-bearing: the user just consented to irreversible
 * history loss and must not be left wondering whether to repeat it.
 *
 * Rendered through component-local `setErrors`, never `authStore.loginNotice`:
 * Login is already mounted on this path and the notice is consumed by a
 * useState initializer plus a mount-only effect, so a staged notice would never
 * render — the exact silent failure #2415 exists to close.
 */
const KEY_RESET_REAUTH_NOTICE =
  'Your encryption keys were reset, but we could not keep you signed in. Sign in again to continue — your new keys are already active.';

class LoginOriginChangedError extends E2EEInitTeardownError {
  constructor() {
    super();
    this.name = 'LoginOriginChangedError';
  }
}

class LoginOwnershipChangedError extends E2EEInitTeardownError {}

/**
 * Map a login-flow error to its user-facing message. A teardown abort
 * additionally stages TEARDOWN_ABORT_NOTICE as a one-shot
 * authStore.loginNotice: the early access-token set usually navigates "/"
 * into the authenticated tree before the abort reaches a catch, so THIS
 * Login instance is already unmounted and its setErrors/setMfaError are
 * no-ops — while the teardown also cleared the token, bouncing the user to
 * a FRESH Login, which seeds its error banner from the staged notice
 * (PR #2337).
 */
function describeLoginError(error: unknown, fallback: string): string {
  if (error instanceof E2EEInitTeardownError) {
    // Stage the one-shot notice only when no live session took over: after a
    // successful successor login the user is IN the app, and a staged notice
    // would surface stale on a much-later Login mount.
    if (useAuthStore.getState().accessToken === null) {
      useAuthStore.getState().setLoginNotice(TEARDOWN_ABORT_NOTICE);
    }
    return TEARDOWN_ABORT_NOTICE;
  }
  return error instanceof Error ? error.message : fallback;
}

export interface LoginProps {
  onBack: () => void;
  onSuccess: (data: { accessToken: string; user?: UserProfile; rememberMe: boolean }) => void;
  onSwitchToRegister: () => void;
  onForgotPassword: () => void;
}

interface FormData {
  email: string;
  password: string;
  rememberMe: boolean;
}

interface FormErrors {
  email?: string;
  password?: string;
  general?: string;
}

const EMPTY_OAUTH_PROVIDERS: string[] = [];

interface PasswordFieldProps {
  value: string;
  onChange: (e: React.ChangeEvent<HTMLInputElement>) => void;
  disabled: boolean;
  error?: string;
}

// Login password field with an accessible show/hide toggle (#1917). Extracted
// as a focused presentational component so the visibility ternaries live here
// instead of inflating Login's cognitive complexity (SonarQube S3776). Owns its
// own default-hidden `showPassword` state, which resets on unmount — a revealed
// password never persists across the MFA/SSO/success transitions or to disk.
// Only the input `type` is driven by state; `value`/`onChange` are untouched, so
// the submitted credential is byte-identical whether shown or hidden.
const PasswordField: React.FC<PasswordFieldProps> = ({ value, onChange, disabled, error }) => {
  const [showPassword, setShowPassword] = useState(false);
  const toggleLabel = showPassword ? 'Hide password' : 'Show password';
  return (
    <div className="form-group">
      <label htmlFor="login-password" className="form-label">
        Password
      </label>
      <div className="password-input-wrapper">
        <input
          id="login-password"
          type={showPassword ? 'text' : 'password'}
          className={`form-input ${error ? 'error' : ''}`}
          placeholder="Enter your password"
          value={value}
          onChange={onChange}
          disabled={disabled}
        />
        <button
          type="button"
          className="password-toggle-btn"
          onClick={() => setShowPassword((v) => !v)}
          aria-label={toggleLabel}
          aria-pressed={showPassword}
          title={toggleLabel}
          disabled={disabled}
        >
          {showPassword ? <EyeOff size={18} /> : <Eye size={18} />}
        </button>
      </div>
      {error && <span className="form-error">{error}</span>}
    </div>
  );
};

const Login: React.FC<LoginProps> = ({
  onBack,
  onSuccess,
  onSwitchToRegister,
  onForgotPassword,
}) => {
  const [formData, setFormData] = useState<FormData>({
    email: '',
    password: '',
    rememberMe: false,
  });

  // Seeded from any staged one-shot notice (a teardown-aborted prior login —
  // see describeLoginError); the mount effect below consumes the store copy.
  const [errors, setErrors] = useState<FormErrors>(() => {
    const notice = useAuthStore.getState().loginNotice;
    return notice ? { general: notice } : {};
  });
  const [isSubmitting, setIsSubmitting] = useState(false);

  // Consume the staged notice so it renders exactly once. StrictMode-safe:
  // the seeded state survives the simulated remount, and the second effect
  // pass is a no-op on the already-cleared store.
  useEffect(() => {
    if (useAuthStore.getState().loginNotice) {
      useAuthStore.getState().setLoginNotice(null);
    }
  }, []);

  // SSO state — populated when the server returns 403 account_uses_sso. The
  // password form is replaced with a list of SSO-only buttons so the user is
  // never stranded with credentials they can't actually use.
  const [ssoOnlyProviders, setSsoOnlyProviders] = useState<string[] | null>(null);
  const { begin: beginSSO } = useSSOFlow();
  const ssoAuthenticating = useSSOStore((state) => state.state.phase === 'authenticating');
  const authBusy = isSubmitting || ssoAuthenticating;
  const oauthProviders = useClientConfigStore(
    (state) => state.serverCapabilities?.auth.oauthProviders ?? EMPTY_OAUTH_PROVIDERS
  );
  const showGoogleSSO = oauthProviders.includes('google');
  const showAppleSSO = oauthProviders.includes('apple');
  const hasDefaultSSO = showGoogleSSO || showAppleSSO;

  // MFA state
  const [mfaRequired, setMfaRequired] = useState(false);
  const [mfaChallengeToken, setMfaChallengeToken] = useState('');
  const [mfaMethods, setMfaMethods] = useState<string[]>([]);
  const [mfaMode, setMfaMode] = useState<MFAMethodCategory | 'method-select'>('totp');
  const [mfaRecoveryOnly, setMfaRecoveryOnly] = useState<string[]>([]);
  const [mfaError, setMfaError] = useState('');
  const [mfaServerSelection, setMfaServerSelection] = useState<RuntimeServerSelection | null>(null);
  const [webauthnOptions, setWebauthnOptions] = useState<PublicKeyCredentialRequestOptions | null>(
    null
  );

  // Promise-deferred key-recovery prompt: the login catch awaits the user's
  // decision before resetting keys (consented data loss) or aborting (#1293).
  // The decision carries an optional MFA code for the step-up re-auth retry.
  const [keyRecoveryResolver, setKeyRecoveryResolver] = useState<
    ((decision: { action: 'reset' | 'cancel'; mfaCode?: string }) => void) | null
  >(null);
  // When the server demands MFA for the destructive reset, the prompt re-opens
  // in MFA-entry mode.
  const [keyRecoveryMfaRequired, setKeyRecoveryMfaRequired] = useState(false);

  // The double-arrow is load-bearing: setKeyRecoveryResolver(() => resolve)
  // STORES `resolve` in state. Without it React would CALL `resolve` as a
  // state-updater. Do NOT "simplify" to setKeyRecoveryResolver(resolve).
  const promptKeyRecovery = (): Promise<{ action: 'reset' | 'cancel'; mfaCode?: string }> =>
    new Promise((resolve) => setKeyRecoveryResolver(() => resolve));

  // Non-destructive abort: clear the early-set token so there is no
  // half-authenticated state, and surface guidance on the login screen (#1293).
  const abortKeyRecovery = (expectedAuthGeneration?: number) => {
    const auth = useAuthStore.getState();
    if (expectedAuthGeneration !== undefined && auth.authGeneration !== expectedAuthGeneration) {
      return;
    }
    auth.clearAccessToken();
    setErrors({
      general:
        'Your encryption keys couldn’t be recovered on this device. You can try again, or recover your account on a device that still has your keys.',
    });
    setIsSubmitting(false);
  };

  // If Login unmounts while the prompt is open, settle the pending promise to
  // 'cancel' so the suspended login flow doesn't leak a never-collected closure
  // holding the password + tokens (code-review hardening, #1293).
  useEffect(() => {
    return () => {
      keyRecoveryResolver?.({ action: 'cancel' });
    };
  }, [keyRecoveryResolver]);

  const validateForm = (): boolean => {
    const newErrors: FormErrors = {};

    // Email validation
    const emailRegex = /^[^\s@]+@[^\s@.]+(?:\.[^\s@.]+)+$/;
    if (!formData.email) {
      newErrors.email = 'Email is required';
    } else if (!emailRegex.test(formData.email)) {
      newErrors.email = 'Invalid email format';
    }

    // Password validation
    if (!formData.password) {
      newErrors.password = 'Password is required';
    }

    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (ssoAuthenticating) return;
    setErrors({});

    if (!validateForm()) {
      return;
    }

    // Bind the whole submission to its invocation-time origin before the first
    // await. Safe-storage permission checking is an IPC boundary too; a server
    // switch while it is pending must not retarget the credentials afterward.
    const requestSelection = captureRuntimeServerSelection();
    const requestApiBase = requestSelection.apiBase;

    // Fail-closed safeStorage enforcement (#197)
    const storageError = await checkSafeStorage();
    if (storageError) {
      setErrors({ general: storageError });
      return;
    }
    if (!runtimeServerSelectionIsCurrent(requestSelection)) {
      setErrors({ general: 'Server selection changed. Please try again.' });
      return;
    }

    setIsSubmitting(true);

    try {
      console.debug('Logging in...');

      const machineId = await ensureMachineId(requestApiBase);
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        throw new LoginOriginChangedError();
      }

      // Login with backend
      const response = await fetch(`${requestApiBase}/api/v1/auth/login`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(machineId ? { 'X-Machine-Id': machineId } : {}),
        },
        credentials: 'include', // Include cookies for refresh token
        body: JSON.stringify({
          email: formData.email,
          password: formData.password,
          remember_me: formData.rememberMe,
        }),
      });

      const data = await parseLoginResponseJson(response, requestApiBase);
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        await revokeMalformedLoginSession(data, requestApiBase, responseIssuedSessionID(response));
        throw new LoginOriginChangedError();
      }

      // SSO short-circuits — see helper closures below for the per-status
      // routing (403 account_uses_sso, 500 sso_provider_lookup_failed,
      // 500 sso_account_misconfigured). Extracted so handleSubmit stays under
      // SonarQube's S3776 cognitive-complexity threshold.
      if (tryApplyAccountUsesSSO(response, data)) return;
      if (tryApplySSO500Error(response, data)) return;

      if (!response.ok) {
        throw new Error(responseError(data, 'Login failed'));
      }

      // Check if MFA is required
      if (isRecord(data) && data.mfa_required === true) {
        const mfaResponse = MFARequiredResponseSchema.safeParse(data);
        if (!mfaResponse.success) {
          await revokeMalformedLoginSession(
            data,
            requestApiBase,
            responseIssuedSessionID(response)
          );
          throw new Error('Server returned an invalid MFA challenge.');
        }
        applyMfaRequiredFromResponse(mfaResponse.data, requestSelection);
        return;
      }

      await completeLoginFromResponse(
        await parseLoginSuccessResponse(data, requestApiBase, responseIssuedSessionID(response)),
        requestSelection
      );
    } catch (error) {
      console.error('Login error:', errorMessage(error));
      setErrors({
        general: describeLoginError(error, 'Login failed. Please try again.'),
      });
    } finally {
      setIsSubmitting(false);
    }
  };

  // 403 account_uses_sso short-circuit: backend signals the account is SSO-only
  // (password login disabled, ≥1 identity provider linked). Swap the form for
  // the SSO-only view rather than showing a generic error. Lockout counters
  // are NOT engaged on this branch — see services/control-plane/internal/auth/
  // handlers.go (PasswordLoginDisabled handling). Returns true if handled.
  // Extracted from handleSubmit to stay under S3776 (the inlined ternary on
  // data.providers contributed nesting penalty). Behavior unchanged.
  const tryApplyAccountUsesSSO = (response: Response, data: unknown): boolean => {
    if (response.status !== 403) return false;
    const parsed = LoginErrorResponseSchema.safeParse(data);
    if (!parsed.success || parsed.data.error_code !== 'account_uses_sso') return false;
    setSsoOnlyProviders(parsed.data.providers ?? []);
    setIsSubmitting(false);
    return true;
  };

  // Map a 500 SSO short-circuit response to the appropriate user-facing
  // message and apply it. Returns true if the response was handled (caller
  // should `return` immediately) or false if not a known SSO 500 case (caller
  // should fall through). Extracted from handleSubmit so that handler stays
  // under SonarQube's S3776 cognitive-complexity threshold; behavior is
  // unchanged. See the inline comment at the call site for the two error_codes
  // and their UX rationale.
  const tryApplySSO500Error = (response: Response, data: unknown): boolean => {
    if (response.status !== 500) return false;
    const parsed = LoginErrorResponseSchema.safeParse(data);
    if (!parsed.success) return false;
    const code = parsed.data.error_code;
    let message: string | null = null;
    if (code === 'sso_provider_lookup_failed') {
      message =
        "We couldn't load your sign-in options. Please try again in a moment, or contact support if this continues.";
    } else if (code === 'sso_account_misconfigured') {
      message =
        "This account isn't fully set up for sign-in. Please contact support — error code: SSO_MISCONFIG.";
    }
    if (message === null) return false;
    setErrors({ general: message });
    setIsSubmitting(false);
    return true;
  };

  // Surface MFA challenge to the user when the server returns mfa_required.
  // Extracted from handleSubmit so that handler stays under SonarQube's S3776
  // cognitive-complexity threshold; behavior is unchanged.
  const applyMfaRequiredFromResponse = (
    data: MFARequiredResponse,
    requestSelection: RuntimeServerSelection
  ) => {
    const serverMethods = data.methods;
    const recoveryOnly = data.recovery_only_methods ?? [];
    const parsedWebAuthnOptions = data.webauthn_options
      ? parseWebAuthnOptions(data.webauthn_options)
      : null;
    setMfaChallengeToken(data.mfa_challenge_token);
    setMfaMethods(serverMethods);
    setMfaRecoveryOnly(recoveryOnly);
    setMfaServerSelection(requestSelection);
    setMfaMode(getDefaultMethod(serverMethods, recoveryOnly));
    setWebauthnOptions(parsedWebAuthnOptions);
    setMfaRequired(true);
    setIsSubmitting(false);
  };

  // Bundled rather than passed positionally: the #2415 `adoptLiveContinuation`
  // addition took this to EIGHT parameters and tripped `typescript:S107` (max 7).
  // The repo hit the same wall server-side on `users.NewHandler` and answered it
  // the same way — a deps object, not a shorter list.
  interface KeyRecoveryDeps {
    teardownEpoch: number | undefined;
    abortedSession: AbortedSessionRef;
    requestSelection: RuntimeServerSelection;
    abortForOriginChange: () => Promise<never>;
    initializationGuard: E2EEInitializationGuard;
    requireFlowOwnership: () => Promise<void>;
    // Set the caller's flowInitializationReceipt from the recovery initialize()'s
    // OWN returned receipt (#2423), so an ownership/origin abort inside this helper
    // wipes the committed keyset instead of no-op'ing on a null receipt (CWE-212).
    captureFlowReceipt: (receipt: E2EEInitializationReceipt | null) => void;
    // #2415: invoked SYNCHRONOUSLY the instant the committed reset's continuation
    // pair is parsed, before the next await. Re-points `abortedSession` at the
    // live session and adopts it. Without this the pair is only adopted after
    // performReset returns, and every abort site in between revokes the login pair
    // the reset already killed while the live continuation session stays
    // authenticated on the server (CWE-613).
    adoptLiveContinuation: (pair: ContinuationPair) => void;
  }

  // Drives the consented key-recovery flow when the login unwrap fails: prompts
  // for consent, performs the step-up-authenticated reset (current password +
  // MFA retry), and returns false if the user aborted. On a committed reset it
  // returns the #2415 continuation pair the server appended to the response, or
  // null when the server deliberately withheld it — the caller adopts the pair
  // or fails closed, and MUST NOT retry. Throws on reset failure (the caller
  // surfaces the error; the token is cleared here first). Extracted from
  // completeLoginFromResponse to keep that function under the cognitive-
  // complexity threshold (#1293).
  const handleKeyRecovery = async ({
    teardownEpoch,
    abortedSession,
    requestSelection,
    abortForOriginChange,
    initializationGuard,
    requireFlowOwnership,
    captureFlowReceipt,
    adoptLiveContinuation,
  }: KeyRecoveryDeps): Promise<ContinuationPair | null | false> => {
    // Clear ONLY this flow's own early-set access token — never a successor's.
    // Deduplicated from the two reset-error branches below and kept out of the
    // reset flow so this handler stays under the S3776 cognitive-complexity
    // threshold.
    const clearAbortedFlowToken = () => {
      const auth = useAuthStore.getState();
      if (
        abortedSession.authGeneration !== undefined &&
        auth.authGeneration === abortedSession.authGeneration
      ) {
        auth.clearAccessToken();
      }
    };

    // On ANY reset failure, clear the early-set token then rethrow so a failed
    // reset cannot strand the user in a half-authenticated state (the cancel
    // branch's invariant — spec §3.3.4). A teardown during the recovery init
    // follows the SAME abort contract as the primary init path (Codex P1,
    // #2337): strip only this flow's own token and revoke the aborted server
    // session — otherwise the login's refresh-token row + HttpOnly cookie
    // outlive the logout-class teardown. Always throws (never returns).
    const handleResetError = async (resetError: unknown): Promise<never> => {
      if (resetError instanceof E2EEInitTeardownError) {
        clearAbortedFlowToken();
        if (
          !(resetError instanceof LoginOriginChangedError) &&
          !(resetError instanceof LoginOwnershipChangedError)
        ) {
          await revokeAbortedSession(abortedSession);
        }
        throw resetError;
      }
      clearAbortedFlowToken();
      throw resetError;
    };

    // The access token set at login authenticates the reset PUT. The reset is a
    // destructive, step-up-authenticated operation, so it sends the current
    // password (already in hand from the login form) and, when the server
    // demands it, an MFA code; then re-inits E2EE with the new keys. Returns
    // the #2415 continuation pair on success (null when the server withheld it),
    // or false if the user cancelled the MFA step-up. Kept as its own closure
    // (measured separately for cognitive complexity) so this handler stays under
    // the S3776 threshold; every origin/ownership fence is preserved exactly,
    // and any failure propagates to handleResetError via the catch.
    const performReset = async (
      resetDecision: Awaited<ReturnType<typeof promptKeyRecovery>>
    ): Promise<ContinuationPair | null | false> => {
      const newKeys = await generateRegistrationKeys(formData.password);
      const publicKeyB64 = await exportPublicKey(newKeys.publicKey);
      const sendReset = (mfaCode?: string) => {
        if (!runtimeServerSelectionIsCurrent(requestSelection)) {
          return abortForOriginChange();
        }
        return apiFetch('/api/v1/users/me/keys', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            wrapped_private_key: newKeys.wrappedPrivateKey,
            key_derivation_salt: newKeys.keyDerivationSalt,
            key_derivation_alg: newKeys.keyDerivationAlg,
            public_key: publicKeyB64,
            current_password: formData.password,
            mfa_code: mfaCode,
            acknowledge_data_loss: true,
          }),
        });
      };

      await requireFlowOwnership();
      let replaceRes = await sendReset(resetDecision.mfaCode);
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        await abortForOriginChange();
      }
      await requireFlowOwnership();

      // Step-up MFA: if the server requires an MFA code, re-open the prompt in
      // MFA-entry mode and retry once with the supplied code.
      if (replaceRes.status === 403) {
        const body = await replaceRes.json().catch(() => ({}));
        if (body?.error === 'mfa_required') {
          setKeyRecoveryMfaRequired(true);
          const mfaDecision = await promptKeyRecovery();
          setKeyRecoveryResolver(null);
          setKeyRecoveryMfaRequired(false);
          if (!runtimeServerSelectionIsCurrent(requestSelection)) {
            await abortForOriginChange();
          }
          await requireFlowOwnership();
          if (mfaDecision.action === 'cancel') {
            abortKeyRecovery(abortedSession.authGeneration);
            return false;
          }
          await requireFlowOwnership();
          replaceRes = await sendReset(mfaDecision.mfaCode);
        }
      }

      if (!replaceRes.ok) throw new Error('Failed to reset encryption keys. Please try again.');

      // #2415: the continuation pair lives in THIS body. The reset revoked every
      // refresh token for this user — including the one the login that led here
      // just minted — so everything the caller persists downstream must come
      // from the pair, never from the login response. A missing pair is a
      // deliberate server outcome, never a transport error and never retried.
      const replaceBody = await replaceRes.json().catch(() => ({}));
      const continuation = isRecord(replaceBody) ? parseContinuationPair(replaceBody) : null;
      // #2415 FIX: no await may separate the parse from the re-point. From this
      // line on, the login pair is a corpse and the continuation pair is the
      // only live session this flow owns on the server.
      if (continuation !== null) adoptLiveContinuation(continuation);

      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        await abortForOriginChange();
      }
      await requireFlowOwnership();
      const recoveryReceipt = await e2eeService.initialize(
        formData.password,
        newKeys.wrappedPrivateKey,
        newKeys.keyDerivationSalt,
        newKeys.keyDerivationAlg,
        initializationGuard,
        teardownEpoch
      );
      captureFlowReceipt(recoveryReceipt);
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        await abortForOriginChange();
      }
      await requireFlowOwnership();
      console.debug('E2EE keys reset and service initialized!');
      return continuation;
    };

    const decision = await promptKeyRecovery();
    setKeyRecoveryResolver(null);
    if (!runtimeServerSelectionIsCurrent(requestSelection)) {
      await abortForOriginChange();
    }
    await requireFlowOwnership();

    if (decision.action === 'cancel') {
      abortKeyRecovery(abortedSession.authGeneration);
      return false;
    }

    try {
      return await performReset(decision);
    } catch (resetError) {
      return await handleResetError(resetError);
    }
  };

  // Complete login after receiving tokens (shared between direct login and MFA verify)
  const completeLoginFromResponse = async (
    data: LoginSuccessResponse,
    requestSelection: RuntimeServerSelection
  ) => {
    console.debug('Login successful, unwrapping private key...');
    const requestApiBase = requestSelection.apiBase;

    // Capture the teardown epoch BEFORE publishing the access token: setting
    // the token arms authenticated effects that can trigger an authoritative
    // 401 -> nuclearReset() teardown while unwrap/Argon2id below is still in
    // flight. Passing this epoch into initialize() makes a teardown anywhere
    // in the login span abort the key commit (E2EEInitTeardownError) instead
    // of letting the flow continue into a half-authenticated state
    // (Codex P1-A/P1-B, PR #2337).
    const teardownEpoch = e2eeService.captureTeardownEpoch();

    // Abort-path token strip (Gitar/Codex P1, PR #2337): a teardown landing
    // while an await below is in flight clears the access token, but this
    // flow could leave it — or re-publish it — resident after the abort
    // throw, and routing would then treat the user as authenticated with
    // E2EE cleared. Identity-guarded so a rapid successor login's fresh
    // token is never stripped by this stale continuation (same
    // session-binding discipline as revokeAbortedSession).
    let flowAuthGeneration: number | null = null;
    const stripAbortedToken = () => {
      const auth = useAuthStore.getState();
      if (flowAuthGeneration !== null && auth.authGeneration === flowAuthGeneration) {
        auth.clearAccessToken();
      }
    };

    // One session reference for every abort site: this flow's own credentials
    // plus the API origin they belong to (self-hosted switch hazard — see
    // AbortedSessionRef in apiClient).
    const abortedSession: AbortedSessionRef = {
      accessToken: data.access_token,
      refreshToken: data.refresh_token,
      sessionId: data.session_id ?? null,
      apiBase: requestApiBase,
    };

    // #2415: the continuation pair a consented key reset returned, or null when
    // no reset happened (the inline unwrap succeeded). ReplaceMyKeys revokes
    // every refresh token this user holds — including the one this login just
    // minted — so once a reset commits, `data.access_token` / `data.refresh_token`
    // are DEAD and the keychain persist below must use the pair instead.
    let continuation: ContinuationPair | null = null;

    /**
     * Adopt a committed reset's continuation pair as a NEW auth lifecycle and
     * re-point every abort site at the credentials that are actually live.
     * Returns false when the login must stop.
     *
     * The adoption and the re-point are ONE synchronous step with no intervening
     * await, deliberately: until the re-point lands, `abortedSession` still holds
     * the login response's tokens, which the reset has just revoked — so an abort
     * taken in that window would revoke NOTHING and leave the live continuation
     * session authenticated on the server. Mutation is the established idiom here
     * (see the flowAuthGeneration reassignment below).
     *
     * `beginAuthLifecycleIfCurrent` rather than a rotation: the pair is a
     * brand-new server session, so it bumps `authGeneration` honestly and never
     * goes near `applyRefreshedCredentials`' previous_session_id lineage proof.
     * Both `flowAuthGeneration` and `abortedSession.authGeneration` follow it so
     * `flowOwnsAuthStore()` and the abort-path token strips keep matching.
     */
    let continuationAdopted = false;

    /**
     * #2415 FIX. Re-point every abort site at the LIVE continuation credentials
     * and adopt them as a new auth lifecycle. Called synchronously from inside
     * performReset the instant the pair is parsed — the re-point must not trail
     * the parse across any await.
     *
     * The re-point happens even when the CAS declines: the login pair is already
     * revoked server-side, so revoking it is a guaranteed no-op, whereas the
     * continuation row is live for 30 days with remember_me = true. An abort
     * must always be able to reach the session this flow actually created.
     */
    const adoptLiveContinuation = (pair: ContinuationPair): void => {
      abortedSession.accessToken = pair.accessToken;
      abortedSession.refreshToken = pair.refreshToken;
      abortedSession.sessionId = pair.sessionId;
      continuation = pair;
      // Narrowing only: flowAuthGeneration is published before the unwrap that
      // leads here, so null is unreachable — and a non-null assertion is not
      // allowed on an auth path.
      if (flowAuthGeneration === null) return;
      const adoptedGeneration = useAuthStore
        .getState()
        .beginAuthLifecycleIfCurrent(flowAuthGeneration, pair.accessToken, pair.sessionId);
      // A successor lifecycle became current while the reset was in flight; do
      // not clobber it with a half-applied adoption.
      if (adoptedGeneration === null) return;
      flowAuthGeneration = adoptedGeneration;
      abortedSession.authGeneration = adoptedGeneration;
      continuationAdopted = true;
    };

    const adoptResetContinuation = (pair: ContinuationPair | null): boolean => {
      if (pair === null) {
        // Committed reset, no session (§4.2). Close the prompt FIRST: it is a
        // native <dialog> opened with showModal() that owns a focus trap, so
        // leaving it open strands the user away from the sign-in form they now
        // have to use. Mirrors abortKeyRecovery. The known-revoked login refresh
        // token is never persisted on this path — the caller stops here.
        setKeyRecoveryResolver(null);
        // performReset already committed a NEW keyset via e2eeService.initialize,
        // so returning to the login screen without clearing it leaves account A's
        // private key resident on a mounted Login with no session — and a
        // different account signing in here would run its initialize against a
        // singleton still holding A's keys. Every other post-commit abort in this
        // function clears; the ChangePassword sibling clears via nuclearReset.
        // clearFlowSessionKeys (not bare clearKeys) routes through
        // clearKeysIfInitializationCurrent, so it cannot wipe a successor's
        // committed keyset. Safe to drop: the server durably holds the new
        // wrapped key, so re-login re-derives it. (CWE-212)
        clearFlowSessionKeys();
        // Generation-guarded, like every other token strip in this file — a bare
        // clearAccessToken() would log out a successor that became current in the
        // microtask window across the awaited handleKeyRecovery boundary.
        stripAbortedToken();
        setErrors({ general: KEY_RESET_REAUTH_NOTICE });
        setIsSubmitting(false);
        return false;
      }
      // The pair was adopted inside performReset, before its first post-parse
      // await (#2415 FIX). Here we only report whether this flow still owns the
      // auth store; a declined CAS already left abortedSession pointing at the
      // live session so the caller's abort revokes it rather than a corpse.
      if (!continuationAdopted) {
        // A successor lifecycle owns the store, so touch NOTHING global — no
        // token strip, no notice, no key teardown. But this component's own
        // spinner and modal are ours to release: the caller returns immediately
        // after this, and without these the user is left on a disabled button
        // behind a focus-trapping <dialog> with no explanation.
        setKeyRecoveryResolver(null);
        setIsSubmitting(false);
      }
      return continuationAdopted;
    };

    let credentialOwner: CredentialOwner | null = null;
    const flowOwnsAuthStore = () => {
      return (
        flowAuthGeneration !== null && useAuthStore.getState().authGeneration === flowAuthGeneration
      );
    };
    const clearAbortedDiskTokens = async () => {
      if (credentialOwner !== null) {
        await globalThis.electron?.clearTokensIfOwner?.(credentialOwner);
      }
    };
    let flowInitializationReceipt: E2EEInitializationReceipt | null = null;
    const clearFlowSessionKeys = () => {
      e2eeService.clearKeysIfInitializationCurrent(flowInitializationReceipt);
    };
    // Read the captured receipt's session keys through a closure: the only
    // writes to flowInitializationReceipt now happen inside unwrapOrRecoverKeys
    // (a closure), which TypeScript's outer-scope control-flow analysis cannot
    // observe — a direct outer read would narrow to null. The closure sees the
    // declared receipt type and the live value.
    const capturedSessionKeysForPersist = () => flowInitializationReceipt?.sessionKeys ?? null;
    const abortForOriginChange = async (): Promise<never> => {
      if (flowOwnsAuthStore()) {
        // The key material and renderer token still belong to this old-origin
        // flow. Clear them before the successor origin can observe either.
        e2eeService.clearKeys();
        useAuthStore.getState().clearAccessToken();
        if (credentialOwner !== null) await clearAbortedDiskTokens();
      } else {
        // A successor may already own auth. Clear only if the singleton still
        // holds this flow's exact keyset; never erase keys a successor committed.
        clearFlowSessionKeys();
      }
      await revokeAbortedSession(abortedSession);
      throw new LoginOriginChangedError();
    };
    const abortForLostOwnership = async (): Promise<never> => {
      // The auth store belongs to a successor (or was cleared). Remove only
      // this flow's exact in-memory keyset and only its disk copy; both helpers
      // decline to touch successor-owned state.
      clearFlowSessionKeys();
      if (credentialOwner !== null) await clearAbortedDiskTokens();
      await revokeAbortedSession(abortedSession);
      throw new LoginOwnershipChangedError();
    };
    const requireFlowOwnership = async (): Promise<void> => {
      if (!flowOwnsAuthStore()) await abortForLostOwnership();
    };
    // Combined origin + ownership fence: the exact `if (!current)
    // abortForOriginChange(); await requireFlowOwnership();` pair that guards
    // every post-await boundary in this flow. Extracted so the pair is a single
    // call (measured separately for cognitive complexity), keeping
    // completeLoginFromResponse under the S3776 threshold — with the fence order
    // (origin first, then ownership) preserved exactly.
    const fenceOriginAndOwnership = async (): Promise<void> => {
      if (!runtimeServerSelectionIsCurrent(requestSelection)) await abortForOriginChange();
      await requireFlowOwnership();
    };
    const abortForCredentialPersistenceFailure = async (error: unknown): Promise<never> => {
      clearFlowSessionKeys();
      stripAbortedToken();
      await revokeAbortedSession(abortedSession);
      throw error;
    };

    if (!runtimeServerSelectionIsCurrent(requestSelection)) await abortForOriginChange();

    // Set access token early so e2eeService can make authenticated API calls (e.g., key migration)
    flowAuthGeneration = useAuthStore
      .getState()
      .beginAuthLifecycle(data.access_token, data.session_id ?? null);
    abortedSession.authGeneration = flowAuthGeneration;
    // #2346: hold App's passive "/" route at AuthFlow until THIS login's E2EE is
    // ready. Without it, the early token set above immediately re-renders "/"
    // into <Navigate to="/app/dms"> and unmounts Login, so an inline unwrap
    // failure can never surface the consented key-recovery prompt (Login-local
    // state) and the user is stranded authenticated-but-undecryptable. Bound to
    // flowAuthGeneration so a superseded/aborted flow's stale value can never
    // gate a successor session (owner/generation-bound admission invariant,
    // cf. #2424). Set with no intervening await after beginAuthLifecycle so React
    // observes the token and the gate together on the next render.
    useAuthStore.getState().setPendingE2EEUnlockGeneration(flowAuthGeneration);
    const initializationGuard: E2EEInitializationGuard = {
      signal: new AbortController().signal,
      isCurrent: () => runtimeServerSelectionIsCurrent(requestSelection) && flowOwnsAuthStore(),
    };

    const kdAlg = data.e2ee_keys.key_derivation_alg || 'pbkdf2';
    // Unwrap + initialize the private key, falling back to the consented
    // key-reset prompt on a corrupt-key failure. Returns true to continue the
    // login, false if the user cancelled recovery (caller returns). Kept as its
    // own closure (measured separately for cognitive complexity) so
    // completeLoginFromResponse stays under the S3776 threshold; every fence and
    // the receipt-capture ordering (CWE-212) are preserved exactly, and a
    // teardown/abort still rethrows to the outer login catch.
    const unwrapOrRecoverKeys = async (): Promise<boolean> => {
      try {
        await unwrapLoginKeys(
          formData.password,
          data.e2ee_keys.wrapped_private_key,
          data.e2ee_keys.key_derivation_salt,
          kdAlg
        );
        await fenceOriginAndOwnership();
        // e2eeService.initialize handles PBKDF2→Argon2id migration automatically.
        // #2423: initialize() returns THIS invocation's commit receipt (null if
        // its guard declined the commit), captured BEFORE the origin/ownership
        // re-checks below. Otherwise, if ownership is lost in the microtask window
        // between the synchronous key commit and requireFlowOwnership(), the
        // abort's clearFlowSessionKeys() would run against a still-null receipt
        // and could not wipe the committed keyset (CWE-212). The returned receipt
        // is null when no keyset committed, and clearKeysIfInitializationCurrent is
        // identity+attempt-guarded, so it can never erase a successor's keys.
        flowInitializationReceipt = await e2eeService.initialize(
          formData.password,
          data.e2ee_keys.wrapped_private_key,
          data.e2ee_keys.key_derivation_salt,
          kdAlg,
          initializationGuard,
          teardownEpoch
        );
        await fenceOriginAndOwnership();
        console.debug('Private key unwrapped and E2EE service initialized!');
        return true;
      } catch (unwrapError) {
        // A teardown mid-login is NOT a corrupt-key condition — never route it
        // into the consented key-reset prompt. Rethrow so the outer login catch
        // surfaces an error WITHOUT storing tokens or calling onSuccess (the
        // session this login belonged to is gone).
        if (unwrapError instanceof E2EEInitTeardownError) {
          stripAbortedToken();
          if (
            !(unwrapError instanceof LoginOriginChangedError) &&
            !(unwrapError instanceof LoginOwnershipChangedError)
          ) {
            await revokeAbortedSession(abortedSession);
          }
          throw unwrapError;
        }
        if (!runtimeServerSelectionIsCurrent(requestSelection)) await abortForOriginChange();
        console.warn(
          'Key unwrap failed; prompting for consented key reset',
          errorMessage(unwrapError)
        );
        const recovered = await handleKeyRecovery({
          teardownEpoch,
          abortedSession,
          requestSelection,
          abortForOriginChange,
          initializationGuard,
          requireFlowOwnership,
          captureFlowReceipt: (receipt) => {
            flowInitializationReceipt = receipt;
          },
          adoptLiveContinuation,
        });
        if (recovered === false) return false;
        // #2415: adopt the pair (or fail closed when the server withheld it)
        // before ANY further await — see adoptResetContinuation.
        if (!adoptResetContinuation(recovered)) return false;
        await fenceOriginAndOwnership();
        return true;
      }
    };
    if (!(await unwrapOrRecoverKeys())) return;

    // #2346: E2EE is now ready (inline unwrap succeeded, or the user completed
    // consented key recovery). Release the "/" route hold so post-login
    // navigation proceeds normally. COMPARE-AND-CLEAR on flowAuthGeneration: if
    // this flow lost ownership across the awaits above and a successor login
    // armed its OWN hold, this is a no-op — a plain clear(null) would release the
    // successor into the app before its E2EE is ready (successor-race strand;
    // CodeRabbit review, PR #2435). The recovery-cancel path returned above
    // (abortKeyRecovery already cleared the token, bumping the generation), so
    // the stale gate value is inert there too.
    useAuthStore.getState().clearPendingE2EEUnlockGenerationIfCurrent(flowAuthGeneration);

    // Pre-admit epoch check #1: initialize()'s fence covers only the span up
    // to the key commit. A teardown landing after it resolved must not let us
    // persist a refresh token for a dead session (Codex P1, PR #2337).
    if (e2eeService.wasTornDownSince(teardownEpoch)) {
      stripAbortedToken();
      await revokeAbortedSession(abortedSession);
      throw new E2EEInitTeardownError();
    }
    await fenceOriginAndOwnership();

    // Persist this flow's refresh token to the OS keychain (desktop only) and
    // record its CredentialOwner. Kept as its own closure (measured separately
    // for cognitive complexity) so completeLoginFromResponse stays under the
    // S3776 threshold; every persistence-failure abort is preserved exactly.
    const persistRefreshTokenForDesktop = async (): Promise<void> => {
      if (globalThis.electron) {
        if (!globalThis.electron.storeRefreshToken) {
          await abortForCredentialPersistenceFailure(
            new Error('Desktop refresh-token persistence is unavailable.')
          );
        }
        let storedOwner: CredentialOwner | { status: 'rejected' } | null = null;
        try {
          // #2415: the continuation credentials when a consented key reset ran
          // (the login pair is revoked by then), otherwise the login's own.
          // `rememberMe` deliberately stays on the LOGIN response's flag — the
          // desktop does not adopt the server's forced remember_me.
          storedOwner = await globalThis.electron.storeRefreshToken({
            refreshToken: continuation?.refreshToken ?? data.refresh_token,
            rememberMe: data.remember_me ?? formData.rememberMe,
            apiBase: requestApiBase,
            accessToken: continuation?.accessToken ?? data.access_token,
          });
        } catch (storeError) {
          await abortForCredentialPersistenceFailure(storeError);
        }
        if (typeof storedOwner === 'number') {
          credentialOwner = storedOwner;
        } else {
          await abortForCredentialPersistenceFailure(
            new Error('Desktop rejected refresh-token persistence.')
          );
        }
      }
    };
    await persistRefreshTokenForDesktop();
    await fenceOriginAndOwnership();
    // Deliberately NO token re-publish here: the early set above already
    // installed it, and an unconditional re-set would resurrect a token that
    // a mid-await teardown just cleared — past the final abort check below
    // (Gitar/Codex P1, PR #2337). The admit gate before onSuccess verifies
    // the early-published token is still the live one instead.

    // Persist E2EE keys for restart-survival; failure warns only and never
    // clears the valid in-session keys (#1278/#1288 — see persistE2EESessionKeys).
    await requireFlowOwnership();
    await persistE2EESessionKeys(capturedSessionKeysForPersist(), credentialOwner ?? undefined);
    await fenceOriginAndOwnership();

    // Hydrate all post-login user state — preferences, saved GIFs, notification
    // mute prefs, and the entitlement capability set. Extracted to the shared
    // helper (#1297) so SSO and session-restore hydrate identically; failures
    // of individual steps are non-fatal (each swallows its own network blips).
    await requireFlowOwnership();
    const hydrationGuard = beginPostLoginHydrationGuard();
    await hydratePostLogin({
      signal: hydrationGuard.signal,
      isCurrent: () =>
        hydrationGuard.isCurrent() &&
        runtimeServerSelectionIsCurrent(requestSelection) &&
        flowOwnsAuthStore(),
    });
    await fenceOriginAndOwnership();

    // The storeRefreshToken IPC above may have persisted this dead flow's
    // refresh token + cached access token to DISK. The server revoke below is
    // best-effort — if it fails (network), a later restoreSession() would
    // resurrect the torn-down session from that disk copy (Codex P1, #2337).
    // Identity-guarded: when a successor session owns the renderer, its own
    // storeRefreshToken owns the disk copy too — never wipe it.

    // Pre-admit epoch check #2 — immediately before completing login: a
    // teardown during the token-store / persist / hydrate awaits above must
    // abort here, or AuthFlow would restore the rejected access token and
    // navigate into the app with E2EE cleared (Codex P1, PR #2337).
    if (e2eeService.wasTornDownSince(teardownEpoch)) {
      stripAbortedToken();
      await clearAbortedDiskTokens();
      await revokeAbortedSession(abortedSession);
      throw new E2EEInitTeardownError();
    }

    // Token-lifecycle admit gate (Codex P1, PR #2337): an epoch check proves
    // key custody only at that instant; auth ownership can be cleared or
    // replaced at a later await boundary. If this flow's session no longer owns
    // the auth store (cleared, or replaced by a successor login), it must not
    // be admitted: abort instead of completing with credentials the auth
    // layer already rejected. The client-owned generation remains stable while
    // a refresh legitimately rotates both access token and session ID, unlike
    // either server credential; a successor login always starts a new one.
    const liveAuth = useAuthStore.getState();
    const stillOwnsStore = liveAuth.authGeneration === flowAuthGeneration;
    if (!stillOwnsStore) {
      await abortForLostOwnership();
    }

    // A successful admit clears any abort notice a torn-down EARLIER flow
    // staged while this login was completing (Codex P1, PR #2337): the user
    // is in the app now, and a surviving notice would resurface on a much
    // later Login mount as if THAT sign-in had just failed.
    useAuthStore.getState().setLoginNotice(null);

    onSuccess({
      // The LIVE token, not data.access_token: when ownership was established
      // via session_id, a mid-flight reactive refresh may have legitimately
      // rotated the token. AuthFlow writes this value back into authStore, so
      // passing the original would clobber the refreshed credential and leave
      // the session holding an expired token inside the refresh cooldown
      // (Codex P1, #2337).
      accessToken: useAuthStore.getState().accessToken ?? data.access_token,
      user: data.user,
      rememberMe: data.remember_me ?? formData.rememberMe,
    });
  };

  const handleMFAVerify = async (code: string, method: string) => {
    setIsSubmitting(true);
    setMfaError('');
    try {
      if (mfaServerSelection === null) throw new Error('MFA challenge origin is unavailable.');
      const requestSelection = mfaServerSelection;
      const requestApiBase = requestSelection.apiBase;
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        throw new LoginOriginChangedError();
      }
      const machineId = await ensureMachineId(requestApiBase);
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        throw new LoginOriginChangedError();
      }
      const res = await fetch(`${requestApiBase}/api/v1/auth/mfa/verify`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(machineId ? { 'X-Machine-Id': machineId } : {}),
        },
        credentials: 'include',
        body: JSON.stringify({
          mfa_challenge_token: mfaChallengeToken,
          method,
          code,
        }),
      });

      const data = await parseLoginResponseJson(res, requestApiBase);
      if (!res.ok) throw new Error(responseError(data, 'Verification failed'));
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        await revokeMalformedLoginSession(data, requestApiBase, responseIssuedSessionID(res));
        throw new LoginOriginChangedError();
      }

      // MFA verify returns full login response (tokens + user + keys)
      await completeLoginFromResponse(
        await parseLoginSuccessResponse(data, requestApiBase, responseIssuedSessionID(res)),
        requestSelection
      );
    } catch (err) {
      setMfaError(describeLoginError(err, 'Verification failed'));
    } finally {
      setIsSubmitting(false);
    }
  };

  const handleChange = (field: keyof FormData) => (e: React.ChangeEvent<HTMLInputElement>) => {
    const value = e.target.type === 'checkbox' ? e.target.checked : e.target.value;
    setFormData((prev) => ({ ...prev, [field]: value }));
    // Clear error for this field when user starts typing
    if (field in errors && errors[field as keyof FormErrors]) {
      setErrors((prev) => ({ ...prev, [field]: undefined }));
    }
  };

  const available = useMemo(
    () => getAvailableCategories(mfaMethods, mfaRecoveryOnly),
    [mfaMethods, mfaRecoveryOnly]
  );
  const hasMultipleMethods = available.length > 1;
  const defaultMethod = useMemo(
    () => getDefaultMethod(mfaMethods, mfaRecoveryOnly),
    [mfaMethods, mfaRecoveryOnly]
  );

  const handleWebAuthnSuccess = async (credential: Credential) => {
    const pkc = credential as PublicKeyCredential;
    const response = pkc.response as AuthenticatorAssertionResponse;

    // Encode ArrayBuffers as base64url (no padding) for the go-webauthn library
    const toBase64url = (buf: ArrayBuffer): string => {
      const bytes = new Uint8Array(buf);
      let binary = '';
      for (const byte of bytes) binary += String.fromCodePoint(byte);
      return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replaceAll(/=+$/g, '');
    };

    const assertion = {
      id: pkc.id,
      rawId: toBase64url(pkc.rawId),
      type: pkc.type,
      response: {
        authenticatorData: toBase64url(response.authenticatorData),
        clientDataJSON: toBase64url(response.clientDataJSON),
        signature: toBase64url(response.signature),
        userHandle: response.userHandle ? toBase64url(response.userHandle) : null,
      },
    };

    setIsSubmitting(true);
    setMfaError('');
    try {
      if (mfaServerSelection === null) throw new Error('MFA challenge origin is unavailable.');
      const requestSelection = mfaServerSelection;
      const requestApiBase = requestSelection.apiBase;
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        throw new LoginOriginChangedError();
      }
      const machineId = await ensureMachineId(requestApiBase);
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        throw new LoginOriginChangedError();
      }
      const res = await fetch(`${requestApiBase}/api/v1/auth/mfa/verify`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(machineId ? { 'X-Machine-Id': machineId } : {}),
        },
        credentials: 'include',
        body: JSON.stringify({
          mfa_challenge_token: mfaChallengeToken,
          method: 'webauthn',
          assertion,
        }),
      });
      const data = await parseLoginResponseJson(res, requestApiBase);
      if (!res.ok) throw new Error(responseError(data, 'Verification failed'));
      if (!runtimeServerSelectionIsCurrent(requestSelection)) {
        await revokeMalformedLoginSession(data, requestApiBase, responseIssuedSessionID(res));
        throw new LoginOriginChangedError();
      }
      await completeLoginFromResponse(
        await parseLoginSuccessResponse(data, requestApiBase, responseIssuedSessionID(res)),
        requestSelection
      );
    } catch (err) {
      setMfaError(describeLoginError(err, 'Verification failed'));
    } finally {
      setIsSubmitting(false);
    }
  };

  let mfaSubtitle: string;
  if (mfaMode === 'totp') mfaSubtitle = 'Enter the 6-digit code from your authenticator app';
  else if (mfaMode === 'backup') mfaSubtitle = 'Enter one of your backup codes';
  else if (mfaMode === 'webauthn') mfaSubtitle = 'Use your security key or biometrics';
  else if (mfaMode === 'email-sms') mfaSubtitle = 'Enter the verification code sent to you';
  else mfaSubtitle = 'Select a verification method';

  // MFA Step UI
  if (mfaRequired) {
    return (
      <div className="login-container">
        <div className="login-content">
          <div className="login-header">
            <img
              src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
              className="login-logo"
              alt="Concord Voice"
            />
            <h2 className="login-title">Two-Factor Authentication</h2>
            <p className="login-subtitle">{mfaSubtitle}</p>
          </div>

          <div className="login-form">
            {mfaMode === 'method-select' && (
              <MFAMethodPicker
                methods={mfaMethods}
                excludeMethods={mfaRecoveryOnly}
                currentMethod={defaultMethod}
                onSelect={(method) => {
                  setMfaMode(method);
                  setMfaError('');
                }}
                onCancel={() => {
                  setMfaMode(defaultMethod);
                  setMfaError('');
                }}
              />
            )}

            {mfaMode === 'totp' && (
              <TOTPInput
                onSubmit={(code) => handleMFAVerify(code, 'totp')}
                disabled={isSubmitting}
                error={mfaError}
              />
            )}

            {mfaMode === 'backup' && (
              <BackupCodeInput
                onSubmit={(code) => handleMFAVerify(code, 'backup_code')}
                disabled={isSubmitting}
                error={mfaError}
              />
            )}

            {mfaMode === 'webauthn' && (
              <>
                {webauthnOptions ? (
                  <WebAuthnPrompt
                    requestOptions={webauthnOptions}
                    onSuccess={handleWebAuthnSuccess}
                    onError={(msg) => setMfaError(msg)}
                    onCancel={() => {
                      setMfaMode('method-select');
                      setMfaError('');
                    }}
                  />
                ) : (
                  <div style={{ textAlign: 'center' }}>
                    <p style={{ color: 'var(--text-secondary, #bbb)', fontSize: 14 }}>
                      WebAuthn verification will be triggered by the server challenge.
                    </p>
                    {mfaError && <p className="totp-error">{mfaError}</p>}
                  </div>
                )}
              </>
            )}

            {mfaMode === 'email-sms' && (
              <TOTPInput
                onSubmit={(code) =>
                  handleMFAVerify(code, mfaMethods.includes('email') ? 'email' : 'sms')
                }
                disabled={isSubmitting}
                error={mfaError}
              />
            )}

            {/* "Choose another form" link */}
            {mfaMode !== 'method-select' && hasMultipleMethods && (
              <button
                type="button"
                className="mfa-choose-another"
                onClick={() => {
                  setMfaMode('method-select');
                  setMfaError('');
                }}
                disabled={isSubmitting}
              >
                Choose another form of verification
              </button>
            )}

            {isSubmitting && (
              <div style={{ display: 'flex', justifyContent: 'center', marginTop: 12 }}>
                <LoadingSpinner size="small" inline />
              </div>
            )}

            <button
              type="button"
              className="login-back-btn"
              onClick={() => {
                setMfaRequired(false);
                setMfaError('');
                setMfaServerSelection(null);
              }}
              disabled={isSubmitting}
            >
              ← Back to login
            </button>
          </div>
        </div>
      </div>
    );
  }

  // SSO-only account: the user logged in with a password some time ago, but
  // their account is now linked to one or more SSO providers and password
  // sign-in is disabled. Show the relevant provider button(s) instead of the
  // password form.
  if (ssoOnlyProviders) {
    // Empty providers array is the impossible-state case: backend says
    // password is disabled, but listSSOProviders returned no rows. Render
    // an explicit error message rather than an empty page that looks like
    // a UI bug.
    const isEmpty = ssoOnlyProviders.length === 0;
    return (
      <div className="login-container">
        <div className="login-content">
          <div className="login-header">
            <img
              src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
              className="login-logo"
              alt="Concord Voice"
            />
            <h2 className="login-title">Welcome Back</h2>
            <p className="login-subtitle">
              {isEmpty
                ? "We couldn't load your sign-in options. Please try again in a moment or contact support."
                : 'This account uses Single Sign-On. Continue with the provider you originally linked.'}
            </p>
          </div>

          <div className="login-form login-form--sso-only">
            {!isEmpty && ssoOnlyProviders.includes('google') && (
              <SSOButton provider="google" onClick={() => beginSSO('google')} disabled={authBusy} />
            )}
            {!isEmpty && ssoOnlyProviders.includes('apple') && (
              <SSOButton provider="apple" onClick={() => beginSSO('apple')} disabled={authBusy} />
            )}

            <button
              type="button"
              className="login-back-btn"
              onClick={() => setSsoOnlyProviders(null)}
              disabled={authBusy}
            >
              ← Back to login
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="login-container">
      {keyRecoveryResolver && (
        <KeyRecoveryPrompt
          mfaRequired={keyRecoveryMfaRequired}
          onReset={(mfaCode) => keyRecoveryResolver({ action: 'reset', mfaCode })}
          onCancel={() => keyRecoveryResolver({ action: 'cancel' })}
        />
      )}
      <div className="login-content">
        <div className="login-header">
          <img
            src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
            className="login-logo"
            alt="Concord Voice"
          />
          <h2 className="login-title">Welcome Back</h2>
          <p className="login-subtitle">Sign in to your Concord Voice account</p>
        </div>

        {/* SSO entry point — sits above the password form. Clicking begins the
            loopback OAuth flow; subsequent UI is driven by useSSOStore state
            (rendered by AuthFlow once Task 21 wires it). Apple SSO (#271)
            lives next to Google to satisfy App Store policy parity (mobile
            clients #205 require both when either is offered). */}
        {hasDefaultSSO && (
          <div className="login-sso-row">
            {showGoogleSSO && (
              <SSOButton provider="google" onClick={() => beginSSO('google')} disabled={authBusy} />
            )}
            {showAppleSSO && (
              <SSOButton provider="apple" onClick={() => beginSSO('apple')} disabled={authBusy} />
            )}
          </div>
        )}
        {hasDefaultSSO && (
          <div className="login-divider" role="separator" aria-label="or sign in with email">
            <span className="login-divider__text">or</span>
          </div>
        )}

        <form className="login-form" onSubmit={handleSubmit}>
          {/* Email */}
          <div className="form-group">
            <label htmlFor="login-email" className="form-label">
              Email
            </label>
            <input
              id="login-email"
              type="email"
              className={`form-input ${errors.email ? 'error' : ''}`}
              placeholder="you@example.com"
              value={formData.email}
              onChange={handleChange('email')}
              disabled={authBusy}
              autoFocus
            />
            {errors.email && <span className="form-error">{errors.email}</span>}
          </div>

          {/* Password */}
          <PasswordField
            value={formData.password}
            onChange={handleChange('password')}
            disabled={authBusy}
            error={errors.password}
          />

          {/* Remember Me & Forgot Password */}
          <div className="login-options">
            <label className="remember-me-label">
              <input
                type="checkbox"
                checked={formData.rememberMe}
                onChange={handleChange('rememberMe')}
                disabled={authBusy}
              />
              <span>Remember me</span>
            </label>
            <button
              type="button"
              className="forgot-password-link"
              onClick={onForgotPassword}
              disabled={authBusy}
            >
              Forgot password?
            </button>
          </div>

          {/* General Error */}
          {errors.general && (
            <div className="form-error-banner" role="alert">
              <span>{errors.general}</span>
            </div>
          )}

          {/* Submit Button */}
          <button type="submit" className="login-submit-btn" disabled={authBusy}>
            {isSubmitting ? (
              <>
                Signing In...
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              'Sign In'
            )}
          </button>

          {/* Back Button */}
          <button type="button" className="login-back-btn" onClick={onBack} disabled={authBusy}>
            ← Back to Connection Options
          </button>
        </form>

        <div className="login-footer">
          <p className="footer-text">
            Don&apos;t have an account?{' '}
            <button
              className="switch-to-register-btn"
              onClick={onSwitchToRegister}
              disabled={authBusy}
            >
              Create one
            </button>
          </p>
        </div>
      </div>
    </div>
  );
};

export default Login;
