// @vitest-environment node
import { describe, expect, it, vi } from 'vitest';
import net, { type AddressInfo, type LookupFunction } from 'node:net';
import http from 'node:http';
import {
  EgressDeniedError,
  guardedRequest,
  makeGuardedLookup,
  resolveForDisplay,
} from '@/main/guardedRequest';

// Fake resolver factory: returns fixed address lists per call, mimicking dns.lookup(all:true).
function fakeResolver(...calls: string[][]): typeof import('node:dns').lookup {
  let i = 0;
  const fn = (_host: string, _opts: unknown, cb: (e: Error | null, a: unknown) => void) => {
    const addrs = calls[Math.min(i++, calls.length - 1)].map((address) => ({
      address,
      family: address.includes(':') ? 6 : 4,
    }));
    cb(null, addrs);
  };
  return fn as unknown as typeof import('node:dns').lookup;
}

describe('makeGuardedLookup', () => {
  const run = (lookup: LookupFunction, host = 'guard.invalid') =>
    new Promise<{ err: Error | null; addrs: unknown }>((resolve) => {
      lookup(host, { all: true } as never, (err, addrs) => resolve({ err, addrs }));
    });

  it('denies wholesale when ANY resolved entry is tier 1 (Happy Eyeballs #13)', async () => {
    const lookup = makeGuardedLookup({
      isOriginApproved: () => true,
      resolver: fakeResolver(['93.184.216.34', '169.254.169.254']),
    });
    const { err } = await run(lookup);
    expect(err).toBeInstanceOf(EgressDeniedError);
    expect((err as EgressDeniedError).tier).toBe('tier1');
  });

  it('fails closed on an empty resolution', async () => {
    const lookup = makeGuardedLookup({ isOriginApproved: () => true, resolver: fakeResolver([]) });
    const { err } = await run(lookup);
    expect(err).toBeInstanceOf(EgressDeniedError);
  });

  it('permits a fully tier-2 result only when the origin is approved', async () => {
    const denied = makeGuardedLookup({
      isOriginApproved: () => false,
      resolver: fakeResolver(['10.0.0.5']),
    });
    expect((await run(denied)).err).toBeInstanceOf(EgressDeniedError);

    const allowed = makeGuardedLookup({
      isOriginApproved: () => true,
      resolver: fakeResolver(['10.0.0.5']),
    });
    expect((await run(allowed)).err).toBeNull();
  });
});

function startServer(handler: http.RequestListener): Promise<{ url: string; close: () => void }> {
  return new Promise((resolve) => {
    const srv = http.createServer(handler);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address() as AddressInfo;
      resolve({ url: `http://127.0.0.1:${port}`, close: () => srv.close() });
    });
  });
}

