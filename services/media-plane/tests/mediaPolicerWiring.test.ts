import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * #2153 wiring contract for the media-rate policer.
 *
 * Every policer decision is unit-tested in mediaPolicer.test.ts,
 * mediaPolicerTick.test.ts and roomManager.test.ts. None of those tests can
 * see whether index.ts actually starts the tick, stops it on shutdown, routes a
 * client close through the final-sample path, or maps the new acks: index.ts
 * is coverage-excluded and never imported by a test. A handler that kept
 * calling `closeProducer` would leave every behavioural test green while the
 * churn bypass the slot buckets exist to close stayed open.
 *
 * So, like producerSupersession.test.ts, this file scans the real source. It
 * asserts presence and order of call sites, not behaviour.
 */

const SRC = join(__dirname, '..', 'src');

/** Source with comments stripped, so prose mentioning a call is not a call. */
function codeOf(relativePath: string): string {
  const raw = readFileSync(join(SRC, relativePath), 'utf8');
  return raw.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/[^\n]*/g, '');
}

function countOf(haystack: string, needle: string): number {
  return haystack.split(needle).length - 1;
}

/** The text from `start` up to (not including) the next `end` after it. */
function between(haystack: string, start: string, end: string): string {
  const from = haystack.indexOf(start);
  expect(from, `missing ${start}`).toBeGreaterThanOrEqual(0);
  const to = haystack.indexOf(end, from + start.length);
  expect(to, `missing ${end} after ${start}`).toBeGreaterThan(from);
  return haystack.slice(from, to);
}

/** The argument list of every `.emit('<event>', …)` call, by balanced parentheses. */
function emitArgs(haystack: string, event: string): string[] {
  const needle = `.emit('${event}'`;
  const calls: string[] = [];
  for (let at = haystack.indexOf(needle); at >= 0; at = haystack.indexOf(needle, at + 1)) {
    const open = at + '.emit'.length;
    let depth = 0;
    let close = open;
    for (; close < haystack.length; close++) {
      if (haystack[close] === '(') depth++;
      if (haystack[close] === ')' && --depth === 0) break;
    }
    calls.push(haystack.slice(open, close + 1));
  }
  return calls;
}

describe('media policer wiring contract (#2153)', () => {
  const index = codeOf('index.ts');

  it('constructs and starts exactly one tick before any socket can connect', () => {
    expect(countOf(index, 'createMediaPolicerTick(')).toBe(1);
    expect(countOf(index, 'mediaPolicerTick.start()')).toBe(1);

    const startedAt = index.indexOf('mediaPolicerTick.start()');
    // The gate, the clock and the final-sample sink are installed before the
    // tick starts, and all of it before the connection handler is registered.
    for (const setter of [
      'roomManager.setMonotonicClock(mediaPolicyClock)',
      'roomManager.setMediaPolicyGate(mediaPolicyLedger)',
      'roomManager.setIngressFinalizer(',
    ]) {
      const at = index.indexOf(setter);
      expect(at, setter).toBeGreaterThanOrEqual(0);
      expect(at, setter).toBeLessThan(startedAt);
    }
    expect(index.indexOf("io.on('connection'")).toBeGreaterThan(startedAt);
  });

  it('shares one monotonic clock between the RoomManager and the tick', () => {
    const construction = between(index, 'createMediaPolicerTick(', '});');
    expect(construction).toContain('now: mediaPolicyClock');
    expect(construction).toContain('ledger: mediaPolicyLedger');
    expect(construction).toContain('policer: mediaPolicer');
    expect(index).toContain('mediaPolicer.recordFinal(');
  });

  it('stops the tick in shutdown before the rooms are closed', () => {
    const shutdown = between(
      index,
      'const shutdown = (): Promise<void> => {',
      "process.on('SIGTERM'"
    );
    const stoppedAt = shutdown.indexOf('mediaPolicerTick.stop()');
    expect(stoppedAt).toBeGreaterThanOrEqual(0);
    expect(shutdown.indexOf('roomManager.closeAll()')).toBeGreaterThan(stoppedAt);
  });

  it('routes the client close-producer handler through the final-sample path', () => {
    const handler = between(index, "'close-producer'", 'withRateLimit(socket,');
    expect(handler).toContain('roomManager.closeProducerFromClient(');
    // No handler in index.ts may bypass it: every other closeProducer caller is
    // inside RoomManager (spec 5.6).
    expect(countOf(index, 'roomManager.closeProducer(')).toBe(0);
  });

  it('maps the latched resume, the produce cooldown and the join cooldown acks', () => {
    const resume = between(index, "'resume-producer'", 'withRateLimit(socket,');
    expect(resume).toContain("outcome === 'media_policy_paused'");
    expect(resume).toContain('callback(MEDIA_POLICY_PAUSED_RESUME_ACK)');
    // The refusal returns before the room hears producer-resumed.
    expect(resume.indexOf('MEDIA_POLICY_PAUSED_RESUME_ACK')).toBeLessThan(
      resume.indexOf("emit('producer-resumed'")
    );

    const produce = between(index, "'produce',", "'consume'");
    expect(produce).toContain('error instanceof MediaPolicyCooldownError');
    expect(produce).toContain('mediaPolicyCooldownAck(error)');

    const joinAck = between(index, 'function joinErrorAck(', 'function emitJoinError(');
    expect(joinAck).toContain('error instanceof MediaPolicyCooldownError');
    expect(joinAck).toContain('mediaPolicyCooldownAck(error)');
    const emitJoin = between(index, 'function emitJoinError(', 'function getKeyframeSenderUserId(');
    expect(emitJoin).toContain('joinErrorAck(error)');
  });

  it('keeps the NATS force-disconnect an access revocation', () => {
    const subscription = between(
      index,
      "'voice.enforce.disconnect'",
      "'voice.enforce.permissions'"
    );
    expect(subscription).toContain("{ reason: 'access_revoked' }");
    expect(subscription).not.toContain('media_policy');
  });

  it('puts kind and source on every producer-paused and producer-resumed emit', () => {
    const sites = [...emitArgs(index, 'producer-paused'), ...emitArgs(index, 'producer-resumed')];
    // index.ts:835 (user mute), :1331 (pause-producer), :1366 (resume-producer).
    // The policer's own pause is emitted from lib/mediaPolicerTick.ts.
    expect(sites).toHaveLength(3);
    for (const args of sites) {
      expect(args).toMatch(/\bkind\b/);
      expect(args).toMatch(/\bsource\b/);
    }
  });
});
