import { describe, it, expect, vi } from 'vitest';

import {
  compileTrustedProxies,
  createAdmissionGate,
  resolveClientIp,
  HANDSHAKE_BURST,
  MAX_TRACKED_IPS,
  type AdmissionRequest,
} from '../src/lib/admissionGate.js';

/** Compile without a warn sink for the cases where no warning is expected. */
function trust(...entries: string[]) {
  return compileTrustedProxies(entries, () => {});
}

function makeClock(startMs = 1_000_000) {
  let now = startMs;
  return {
    now: () => now,
    advance: (ms: number) => {
      now += ms;
    },
  };
}

function req(
  headers: Record<string, string | undefined>,
  remoteAddress = '10.0.0.9'
): AdmissionRequest {
  return { headers, socket: { remoteAddress } } as AdmissionRequest;
}

/**
 * A request whose peer address is genuinely absent (socket destroyed before the
 * handshake is verified, or a non-TCP transport). Deliberately a separate helper
 * rather than `req(headers, undefined)`: an explicitly-passed `undefined`
 * TRIGGERS the default parameter above, so that form yields '10.0.0.9' and the
 * unresolvable-address tests would pass without ever exercising the path.
 */
function peerlessReq(headers: Record<string, string | undefined>): AdmissionRequest {
  return { headers, socket: {} } as AdmissionRequest;
}

describe('resolveClientIp', () => {
  it('prefers X-Real-IP when the peer is the trusted proxy', () => {
    expect(resolveClientIp(req({ 'x-real-ip': '203.0.113.7' }), trust('10.0.0.9'))).toBe(
      '203.0.113.7'
    );
  });

  it('falls back to the last hop of X-Forwarded-For', () => {
    const r = req({ 'x-forwarded-for': '198.51.100.1, 203.0.113.7' });
    expect(resolveClientIp(r, trust('10.0.0.9'))).toBe('203.0.113.7');
  });

  it('IGNORES forwarded headers from an untrusted peer (spoofing defence)', () => {
    const r = req({ 'x-real-ip': '1.2.3.4' }, '198.51.100.55');
    expect(resolveClientIp(r, trust('10.0.0.9'))).toBe('198.51.100.55');
  });

  it('returns null when no address can be determined', () => {
    expect(resolveClientIp(peerlessReq({}), trust('10.0.0.9'))).toBeNull();
  });

  // Production trusts the whole RFC1918 space, so every container and LAN host
  // that reaches this port directly is INSIDE the trusted set. Without a parse
  // guard each one could name its own counter key and churn the bounded store
  // (#2032 red-team FIX 4). Defence in depth — nginx always sets a real address.
  // Behaviour change (#2793 Gitar review): a TRUSTED peer with no attributable
  // client address now fails OPEN rather than falling through to the proxy's own
  // address. The old form put every such request in one shared bucket, so a
  // handful of clients could trip the ceiling deployment-wide — the exact
  // fail-shared outcome createAdmissionGate refuses when no proxy is configured.
  it('fails OPEN when a trusted peer sends an X-Real-IP that is not an address', () => {
    const r = req({ 'x-real-ip': 'not-an-ip' });
    expect(resolveClientIp(r, trust('10.0.0.9'))).toBeNull();
  });

  it('ignores an unparseable X-Real-IP but still honours a valid X-Forwarded-For', () => {
    const r = req({ 'x-real-ip': 'attacker-chosen-key', 'x-forwarded-for': '203.0.113.7' });
    expect(resolveClientIp(r, trust('10.0.0.9'))).toBe('203.0.113.7');
  });

  it('fails OPEN when the X-Forwarded-For last hop is not an address', () => {
    const r = req({ 'x-forwarded-for': '203.0.113.7, not-an-ip' });
    expect(resolveClientIp(r, trust('10.0.0.9'))).toBeNull();
  });

  it('fails OPEN when a trusted peer forwards no client headers at all', () => {
    expect(resolveClientIp(req({}), trust('10.0.0.9'))).toBeNull();
  });

  // The untrusted case is unchanged and must stay that way: an untrusted peer
  // IS the client, so it is attributed directly rather than failing open.
  it('still attributes an UNTRUSTED peer to its own address', () => {
    expect(resolveClientIp(req({}, '198.51.100.55'), trust('10.0.0.9'))).toBe('198.51.100.55');
  });

  it('still accepts an IPv6 X-Real-IP', () => {
    const r = req({ 'x-real-ip': '2001:db8::1' });
    expect(resolveClientIp(r, trust('10.0.0.9'))).toBe('2001:db8::1');
  });
});