describe('guardedRequest — loopback approve/deny (#7/#8)', () => {
  it('denies an unapproved loopback origin before any bytes', async () => {
    const srv = await startServer((_req, res) => res.end('{}'));
    try {
      await expect(
        guardedRequest(`${srv.url}/api/v1/client/config`, { isOriginApproved: () => false })
      ).rejects.toBeInstanceOf(EgressDeniedError);
    } finally {
      srv.close();
    }
  });

  it('connects to an approved loopback origin (the real self-hoster path)', async () => {
    const srv = await startServer((req, res) => {
      res.setHeader('content-type', 'application/json');
      res.end(JSON.stringify({ path: req.url }));
    });
    try {
      const origin = srv.url;
      const r = await guardedRequest(`${srv.url}/api/v1/client/config`, {
        isOriginApproved: (o) => o === origin,
      });
      expect(r.ok).toBe(true);
      expect(await r.json()).toEqual({ path: '/api/v1/client/config' });
    } finally {
      srv.close();
    }
  });

  it('rejects an oversized body at 64 KiB without calling JSON.parse', async () => {
    const srv = await startServer((_req, res) => res.end('x'.repeat(1024 * 1024)));
    const spy = vi.spyOn(JSON, 'parse');
    try {
      const origin = srv.url;
      await expect(
        guardedRequest(`${srv.url}/big`, { isOriginApproved: (o) => o === origin })
      ).rejects.toThrow(/too large/i);
      expect(spy).not.toHaveBeenCalled();
    } finally {
      spy.mockRestore();
      srv.close();
    }
  });

  it('denies a redirect hop that targets a tier-1 address (#4)', async () => {
    const srv = await startServer((_req, res) => {
      res.statusCode = 302;
      res.setHeader('location', 'http://169.254.169.254/latest/meta-data/');
      res.end();
    });
    try {
      const origin = srv.url;
      await expect(
        guardedRequest(`${srv.url}/api/v1/client/config`, { isOriginApproved: (o) => o === origin })
      ).rejects.toBeInstanceOf(EgressDeniedError);
    } finally {
      srv.close();
    }
  });

  it('rejects a response whose origin changed (mandatory origin pin)', async () => {
    const srv = await startServer((_req, res) => {
      res.statusCode = 302;
      res.setHeader('location', 'http://127.0.0.1:1/elsewhere');
      res.end();
    });
    try {
      const origin = srv.url;
      await expect(
        guardedRequest(`${srv.url}/x`, { isOriginApproved: (o) => o === origin })
      ).rejects.toThrow();
    } finally {
      srv.close();
    }
  });

  // #2354 follow-up: the pin must reject the HOP, not the body. When it ran after the
  // response was read, a redirect to a second admissible origin was fully dialled —
  // the caller saw a throw while the victim had already served the request.
  it('refuses a cross-origin redirect BEFORE the second origin is dialled', async () => {
    const victimHits: string[] = [];
    const victim = await startServer((req, res) => {
      victimHits.push(req.url ?? '');
      res.end(JSON.stringify({ internal: 'B-SIDE' }));
    });
    const redirector = await startServer((_req, res) => {
      res.statusCode = 302;
      res.setHeader('location', `${victim.url}/secret-path`);
      res.end();
    });
    try {
      await expect(
        guardedRequest(`${redirector.url}/api/v1/client/config`, {
          // BOTH origins are approved: the pin, not the egress guard, is the only
          // thing standing between hop 1 and hop 2.
          isOriginApproved: (o) => o === redirector.url || o === victim.url,
        })
      ).rejects.toThrow('Probe response origin changed');

      expect(victimHits).toEqual([]);
    } finally {
      redirector.close();
      victim.close();
    }
  });

  it('emits no Cookie/Authorization header it was not given and never touches defaultSession', async () => {
    let seen: http.IncomingHttpHeaders = {};
    const srv = await startServer((req, res) => {
      seen = req.headers;
      res.end('{}');
    });
    try {
      const origin = srv.url;
      await guardedRequest(`${srv.url}/x`, {
        isOriginApproved: (o) => o === origin,
        headers: { Accept: 'application/json' },
      });
      expect(seen.cookie).toBeUndefined();
      expect(seen.authorization).toBeUndefined();
    } finally {
      srv.close();
    }
  });

  it('blocks 169.254.169.254 before the socket is dialed (#2354 regression)', async () => {
    const connect = vi.spyOn(net, 'createConnection');
    try {
      await expect(
        guardedRequest('http://169.254.169.254/latest/meta-data/', { isOriginApproved: () => true })
      ).rejects.toBeInstanceOf(EgressDeniedError);
      expect(connect).not.toHaveBeenCalled();
    } finally {
      connect.mockRestore();
    }
  });

  it('rejects when the responding origin differs from the pinned one, even if both are approved', async () => {
    const dest = await startServer((_req, res) => res.end('{}'));
    const src = await startServer((_req, res) => {
      res.statusCode = 302;
      res.setHeader('location', `${dest.url}/elsewhere`);
      res.end();
    });
    try {
      // Both origins approved, so the per-hop admission passes and execution reaches the pin.
      await expect(
        guardedRequest(`${src.url}/x`, {
          isOriginApproved: (o) => o === src.url || o === dest.url,
        })
      ).rejects.toThrow(/origin changed/i);
    } finally {
      src.close();
      dest.close();
    }
  });
});

// The global agents default to keepAlive on Node >= 19 and Agent.getName() keys the pool
// on host/port/family only — not on `lookup`, not on approval state — so a pooled socket
// is reused with NEITHER guard running. Harmless while approvals are monotonic; live the
// moment revocation lands. This asserts the mechanism is simply absent.
describe('guardedRequest — no socket pooling (#2354 review item 5)', () => {
  it('opens a fresh connection per request instead of reusing a pooled one', async () => {
    let connections = 0;
    const srv = http.createServer((_req, res) => res.end('{}'));
    srv.on('connection', () => {
      connections += 1;
    });
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', () => r()));
    const { port } = srv.address() as AddressInfo;
    const origin = `http://127.0.0.1:${port}`;
    try {
      await guardedRequest(`${origin}/a`, { isOriginApproved: (o) => o === origin });
      await guardedRequest(`${origin}/b`, { isOriginApproved: (o) => o === origin });
      // A pooling agent would serve the second request on the first socket (1).
      expect(connections).toBe(2);
    } finally {
      srv.close();
    }
  });
});

