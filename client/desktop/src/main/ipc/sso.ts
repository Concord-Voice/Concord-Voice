/**
 * SSO IPC bridge — wires the loopback HTTP server in `ssoLoopback.ts` to the
 * renderer's OAuth flow.
 *
 * Channel surface (10):
 *   - `sso:startLoopback` (invoke)  — spin up a 127.0.0.1 ephemeral-port server
 *                                     and return `{port, redirectURI}` for the
 *                                     renderer to embed in the provider URL.
 *   - `sso:awaitCallback` (invoke)  — await the captured `{code, state}` for a
 *                                     previously started loopback (keyed by
 *                                     port, since the renderer already knows it).
 *   - `sso:cancelLoopback` (send)   — best-effort tear-down for a port the
 *                                     renderer abandoned (user closed the
 *                                     auth tab, switched flows, etc).
 *   - `sso:appleSignIn` (invoke)    — run the FULL client-driven Apple flow
 *                                     (#974) in the main process: PKCE,
 *                                     loopback, broker client_secret, Apple
 *                                     /auth/token, local jose verification,
 *                                     /session POST. Resolves to the
 *                                     discriminated AppleSignInResult; no
 *                                     OAuth material crosses IPC.
 *   - `sso:appleCancel` (send)      — tear down the in-flight Apple flow.
 *   - `sso:googleSignIn` (invoke)   — run the FULL client-driven Google flow
 *                                     (#975): PKCE, loopback, Google /token with
 *                                     the embedded client_secret, local jose
 *                                     verification, /session POST. Resolves to
 *                                     SSOSignInResult; no OAuth material crosses
 *                                     IPC. Simpler than Apple — no broker.
 *   - `sso:googleCancel` (send)     — tear down the in-flight Google flow.
 *   - `sso:completeRegistration` / `sso:completeLink` (invoke) — submit the
 *                                     final server exchange in main, store the
 *                                     refresh credential under the reserved
 *                                     SSO owner, and return no refresh token.
 *   - `sso:completeMFA` (invoke)    — submit the SSO MFA proof (#2424) in main,
 *                                     store the refresh credential under the
 *                                     owner reserved at sign-in, and return no
 *                                     refresh token. Keeps the MFA path at parity
 *                                     with the direct/registration custody model.
 *
 * Sender-frame validation (the only layer the renderer cannot bypass) is
 * enforced via `isPermittedFrameUrl` — the same helper `openExternal.ts` uses,
 * which keeps the trust boundary consistent across IPC handlers. An untrusted
 * frame causes the invoke handlers to throw (renderer sees a rejected promise)
 * and the send handler to silently no-op.
 *
 * Per-port handle accounting lets `sso:cancelLoopback` and `sso:awaitCallback`
 * find the running server. We also auto-clean the entry once the loopback
 * promise settles — if the renderer never calls awaitCallback the active map
 * does not leak forever.
 */
import { ipcMain, net, shell, type IpcMainEvent, type IpcMainInvokeEvent } from 'electron';

import { getApiBaseUrl, PRODUCTION_API_BASE } from '../apiBaseUrl';
import { getMachineId } from '../machineId';
import { cancelActiveAppleFlow, runAppleSignIn } from '../oauth/apple/appleFlow';
import { appleTokenCall } from '../oauth/apple/appleTokenCall';
import { verifyAppleIDToken } from '../oauth/apple/idTokenVerifier';
import { signClientSecret } from '../oauth/apple/signClientSecret';
import { loadGoogleClientSecret } from '../oauth/google/clientSecret';
import { cancelActiveGoogleFlow, runGoogleSignIn } from '../oauth/google/googleFlow';
import { googleTokenCall } from '../oauth/google/googleTokenCall';
import { verifyGoogleIDToken } from '../oauth/google/idTokenVerifier';
import { normalizeSelfHostedUrl } from '../selfHostedProbe';
import { isValidatedSelfHostedApiBase } from '../selfHostedProfile';
import { startLoopback, type LoopbackHandle } from '../ssoLoopback';
import {
  clearTokensIfOwner,
  credentialOwnerIsCurrent,
  reserveCredentialOwner,
  storeRefreshTokenIfOwner,
} from '../tokenManager';

