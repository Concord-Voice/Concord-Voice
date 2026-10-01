// Shared wire fixtures for the #3509 reproductions: the step-up token contract
// (design spec "Developer decisions, 2026-10-01", T-2, T-4). The password goes
// only to the mint endpoint; the route is retried with { step_up_token }.

export const MINT_PATH = '/api/v1/auth/step-up/password';
export const MINTED_TOKEN = 'minted-single-use-token';
// Bound to a constant: detect-secrets flags keyword/literal adjacency.
export const FIXTURE_PW = 'hunter2-fixture';

/** One request the stand-in server saw. */
export type Wire = { path: string; method: string; body: Record<string, unknown> };

export function parseBody(text: string | null | undefined): Record<string, unknown> {
  return text ? (JSON.parse(text) as Record<string, unknown>) : {};
}

export type StubVerdict = 'minted' | 'token' | 'stale-client' | 'prompt';

/** What the post-fix server does with a route request carrying this body. */
export function routeVerdict(body: Record<string, unknown>): Exclude<StubVerdict, 'minted'> {
  if (body.step_up_token === MINTED_TOKEN) return 'token';
  if ('current_password' in body) return 'stale-client';
  return 'prompt';
}