describe('guardedRequest — caller signal composes with the deadline (Gitar, #2668)', () => {
  // `signal: opts.signal ?? overall` silently DROPPED the 10 s wall-clock deadline for any
  // caller that supplied its own signal, leaving the request bounded only by the 8 s
  // per-socket inactivity timer — which resets on every byte, so a slow-drip server holds
  // it open indefinitely. The fix is AbortSignal.any: the caller's signal ADDS a
  // cancellation source, it never replaces the deadline. Both directions are asserted
  // because a fix that honoured only one bound would be the same class of defect.
  const slowDrip = () =>
    http.createServer((_req, res) => {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      // Never ends. A byte every 20 ms keeps the socket timer permanently reset.
      const t = setInterval(() => res.write(' '), 20);
      res.on('close', () => clearInterval(t));
    });

  it('still aborts on the caller signal (the caller bound is honoured)', async () => {
    const srv = slowDrip();
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', () => r()));
    const { port } = srv.address() as AddressInfo;
    const origin = `http://127.0.0.1:${port}`;
    try {
      const ac = new AbortController();
      const p = guardedRequest(`${origin}/x`, {
        isOriginApproved: (o) => o === origin,
        signal: ac.signal,
      });
      setTimeout(() => ac.abort(), 30);
      await expect(p).rejects.toThrow();
    } finally {
      srv.close();
    }
  });

  it('still aborts on the overall deadline even when the caller passes a signal', async () => {
    // The regression: with `?? overall`, this never-aborted caller signal replaced the
    // deadline outright. Proven with a short injected deadline rather than waiting 10 s.
    const srv = slowDrip();
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', () => r()));
    const { port } = srv.address() as AddressInfo;
    const origin = `http://127.0.0.1:${port}`;
    const realTimeout = AbortSignal.timeout.bind(AbortSignal);
    const spy = vi.spyOn(AbortSignal, 'timeout').mockImplementation(() => realTimeout(60));
    try {
      const never = new AbortController().signal; // never fires
      await expect(
        guardedRequest(`${origin}/x`, {
          isOriginApproved: (o) => o === origin,
          signal: never,
        })
      ).rejects.toThrow();
    } finally {
      spy.mockRestore();
      srv.close();
    }
  });
});

