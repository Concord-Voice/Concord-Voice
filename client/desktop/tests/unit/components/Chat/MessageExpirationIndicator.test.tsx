import { describe, it, expect } from 'vitest';
import { expirationClause } from '../../../../src/renderer/components/Chat/MessageExpirationIndicator';
import type { ExpirationPolicy } from '../../../../src/renderer/services/messaging/expirationPolicyApi';

const policy = (over: Partial<ExpirationPolicy> = {}): ExpirationPolicy =>
  ({
    windowSeconds: 86400,
    updatedAt: null,
    revision: 1,
    backfillPending: false,
    ...over,
  }) as ExpirationPolicy;

describe('expirationClause', () => {
  it('names the window as a clause that completes the encryption sentence', () => {
    expect(expirationClause(policy(), 'ready')).toBe(' and expire after 24 hours');
  });

  it('says messages never expire when the policy is read and no window is set', () => {
    expect(expirationClause(policy({ windowSeconds: null }), 'ready')).toBe(' and never expire');
  });

  // The load-bearing one. `unavailable` means the policy could not be READ, which is not the
  // same fact as "no window is set" — and the two would otherwise collapse onto the same
  // reassuring sentence. Claiming permanence out of a failed lookup is the failure mode this
  // whole function exists to avoid, so it is asserted against the exact string that would be
  // wrong rather than merely against null.
  it('says NOTHING about retention when the policy could not be read', () => {
    expect(expirationClause(policy(), 'unavailable')).toBeNull();
    expect(expirationClause(policy({ windowSeconds: null }), 'unavailable')).toBeNull();
    expect(expirationClause(null, 'unavailable')).toBeNull();
    expect(expirationClause(policy(), 'unavailable')).not.toBe(' and never expire');
  });

  // `onRefresh` sets `loading` before every fetch, including the one `connection-recovered`
  // triggers. Gating on state would blink the retention claim out and back on each reconnect.
  it('keeps the last known window while a refresh is in flight', () => {
    expect(expirationClause(policy(), 'loading')).toBe(' and expire after 24 hours');
  });

  it('folds an in-flight backfill into the same clause rather than a second notice', () => {
    expect(expirationClause(policy({ backfillPending: true }), 'ready')).toBe(
      ' and expire after 24 hours · still processing older messages'
    );
  });

  it('returns a leading-space clause so the caller concatenates without adding glue', () => {
    const clause = expirationClause(policy(), 'ready');
    expect(clause?.startsWith(' and ')).toBe(true);
    expect(`Messages are Encrypted End-to-End${clause ?? ''}`).toBe(
      'Messages are Encrypted End-to-End and expire after 24 hours'
    );
  });

  it('stays silent on an unrecognised window rather than naming a raw number', () => {
    expect(expirationClause(policy({ windowSeconds: 12345 as never }), 'ready')).toBeNull();
  });
});
