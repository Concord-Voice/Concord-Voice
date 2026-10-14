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

// #3459: pinned messages are a qualifier on the SAME sentence, never a second one.
describe('expirationClause pin qualifier (#3459)', () => {
  const SENTENCE = 'Messages are Encrypted End-to-End';

  it('appends ", except pinned messages" only for a server-sent includePinned === false', () => {
    expect(expirationClause(policy({ includePinned: false }), 'ready')).toBe(
      ' and expire after 24 hours, except pinned messages'
    );
  });

  it.each([
    ['true', policy({ includePinned: true })],
    ['absent (a server that predates #3459)', policy()],
  ])('adds nothing when includePinned is %s', (_name, p) => {
    expect(expirationClause(p, 'ready')).toBe(' and expire after 24 hours');
  });

  it('stays ONE sentence: the whole composer string has a single terminal clause and no extra period', () => {
    const text = `${SENTENCE}${expirationClause(policy({ includePinned: false }), 'ready') ?? ''}`;
    expect(text).toBe(
      'Messages are Encrypted End-to-End and expire after 24 hours, except pinned messages'
    );
    // The issue's example ("... Pinned messages are kept.") was a second sentence; this is not.
    expect(text).not.toMatch(/\./);
  });

  it('keeps the backfill qualifier LAST, after the pin exception', () => {
    expect(expirationClause(policy({ includePinned: false, backfillPending: true }), 'ready')).toBe(
      ' and expire after 24 hours, except pinned messages · still processing older messages'
    );
  });

  it('says nothing about pins when there is no window to except them from', () => {
    expect(expirationClause(policy({ windowSeconds: null, includePinned: false }), 'ready')).toBe(
      ' and never expire'
    );
  });

  it('says NOTHING about retention for an unreadable policy, whatever it last claimed about pins', () => {
    expect(expirationClause(policy({ includePinned: false }), 'unavailable')).toBeNull();
    expect(expirationClause(null, 'unavailable')).toBeNull();
  });

  it('keeps the last known pin exception while a refresh is in flight', () => {
    expect(expirationClause(policy({ includePinned: false }), 'loading')).toBe(
      ' and expire after 24 hours, except pinned messages'
    );
  });
});
