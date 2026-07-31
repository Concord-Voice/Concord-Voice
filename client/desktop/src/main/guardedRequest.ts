import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import dns from 'node:dns';
import { classifyAddress, isDialPermitted, type EgressDecision } from './egressPolicy';

export class EgressDeniedError extends Error {
  readonly tier: EgressDecision['tier'];
  readonly reason?: string;
  constructor(decision: EgressDecision) {
    super('egress denied');
    this.name = 'EgressDeniedError';
    this.tier = decision.tier;
    this.reason = 'reason' in decision ? decision.reason : undefined;
  }
}

// Validates EVERY resolved address (Happy Eyeballs dials past addresses[0]).
// Denies wholesale; a mixed good/bad result is anomalous and the good half must not pass.
export function makeGuardedLookup(deps: {
  isOriginApproved: () => boolean;
  resolver?: typeof dns.lookup;
}): net.LookupFunction {
  const resolver = deps.resolver ?? dns.lookup;
  return ((hostname, opts, cb) => {
    resolver(hostname, { ...(opts as object), all: true } as never, (err, addrs) => {
      if (err) return cb(err, '', 0); // fail closed
      const list = (Array.isArray(addrs) ? addrs : []) as { address: string; family: number }[];
      if (list.length === 0) {
        return cb(new EgressDeniedError({ tier: 'invalid', reason: 'empty' }), '', 0);
      }
      for (const a of list) {
        const decision = classifyAddress(a.address);
        if (!isDialPermitted(decision, deps.isOriginApproved())) {
          return cb(new EgressDeniedError(decision), '', 0);
        }
      }
      const all = (opts as { all?: boolean })?.all;
      return all
        ? (cb as unknown as (e: null, a: typeof list) => void)(null, list)
        : cb(null, list[0].address, list[0].family);
    });
  }) as net.LookupFunction;
}

export interface GuardedResponse {
  ok: boolean;
  status: number;
  url: string;
  json(): Promise<unknown>;
}

export interface GuardedRequestOptions {
  method?: 'GET' | 'POST';
  headers?: Record<string, string>;
  isOriginApproved: (origin: string) => boolean;
  resolver?: typeof dns.lookup;
  signal?: AbortSignal;
  maxBodyBytes?: number;
}

const MAX_BODY_BYTES = 64 * 1024;
const MAX_REDIRECTS = 3;
const SOCKET_TIMEOUT_MS = 8000;
const OVERALL_TIMEOUT_MS = 10000;

// An IP-literal host short-circuits `lookup` (net.Socket.connect skips it when
// net.isIP(host) is truthy), so a lookup-only guard is 100% bypassable — measured:
// `host: '127.0.0.1'` served 200 with the guard never called. The literal case must
// therefore be decided HERE, textually, before the request is made. A literal cannot
// rebind, so this IS connection time for that case; hostnames are decided at dial
// time by makeGuardedLookup. Both paths compose on the one `isOriginApproved`
// predicate.
function assertLiteralAdmissible(url: URL, isOriginApproved: (origin: string) => boolean): void {
  const host = url.hostname.replace(/^\[|\]$/g, '');
  if (net.isIP(host) !== 0) {
    const decision = classifyAddress(host);
    if (!isDialPermitted(decision, isOriginApproved(url.origin))) {
      throw new EgressDeniedError(decision);
    }
  }
}

