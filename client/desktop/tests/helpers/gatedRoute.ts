/**
 * MSW harness for the dangerous-action (D1) swap hosts (#3456 §3.3, §3.4): a
 * gated route that refuses a code-less request the way `mfaenforce.Require`
 * does, and answers the re-send that carries `mfa_code`. The requirements read
 * is stubbed too, since the real dialog issues it when it opens.
 *
 * The recorded `requests` are the assertion target: what was sent, to which
 * route, how many times, and with which body, is the contract these suites pin.
 */
import { http, HttpResponse, type HttpResponseInit } from 'msw';
import { server as mswServer } from '../mocks/server';

export const GATED_API_BASE = 'http://localhost:8080';

// Named fixtures: the pre-commit detect-secrets hook flags credential-shaped keys.
export const FIXTURE_OTP = '314159';
export const FIXTURE_OTP_2 = '271828';

/** The label `StepUpCredentials` puts on the authenticator-app code field. */
export const CODE_LABEL = 'Authenticator app code';
export const DIALOG_TITLE = "Confirm it's you";
export const ENROLMENT_TEXT = 'Set up an authenticator app or security key in Settings to do this.';
export const SETUP_LINK = 'Set up verification';

export interface GatedReply {
  status: number;
  body?: unknown;
  init?: HttpResponseInit;
}

/** `mfaenforce.Require`'s refusal of a request that carries no code (#3456 V15). */
export const MFA_REQUIRED: GatedReply = {
  status: 403,
  body: { error: 'MFA verification required', mfa_required: true, methods: ['totp'] },
};

/** The same gate for an actor who has no factor to give. */
export const ENROLMENT_REQUIRED: GatedReply = {
  status: 403,
  body: { error: 'MFA enrollment required', mfa_enrollment_required: true },
};

export const INVALID_CODE: GatedReply = { status: 403, body: { error: 'Invalid MFA code' } };

export interface RecordedRequest {
  /** Parsed JSON body; null when the request carried none. */
  body: Record<string, unknown> | null;
  contentType: string | null;
}

export interface GatedRouteOptions {
  method: 'post' | 'delete';
  url: string;
  /** The answer to a request with no `mfa_code`. Omitted: a refusal asking for a code. */
  first?: GatedReply;
  /** The answers to requests that carry `mfa_code`, in order. The last one repeats. Omitted: 200. */
  retries?: readonly GatedReply[];
}

async function readBody(request: Request): Promise<Record<string, unknown> | null> {
  const text = await request.text();
  if (text === '') return null;
  return JSON.parse(text) as Record<string, unknown>;
}

function reply({ status, body, init }: GatedReply): Response {
  return HttpResponse.json(body ?? {}, { status, ...init });
}

/** Installs the gated route and returns the list its requests are recorded in. */
export function stubGatedRoute(options: GatedRouteOptions): RecordedRequest[] {
  const requests: RecordedRequest[] = [];
  const retries = options.retries ?? [{ status: 200, body: { message: 'ok' } }];
  let retryIndex = 0;
  const handler = async ({ request }: { request: Request }) => {
    const body = await readBody(request);
    requests.push({ body, contentType: request.headers.get('Content-Type') });
    if (body === null || !('mfa_code' in body)) return reply(options.first ?? MFA_REQUIRED);
    const answer = retries[Math.min(retryIndex, retries.length - 1)];
    retryIndex += 1;
    return reply(answer);
  };
  mswServer.use(
    options.method === 'post' ? http.post(options.url, handler) : http.delete(options.url, handler)
  );
  return requests;
}

/** The requirements read the dialog issues on open: an account with an authenticator app. */
export function stubStepUpRead(methods: string[] = ['totp']): void {
  mswServer.use(
    http.get(`${GATED_API_BASE}/api/v1/mfa/step-up`, () =>
      HttpResponse.json({
        methods,
        default_method: methods[0] ?? null,
        backup_code_available: false,
      })
    )
  );
}

/** The `mfa_code`s sent, in order. */
export function codesSent(requests: readonly RecordedRequest[]): unknown[] {
  return requests
    .filter((r) => r.body !== null && 'mfa_code' in r.body)
    .map((r) => r.body?.mfa_code);
}
