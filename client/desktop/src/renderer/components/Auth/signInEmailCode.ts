import { useCallback, useEffect, useRef, useState } from 'react';
import {
  runtimeServerSelectionIsCurrent,
  type RuntimeServerSelection,
} from '../../services/system/runtimeServerBase';
import {
  challengeEntryIsLive,
  pruneExpiredChallengeEntries,
} from '../../services/system/challengeIssuer';
import type { MFAMethodCategory } from './MFAMethodPicker';

/**
 * Sign-in email code delivery for the challenge-token surfaces: the login
 * page's two-factor step and `MFAChallengeModal`. `/auth/mfa/verify` answers an
 * email code only after this route has sent one for the same challenge, and
 * the route takes nothing but the challenge token, so every challenge purpose
 * calls it from the renderer. The server sends at most one code per challenge
 * and answers 429 for a second request, but releases that claim when storing
 * or delivering the code fails, so a failed send can be asked for again.
 */
export const SIGN_IN_EMAIL_CODE_PATH = '/api/v1/auth/mfa/email/send';

export const SIGN_IN_EMAIL_CODE_ERROR =
  "Couldn't send your email code. Check your connection and try again.";

// A send that never settles would leave the email panel with no error and no
// way to ask again. The signal also bounds reading the answer's body.
const SIGN_IN_EMAIL_CODE_TIMEOUT_MS = 30_000;

/**
 * How a send ended. `failed` is a network error, a timeout, a 5xx, or a 429
 * from the route's rate limiter: the limiter answers before the handler claims
 * the challenge's one send, and a 5xx gives that claim back, so asking again
 * can work. `refused` is any other 4xx, after which asking again cannot help.
 * That includes the handler's own 429, which means a code for this challenge
 * was already sent.
 */
export type SignInEmailCodeResult =
  { kind: 'sent' } | { kind: 'refused' | 'failed'; message: string };

// The answer's `field` text, or the generic message when there is none to read.
async function answerText(res: Response, field: 'error' | 'message'): Promise<string> {
  const body: unknown = await res.json().catch(() => null);
  const text: unknown = typeof body === 'object' && body !== null ? Reflect.get(body, field) : null;
  return typeof text === 'string' && text ? text : SIGN_IN_EMAIL_CODE_ERROR;
}

/**
 * Asks the server to email the code for a challenge. A failure carries the
 * message to show: the server's text, or a generic one when there is none to
 * read. Never rejects. The token is a bearer credential for its challenge, so
 * it travels only in the JSON body, never in the URL or a log.
 */
export async function requestSignInEmailCode(
  url: string,
  challengeToken: string
): Promise<SignInEmailCodeResult> {
  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ mfa_challenge_token: challengeToken }),
      // A send that timed out may still have reached the server. Asking again
      // then draws the 429 for a code already sent, which is final and true.
      signal: AbortSignal.timeout(SIGN_IN_EMAIL_CODE_TIMEOUT_MS),
    });
    if (res.ok) return { kind: 'sent' };
    // Only the rate limiter's 429 carries Retry-After, and its `message` (not
    // its `error`) names the wait.
    if (res.status === 429 && res.headers.get('Retry-After') !== null) {
      return { kind: 'failed', message: await answerText(res, 'message') };
    }
    const kind = res.status >= 500 ? 'failed' : 'refused';
    return { kind, message: await answerText(res, 'error') };
  } catch {
    // The request never reached the server, or it timed out.
    return { kind: 'failed', message: SIGN_IN_EMAIL_CODE_ERROR };
  }
}

// One entry per challenge token, kept for as long as the server keeps the
// challenge. A surface can unmount and mount again on one challenge (App
// renders the modal in the restoring tree and again in the main tree) and
// StrictMode replays every mount, so a guard one mounted instance held would
// ask twice. An entry stays while its send is out and once it is sent or
// refused; a `failed` answer removes it, since only then is asking again any
// use. An entry older than the challenge TTL counts as absent, and each new
// send prunes those, so the map holds only challenges that can still be live.
interface SendEntry {
  readonly answer: Promise<SignInEmailCodeResult>;
  readonly recordedAt: number;
}

const sends = new Map<string, SendEntry>();

function sendOnce(apiBase: string, challengeToken: string): Promise<SignInEmailCodeResult> {
  const now = Date.now();
  const existing = sends.get(challengeToken);
  if (existing && challengeEntryIsLive(existing.recordedAt, now)) return existing.answer;
  pruneExpiredChallengeEntries(sends, now);
  const answer: Promise<SignInEmailCodeResult> = requestSignInEmailCode(
    `${apiBase}${SIGN_IN_EMAIL_CODE_PATH}`,
    challengeToken
  ).then((result) => {
    // Only this send's own entry: a test reset may have replaced it.
    if (result.kind === 'failed' && sends.get(challengeToken)?.answer === answer) {
      sends.delete(challengeToken);
    }
    return result;
  });
  sends.set(challengeToken, { answer, recordedAt: now });
  return answer;
}