export async function guardedRequest(
  rawUrl: string,
  opts: GuardedRequestOptions
): Promise<GuardedResponse> {
  const cap = opts.maxBodyBytes ?? MAX_BODY_BYTES;
  // The overall deadline is a FLOOR, not a default. A caller-supplied signal ADDS a
  // cancellation source; it must never replace the wall-clock bound. `opts.signal ?? overall`
  // reads as a sensible default and is a footgun: the next caller that passes a signal
  // silently loses the 10 s deadline and is bounded only by the 8 s per-socket timer —
  // which resets on every byte, so a slow-drip server holds the request open indefinitely.
  // No current caller passes one, so this is defence for the caller that does. (Gitar #2668.)
  const overall = AbortSignal.timeout(OVERALL_TIMEOUT_MS);
  const deadline = opts.signal ? AbortSignal.any([opts.signal, overall]) : overall;
  let target = new URL(rawUrl);
  const pinnedOrigin = target.origin;

  for (let hop = 0; hop <= MAX_REDIRECTS; hop++) {
    assertLiteralAdmissible(target, opts.isOriginApproved); // per-hop full admission
    const lookup = makeGuardedLookup({
      isOriginApproved: () => opts.isOriginApproved(target.origin),
      resolver: opts.resolver,
    });
    const isHttps = target.protocol === 'https:';
    const mod = isHttps ? https : http;
    // A per-call agent with keepAlive OFF. The global agents default to keepAlive on
    // Node >= 19, and Agent.getName() keys the socket pool on host/port/family — NOT
    // on `lookup` and not on approval state — so a pooled socket is reused without
    // createConnection and NEITHER guard runs for it. Approvals are monotonic today,
    // so that is not yet exploitable; it goes live the moment revocation lands, when a
    // socket to a just-revoked origin would survive for the agent's idle timeout.
    // Pooling buys nothing here anyway: two short GETs for the probe, one POST for logout.
    const agent = new mod.Agent({ keepAlive: false });
    // NOTE: do NOT set the `ca` option. Node's `ca` REPLACES the trust store; on macOS
    // getCACertificates('system') is 41 certs disjoint from bundled and breaks every
    // public-CA self-hosted server (measured, node 24.18.0). The default already consults
    // the OS store (default == bundled ∪ system). See ADR-0035 §5.5.
    const res = await new Promise<http.IncomingMessage>((resolve, reject) => {
      const req = mod.request(
        target,
        {
          method: opts.method ?? 'GET',
          headers: opts.headers ?? { Accept: 'application/json' },
          lookup,
          agent,
          signal: deadline,
        },
        resolve
      );
      req.setTimeout(SOCKET_TIMEOUT_MS, () => req.destroy(new Error('socket timeout')));
      req.on('error', reject);
      req.end();
    });

    const status = res.statusCode ?? 0;
    if (status >= 300 && status < 400 && res.headers.location) {
      res.resume();
      const next = new URL(res.headers.location, target);
      // Egress class first: it is the absolute denial and carries the more precise
      // verdict (a tier-1 hop must report as tier 1, not as an origin change).
      assertLiteralAdmissible(next, opts.isOriginApproved);
      // Then the pin — which gates the HOP, not the body. Checked after the read, it
      // suppressed the response while the second origin had already served the request,
      // so a redirect to any other admissible origin was still a real dial.
      if (next.origin !== pinnedOrigin) throw new Error('Probe response origin changed');
      target = next; // re-run full admission next iteration
      continue;
    }

    const chunks: Buffer[] = [];
    let size = 0;
    const body = await new Promise<Buffer>((resolve, reject) => {
      res.on('data', (c: Buffer) => {
        size += c.length;
        if (size > cap) {
          res.destroy();
          reject(new Error('response body too large'));
          return;
        }
        chunks.push(c);
      });
      res.on('end', () => resolve(Buffer.concat(chunks)));
      res.on('error', reject);
    });

    // Invariant backstop for the pin above: `target` is reassigned only in the redirect
    // branch, which already refused a cross-origin hop, so this cannot fire today. It
    // stays so a future hop that forgets the check still fails closed rather than
    // silently returning another origin's body.
    if (target.origin !== pinnedOrigin) throw new Error('Probe response origin changed');
    return {
      ok: status >= 200 && status < 300,
      status,
      url: target.toString(),
      json: async () => JSON.parse(body.toString('utf8')) as unknown,
    };
  }
  throw new Error('too many redirects');
}

export type ResolveForDisplayResult =
  // `address` is the representative the `decision` was taken on and the one recorded as
  // `tierAtApproval`; `addresses` is the FULL resolution, shown verbatim on the dialog.
  // They differ whenever a name resolves to a mixed set — see the wholesale rule below.
  | { ok: true; address: string; addresses: string[]; decision: EgressDecision }
  | { ok: false; kind: 'tier1'; reason: string }
  | { ok: false; kind: 'unreachable' };