import type { MainSSOSignInResult } from '../oauth/ssoFlowShared';
import type { CredentialOwner } from '../ipcContract';
import type {
  SSOCompleteLinkPayload,
  SSOCompleteMFAPayload,
  SSOCompleteRegistrationPayload,
  SSOCompletionErrorBody,
  SSOCompletionResult,
  SSOSignInResult,
} from '../../shared/sso';

import { isPermittedFrameUrl } from './frameValidation';

type RemoteSpaOriginProvider = () => string | null;

/**
 * Resolves any fetch-input shape to its URL string — String(input) on a
 * Request would coerce to '[object Request]' (sonar S6551). Typed via
 * Parameters<typeof fetch>[0] instead of naming RequestInfo: the
 * main-process build (tsconfig.main.json) has no DOM lib, so the
 * RequestInfo alias doesn't exist there — CI's build:main rejects it
 * while the root tsconfig's typecheck accepts it.
 */
function resolveFetchUrl(input: Parameters<typeof fetch>[0]): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.href;
  return input.url;
}

const active = new Map<number, LoopbackHandle>();
const SESSION_ID_HEADER = 'X-Concord-Session-ID';
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

interface PendingSSOCompletion {
  apiBase: string;
  credentialOwner: CredentialOwner;
  provider: 'google' | 'apple';
  branch: 'new_user' | 'account_link';
  ssoToken: string;
}

// #2424: the reserved owner + challenge token held across an SSO MFA challenge.
// The renderer collects the MFA proof (TOTP code or WebAuthn assertion) and calls
// sso:completeMFA; main re-validates this state, submits the proof, and stores the
// resulting refresh credential under `credentialOwner` — the raw refresh token
// never crosses back to the renderer (parity with the direct SSO path).
interface PendingSSOMFA {
  apiBase: string;
  credentialOwner: CredentialOwner;
  provider: 'google' | 'apple';
  mfaChallengeToken: string;
}

interface MainCompletionTokens {
  accessToken: string;
  refreshToken: string;
  sessionId: string;
  rememberMe: boolean;
}

// Google's client_secret is a non-confidential build constant (Google's
// native-app guidance — PKCE is the control), read once from the
// main-process-only resource. Never IPC-exposed to the renderer.
let cachedGoogleClientSecret: string | undefined;
function googleClientSecret(): string {
  cachedGoogleClientSecret ??= loadGoogleClientSecret();
  return cachedGoogleClientSecret;
}

/** Validates the IPC sender frame matches a permitted SPA origin. */
function checkFrame(
  event: IpcMainInvokeEvent | IpcMainEvent,
  getSpaBaseUrl: RemoteSpaOriginProvider
): boolean {
  const url = event.senderFrame?.url ?? '';
  return isPermittedFrameUrl(url, getSpaBaseUrl());
}

function approvedApiBase(value: unknown): string | null {
  if (typeof value !== 'string') return null;
  const normalized = normalizeSelfHostedUrl(value);
  if (!normalized.ok || normalized.apiBase !== value) return null;
  return value === PRODUCTION_API_BASE ||
    value === getApiBaseUrl() ||
    isValidatedSelfHostedApiBase(value)
    ? value
    : null;
}

function isBoundedString(value: unknown, maxLength: number): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= maxLength;
}

