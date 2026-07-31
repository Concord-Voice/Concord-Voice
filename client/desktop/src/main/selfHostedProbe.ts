import { classifyAddress } from './egressPolicy';
import { EgressDeniedError, guardedRequest, type GuardedResponse } from './guardedRequest';
import type { SelfHostedProbeResult } from './ipcContract';
import { isTier2DialApproved } from './selfHostedProfile';

// `headers` only. The pre-#2354 shape also declared `credentials: 'omit'` and
// `redirect: 'manual'`, which the guarded fetcher never consumed: guardedRequest has no
// cookie jar at all, and it FOLLOWS up to 3 redirects, re-running full egress admission
// on each hop and refusing any cross-origin one, rather than surfacing the 3xx. A
// self-hosted server that 301s /api/v1/client/config therefore probes successfully —
// defensible, but `redirect: 'manual'` said the opposite, so the inert options are gone.
type ProbeFetch = (url: string, init: { headers: { Accept: string } }) => Promise<GuardedResponse>;

type NormalizedSelfHostedUrl =
  { ok: true; apiBase: string } | { ok: false; code: string; message: string };

const REQUEST_INIT = {
  headers: { Accept: 'application/json' },
};

function error(code: string, message: string): SelfHostedProbeResult {
  return { status: 'error', code, message };
}

// Recognises `localhost` (name) OR any loopback IP literal (127.0.0.0/8, ::1) so http
// is permitted for the whole loopback class — not just the exact strings the old
// isLocalhost matched. classifyAddress runs only for IP literals; hostnames fall through.
function isHttpAllowedHost(hostname: string): boolean {
  if (hostname.toLowerCase() === 'localhost') return true;
  const decision = classifyAddress(hostname.replace(/^\[|\]$/g, ''));
  return decision.tier === 'tier2' && decision.reason === 'loopback';
}

// Both discovery endpoints must answer with a bounded JSON object. This is a
// PASS-THROUGH parser, not a projection: the probe forwards the server's own
// payload to the renderer, so narrowing it here would silently drop fields.
const MAX_JSON_KEYS = 200;

/** RFC 1035 §2.3.4 — the maximum length of a domain name in presentation form. */
const MAX_HOSTNAME_LENGTH = 253;

// JSON.parse creates a literal `__proto__` as an OWN property and structured clone
// preserves it across the IPC hop, so a server-chosen `__proto__` would ride the
// pass-through into the renderer. Inert while the renderer reads only `apiBase`, but the
// pass-through exists to be extended, and the first `Object.assign({}, clientConfig)`
// downstream is CWE-1321. Strip at the boundary that owns the bound.
const FORBIDDEN_KEYS = ['__proto__', 'constructor', 'prototype'];

function boundedObject(value: unknown): Record<string, unknown> | null {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
  const obj = value as Record<string, unknown>;
  const keys = Object.keys(obj);
  if (keys.length > MAX_JSON_KEYS) return null;
  const safe: Record<string, unknown> = {};
  for (const key of keys) {
    if (FORBIDDEN_KEYS.includes(key)) continue;
    safe[key] = obj[key];
  }
  return safe;
}

export function parseClientConfig(value: unknown): Record<string, unknown> | null {
  return boundedObject(value);
}

export function parseCapabilities(value: unknown): Record<string, unknown> | null {
  return boundedObject(value);
}

export function normalizeSelfHostedUrl(value: string): NormalizedSelfHostedUrl {
  const trimmed = value.trim();
  if (!trimmed) {
    return { ok: false, code: 'invalid_url', message: 'Enter a self-hosted server URL.' };
  }

  const withScheme = /^[a-zA-Z][a-zA-Z\d+.-]*:\/\//.test(trimmed) ? trimmed : `https://${trimmed}`;

  let parsed: URL;
  try {
    parsed = new URL(withScheme);
  } catch {
    return { ok: false, code: 'invalid_url', message: 'Enter a valid self-hosted server URL.' };
  }

  if (parsed.username || parsed.password) {
    return {
      ok: false,
      code: 'credentials_not_allowed',
      message: 'Server URLs must not include usernames or passwords.',
    };
  }

  if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') {
    return {
      ok: false,
      code: 'unsupported_scheme',
      message: 'Self-hosted servers must use HTTP or HTTPS.',
    };
  }

  if (parsed.origin === 'null') {
    return { ok: false, code: 'invalid_url', message: 'Enter a valid self-hosted server URL.' };
  }

  // 253 is the DNS name limit, so a longer host can never resolve — rejecting it
  // loses no legitimate case, and it keeps an unbounded attacker-chosen string out
  // of the approval dialog. Truncating for display is NOT the alternative: ADR-0035
  // requires the host be shown verbatim, never decoded, prettified, or truncated,
  // precisely so `evil.com` cannot be middle-elided into a plausible `bank…com`.
  // Measured on the hostname alone; a long path is irrelevant and never displayed.
  if (parsed.hostname.length > MAX_HOSTNAME_LENGTH) {
    return { ok: false, code: 'invalid_url', message: 'Enter a valid self-hosted server URL.' };
  }

  if (parsed.protocol === 'http:' && !isHttpAllowedHost(parsed.hostname)) {
    return {
      ok: false,
      code: 'https_required',
      message: 'Self-hosted servers must use HTTPS unless the host is localhost.',
    };
  }

  return { ok: true, apiBase: parsed.origin };
}