// A docker bridge address is not a stable literal, so exact-match trust cannot
// work here — these lock the CIDR semantics that replaced it.
describe('compileTrustedProxies', () => {
  it('trusts a peer inside a configured CIDR block', () => {
    const r = req({ 'x-real-ip': '203.0.113.7' }, '172.18.0.5');
    expect(resolveClientIp(r, trust('172.16.0.0/12'))).toBe('203.0.113.7');
  });

  it('does NOT trust a peer outside the block (forwarded headers ignored)', () => {
    // 172.32.0.1 is one range past the end of 172.16.0.0/12 — an exact-prefix
    // string comparison ('172.' / '172.3') would wrongly accept it.
    const r = req({ 'x-real-ip': '1.2.3.4' }, '172.32.0.1');
    expect(resolveClientIp(r, trust('172.16.0.0/12'))).toBe('172.32.0.1');
  });

  it('treats a bare address as a single-host rule (/32)', () => {
    expect(resolveClientIp(req({ 'x-real-ip': '203.0.113.7' }), trust('10.0.0.9'))).toBe(
      '203.0.113.7'
    );
    const neighbour = req({ 'x-real-ip': '1.2.3.4' }, '10.0.0.10');
    expect(resolveClientIp(neighbour, trust('10.0.0.9'))).toBe('10.0.0.10');
  });

  it('matches an IPv4-mapped IPv6 peer against an IPv4 rule', () => {
    // The shape docker actually presents when node listens on '::' — missing it
    // silently reverts the whole gate to the pre-Task-2b broken behaviour.
    const r = req({ 'x-real-ip': '203.0.113.7' }, '::ffff:172.18.0.5');
    expect(resolveClientIp(r, trust('172.16.0.0/12'))).toBe('203.0.113.7');

    // Outside the block the mapped peer is still untrusted, and the bucket key
    // is the peer verbatim (the resolver never rewrites an address it keeps).
    const outside = req({ 'x-real-ip': '1.2.3.4' }, '::ffff:172.32.0.1');
    expect(resolveClientIp(outside, trust('172.16.0.0/12'))).toBe('::ffff:172.32.0.1');
  });

  it('matches IPv6 blocks and bare IPv6 addresses', () => {
    const inBlock = req({ 'x-real-ip': '203.0.113.7' }, '2001:db8::1');
    expect(resolveClientIp(inBlock, trust('2001:db8::/32'))).toBe('203.0.113.7');
    expect(resolveClientIp(inBlock, trust('2001:db8::1'))).toBe('203.0.113.7');

    const outBlock = req({ 'x-real-ip': '1.2.3.4' }, '2001:db9::1');
    expect(resolveClientIp(outBlock, trust('2001:db8::/32'))).toBe('2001:db9::1');
  });

  it('skips malformed entries without throwing and keeps the valid ones', () => {
    const warn = vi.fn();
    let compiled!: ReturnType<typeof compileTrustedProxies>;
    expect(() => {
      compiled = compileTrustedProxies(
        ['not-a-cidr', '172.16.0.0/99', '10.0.0.0/', '/12', '', '172.16.0.0/12'],
        warn
      );
    }).not.toThrow();

    expect(compiled.size).toBe(1);
    // The empty entry is skipped silently; the four malformed ones are reported.
    expect(warn).toHaveBeenCalledTimes(4);
    const r = req({ 'x-real-ip': '203.0.113.7' }, '172.18.0.5');
    expect(resolveClientIp(r, compiled)).toBe('203.0.113.7');
  });

  // CodeQL js/clear-text-logging (high) flagged the previous form, which echoed
  // the rejected entry verbatim. The entry comes from TRUSTED_PROXIES, so an
  // operator who pastes the wrong value into that variable would have it printed
  // straight back into a log sink — and it is an address besides, which
  // `[internal]rules/observability.md` #2 asks us to keep out of logs. The gate
  // never logs a REJECTED client IP; it must not log a configured proxy one.
  it('reports a malformed entry by POSITION and never echoes its value', () => {
    const warn = vi.fn();
    compileTrustedProxies(['172.16.0.0/12', 'super-secret-pasted-value'], warn);

    expect(warn).toHaveBeenCalledTimes(1);
    const message = warn.mock.calls[0][0] as string;
    expect(message).not.toContain('super-secret-pasted-value');
    expect(message).toContain('position 2');
  });

  it('yields an empty list when EVERY entry is malformed', () => {
    expect(compileTrustedProxies(['not-a-cidr', '999.999.999.999/8'], () => {}).size).toBe(0);
  });

  // CWE-348. A /0 trusts every source, so X-Real-IP becomes universally
  // spoofable and the handshake ceiling is trivially evaded — the gate would be
  // authenticating the attacker's own claim about where they came from. The
  // control-plane rejects 0.0.0.0/0 and ::/0 explicitly
  // ([internal]install-selfhost.sh); this guards on the prefix
  // length so every all-traffic spelling is covered, not just those literals.
  it('rejects an all-traffic /0 rule in any spelling', () => {
    const warn = vi.fn();
    const compiled = compileTrustedProxies(['0.0.0.0/0', '::/0', '10.0.0.0/0'], warn);

    expect(compiled.size).toBe(0);
    expect(warn).toHaveBeenCalledTimes(3);
  });

  it('keeps valid entries when a /0 is mixed in, and does not trust via the /0', () => {
    const compiled = compileTrustedProxies(['0.0.0.0/0', '172.16.0.0/12'], () => {});
    expect(compiled.size).toBe(1);

    // Inside the legitimate block → forwarded header honoured.
    expect(resolveClientIp(req({ 'x-real-ip': '203.0.113.7' }, '172.18.0.5'), compiled)).toBe(
      '203.0.113.7'
    );
    // A peer that ONLY the discarded /0 would have covered must not be trusted:
    // its forwarded header is ignored and the peer address is used instead.
    expect(resolveClientIp(req({ 'x-real-ip': '203.0.113.7' }, '198.51.100.55'), compiled)).toBe(
      '198.51.100.55'
    );
  });
});

