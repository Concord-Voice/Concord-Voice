/**
 * Pre-upgrade admission gate (#2032).
 *
 * Bounds what per-socket budgets structurally cannot: `socket.data` exists only
 * AFTER a socket is established, so buckets keyed there cannot limit connection
 * ESTABLISHMENT. This gate runs before the socket exists.
 *
 * Extracted from index.ts as a pure predicate for the same reason originGate.ts
 * was (`[internal]rules/media-plane.md`): logic inside the server-construction
 * block is not unit-testable.
 */

import { BlockList, isIPv4, isIPv6 } from 'node:net';

/** 60 handshakes per 10 s = 6/s sustained per source IP. */
export const HANDSHAKE_BURST = 60;
export const HANDSHAKE_WINDOW_MS = 10_000;
/** Hard cap on tracked IPs — the store must not become the DoS it prevents. */
export const MAX_TRACKED_IPS = 10_000;

export interface AdmissionRequest {
  headers: Record<string, string | string[] | undefined>;
  socket: { remoteAddress?: string };
}

export interface AdmissionGateOptions {
  now?: () => number;
  /**
   * Peer addresses or CIDR blocks whose forwarded headers may be trusted (the
   * nginx front). An EMPTY list makes the gate inert — see createAdmissionGate.
   */
  trustedProxies: readonly string[];
  /** PII-safe rejection hook — receives no address. */
  onReject?: () => void;
  /**
   * Sink for the one-shot configuration warnings emitted at construction.
   * Defaults to console.warn so the lib stays free of the winston/config import
   * chain; index.ts passes the service logger.
   */
  warn?: (message: string) => void;
}

/** A compiled trusted-proxy allowlist. `size === 0` means nothing is trusted. */
export interface TrustedProxies {
  /** Number of rules that parsed. Zero disables the gate entirely. */
  readonly size: number;
  /** True when `peer` falls inside a configured address or CIDR block. */
  readonly has: (peer: string) => boolean;
}

type IpFamily = 'ipv4' | 'ipv6';

function ipFamily(address: string): IpFamily | null {
  if (isIPv4(address)) return 'ipv4';
  if (isIPv6(address)) return 'ipv6';
  return null;
}

/**
 * Add one `TRUSTED_PROXIES` entry to `list`. Returns false when the entry is
 * malformed, so the caller can skip it rather than throw: a single typo must
 * not take the SFU down at startup.
 */
function addTrustedEntry(list: BlockList, entry: string): boolean {
  const slash = entry.lastIndexOf('/');

  try {
    if (slash === -1) {
      // A bare address is its own /32 (or /128) — BlockList.addAddress is
      // exactly that single-address rule.
      const family = ipFamily(entry);
      if (!family) return false;
      list.addAddress(entry, family);
      return true;
    }

    const address = entry.slice(0, slash);
    const prefixText = entry.slice(slash + 1);
    const family = ipFamily(address);
    if (!family) return false;
    if (!/^\d{1,3}$/.test(prefixText)) return false;
    const prefix = Number(prefixText);
    if (prefix > (family === 'ipv4' ? 32 : 128)) return false;
    // Reject an all-traffic rule (CWE-348). A /0 trusts EVERY source, which
    // makes X-Real-IP universally spoofable and the handshake ceiling trivially
    // evadable — the gate would authenticate the attacker's own claim about
    // where they came from. Mirrors the control-plane's explicit 0.0.0.0/0 and
    // ::/0 rejection in [internal]install-selfhost.sh, but guards on
    // the prefix length so every spelling is covered, not just those two
    // literals. Skipping (not throwing) keeps startup alive; a list that was
    // ONLY /0 entries compiles to size 0 and degrades to the inert gate.
    if (prefix === 0) return false;
    list.addSubnet(address, prefix, family);
    return true;
  } catch {
    // Defence in depth: node throws on inputs isIPv4/isIPv6 accepted but
    // BlockList rejects. Skipping keeps startup alive; an all-skipped list
    // degrades to the inert gate, never to a deployment-wide shared bucket.
    return false;
  }
}

