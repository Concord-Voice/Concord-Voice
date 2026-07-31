// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('electron', () => ({
  net: { fetch: vi.fn() },
  app: { getPath: () => '/tmp/td' },
}));

import type { GuardedResponse } from '@/main/guardedRequest';
import { EgressDeniedError } from '@/main/guardedRequest';
import {
  _resetSelfHostedProfileForTesting,
  isValidatedSelfHostedApiBase,
} from '@/main/selfHostedProfile';
import {
  normalizeSelfHostedUrl,
  parseCapabilities,
  parseClientConfig,
  probeSelfHostedServer,
} from '@/main/selfHostedProbe';

// The probe's origin pin is MANDATORY (#2354): a response carrying no url is a
// failure, so every fixture states the origin it answered on.
function jsonResponse(
  body: unknown,
  status = 200,
  url = 'https://homelab.lan/probe'
): GuardedResponse {
  return {
    ok: status >= 200 && status < 300,
    status,
    url,
    json: () => Promise.resolve(body),
  };
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

  // 253 is the DNS name limit, so a longer host can never resolve — rejecting it
  // loses no legitimate case and keeps an unbounded string out of the dialog. The
  // alternative, eliding it for display, is forbidden: ADR-0035 requires the host
  // be shown verbatim so `evil.com` cannot be middle-elided into `bank…com`.
  it('rejects a hostname longer than the DNS limit', () => {
    const overlong = `${'a'.repeat(120)}.${'b'.repeat(120)}.${'c'.repeat(20)}.lan`;
    expect(overlong.length).toBeGreaterThan(253);
    expect(normalizeSelfHostedUrl(`https://${overlong}`)).toMatchObject({
      ok: false,
      code: 'invalid_url',
    });
  });

  it('admits a hostname exactly at the DNS limit', () => {
    // 253 chars: three labels plus the separating dots.
    const atLimit = `${'a'.repeat(63)}.${'b'.repeat(63)}.${'c'.repeat(63)}.${'d'.repeat(61)}`;
    expect(atLimit).toHaveLength(253);
    expect(normalizeSelfHostedUrl(`https://${atLimit}`)).toEqual({
      ok: true,
      apiBase: `https://${atLimit}`,
    });
  });

  it('measures the hostname, not the whole URL', () => {
    // A short host with a long path/query must still pass.
    expect(normalizeSelfHostedUrl(`https://homelab.lan/${'p'.repeat(400)}`)).toEqual({
      ok: true,
      apiBase: 'https://homelab.lan',
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
      headers: { Accept: 'application/json' },
    });
  });

  it('fails when a probe response resolves on a different origin', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ spaIpcContract: 17 }, 200, 'https://evil.test/x'));

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

  it('returns discovery payloads without minting trust on success', async () => {
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
      headers: { Accept: 'application/json' },
    });
    expect(isValidatedSelfHostedApiBase('https://homelab.lan')).toBe(false); // probe no longer mints (#2354)
  });

  it('permits http for the whole loopback range, closing the 127.0.0.2 hole', () => {
    expect(normalizeSelfHostedUrl('http://127.0.0.2:8080')).toEqual({
      ok: true,
      apiBase: 'http://127.0.0.2:8080',
    });
    expect(normalizeSelfHostedUrl('http://[::1]:8080')).toEqual({
      ok: true,
      apiBase: 'http://[::1]:8080',
    });
    // A non-loopback literal still requires HTTPS.
    expect(normalizeSelfHostedUrl('http://10.0.0.5:8080')).toMatchObject({
      ok: false,
      code: 'https_required',
    });
  });

  it('rejects a non-object or oversized client config as a schema mismatch', () => {
    expect(parseClientConfig('<html>')).toBeNull();
    expect(parseClientConfig(null)).toBeNull();
    expect(parseClientConfig([1, 2, 3])).toBeNull();
    expect(parseClientConfig({ spaIpcContract: 17 })).toEqual({ spaIpcContract: 17 });

    const oversized = Object.fromEntries(Array.from({ length: 201 }, (_, i) => [`k${i}`, i]));
    expect(parseClientConfig(oversized)).toBeNull();
  });

  // JSON.parse creates a literal `__proto__` as an OWN property and structured clone
  // preserves it across the IPC hop, so a server-chosen one would ride the pass-through
  // into the renderer and poison the first `Object.assign({}, clientConfig)` (CWE-1321).
  it('strips prototype-polluting keys from the pass-through payload (#2354 review item 12)', () => {
    const hostile = JSON.parse(
      '{"apiBase":"https://x.lan","__proto__":{"polluted":true},"constructor":1,"prototype":2}'
    ) as unknown;
    expect(Object.keys(hostile as object)).toContain('__proto__'); // own property, as parsed

    const parsed = parseClientConfig(hostile);
    expect(parsed).toEqual({ apiBase: 'https://x.lan' });
    expect(Object.keys(parsed as object)).not.toContain('__proto__');
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
  });

  it('rejects a non-object capabilities payload and passes an object through', () => {
    expect(parseCapabilities('nope')).toBeNull();
    const caps = { auth: { oauthProviders: ['google'] } };
    expect(parseCapabilities(caps)).toEqual(caps);
  });

  it('fails when a probe response carries no url (mandatory origin pin)', async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(jsonResponse({ spaIpcContract: 17 }, 200, ''));

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toMatchObject({
      status: 'error',
      code: 'client_config_failed',
    });
  });

  // The two tiers carry different verdicts, so they must not share one wire code.
  // `address_not_allowed` is the renderer's TERMINAL "Concord can never connect"
  // copy; a tier-2 address is the approvable self-hosting case and is recoverable
  // through the approval ceremony. Reporting tier 2 as terminal would tell a
  // self-hoster their own loopback server is permanently unreachable — and would
  // ship a tier-2 reason token (`loopback`/`private`) into a renderer-visible string.
  it('maps a tier-1 denied egress to address_not_allowed rather than a generic failure', async () => {
    const fetcher = vi
      .fn()
      .mockRejectedValueOnce(
        new EgressDeniedError({ tier: 'tier1', reason: 'metadata_link_local' })
      );

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toEqual({
      status: 'error',
      code: 'address_not_allowed',
      message: 'metadata_link_local',
    });
  });

  it('maps a tier-2 denied egress to origin_not_approved, never address_not_allowed', async () => {
    const fetcher = vi
      .fn()
      .mockRejectedValueOnce(new EgressDeniedError({ tier: 'tier2', reason: 'loopback' }));

    const result = await probeSelfHostedServer('https://homelab.lan', fetcher);

    expect(result).toEqual({
      status: 'error',
      code: 'origin_not_approved',
      message: '',
    });
    expect(result).not.toMatchObject({ code: 'address_not_allowed' });
  });

  it('maps a tier-1 denied egress on the capabilities hop too', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ spaIpcContract: 17 }))
      .mockRejectedValueOnce(new EgressDeniedError({ tier: 'tier1', reason: 'multicast' }));

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toEqual({
      status: 'error',
      code: 'address_not_allowed',
      message: 'multicast',
    });
  });

  it('maps a tier-2 denied egress on the capabilities hop to origin_not_approved', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ spaIpcContract: 17 }))
      .mockRejectedValueOnce(new EgressDeniedError({ tier: 'tier2', reason: 'private' }));

    const result = await probeSelfHostedServer('https://homelab.lan', fetcher);

    expect(result).toEqual({
      status: 'error',
      code: 'origin_not_approved',
      message: '',
    });
    expect(result).not.toMatchObject({ code: 'address_not_allowed' });
  });

  it('rejects a client config that is not an object even when the fetch succeeds', async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(jsonResponse('<html>not json</html>'));

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toEqual({
      status: 'error',
      code: 'client_config_failed',
      message: 'The server did not respond like a Concord server.',
    });
  });

  it('rejects a capabilities payload that is not an object', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ spaIpcContract: 17 }))
      .mockResolvedValueOnce(jsonResponse(['not', 'an', 'object']));

    await expect(probeSelfHostedServer('https://homelab.lan', fetcher)).resolves.toEqual({
      status: 'error',
      code: 'capabilities_failed',
      message: 'The server did not respond like a Concord server.',
    });
  });
});