/**
 * Forget every send. Exported for tests ONLY: tokens repeat across a file's
 * tests, and Vitest isolates modules per file rather than per test. Production
 * never calls it.
 */
export function __resetSignInEmailCodeForTests(): void {
  sends.clear();
}

/** How many sends are kept, expired or not. Exported for tests ONLY. */
export function __signInEmailCodeSendCountForTests(): number {
  return sends.size;
}

interface SignInEmailCodeOptions {
  /** The challenge's token. */
  challengeToken: string | null;
  /**
   * The server selection the challenge arrived under, or null when no
   * challenge is open. The code is requested from that server, and only while
   * it is still the selection: the token belongs to the server that issued it.
   */
  serverSelection: RuntimeServerSelection | null;
  /** The method the surface is showing. */
  mode: MFAMethodCategory | 'method-select';
  /** The challenge's `methods`. `email-sms` also covers SMS, which has no send route. */
  methods: readonly string[];
  /**
   * The surface's message for a challenge whose selection has moved on, shown
   * when the move stops a send from leaving or its answer from being shown.
   */
  originChangedError: string;
  /**
   * An authoritative check that the challenge is still live, when the surface
   * has one (the modal's store). Checked before a send leaves and again before
   * its result is shown.
   */
  isCurrent?: (challengeToken: string) => boolean;
}

export interface SignInEmailCode {
  /**
   * The send's error for the challenge on screen, or `''`. It has its own
   * line: the code input's error slot belongs to verify.
   */
  sendError: string;
  /** The last send for the challenge on screen failed in a way asking again can fix. */
  failed: boolean;
  /** Asks once more after a failed send. */
  retry: () => void;
}

interface SendState {
  token: string;
  kind: 'pending' | SignInEmailCodeResult['kind'] | 'moved';
  message?: string;
}

/**
 * Requests the email code when the surface shows `email-sms` for a challenge
 * that lists `email`: as its default, or after a switch into it. Success shows
 * nothing new, since the subtitle already says a code was sent. A result is
 * shown only while its challenge is still on screen, in email mode, under the
 * selection it arrived under. After a `failed` send the surface offers
 * `retry`; a `refused` one is final.
 */
export function useSignInEmailCode({
  challengeToken,
  serverSelection,
  mode,
  methods,
  originChangedError,
  isCurrent,
}: SignInEmailCodeOptions): SignInEmailCode {
  // No selection means no challenge is open: the login page clears both on
  // its way back to the password form.
  const token = serverSelection === null ? null : challengeToken;
  const shouldSend = mode === 'email-sms' && methods.includes('email');
  const [state, setState] = useState<SendState | null>(null);
  // Bumped by retry. The failed send's entry is already gone, so the effect
  // below asks again.
  const [attempt, setAttempt] = useState(0);

  // Read when a send leaves or settles, so a caller's new callback never
  // re-runs the effect below.
  const isCurrentRef = useRef(isCurrent);
  useEffect(() => {
    isCurrentRef.current = isCurrent;
  }, [isCurrent]);

  // Re-runs on every way back into email mode. `sendOnce` hands a re-run the
  // send already out or answered, so StrictMode's replay, a switch away and
  // back, and a remount all ask once; only a failure is asked for again.
  useEffect(() => {
    if (!token || !shouldSend || serverSelection === null) return;
    if (isCurrentRef.current?.(token) === false) return;
    let active = true;
    // Nothing leaves once the selection has moved on.
    const answer = runtimeServerSelectionIsCurrent(serverSelection)
      ? sendOnce(serverSelection.apiBase, token)
      : Promise.resolve(null);
    void answer.then((result) => {
      if (!active || isCurrentRef.current?.(token) === false) return;
      // Checked again: the selection can move while a send is out.
      if (result === null || !runtimeServerSelectionIsCurrent(serverSelection)) {
        setState({ token, kind: 'moved' });
        return;
      }
      setState({ token, ...result });
    });
    return () => {
      active = false;
      // Clears what this run showed. Coming back into email mode after a
      // failure asks again, and the old failure must not sit beside the new
      // send while it is out.
      setState(null);
    };
  }, [token, shouldSend, serverSelection, attempt]);

  const current = token && shouldSend && state?.token === token ? state : null;
  const failed = current?.kind === 'failed';
  let sendError = '';
  if (current?.kind === 'moved') sendError = originChangedError;
  else if (current?.message) sendError = current.message;

  // A second click before the button unmounts finds the new send already out.
  const retry = useCallback(() => {
    if (failed) setAttempt((n) => n + 1);
  }, [failed]);

  return { sendError, failed, retry };
}