describe('createAdmissionGate', () => {
  it('allows requests under the ceiling', () => {
    const clock = makeClock();
    const gate = createAdmissionGate({ now: clock.now, trustedProxies: ['10.0.0.9'] });
    const cb = vi.fn();
    for (let i = 0; i < HANDSHAKE_BURST; i += 1) {
      gate(req({ 'x-real-ip': '203.0.113.7' }), cb);
    }
    expect(cb).toHaveBeenCalledTimes(HANDSHAKE_BURST);
    expect(cb).toHaveBeenLastCalledWith(null, true);
  });

  it('denies once the ceiling is exceeded', () => {
    const clock = makeClock();
    const gate = createAdmissionGate({ now: clock.now, trustedProxies: ['10.0.0.9'] });
    const cb = vi.fn();
    for (let i = 0; i < HANDSHAKE_BURST + 1; i += 1) {
      gate(req({ 'x-real-ip': '203.0.113.7' }), cb);
    }
    expect(cb).toHaveBeenLastCalledWith(null, false);
  });

  it('re-allows after the window slides', () => {
    const clock = makeClock();
    const gate = createAdmissionGate({ now: clock.now, trustedProxies: ['10.0.0.9'] });
    const cb = vi.fn();
    for (let i = 0; i < HANDSHAKE_BURST + 1; i += 1) {
      gate(req({ 'x-real-ip': '203.0.113.7' }), cb);
    }
    clock.advance(60_000);
    gate(req({ 'x-real-ip': '203.0.113.7' }), cb);
    expect(cb).toHaveBeenLastCalledWith(null, true);
  });

  // The store uses two alternating fixed windows, so `previous + current` is the
  // count that matters. The slide test above advances past BOTH windows and hits
  // the full-reset branch; this covers the PARTIAL slide, where current becomes
  // previous and the budget is only partly reclaimed. (CodeRabbit #2793.)
  it('reclaims only part of the budget on a partial window slide', () => {
    const clock = makeClock();
    const gate = createAdmissionGate({ now: clock.now, trustedProxies: ['10.0.0.9'] });
    const cb = vi.fn();
    const hit = () => gate(req({ 'x-real-ip': '203.0.113.7' }), cb);

    // Fill the first window completely.
    for (let i = 0; i < HANDSHAKE_BURST; i += 1) hit();
    hit();
    expect(cb).toHaveBeenLastCalledWith(null, false);

    // Slide ONE window: the full count moves to `previous`, so the ceiling is
    // still met and the caller stays denied — not reset.
    clock.advance(11_000);
    hit();
    expect(cb).toHaveBeenLastCalledWith(null, false);

    // Slide a second window: `previous` finally ages out and the caller is
    // admitted again.
    clock.advance(11_000);
    hit();
    expect(cb).toHaveBeenLastCalledWith(null, true);
  });

  it('keys separate IPs independently', () => {
    const clock = makeClock();
    const gate = createAdmissionGate({ now: clock.now, trustedProxies: ['10.0.0.9'] });
    const cb = vi.fn();
    for (let i = 0; i < HANDSHAKE_BURST + 1; i += 1) {
      gate(req({ 'x-real-ip': '203.0.113.7' }), cb);
    }
    gate(req({ 'x-real-ip': '203.0.113.8' }), cb);
    expect(cb).toHaveBeenLastCalledWith(null, true);
  });

  // DELIBERATE ASSERTION: fail-open is the specified behaviour here and nowhere
  // else in #2032. A mis-resolved IP is an operator/topology error; failing
  // closed on it is a total outage against a threat the rate and structural
  // layers still bound. A future "hardening" change must argue with this test.
  it('ALLOWS when the client IP cannot be resolved (documented fail-open)', () => {
    const clock = makeClock();
    const gate = createAdmissionGate({ now: clock.now, trustedProxies: ['10.0.0.9'] });
    const cb = vi.fn();
    gate(peerlessReq({}), cb);
    expect(cb).toHaveBeenCalledWith(null, true);
    // Falsifiability: an unresolvable address must short-circuit BEFORE the
    // counter store, so the allow cannot be the ordinary under-ceiling allow.
    expect(gate.trackedCount()).toBe(0);
  });

  // Fail INERT, not fail-shared. With no trusted proxy configured every client
  // resolves to the nginx peer address, so a counting gate would deny the whole
  // deployment on one reconnect storm. The misconfiguration must degrade to the
  // pre-gate behaviour instead.
  describe('with no trusted proxies configured', () => {
    it('allows unconditionally and tracks nothing', () => {
      const clock = makeClock();
      const gate = createAdmissionGate({ now: clock.now, trustedProxies: [], warn: () => {} });
      const cb = vi.fn();
      const onReject = vi.fn();
      const gateWithReject = createAdmissionGate({
        now: clock.now,
        trustedProxies: [],
        onReject,
        warn: () => {},
      });

      // Far past the ceiling, from one resolvable peer — the exact shape that
      // would otherwise share a single deployment-wide bucket.
      for (let i = 0; i < HANDSHAKE_BURST * 3; i += 1) {
        gate(req({}, '172.18.0.5'), cb);
        gateWithReject(req({}, '172.18.0.5'), () => {});
      }

      expect(cb).toHaveBeenCalledTimes(HANDSHAKE_BURST * 3);
      expect(cb.mock.calls.every(([err, allow]) => err === null && allow === true)).toBe(true);
      expect(gate.trackedCount()).toBe(0);
      expect(onReject).not.toHaveBeenCalled();
    });

    it('warns once at construction that the gate is inactive', () => {
      const warn = vi.fn();
      const gate = createAdmissionGate({ trustedProxies: [], warn });

      expect(warn).toHaveBeenCalledTimes(1);
      expect(warn.mock.calls[0][0]).toContain('TRUSTED_PROXIES');
      expect(warn.mock.calls[0][0]).toContain('INACTIVE');

      // Not re-emitted per request — this is a startup diagnostic, not a hot-path log.
      gate(req({}, '172.18.0.5'), vi.fn());
      expect(warn).toHaveBeenCalledTimes(1);
    });

    it('is inert when every configured entry is malformed', () => {
      const warn = vi.fn();
      const gate = createAdmissionGate({ trustedProxies: ['not-a-cidr'], warn });
      const cb = vi.fn();
      for (let i = 0; i < HANDSHAKE_BURST + 1; i += 1) {
        gate(req({}, '172.18.0.5'), cb);
      }
      expect(cb).toHaveBeenLastCalledWith(null, true);
      expect(gate.trackedCount()).toBe(0);
    });
  });

  it('bounds its own store and allows on eviction', () => {
    const clock = makeClock();
    const gate = createAdmissionGate({ now: clock.now, trustedProxies: ['10.0.0.9'] });
    const cb = vi.fn();
    for (let i = 0; i < MAX_TRACKED_IPS + 5000; i += 1) {
      gate(req({ 'x-real-ip': `203.0.${Math.floor(i / 256)}.${i % 256}` }), cb);
    }
    expect(gate.trackedCount()).toBeLessThanOrEqual(MAX_TRACKED_IPS);
    expect(cb).toHaveBeenLastCalledWith(null, true);
  });
});
