// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('electron', () => ({
  net: { fetch: vi.fn() },
  app: { getPath: () => '/tmp/td' },
}));

import {
  _resetSelfHostedProfileForTesting,
  isValidatedSelfHostedApiBase,
} from '@/main/selfHostedProfile';
import { normalizeSelfHostedUrl, probeSelfHostedServer } from '@/main/selfHostedProbe';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function jsonResponseWithUrl(body: unknown, url: string): Response {
  return {
    ok: true,
    status: 200,
    url,
    json: () => Promise.resolve(body),
  } as Response;
}

describe('selfHostedProbe', () => {
  beforeEach(() => {
    _resetSelfHostedProfileForTesting();
  });

  it('normalizes a bare host to an HTTPS origin', () => {
    expect(normalizeSelfHostedUrl(' homelab.lan:8443/path ')).toEqual({
      ok: true,
      apiBase: 'https://homelab.lan:8443',
    });
  });

  it('rejects invalid URLs and credentials', () => {
    expect(normalizeSelfHostedUrl('')).toMatchObject({
      ok: false,
      code: 'invalid_url',
    });
    const credentialUrl = new URL('https://homelab.lan');
    credentialUrl.username = 'user';

    expect(normalizeSelfHostedUrl(credentialUrl.toString())).toMatchObject({
      ok: false,
      code: 'credentials_not_allowed',
    });
  });

  it('rejects non-localhost HTTP URLs', () => {
    expect(normalizeSelfHostedUrl('http://homelab.lan')).toEqual({
      ok: false,
      code: 'https_required',
      message: 'Self-hosted servers must use HTTPS unless the host is localhost.',
    });
  });

  it('allows localhost HTTP for local development', () => {
    expect(normalizeSelfHostedUrl('http://localhost:8080/setup')).toEqual({
      ok: true,
      apiBase: 'http://localhost:8080',
    });
  });

  it('fails when /client/config cannot be fetched', async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(jsonResponse({ error: 'nope' }, 500));

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toEqual({
      status: 'error',
      code: 'client_config_failed',
      message: 'Could not load /api/v1/client/config from the self-hosted server.',
    });
    expect(fetcher).toHaveBeenCalledWith('https://homelab.lan/api/v1/client/config', {
      credentials: 'omit',
      headers: { Accept: 'application/json' },
      redirect: 'manual',
    });
  });

  it('fails when a probe response resolves on a different origin', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponseWithUrl({ spaIpcContract: 17 }, 'https://evil.test/x'));

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toEqual({
      status: 'error',
      code: 'client_config_failed',
      message: 'Could not load /api/v1/client/config from the self-hosted server.',
    });
  });

  it('fails when /server/capabilities cannot be fetched', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ spaIpcContract: 17 }))
      .mockResolvedValueOnce(jsonResponse({ error: 'nope' }, 404));

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toEqual({
      status: 'error',
      code: 'capabilities_failed',
      message: 'Could not load /api/v1/server/capabilities from the self-hosted server.',
    });
  });

  it('returns discovery payloads and remembers the validated origin on success', async () => {
    const clientConfig = { spaIpcContract: 17 };
    const capabilities = { auth: { oauthProviders: ['google'] } };
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(clientConfig))
      .mockResolvedValueOnce(jsonResponse(capabilities));

    await expect(probeSelfHostedServer('https://homelab.lan/path', fetcher)).resolves.toEqual({
      status: 'ok',
      apiBase: 'https://homelab.lan',
      clientConfig,
      capabilities,
    });
    expect(fetcher).toHaveBeenNthCalledWith(2, 'https://homelab.lan/api/v1/server/capabilities', {
      credentials: 'omit',
      headers: { Accept: 'application/json' },
      redirect: 'manual',
    });
    expect(isValidatedSelfHostedApiBase('https://homelab.lan')).toBe(true);
  });
});
