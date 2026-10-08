import type { Mock } from 'vitest';

/**
 * A scripted `apiFetch` for the step-up hosts (#7, #8, #9): `GET
 * /api/v1/mfa/step-up` is answered by path, every other request by `route`. The
 * host's own requests and the hook's read share one mocked `apiFetch`, so a
 * queued `*Once` response would be taken by whichever asks first.
 */

export const STEP_UP_READ_PATH = '/api/v1/mfa/step-up';

export function jsonResponse(status: number, body: unknown = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** The read's 200: the inline methods the account can use. */
export function readOffers(methods: string[], backupCodeAvailable = false): Response {
  return jsonResponse(200, {
    methods,
    default_method: methods[0] ?? null,
    backup_code_available: backupCodeAvailable,
  });
}

type Reply = Response | Promise<Response>;

export interface StepUpApiScript {
  /** The read's answer, built per request. Omitted: an account with no inline method. */
  read?: () => Reply;
  /** Every request that is not the read. */
  route: (path: string, init: RequestInit) => Reply;
}

/** Installs the script on `mockApiFetch`, replacing any earlier one. */
export function installStepUpApi(mockApiFetch: Mock, script: StepUpApiScript): void {
  mockApiFetch.mockReset().mockImplementation(async (path: string, init?: RequestInit) => {
    if (path === STEP_UP_READ_PATH) return (script.read ?? (() => readOffers([])))();
    return script.route(path, init ?? {});
  });
}

/** The parsed JSON bodies sent to `path`, in order. */
export function bodiesTo(mockApiFetch: Mock, path: string): Record<string, unknown>[] {
  return mockApiFetch.mock.calls
    .filter((c) => c[0] === path && typeof (c[1] as RequestInit | undefined)?.body === 'string')
    .map((c) => JSON.parse((c[1] as { body: string }).body) as Record<string, unknown>);
}

/** How many times the requirements read was issued. */
export function readCount(mockApiFetch: Mock): number {
  return mockApiFetch.mock.calls.filter((c) => c[0] === STEP_UP_READ_PATH).length;
}
