// @vitest-environment node
/**
 * #2354 follow-up — the consent TIER must bound the dial, not just the origin.
 *
 * The ceremony shows the user one resolved address and its class ("Resolves to:
 * 203.0.113.10, on the internet"), and `commitSelfHostedApproval` records that
 * class as `tierAtApproval`. Nothing used to read it back: the tier-2 branch of
 * `isDialPermitted` keyed on origin membership alone, so a public-address consent
 * silently authorised the whole loopback / RFC1918 / ULA / CGNAT space under that
 * name after a DNS flip (CWE-918 read primitive, CWE-350).
 *
 * Every module here is the REAL production module and the fetcher is
 * `probeSelfHostedServer`'s own default — no guard, predicate, resolver, or
 * classifier is stubbed. The internal service is a real loopback HTTP server, so
 * its `hits` array is direct proof of whether a socket was opened.
 */
import { afterAll, beforeEach, describe, expect, it, vi } from 'vitest';
import fs from 'node:fs';
import http from 'node:http';
import { type AddressInfo } from 'node:net';
import os from 'node:os';
import path from 'node:path';

// A real temp userData root so the durable approval store round-trips. The factory
// reads `dir` lazily (at getPath() call time), so the hoisted mock misses the TDZ.
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dial-consent-'));
vi.mock('electron', () => ({ app: { getPath: () => dir } }));

import { _resetApprovalsCacheForTesting } from '@/main/selfHostedApprovals';
import { probeSelfHostedServer } from '@/main/selfHostedProbe';
import {
  _resetSelfHostedProfileForTesting,
  commitSelfHostedApproval,
  isValidatedSelfHostedApiBase,
} from '@/main/selfHostedProfile';

const servers: http.Server[] = [];

/** A real internal service on loopback. `hits` is the proof a dial happened. */
async function startInternalService(body: unknown): Promise<{ port: number; hits: string[] }> {
  const hits: string[] = [];
  const srv = http.createServer((req, res) => {
    hits.push(req.url ?? '');
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify(body));
  });
  servers.push(srv);
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
  return { port: (srv.address() as AddressInfo).port, hits };
}

afterAll(() => {
  for (const s of servers) s.close();
  fs.rmSync(dir, { recursive: true, force: true });
});

beforeEach(() => {
  _resetSelfHostedProfileForTesting();
  _resetApprovalsCacheForTesting();
  fs.rmSync(path.join(dir, 'self-hosted-approvals.json'), { force: true });
});

describe('self-hosted dial consent is bounded by the ceremony tier (#2354)', () => {
  it('denies an IP-literal loopback dial for an origin approved at a PUBLIC address', async () => {
    const internal = await startInternalService({ admin_token: 'never-fetched' });
    const origin = `http://127.0.0.1:${internal.port}`;

    // The ceremony displayed a public address, so consent is recorded as 'public'.
    expect(commitSelfHostedApproval(origin, '203.0.113.10')).toBe(true);
    // Origin trust IS still minted — credential custody is a separate grant.
    expect(isValidatedSelfHostedApiBase(origin)).toBe(true);

    const result = await probeSelfHostedServer(origin); // real default fetcher

    expect(result).toEqual({ status: 'error', code: 'origin_not_approved', message: '' });
    expect(internal.hits).toHaveLength(0); // never dialled
  });

  it('denies the hostname rebind case: a public-consent name that now resolves to loopback', async () => {
    const internal = await startInternalService({ admin_token: 'never-fetched' });
    // `localhost` is the one hostname normalizeSelfHostedUrl admits over http, and the
    // system resolver really answers 127.0.0.1 for it — a genuine name-to-tier-2
    // resolution with no injected resolver, exactly the post-consent DNS flip.
    const origin = `http://localhost:${internal.port}`;
    expect(commitSelfHostedApproval(origin, '203.0.113.10')).toBe(true);

    const result = await probeSelfHostedServer(origin);

    expect(result).toEqual({ status: 'error', code: 'origin_not_approved', message: '' });
    expect(internal.hits).toHaveLength(0);
  });

  it('REGRESSION: a genuine tier-2 consent still probes both endpoints end to end', async () => {
    const internal = await startInternalService({ version: '1.0', capabilities: ['voice'] });
    const origin = `http://127.0.0.1:${internal.port}`;

    // The user approved a LAN server whose ceremony displayed a private address.
    expect(commitSelfHostedApproval(origin, '10.0.0.9')).toBe(true);

    const result = await probeSelfHostedServer(origin);

    expect(result.status).toBe('ok');
    expect(internal.hits).toEqual(['/api/v1/client/config', '/api/v1/server/capabilities']);
  });

  it('REGRESSION: a tier-2 consent still permits the hostname path', async () => {
    const internal = await startInternalService({ version: '1.0' });
    const origin = `http://localhost:${internal.port}`;
    expect(commitSelfHostedApproval(origin, '127.0.0.1')).toBe(true);

    const result = await probeSelfHostedServer(origin);

    expect(result.status).toBe('ok');
    expect(internal.hits).toEqual(['/api/v1/client/config', '/api/v1/server/capabilities']);
  });

  it('an origin with no approval at all is still denied at the same address', async () => {
    // Body is irrelevant here — this test asserts the service is never reached.
    const internal = await startInternalService({ version: '1.0' });

    const result = await probeSelfHostedServer(`http://127.0.0.1:${internal.port}`);

    expect(result).toEqual({ status: 'error', code: 'origin_not_approved', message: '' });
    expect(internal.hits).toHaveLength(0);
  });
});