// An OS resolver can sit on an unanswered query far longer than a user will wait for
// the connect flow, so the caller's wait is bounded independently of the query's.
export const RESOLVE_DISPLAY_TIMEOUT_MS = 5000;

// A pre-resolution for the dialog's display line and a tier-1 fast-fail. It is NOT the
// security check — no dial is authorized by its result; the authoritative check re-runs
// inside guardedRequest on every dial and every redirect hop. Wholesale-deny: any tier-1
// entry refuses with no dialog. See ADR-0035 §5.3.
export async function resolveForDisplay(
  host: string,
  resolver: typeof dns.lookup = dns.lookup,
  timeoutMs: number = RESOLVE_DISPLAY_TIMEOUT_MS
): Promise<ResolveForDisplayResult> {
  const bare = host.replace(/^\[|\]$/g, '');
  if (net.isIP(bare) !== 0) {
    const decision = classifyAddress(bare);
    if (decision.tier === 'tier1') return { ok: false, kind: 'tier1', reason: decision.reason };
    return { ok: true, address: bare, addresses: [bare], decision };
  }
  const lookup = new Promise<ResolveForDisplayResult>((resolve) => {
    resolver(host, { all: true } as never, (err, addrs) => {
      const list = (Array.isArray(addrs) ? addrs : []) as { address: string }[];
      if (err || list.length === 0) return resolve({ ok: false, kind: 'unreachable' });
      const decisions = list.map((a) => classifyAddress(a.address));
      const tier1 = decisions.find((d) => d.tier === 'tier1');
      if (tier1?.tier === 'tier1')
        return resolve({ ok: false, kind: 'tier1', reason: tier1.reason });
      // An entry we cannot classify is neither presentable nor dialable, and the dial
      // guard would refuse it anyway (isDialPermitted fails closed on 'invalid'). Report
      // it as the same verdict a genuine resolution failure produces rather than letting
      // it fall through to the dialog's `public` copy — "on the internet" for an address
      // we could not parse is a false statement to the user.
      if (decisions.some((d) => d.tier === 'invalid')) {
        return resolve({ ok: false, kind: 'unreachable' });
      }
      // WHOLESALE, not list[0]. Classifying only the first entry let a name resolving to
      // [public, private] show "on the internet", record `tierAtApproval: 'public'`, and
      // then dead-end on `origin_not_approved` at the tier-2 dial — a state re-approving
      // could not fix, because the ceremony would keep displaying the public address.
      // The STRICTEST member decides, and the dialog shows every address.
      const idx = decisions.findIndex((d) => d.tier === 'tier2');
      const pick = idx === -1 ? 0 : idx;
      resolve({
        ok: true,
        address: list[pick].address,
        addresses: list.map((a) => a.address),
        decision: decisions[pick],
      });
    });
  });

  // ── This timeout does NOT cancel the lookup. ──
  // dns.lookup in callback form wraps uv_getaddrinfo, a libuv-threadpool job with no
  // cancellation API. Losing the race abandons the RESULT; the query keeps its pool
  // slot until the OS resolver gives up on its own schedule. So this bounds how long
  // ONE caller waits, and bounds nothing about resource use — N stalled probes still
  // occupy N of the (default 4) threadpool slots and head-of-line-block every async
  // fs operation in the main process. The structural fix for that is the rejecting
  // single-flight on selfHosted:probeServer, which stops the second lookup from ever
  // being started. Do not read this race as a resource bound.
  //
  // A late answer cannot revive the caller: the race is already settled, and Promise
  // settlement is one-shot. Failing to unreachable is also the correct direction —
  // it is the same verdict a genuine resolution failure produces, so no dial is
  // authorized and no dialog is shown by a lookup we never saw the answer to.
  let timer: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<ResolveForDisplayResult>((resolve) => {
    timer = setTimeout(() => resolve({ ok: false, kind: 'unreachable' }), timeoutMs);
  });
  try {
    return await Promise.race([lookup, deadline]);
  } finally {
    if (timer) clearTimeout(timer); // do not hold the event loop open on the fast path
  }
}