function isProvider(value: unknown): value is 'google' | 'apple' {
  return value === 'google' || value === 'apple';
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isCompleteRegistrationPayload(value: unknown): value is SSOCompleteRegistrationPayload {
  return (
    isRecord(value) &&
    isProvider(value.provider) &&
    isBoundedString(value.ssoToken, 8192) &&
    isBoundedString(value.username, 128) &&
    isBoundedString(value.passphrase, 1024) &&
    isBoundedString(value.wrappedPrivateKey, 16_384) &&
    isBoundedString(value.keyDerivationSalt, 1024) &&
    isBoundedString(value.publicKey, 8192)
  );
}

function isCompleteLinkPayload(value: unknown): value is SSOCompleteLinkPayload {
  return (
    isRecord(value) &&
    isProvider(value.provider) &&
    isBoundedString(value.ssoToken, 8192) &&
    isBoundedString(value.password, 1024)
  );
}

// #2424: the SSO MFA proof the renderer routes to sso:completeMFA. `credentialOwner`
// is the opaque number reserved at sign-in and echoed back through the mfa_challenge
// result; `code` (TOTP/backup) and `assertion` (WebAuthn) are mutually exclusive by
// method, but the control plane is the authority on which is required — this guard
// only bounds shapes at the trust boundary.
function isCompleteMFAPayload(value: unknown): value is SSOCompleteMFAPayload {
  return (
    isRecord(value) &&
    isProvider(value.provider) &&
    isBoundedString(value.mfaChallengeToken, 8192) &&
    typeof value.credentialOwner === 'number' &&
    Number.isSafeInteger(value.credentialOwner) &&
    isBoundedString(value.method, 32) &&
    (value.code === undefined || isBoundedString(value.code, 256)) &&
    (value.assertion === undefined || isRecord(value.assertion))
  );
}

function parseCompletionTokens(value: unknown): MainCompletionTokens | null {
  if (
    !isRecord(value) ||
    !isBoundedString(value.access_token, 32_768) ||
    !isBoundedString(value.refresh_token, 32_768) ||
    !isBoundedString(value.session_id, 256) ||
    !UUID_PATTERN.test(value.session_id) ||
    (value.remember_me !== undefined && typeof value.remember_me !== 'boolean')
  ) {
    return null;
  }
  return {
    accessToken: value.access_token,
    refreshToken: value.refresh_token,
    sessionId: value.session_id,
    rememberMe: value.remember_me ?? true,
  };
}

function sanitizeCompletionErrorBody(value: unknown): SSOCompletionErrorBody | undefined {
  if (!isRecord(value)) return undefined;
  const body: SSOCompletionErrorBody = {};
  if (isBoundedString(value.error_code, 256)) body.error_code = value.error_code;
  if (isBoundedString(value.error, 2048)) body.error = value.error;
  if (isBoundedString(value.detail, 4096)) body.detail = value.detail;
  if (
    typeof value.attempts_remaining === 'number' &&
    Number.isSafeInteger(value.attempts_remaining)
  ) {
    body.attempts_remaining = value.attempts_remaining;
  }
  if (
    typeof value.retry_after_seconds === 'number' &&
    Number.isSafeInteger(value.retry_after_seconds)
  ) {
    body.retry_after_seconds = value.retry_after_seconds;
  }
  return Object.keys(body).length > 0 ? body : undefined;
}

async function responseJson(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    return null;
  }
}

async function revokeExplicitSession(apiBase: string, tokens: MainCompletionTokens): Promise<void> {
  try {
    await net.fetch(`${apiBase}/api/v1/auth/logout`, {
      method: 'POST',
      credentials: 'omit',
      headers: {
        Authorization: `Bearer ${tokens.accessToken}`,
        'X-Refresh-Token': tokens.refreshToken,
      },
    });
  } catch {
    // Best effort; the server-side expiry remains the cleanup backstop.
  }
}

async function revokeCookieBoundSession(apiBase: string, sessionId: string | null): Promise<void> {
  if (!sessionId || !UUID_PATTERN.test(sessionId)) return;
  try {
    await net.fetch(`${apiBase}/api/v1/auth/logout`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'X-Session-ID': sessionId },
    });
  } catch {
    // Best effort; the session ID + cookie hash match prevents successor revoke.
  }
}

function captureSessionId(response: Response): string | null {
  const sessionId = response.headers.get(SESSION_ID_HEADER);
  return sessionId && UUID_PATTERN.test(sessionId) ? sessionId : null;
}

/**
 * registerSSOIPC wires the three sso:* IPC channels used by the renderer's
 * SSO flow. The Electron main process owns the ephemeral loopback HTTP
 * server; the renderer drives the OAuth provider redirect via the system
 * browser and then awaits the captured code+state via awaitCallback.
 *
 * `getSpaBaseUrl` returns the currently-active validated remote SPA origin
 * (or null when the bundled build is loaded). The accessor is injected so
 * main.ts can wire the live value once the SPA loader has decided which
 * build to serve.
 */
