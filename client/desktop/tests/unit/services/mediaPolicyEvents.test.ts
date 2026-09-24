// client/desktop/tests/unit/services/mediaPolicyEvents.test.ts
import { describe, it, expect } from 'vitest';
import {
  RETRY_AFTER_SEC_MAX,
  formatRejoinTime,
  parseForceDisconnect,
  parseMediaPolicyNotice,
  parseProducerStateChange,
  parseRetryAfterSec,
  rejoinAtFrom,
  resolvePausedSource,
  roundUpToMinute,
  type MediaPolicySource,
} from '@/renderer/services/voice/mediaPolicyEvents';

const NOTICE = { producerId: 'p-1', kind: 'audio', source: 'mic', action: 'paused' } as const;

describe('parseMediaPolicyNotice (closed enums, spec A2 / R9)', () => {
  it('accepts the one shape the media plane sends', () => {
    expect(parseMediaPolicyNotice(NOTICE)).toEqual(NOTICE);
  });

  it.each([
    [
      'action closed (R9: the pause-failure close sends no notice)',
      { ...NOTICE, action: 'closed' },
    ],
    ['action cleared (R10: no cleared action exists)', { ...NOTICE, action: 'cleared' }],
    ['unknown source', { ...NOTICE, source: 'hologram' }],
    ['unknown kind', { ...NOTICE, kind: 'data' }],
    ['empty producerId', { ...NOTICE, producerId: '' }],
    ['missing action', { producerId: 'p-1', kind: 'audio', source: 'mic' }],
    ['not an object', 'paused'],
    ['null', null],
  ])('rejects %s', (_name, payload) => {
    expect(parseMediaPolicyNotice(payload)).toBeNull();
  });
});

describe('parseProducerStateChange (legacy-compatible, handoff §1b)', () => {
  it('keeps kind and source when the media plane is new', () => {
    expect(
      parseProducerStateChange({ producerId: 'p', userId: 'u', kind: 'video', source: 'camera' })
    ).toEqual({ producerId: 'p', userId: 'u', kind: 'video', source: 'camera' });
  });

  it('degrades an unknown kind and source to absent instead of rejecting the event', () => {
    const parsed = parseProducerStateChange({
      producerId: 'p',
      userId: 'u',
      kind: 'data',
      source: 'hologram',
    });
    expect(parsed).not.toBeNull();
    expect(parsed?.kind).toBeUndefined();
    expect(parsed?.source).toBeUndefined();
  });

  it('accepts the legacy two-field payload from an old media plane', () => {
    expect(parseProducerStateChange({ producerId: 'p', userId: 'u' })).toEqual({
      producerId: 'p',
      userId: 'u',
    });
  });

  it.each([
    ['missing userId', { producerId: 'p' }],
    ['empty producerId', { producerId: '', userId: 'u' }],
  ])('rejects %s', (_name, payload) => {
    expect(parseProducerStateChange(payload)).toBeNull();
  });
});

describe('parseRetryAfterSec — finite, in (0, 86400] (A2/A3)', () => {
  it.each([
    [1, 1],
    [0.5, 0.5],
    [900, 900],
    [RETRY_AFTER_SEC_MAX, RETRY_AFTER_SEC_MAX],
  ])('accepts %s', (value, expected) => {
    expect(parseRetryAfterSec(value)).toBe(expected);
  });

  it.each([
    ['zero', 0],
    ['negative', -5],
    ['one past the cap', RETRY_AFTER_SEC_MAX + 1],
    ['NaN', Number.NaN],
    ['Infinity', Number.POSITIVE_INFINITY],
    ['numeric string', '600'],
    ['undefined', undefined],
    ['null', null],
  ])('treats %s as absent', (_name, value) => {
    expect(parseRetryAfterSec(value)).toBeNull();
  });
});

describe('parseForceDisconnect', () => {
  it('carries a valid retryAfterSec for media_policy', () => {
    expect(
      parseForceDisconnect({ channelId: 'ch', reason: 'media_policy', retryAfterSec: 900 })
    ).toEqual({ channelId: 'ch', reason: 'media_policy', retryAfterSec: 900 });
  });

  it('keeps the event but drops an out-of-range retryAfterSec (A2: treated as absent)', () => {
    expect(
      parseForceDisconnect({ channelId: 'ch', reason: 'media_policy', retryAfterSec: 1e9 })
    ).toEqual({ channelId: 'ch', reason: 'media_policy', retryAfterSec: null });
  });

  it('parses the access_revoked payload with no retryAfterSec', () => {
    expect(parseForceDisconnect({ channelId: 'ch', reason: 'access_revoked' })).toEqual({
      channelId: 'ch',
      reason: 'access_revoked',
      retryAfterSec: null,
    });
  });

  it.each([
    ['unknown reason', { channelId: 'ch', reason: 'kicked' }],
    ['missing reason', { channelId: 'ch' }],
    ['missing channelId', { reason: 'media_policy', retryAfterSec: 60 }],
  ])('rejects %s', (_name, payload) => {
    expect(parseForceDisconnect(payload)).toBeNull();
  });
});

describe('resolvePausedSource', () => {
  const local = new Map<string, MediaPolicySource>([['cam-1', 'camera']]);

  it('prefers the payload source', () => {
    expect(resolvePausedSource({ producerId: 'cam-1', userId: 'u', source: 'screen' }, local)).toBe(
      'screen'
    );
  });

  it('falls back to the local producer/consumer map for an old media plane', () => {
    expect(resolvePausedSource({ producerId: 'cam-1', userId: 'u' }, local)).toBe('camera');
  });

  it('returns null when neither knows (the caller then applies legacy isMuted)', () => {
    expect(resolvePausedSource({ producerId: 'x', userId: 'u' }, local)).toBeNull();
  });
});

describe('rejoin time', () => {
  it('rejoinAtFrom maps null to null and seconds to an absolute ms instant', () => {
    expect(rejoinAtFrom(null, 1_000)).toBeNull();
    expect(rejoinAtFrom(900, 1_000)).toBe(901_000);
  });

  it('rounds UP to the next whole minute, never down', () => {
    const at = Date.UTC(2026, 8, 23, 15, 44, 1);
    expect(roundUpToMinute(at)).toBe(Date.UTC(2026, 8, 23, 15, 45, 0));
  });

  it('leaves an exact minute where it is', () => {
    const at = Date.UTC(2026, 8, 23, 15, 45, 0);
    expect(roundUpToMinute(at)).toBe(at);
  });

  it('formats the rounded instant as a short wall-clock time in the user locale', () => {
    const at = Date.UTC(2026, 8, 23, 15, 44, 1);
    const expected = new Intl.DateTimeFormat(undefined, { timeStyle: 'short' }).format(
      new Date(Date.UTC(2026, 8, 23, 15, 45, 0))
    );
    expect(formatRejoinTime(at)).toBe(expected);
  });
});