describe('resolveForDisplay — dialog pre-resolution and tier-1 fast-fail', () => {
  it('fast-fails a tier-1 IP literal with no resolver call', async () => {
    const resolver = vi.fn();
    await expect(resolveForDisplay('169.254.169.254', resolver as never)).resolves.toEqual({
      ok: false,
      kind: 'tier1',
      reason: 'metadata_link_local',
    });
    expect(resolver).not.toHaveBeenCalled();
  });

  it('returns a tier-2 literal for display (bracketed IPv6 is stripped)', async () => {
    await expect(resolveForDisplay('[::1]')).resolves.toEqual({
      ok: true,
      address: '::1',
      addresses: ['::1'],
      decision: { tier: 'tier2', reason: 'loopback' },
    });
  });

  it('fast-fails wholesale when ANY resolved entry is tier 1', async () => {
    await expect(
      resolveForDisplay('guard.invalid', fakeResolver(['10.0.0.5', '169.254.169.254']))
    ).resolves.toEqual({ ok: false, kind: 'tier1', reason: 'metadata_link_local' });
  });

  it('reports the sole address when the whole resolution is admissible', async () => {
    await expect(
      resolveForDisplay('guard.invalid', fakeResolver(['93.184.216.34']))
    ).resolves.toEqual({
      ok: true,
      address: '93.184.216.34',
      addresses: ['93.184.216.34'],
      decision: { tier: 'public' },
    });
  });

  it.each([
    ['an empty resolution', fakeResolver([])],
    [
      'a resolver error',
      ((_h: string, _o: unknown, cb: (e: Error | null, a: unknown) => void) =>
        cb(new Error('ENOTFOUND'), undefined)) as unknown as typeof import('node:dns').lookup,
    ],
  ])('reports unreachable on %s', async (_label, resolver) => {
    await expect(resolveForDisplay('guard.invalid', resolver)).resolves.toEqual({
      ok: false,
      kind: 'unreachable',
    });
  });

  // The timer bounds the CALLER's wait, not the lookup: getaddrinfo keeps its libuv
  // threadpool slot regardless. These tests pin the caller-visible contract only.
  it('settles as unreachable when the resolver never calls back', async () => {
    const neverCallsBack = (() => {
      /* deliberately never invokes the callback */
    }) as unknown as typeof import('node:dns').lookup;

    await expect(resolveForDisplay('stalled.invalid', neverCallsBack, 20)).resolves.toEqual({
      ok: false,
      kind: 'unreachable',
    });
  });

  it('keeps the real verdict when the resolver answers before the deadline', async () => {
    const slowButInTime = ((
      _host: string,
      _opts: unknown,
      cb: (e: Error | null, a: unknown) => void
    ) => {
      setTimeout(() => cb(null, [{ address: '169.254.169.254', family: 4 }]), 5);
    }) as unknown as typeof import('node:dns').lookup;

    await expect(resolveForDisplay('slow.invalid', slowButInTime, 500)).resolves.toEqual({
      ok: false,
      kind: 'tier1',
      reason: 'metadata_link_local',
    });
  });

  it('a late resolver answer cannot overwrite the timed-out verdict', async () => {
    let late: (() => void) | undefined;
    const answersAfterDeadline = ((
      _host: string,
      _opts: unknown,
      cb: (e: Error | null, a: unknown) => void
    ) => {
      late = () => cb(null, [{ address: '93.184.216.34', family: 4 }]);
    }) as unknown as typeof import('node:dns').lookup;

    const result = await resolveForDisplay('late.invalid', answersAfterDeadline, 20);
    expect(result).toEqual({ ok: false, kind: 'unreachable' });
    late?.(); // the stranded lookup finally lands; the settled promise must not change
    await expect(Promise.resolve(result)).resolves.toEqual({ ok: false, kind: 'unreachable' });
  });

  // Classifying only list[0] showed "on the internet" for a name that also answers on the
  // LAN, recorded tierAtApproval 'public', then dead-ended at the tier-2 dial with
  // origin_not_approved — a state re-approving could not clear, because the ceremony kept
  // displaying the public address. The STRICTEST member of the set decides.
  it('classifies wholesale: a mixed set presents and records tier 2, not the first entry', async () => {
    await expect(
      resolveForDisplay('guard.invalid', fakeResolver(['203.0.113.1', '192.168.1.7']))
    ).resolves.toEqual({
      ok: true,
      address: '192.168.1.7', // the representative the decision (and tierAtApproval) is taken on
      addresses: ['203.0.113.1', '192.168.1.7'], // every address, for the dialog line
      decision: { tier: 'tier2', reason: 'private' },
    });
  });

  it('reports every address even when the whole set is public', async () => {
    await expect(
      resolveForDisplay('guard.invalid', fakeResolver(['93.184.216.34', '203.0.113.1']))
    ).resolves.toEqual({
      ok: true,
      address: '93.184.216.34',
      addresses: ['93.184.216.34', '203.0.113.1'],
      decision: { tier: 'public' },
    });
  });

  // Item 7: an unclassifiable entry used to return ok with an `invalid` decision, which
  // main.ts then labelled "on the internet" via its `public` fallthrough.
  it('reports unreachable when any entry cannot be classified, never "on the internet"', async () => {
    await expect(
      resolveForDisplay('guard.invalid', fakeResolver(['93.184.216.34', 'not-an-ip']))
    ).resolves.toEqual({ ok: false, kind: 'unreachable' });
  });

  it('an IP literal never arms the timer', async () => {
    // Zero deadline: a literal that still resolved correctly proves the race is
    // scoped to the resolver path.
    await expect(resolveForDisplay('10.0.0.5', undefined, 0)).resolves.toEqual({
      ok: true,
      address: '10.0.0.5',
      addresses: ['10.0.0.5'],
      decision: { tier: 'tier2', reason: 'private' },
    });
  });
});