/**
 * Compile the configured trusted-proxy entries into a matcher.
 *
 * Exact string equality cannot work here: nginx reaches the media-plane over a
 * docker bridge whose container address is not a stable literal, so the peer
 * has to be matched by RANGE. Mirrors the control-plane's `TRUSTED_PROXY_CIDRS`
 * (`services/control-plane/pkg/config/config.go:1155` `parseTrustedProxyCIDRs`
 * → gin `SetTrustedProxies`, `internal/api/router.go:131`) — with one deliberate
 * difference: the control-plane REJECTS a malformed entry at startup, while the
 * media-plane skips it. Go's guard exists because an empty control-plane list
 * silently regresses `c.ClientIP()`; here an empty list disables the gate
 * outright and is reported, so failing startup would trade a bounded capacity
 * loss for a voice outage.
 *
 * Accepts bare addresses (`10.0.0.9` → /32, `2001:db8::1` → /128) and CIDR
 * blocks, IPv4 and IPv6.
 */
export function compileTrustedProxies(
  entries: readonly string[],
  warn: (message: string) => void = defaultWarn
): TrustedProxies {
  const list = new BlockList();
  let size = 0;

  for (const [index, raw] of entries.entries()) {
    const entry = raw.trim();
    if (!entry) continue;
    if (addTrustedEntry(list, entry)) {
      size += 1;
    } else {
      // Report the POSITION, never the value. The entry is environment-derived
      // (TRUSTED_PROXIES), and echoing an env-derived string into a log sink is
      // what CodeQL's js/clear-text-logging flags: an operator who pastes the
      // wrong value into that variable would have it printed straight back. The
      // value is also an address, which `[internal]rules/observability.md` #2 asks
      // us to keep out of logs where avoidable — and echoing it would contradict
      // this gate's own posture, since it deliberately never logs a REJECTED
      // client IP. A 1-based position locates the typo in a comma-separated list
      // without reproducing anything the operator supplied.
      warn(
        `Ignoring invalid TRUSTED_PROXIES entry at position ${index + 1} — ` +
          'expected an IP address or a CIDR block.'
      );
    }
  }

  return {
    size,
    has: (peer: string) => {
      const address = peer.trim();
      const family = ipFamily(address);
      if (!family) return false;
      // The family MUST be passed: BlockList.check defaults to 'ipv4', which
      // would silently miss every IPv4-mapped IPv6 peer (`::ffff:172.18.0.5`,
      // the form docker commonly presents). Passing 'ipv6' makes node compare
      // such a peer against the IPv4 rules, as documented for net.BlockList.
      return list.check(address, family);
    },
  };
}

function defaultWarn(message: string): void {
  console.warn(message);
}

function firstHeader(value: string | string[] | undefined): string | undefined {
  if (Array.isArray(value)) return value[0];
  return value;
}

/**
 * Resolve the client address.
 *
 * nginx fronts the media-plane and sets X-Real-IP / X-Forwarded-For, so
 * `req.socket.remoteAddress` is the PROXY's address for every client. Trust the
 * forwarded headers only when the immediate peer is a known proxy — an
 * untrusted peer presenting X-Real-IP is spoofing.
 *
 * Returns null when nothing can be determined; callers MUST fail open on null.
 */
export function resolveClientIp(
  req: AdmissionRequest,
  trustedProxies: TrustedProxies
): string | null {
  const peer = req.socket?.remoteAddress;

  if (peer && trustedProxies.has(peer)) {
    // Only accept a header value that actually PARSES as an address. Production
    // trusts the whole RFC1918 space, so any container or LAN host that reaches
    // this port directly is inside the trusted set and could otherwise set an
    // arbitrary string as its own counter key — churning the bounded store and
    // evicting real entries. A non-address value falls through to
    // X-Forwarded-For, and then to the fail-open return below.
    const realIp = firstHeader(req.headers['x-real-ip'])?.trim();
    if (realIp && ipFamily(realIp)) return realIp;

    const forwarded = firstHeader(req.headers['x-forwarded-for']);
    if (forwarded) {
      const hops = forwarded
        .split(',')
        .map((h) => h.trim())
        .filter(Boolean);
      // $proxy_add_x_forwarded_for appends the peer, so the LAST hop is the
      // address nginx observed — the one we can actually attribute.
      const last = hops.at(-1);
      // Same parse guard as X-Real-IP above — validating only one of the two
      // headers would leave the identical arbitrary-key path open on the other.
      if (last && ipFamily(last)) return last;
    }

    // Trusted peer, but nothing attributable to the actual client: fail OPEN.
    //
    // Falling through to `peer` here would return the PROXY's address, so every
    // such request would land in ONE shared bucket and a handful of clients
    // would trip the 60/10s ceiling for the whole deployment — the exact
    // fail-shared outcome `createAdmissionGate` refuses when no proxy is
    // configured at all. Latent in the shipped topology (nginx.conf always sets
    // X-Real-IP), but a config change that dropped the header would otherwise
    // convert a header omission into a service-wide denial. Returning null
    // matches the documented posture for an unresolvable address: a
    // topology/operator error degrades to pre-gate behaviour, never to a
    // deployment-wide bucket. (#2793 Gitar review.)
    return null;
  }

  // Untrusted peer: it IS the client, so attribute the request to it directly.
  return peer ?? null;
}