export function registerSSOIPC(getSpaBaseUrl: RemoteSpaOriginProvider): void {
  let pendingCompletion: PendingSSOCompletion | null = null;
  const discardPendingCompletion = (completion: PendingSSOCompletion): void => {
    if (pendingCompletion === completion) pendingCompletion = null;
  };
  // #2424: the reserved owner + challenge token held across an SSO MFA challenge,
  // so sso:completeMFA can conditionally store the MFA credential under that exact
  // owner. Cleared on a new sign-in, on completion, and on owner supersession.
  let pendingMFA: PendingSSOMFA | null = null;
  const discardPendingMFA = (mfa: PendingSSOMFA): void => {
    if (pendingMFA === mfa) pendingMFA = null;
  };

  const finishSignIn = async (
    result: MainSSOSignInResult,
    apiBase: string,
    provider: 'google' | 'apple',
    credentialOwner: CredentialOwner,
    cookieBoundSessionId: string | null
  ): Promise<SSOSignInResult> => {
    if (!credentialOwnerIsCurrent(credentialOwner)) {
      if (result.kind === 'tokens') {
        await revokeCookieBoundSession(apiBase, cookieBoundSessionId);
        await revokeExplicitSession(apiBase, { ...result, rememberMe: true });
      } else {
        await revokeCookieBoundSession(apiBase, cookieBoundSessionId);
      }
      return { kind: 'error', code: 'sso_cancelled' };
    }

    if (result.kind === 'tokens') {
      const storedOwner = storeRefreshTokenIfOwner(
        {
          refreshToken: result.refreshToken,
          rememberMe: true,
          apiBase,
          accessToken: result.accessToken,
        },
        credentialOwner
      );
      if (storedOwner === null) {
        await revokeCookieBoundSession(apiBase, cookieBoundSessionId);
        await revokeExplicitSession(apiBase, { ...result, rememberMe: true });
        return { kind: 'error', code: 'sso_cancelled' };
      }
      pendingCompletion = null;
      return {
        kind: 'tokens',
        accessToken: result.accessToken,
        sessionId: result.sessionId,
        credentialOwner: storedOwner,
      };
    }

    if (result.kind === 'sso_token') {
      pendingCompletion = {
        apiBase,
        credentialOwner,
        provider,
        branch: result.branch,
        ssoToken: result.ssoToken,
      };
      return result;
    }

    if (result.kind === 'mfa_challenge') {
      // #2424: preserve the reserved owner across the MFA challenge and return it
      // to the renderer as opaque challenge context. sso:completeMFA re-validates
      // it and stores the resulting credential owner-scoped, keeping the raw
      // refresh token out of the renderer. Do NOT release the owner here — a
      // released owner would force the renderer onto the unrestricted
      // auth:storeRefreshToken path (the owner-CAS gap this change closes).
      pendingMFA = {
        apiBase,
        credentialOwner,
        provider,
        mfaChallengeToken: result.mfaChallengeToken,
      };
      return { ...result, credentialOwner };
    }

    // Terminal error (the only remaining kind): revoke the cookie-bound session
    // and release this reservation so switching to registration can safely use
    // the pre-credential key staging path.
    await revokeCookieBoundSession(apiBase, cookieBoundSessionId);
    clearTokensIfOwner(credentialOwner);
    return result;
  };

  const controlPlaneFetch = (
    sessionUrl: string,
    onSession: (sessionId: string | null) => void
  ): typeof fetch =>
    (async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
      const url = resolveFetchUrl(input);
      const response = await net.fetch(url, { ...init, credentials: 'include' });
      if (url === sessionUrl && response.ok) onSession(captureSessionId(response));
      return response;
    }) as typeof fetch;

  const completeSSO = async (
    completion: PendingSSOCompletion,
    endpoint: 'complete-registration' | 'complete-link',
    requestBody: Record<string, string>
  ): Promise<SSOCompletionResult> => {
    if (!credentialOwnerIsCurrent(completion.credentialOwner)) {
      discardPendingCompletion(completion);
      return { kind: 'error', status: 409, code: 'sso_cancelled' };
    }

    let response: Response;
    try {
      response = await net.fetch(
        `${completion.apiBase}/api/v1/auth/sso/${completion.provider}/${endpoint}`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify(requestBody),
        }
      );
    } catch {
      return { kind: 'error', status: 0, code: `sso_${endpoint}_failed` };
    }

    const cookieBoundSessionId = captureSessionId(response);
    const body = await responseJson(response);
    if (!response.ok) {
      await revokeCookieBoundSession(completion.apiBase, cookieBoundSessionId);
      const safeBody = sanitizeCompletionErrorBody(body);
      return {
        kind: 'error',
        status: response.status,
        code: safeBody?.error_code ?? `sso_${endpoint}_failed`,
        ...(safeBody ? { body: safeBody } : {}),
      };
    }

    const tokens = parseCompletionTokens(body);
    if (!tokens) {
      await revokeCookieBoundSession(completion.apiBase, cookieBoundSessionId);
      discardPendingCompletion(completion);
      clearTokensIfOwner(completion.credentialOwner);
      return { kind: 'error', status: 502, code: 'sso_session_rejected' };
    }

    const storedOwner = storeRefreshTokenIfOwner(
      {
        refreshToken: tokens.refreshToken,
        rememberMe: tokens.rememberMe,
        apiBase: completion.apiBase,
        accessToken: tokens.accessToken,
      },
      completion.credentialOwner
    );
    if (storedOwner === null) {
      await revokeCookieBoundSession(completion.apiBase, cookieBoundSessionId);
      await revokeExplicitSession(completion.apiBase, tokens);
      discardPendingCompletion(completion);
      return { kind: 'error', status: 409, code: 'sso_cancelled' };
    }

    pendingCompletion = null;
    return {
      kind: 'tokens',
      accessToken: tokens.accessToken,
      sessionId: tokens.sessionId,
      credentialOwner: storedOwner,
    };
  };

  // #2424: submit the SSO MFA proof to the control plane IN MAIN and store the
  // resulting refresh credential under the reserved owner, so the raw refresh
  // token never crosses into the renderer (parity with completeSSO / the direct
  // SSO path). Mirrors completeSSO's owner-CAS + fail-closed shape.
  const completeMFA = async (
    mfa: PendingSSOMFA,
    requestBody: Record<string, unknown>
  ): Promise<SSOCompletionResult> => {
    if (!credentialOwnerIsCurrent(mfa.credentialOwner)) {
      discardPendingMFA(mfa);
      return { kind: 'error', status: 409, code: 'sso_cancelled' };
    }

    let response: Response;
    try {
      response = await net.fetch(`${mfa.apiBase}/api/v1/auth/mfa/verify`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          // Same device-binding header the renderer sent pre-#2424; main now
          // owns the POST, so it supplies the machine id for this apiBase.
          'X-Machine-Id': getMachineId(mfa.apiBase),
        },
        credentials: 'include',
        body: JSON.stringify(requestBody),
      });
    } catch {
      return { kind: 'error', status: 0, code: 'sso_mfa_verify_failed' };
    }

    const cookieBoundSessionId = captureSessionId(response);
    const body = await responseJson(response);
    if (!response.ok) {
      await revokeCookieBoundSession(mfa.apiBase, cookieBoundSessionId);
      const safeBody = sanitizeCompletionErrorBody(body);
      return {
        kind: 'error',
        status: response.status,
        code: safeBody?.error_code ?? 'sso_mfa_verify_failed',
        ...(safeBody ? { body: safeBody } : {}),
      };
    }

    const tokens = parseCompletionTokens(body);
    if (!tokens) {
      await revokeCookieBoundSession(mfa.apiBase, cookieBoundSessionId);
      discardPendingMFA(mfa);
      clearTokensIfOwner(mfa.credentialOwner);
      return { kind: 'error', status: 502, code: 'sso_session_rejected' };
    }

    const storedOwner = storeRefreshTokenIfOwner(
      {
        refreshToken: tokens.refreshToken,
        rememberMe: tokens.rememberMe,
        apiBase: mfa.apiBase,
        accessToken: tokens.accessToken,
      },
      mfa.credentialOwner
    );
    if (storedOwner === null) {
      await revokeCookieBoundSession(mfa.apiBase, cookieBoundSessionId);
      await revokeExplicitSession(mfa.apiBase, tokens);
      discardPendingMFA(mfa);
      return { kind: 'error', status: 409, code: 'sso_cancelled' };
    }

    discardPendingMFA(mfa);
    return {
      kind: 'tokens',
      accessToken: tokens.accessToken,
      sessionId: tokens.sessionId,
      credentialOwner: storedOwner,
    };
  };

  ipcMain.handle('sso:startLoopback', async (event) => {
    if (!checkFrame(event, getSpaBaseUrl)) {
      throw new Error('sso:startLoopback rejected: untrusted sender frame');
    }
    const handle = await startLoopback();
    active.set(handle.port, handle);
    // Auto-cleanup if the promise settles outside an awaitCallback call —
    // e.g. the loopback timed out before the renderer wired up the await,
    // or a stale entry from a flow the renderer abandoned without sending
    // sso:cancelLoopback. Without this the active map would leak forever.
    handle.promise
      .catch(() => {})
      .finally(() => {
        active.delete(handle.port);
      });
    return { port: handle.port, redirectURI: handle.redirectURI };
  });

  ipcMain.handle('sso:awaitCallback', async (event, port: number) => {
    if (!checkFrame(event, getSpaBaseUrl)) {
      throw new Error('sso:awaitCallback rejected: untrusted sender frame');
    }
    const handle = active.get(port);
    if (!handle) throw new Error('sso:awaitCallback: unknown port');
    try {
      return await handle.promise;
    } finally {
      active.delete(port);
    }
  });

  ipcMain.on('sso:cancelLoopback', (event, port: number) => {
    if (!checkFrame(event, getSpaBaseUrl)) return;
    const handle = active.get(port);
    if (!handle) return;
    handle.close();
    active.delete(port);
  });

  ipcMain.handle(
    'sso:completeRegistration',
    async (event, requestedApiBase: unknown, payload: unknown): Promise<SSOCompletionResult> => {
      if (!checkFrame(event, getSpaBaseUrl)) {
        throw new Error('sso:completeRegistration rejected: untrusted sender frame');
      }
      const apiBase = approvedApiBase(requestedApiBase);
      if (!apiBase || !isCompleteRegistrationPayload(payload)) {
        return { kind: 'error', status: 400, code: 'sso_invalid_request' };
      }
      const completion = pendingCompletion;
      if (
        !completion ||
        completion.apiBase !== apiBase ||
        completion.provider !== payload.provider ||
        completion.branch !== 'new_user' ||
        completion.ssoToken !== payload.ssoToken
      ) {
        return { kind: 'error', status: 409, code: 'sso_cancelled' };
      }
      return completeSSO(completion, 'complete-registration', {
        sso_token: payload.ssoToken,
        username: payload.username,
        password: payload.passphrase,
        wrapped_private_key: payload.wrappedPrivateKey,
        key_derivation_salt: payload.keyDerivationSalt,
        public_key: payload.publicKey,
      });
    }
  );

  ipcMain.handle(
    'sso:completeLink',
    async (event, requestedApiBase: unknown, payload: unknown): Promise<SSOCompletionResult> => {
      if (!checkFrame(event, getSpaBaseUrl)) {
        throw new Error('sso:completeLink rejected: untrusted sender frame');
      }
      const apiBase = approvedApiBase(requestedApiBase);
      if (!apiBase || !isCompleteLinkPayload(payload)) {
        return { kind: 'error', status: 400, code: 'sso_invalid_request' };
      }
      const completion = pendingCompletion;
      if (
        !completion ||
        completion.apiBase !== apiBase ||
        completion.provider !== payload.provider ||
        completion.branch !== 'account_link' ||
        completion.ssoToken !== payload.ssoToken
      ) {
        return { kind: 'error', status: 409, code: 'sso_cancelled' };
      }
      return completeSSO(completion, 'complete-link', {
        sso_token: payload.ssoToken,
        password: payload.password,
      });
    }
  );

  ipcMain.handle(
    'sso:completeMFA',
    async (event, requestedApiBase: unknown, payload: unknown): Promise<SSOCompletionResult> => {
      if (!checkFrame(event, getSpaBaseUrl)) {
        throw new Error('sso:completeMFA rejected: untrusted sender frame');
      }
      const apiBase = approvedApiBase(requestedApiBase);
      if (!apiBase || !isCompleteMFAPayload(payload)) {
        return { kind: 'error', status: 400, code: 'sso_invalid_request' };
      }
      const mfa = pendingMFA;
      if (!mfa) {
        return { kind: 'error', status: 409, code: 'sso_cancelled' };
      }
      if (
        mfa.apiBase !== apiBase ||
        mfa.provider !== payload.provider ||
        mfa.credentialOwner !== payload.credentialOwner ||
        mfa.mfaChallengeToken !== payload.mfaChallengeToken
      ) {
        return { kind: 'error', status: 409, code: 'sso_cancelled' };
      }
      const requestBody: Record<string, unknown> = {
        mfa_challenge_token: payload.mfaChallengeToken,
        method: payload.method,
      };
      if (payload.code !== undefined) requestBody.code = payload.code;
      if (payload.assertion !== undefined) requestBody.assertion = payload.assertion;
      return completeMFA(mfa, requestBody);
    }
  );

  ipcMain.handle('sso:appleSignIn', async (event, requestedApiBase: unknown) => {
    if (!checkFrame(event, getSpaBaseUrl)) {
      throw new Error('sso:appleSignIn rejected: untrusted sender frame');
    }
    const apiBase = approvedApiBase(requestedApiBase);
    if (!apiBase) {
      throw new Error('sso:appleSignIn rejected: unapproved API origin');
    }
    const credentialOwner = reserveCredentialOwner(apiBase);
    pendingCompletion = null;
    pendingMFA = null;
    let cookieBoundSessionId: string | null = null;
    const result = await runAppleSignIn({
      apiBase,
      // Electron net.fetch + credentials:'include' (NOT Node's fetch): the
      // /session response sets the refresh-token cookie, which must land in
      // the default-session jar — the SAME jar the renderer's /auth/refresh
      // reads. Node's fetch would silently drop it and strand the session at
      // access-token expiry (15 min). Plan deviation D2.
      controlPlaneFetch: controlPlaneFetch(
        `${apiBase}/api/v1/auth/sso/apple/session`,
        (sessionId) => {
          cookieBoundSessionId = sessionId;
        }
      ),
      // Apple endpoints are cookie-less by design — plain global fetch.
      appleFetch: fetch,
      openExternal: async (url: string) => {
        // Defense-in-depth: the authorize URL comes from our own server, but
        // an https-only gate keeps a tampered response from launching
        // arbitrary schemes (passive-nav posture, [internal]rules/electron.md
        // §External-link scheme policy).
        if (!url.startsWith('https://')) {
          throw new Error('sso_initiate_failed');
        }
        await shell.openExternal(url);
      },
      startLoopback,
      signClientSecret,
      appleTokenCall,
      verifyIdToken: verifyAppleIDToken,
    });
    return finishSignIn(result, apiBase, 'apple', credentialOwner, cookieBoundSessionId);
  });

  ipcMain.on('sso:appleCancel', (event) => {
    if (!checkFrame(event, getSpaBaseUrl)) return;
    cancelActiveAppleFlow();
  });

  ipcMain.handle('sso:googleSignIn', async (event, requestedApiBase: unknown) => {
    if (!checkFrame(event, getSpaBaseUrl)) {
      throw new Error('sso:googleSignIn rejected: untrusted sender frame');
    }
    const apiBase = approvedApiBase(requestedApiBase);
    if (!apiBase) {
      throw new Error('sso:googleSignIn rejected: unapproved API origin');
    }
    const credentialOwner = reserveCredentialOwner(apiBase);
    pendingCompletion = null;
    pendingMFA = null;
    let cookieBoundSessionId: string | null = null;
    const result = await runGoogleSignIn({
      apiBase,
      // Embedded non-confidential client_secret (Google native-app guidance);
      // client_id is NOT supplied here — googleFlow parses it from the
      // server-built authorize URL (sourced from the control-plane's
      // GOOGLE_CLIENT_ID config).
      clientSecret: googleClientSecret(),
      // Electron net.fetch + credentials:'include' so /session's refresh-token
      // cookie lands in the default-session jar the renderer reads (parity with
      // the Apple handler above).
      controlPlaneFetch: controlPlaneFetch(
        `${apiBase}/api/v1/auth/sso/google/session`,
        (sessionId) => {
          cookieBoundSessionId = sessionId;
        }
      ),
      // Google endpoints are cookie-less by design — plain global fetch.
      googleFetch: fetch,
      openExternal: async (url: string) => {
        if (!url.startsWith('https://')) {
          throw new Error('sso_initiate_failed');
        }
        await shell.openExternal(url);
      },
      startLoopback,
      googleTokenCall,
      verifyIdToken: verifyGoogleIDToken,
    });
    return finishSignIn(result, apiBase, 'google', credentialOwner, cookieBoundSessionId);
  });

  ipcMain.on('sso:googleCancel', (event) => {
    if (!checkFrame(event, getSpaBaseUrl)) return;
    cancelActiveGoogleFlow();
  });
}