async function fetchJson(apiBase: string, endpoint: string, fetcher: ProbeFetch): Promise<unknown> {
  const response = await fetcher(`${apiBase}${endpoint}`, REQUEST_INIT);
  if (!response.ok) {
    throw new Error(`HTTP ${response.status}`);
  }
  // Mandatory origin pin (#2354): an absent response.url is a failure. The pre-#2354
  // guard was `if (response.url && …)`, so a transport that reported no URL skipped
  // the check entirely.
  if (!response.url || new URL(response.url).origin !== apiBase) {
    throw new Error('Probe response origin changed');
  }
  return response.json();
}

// PII-safe (`[internal]rules/observability.md`): the probe target is renderer-supplied
// and may be a hostname or address, so only the fixed outcome code is logged — never
// the URL, the resolved address, or the raw error object.
function logProbeFailure(code: string): void {
  console.warn('Self-hosted probe request failed:', { code });
}

// The two egress tiers carry different verdicts and must not share one wire code.
// `address_not_allowed` is the renderer's TERMINAL "Concord can never connect"
// copy, which is only true of tier 1. A tier-2 denial means the exact origin is
// not on the durable approved list — the approvable self-hosting case, recoverable
// by re-running the approval ceremony — so it reports `origin_not_approved` with an
// empty message. Two consequences, both load-bearing: a self-hoster is never told
// their own server is permanently unreachable, and a tier-2 reason token
// (`loopback`/`private`/`ula`/`cgnat` — all forbidden renderer vocabulary) can no
// longer ride the tier-1 code into a renderer-visible string.
function egressDenialResult(err: EgressDeniedError): SelfHostedProbeResult {
  if (err.tier === 'tier1') {
    logProbeFailure('address_not_allowed');
    return error('address_not_allowed', err.reason ?? 'reserved');
  }
  logProbeFailure('origin_not_approved');
  return error('origin_not_approved', '');
}

export async function probeSelfHostedServer(
  value: string,
  fetcher: ProbeFetch = (url, init) =>
    guardedRequest(url, {
      method: 'GET',
      headers: init.headers,
      // Dialling a tier-2 address needs an approval TAKEN AT tier 2, not merely
      // origin trust minted by a ceremony that resolved to a public address.
      isOriginApproved: (origin) => isTier2DialApproved(origin),
    })
): Promise<SelfHostedProbeResult> {
  const normalized = normalizeSelfHostedUrl(value);
  if (!normalized.ok) {
    return error(normalized.code, normalized.message);
  }

  let clientConfig: Record<string, unknown> | null;
  try {
    clientConfig = parseClientConfig(
      await fetchJson(normalized.apiBase, '/api/v1/client/config', fetcher)
    );
  } catch (err) {
    if (err instanceof EgressDeniedError) {
      return egressDenialResult(err);
    }
    logProbeFailure('client_config_failed');
    return error(
      'client_config_failed',
      'Could not load /api/v1/client/config from the self-hosted server.'
    );
  }
  if (!clientConfig) {
    logProbeFailure('client_config_failed');
    return error('client_config_failed', 'The server did not respond like a Concord server.');
  }

  let capabilities: Record<string, unknown> | null;
  try {
    capabilities = parseCapabilities(
      await fetchJson(normalized.apiBase, '/api/v1/server/capabilities', fetcher)
    );
  } catch (err) {
    if (err instanceof EgressDeniedError) {
      return egressDenialResult(err);
    }
    logProbeFailure('capabilities_failed');
    return error(
      'capabilities_failed',
      'Could not load /api/v1/server/capabilities from the self-hosted server.'
    );
  }
  if (!capabilities) {
    logProbeFailure('capabilities_failed');
    return error('capabilities_failed', 'The server did not respond like a Concord server.');
  }

  // #2354: a successful probe does NOT mint trust. Minting is the native approval
  // ceremony's sole job (main.ts → commitSelfHostedApproval).
  return {
    status: 'ok',
    apiBase: normalized.apiBase,
    clientConfig,
    capabilities,
  };
}
