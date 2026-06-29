import { net } from 'electron';
import type { SelfHostedProbeResult } from './ipcContract';
import { rememberValidatedSelfHostedApiBase } from './selfHostedProfile';

type ProbeFetch = (
  url: string,
  init: { credentials: 'omit'; headers: { Accept: string }; redirect: 'manual' }
) => Promise<Response>;

type NormalizedSelfHostedUrl =
  | { ok: true; apiBase: string }
  | { ok: false; code: string; message: string };

const REQUEST_INIT = {
  credentials: 'omit' as const,
  headers: { Accept: 'application/json' },
  redirect: 'manual' as const,
};

function error(code: string, message: string): SelfHostedProbeResult {
  return { status: 'error', code, message };
}

function isLocalhost(hostname: string): boolean {
  const host = hostname.toLowerCase();
  return host === 'localhost' || host === '127.0.0.1' || host === '[::1]' || host === '::1';
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

  if (parsed.protocol === 'http:' && !isLocalhost(parsed.hostname)) {
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
  if (response.url && new URL(response.url).origin !== apiBase) {
    throw new Error('Probe response origin changed');
  }
  return response.json();
}

export async function probeSelfHostedServer(
  value: string,
  fetcher: ProbeFetch = (url, init) => net.fetch(url, init)
): Promise<SelfHostedProbeResult> {
  const normalized = normalizeSelfHostedUrl(value);
  if (!normalized.ok) {
    return error(normalized.code, normalized.message);
  }

  let clientConfig: unknown;
  try {
    clientConfig = await fetchJson(normalized.apiBase, '/api/v1/client/config', fetcher);
  } catch {
    return error(
      'client_config_failed',
      'Could not load /api/v1/client/config from the self-hosted server.'
    );
  }

  let capabilities: unknown;
  try {
    capabilities = await fetchJson(normalized.apiBase, '/api/v1/server/capabilities', fetcher);
  } catch {
    return error(
      'capabilities_failed',
      'Could not load /api/v1/server/capabilities from the self-hosted server.'
    );
  }

  rememberValidatedSelfHostedApiBase(normalized.apiBase);
  return {
    status: 'ok',
    apiBase: normalized.apiBase,
    clientConfig,
    capabilities,
  };
}