interface WindowCounters {
  /** Alternating fixed-size windows — no unbounded timestamp arrays. */
  windowStartMs: number;
  current: number;
  previous: number;
}

/**
 * Callback shape of engine.io's `allowRequest` option
 * (`engine.io/build/server.d.ts`: `fn: (err: string | null | undefined,
 * success: boolean) => void`). The gate always passes `null` — on `success ===
 * false` engine.io answers FORBIDDEN and carries this value only as diagnostic
 * context, so it must not describe why the client was rejected.
 */
export type AdmissionCallback = (err: string | null | undefined, success: boolean) => void;

export interface AdmissionGate {
  (req: AdmissionRequest, callback: AdmissionCallback): void;
  trackedCount: () => number;
}

export function createAdmissionGate(options: AdmissionGateOptions): AdmissionGate {
  const now = options.now ?? Date.now;
  const warn = options.warn ?? defaultWarn;
  const trustedProxies = compileTrustedProxies(options.trustedProxies, warn);

  // Fail INERT, not fail-shared. With no trusted proxy configured every client
  // resolves to the nginx peer address, so a counting gate would put the whole
  // deployment in ONE bucket and deny voice service-wide on the first reconnect
  // storm. A misconfiguration must degrade to the pre-gate behaviour — the same
  // fail-open posture the design specifies for an unresolvable IP.
  if (trustedProxies.size === 0) {
    warn(
      'Admission gate INACTIVE: no trusted proxies configured (TRUSTED_PROXIES). ' +
        'Every client would otherwise be attributed to the reverse proxy and share ' +
        'one handshake budget, so the per-source-IP ceiling is disabled. Set ' +
        'TRUSTED_PROXIES to the CIDR the media-plane sees nginx on to enable it.'
    );
    const inert = ((_req, callback) => {
      callback(null, true);
    }) as AdmissionGate;
    inert.trackedCount = () => 0;
    return inert;
  }

  const counters = new Map<string, WindowCounters>();

  const gate = ((req, callback) => {
    const ip = resolveClientIp(req, trustedProxies);

    // Documented fail-open. See the admissionGate test that asserts this.
    if (!ip) {
      callback(null, true);
      return;
    }

    const nowMs = now();
    let entry = counters.get(ip);

    if (!entry) {
      // Bounded store: evict the oldest insertion (Map preserves insertion
      // order) rather than growing without limit under an IPv6-source flood.
      if (counters.size >= MAX_TRACKED_IPS) {
        const oldest = counters.keys().next();
        if (!oldest.done) counters.delete(oldest.value);
      }
      entry = { windowStartMs: nowMs, current: 0, previous: 0 };
      counters.set(ip, entry);
    }

    const elapsed = nowMs - entry.windowStartMs;
    if (elapsed >= HANDSHAKE_WINDOW_MS * 2) {
      entry.previous = 0;
      entry.current = 0;
      entry.windowStartMs = nowMs;
    } else if (elapsed >= HANDSHAKE_WINDOW_MS) {
      entry.previous = entry.current;
      entry.current = 0;
      entry.windowStartMs = nowMs;
    }

    if (entry.previous + entry.current >= HANDSHAKE_BURST) {
      options.onReject?.();
      callback(null, false);
      return;
    }

    entry.current += 1;
    callback(null, true);
  }) as AdmissionGate;

  gate.trackedCount = () => counters.size;
  return gate;
}
