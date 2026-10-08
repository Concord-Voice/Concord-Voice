import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __challengeIssuerCountForTests,
  __resetChallengeIssuersForTests,
  CHALLENGE_TTL_MS,
  challengeEntryIsLive,
  challengeIssuerFor,
  recordChallengeIssuer,
} from '@/renderer/services/system/challengeIssuer';
import type { RuntimeServerSelection } from '@/renderer/services/system/runtimeServerBase';

const SERVER_A: RuntimeServerSelection = { apiBase: 'https://a.test', epoch: 1 };
const SERVER_B: RuntimeServerSelection = { apiBase: 'https://b.test', epoch: 2 };
const T0 = new Date('2026-10-07T12:00:00Z').getTime();

describe('challengeIssuer', () => {
  beforeEach(() => {
    // Only Date: nothing here waits on a timer.
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(T0);
    __resetChallengeIssuersForTests();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('matches the server challenge TTL of five minutes', () => {
    expect(CHALLENGE_TTL_MS).toBe(5 * 60 * 1000);
  });

  it('has no issuer for a token nobody recorded, or for no token', () => {
    expect(challengeIssuerFor('never-recorded')).toBeNull();
    expect(challengeIssuerFor(null)).toBeNull();
    expect(challengeIssuerFor('')).toBeNull();
  });

  it('returns the recorded selection', () => {
    recordChallengeIssuer('tok', SERVER_A);
    expect(challengeIssuerFor('tok')).toBe(SERVER_A);
  });

  it('keeps the first record for a live challenge', () => {
    recordChallengeIssuer('tok', SERVER_A);
    vi.setSystemTime(T0 + 1000);
    recordChallengeIssuer('tok', SERVER_B);
    expect(challengeIssuerFor('tok')).toBe(SERVER_A);
  });

  describe('expiry', () => {
    it('still has the issuer just inside the TTL (control)', () => {
      recordChallengeIssuer('tok', SERVER_A);
      vi.setSystemTime(T0 + CHALLENGE_TTL_MS - 1);
      expect(challengeIssuerFor('tok')).toBe(SERVER_A);
    });

    it('has no issuer once the TTL has passed', () => {
      recordChallengeIssuer('tok', SERVER_A);
      vi.setSystemTime(T0 + CHALLENGE_TTL_MS);
      expect(challengeIssuerFor('tok')).toBeNull();
    });

    it('takes a new record for a token whose old one has expired', () => {
      recordChallengeIssuer('tok', SERVER_A);
      vi.setSystemTime(T0 + CHALLENGE_TTL_MS);
      recordChallengeIssuer('tok', SERVER_B);
      expect(challengeIssuerFor('tok')).toBe(SERVER_B);
    });

    it('prunes expired records when a new one is recorded', () => {
      recordChallengeIssuer('old-1', SERVER_A);
      recordChallengeIssuer('old-2', SERVER_A);
      vi.setSystemTime(T0 + CHALLENGE_TTL_MS);
      recordChallengeIssuer('new', SERVER_B);
      expect(__challengeIssuerCountForTests()).toBe(1);
      expect(challengeIssuerFor('new')).toBe(SERVER_B);
    });

    it('keeps live records when a new one is recorded (control)', () => {
      recordChallengeIssuer('live-1', SERVER_A);
      recordChallengeIssuer('live-2', SERVER_A);
      vi.setSystemTime(T0 + CHALLENGE_TTL_MS - 1);
      recordChallengeIssuer('new', SERVER_B);
      expect(__challengeIssuerCountForTests()).toBe(3);
      expect(challengeIssuerFor('live-1')).toBe(SERVER_A);
    });
  });

  it('counts an entry as live until exactly the TTL', () => {
    expect(challengeEntryIsLive(T0, T0 + CHALLENGE_TTL_MS - 1)).toBe(true);
    expect(challengeEntryIsLive(T0, T0 + CHALLENGE_TTL_MS)).toBe(false);
  });
});
