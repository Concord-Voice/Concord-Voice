import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Camera replace-in-place is a TWO-STEP contract: `roomManager.produce()`
 * creates the producer, and `roomManager.supersedeOlderCameraProducers()` closes
 * the ones it replaced. The steps are deliberately separate — the eviction must
 * land AFTER the handler has announced the new producer, or every remote client
 * sees `producer-closed` before `new-producer` and observes the publisher at
 * zero cameras for a round trip (CodeRabbit review, PR #2824).
 *
 * A two-step contract is a contract the next caller can forget, so this file is
 * the enforcement. It scans the real source rather than exercising behaviour:
 * the behavioural tests live in roomManager.test.ts and compose the two steps
 * themselves, which means they would keep passing even if the handler stopped
 * calling the second one. This is the test that fails in that case.
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

describe('camera supersession call-site contract', () => {
  const index = codeOf('index.ts');

  it('pairs every roomManager.produce() call site with a supersede call', () => {
    const produces = countOf(index, 'roomManager.produce(');
    const supersedes = countOf(index, 'roomManager.supersedeOlderCameraProducers(');

    // Equality is the cheap form of "every produce site is followed by a
    // supersede". It is exact while there is one call site, and it fails loudly
    // the moment someone adds a second produce without the follow-up.
    expect(produces).toBeGreaterThan(0);
    expect(supersedes).toBe(produces);
  });

  it('evicts only AFTER the new producer is announced to the room', () => {
    const producedAt = index.indexOf('roomManager.produce(');
    const announcedAt = index.indexOf("emit('new-producer'");
    const supersededAt = index.indexOf('roomManager.supersedeOlderCameraProducers(');

    expect(producedAt).toBeGreaterThanOrEqual(0);
    expect(announcedAt).toBeGreaterThan(producedAt);
    // The ordering this whole split exists to preserve. Moving the supersede
    // call above the emit puts the removal on the wire before the addition.
    expect(supersededAt).toBeGreaterThan(announcedAt);
  });

  it('does not evict inside roomManager.produce()', () => {
    const roomManager = codeOf('lib/roomManager.ts');
    const produceBody = roomManager.slice(
      roomManager.indexOf('async produce('),
      roomManager.indexOf('async supersedeOlderCameraProducers(')
    );

    expect(produceBody).not.toContain('supersedeOlderCameraProducers(');
  });
});
