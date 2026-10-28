import { describe, expect, it } from 'vitest';
import {
  alsoFromCode,
  codeOverlapNote,
  formatExpiry,
  formatPrice,
  foundersAmount,
  intervalSuffix,
  planDisplayName,
  planChangeSummary,
  sourceDisplay,
  tierDisplayName,
} from '@/renderer/components/Settings/subscription/subscriptionCopy';

describe('subscriptionCopy', () => {
  it('maps wire tiers and sources to display copy, failing closed on unknown values', () => {
    expect(tierDisplayName('free')).toBe('Sonic');
    expect(tierDisplayName('premium')).toBe('Supersonic');
    expect(tierDisplayName('constructor')).toBe('Plan unavailable');
    expect(sourceDisplay('code')).toBe('Redeemed via code');
    expect(sourceDisplay(undefined)).toBeUndefined();
  });

  it('names sold plans and nothing else (#2033)', () => {
    expect(planDisplayName('supersonic')).toBe('Supersonic');
    expect(planDisplayName('wing')).toBe('Wing');
    expect(planDisplayName('hypersonic')).toBeUndefined();
    expect(planDisplayName('constructor')).toBeUndefined();
    expect(planDisplayName(null)).toBeUndefined();
  });

  it('formats dates and omits an unusable one', () => {
    expect(formatExpiry(undefined)).toBeUndefined();
    expect(formatExpiry('not a date')).toBeUndefined();
    expect(formatExpiry('2027-01-15T12:00:00Z')).toMatch(/2027/);
  });

  it('formats minor units as currency and rounds the founders price down', () => {
    expect(formatPrice(899, 'usd')).toBe(
      new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD' }).format(8.99)
    );
    expect(foundersAmount(899)).toBe(449);
    expect(foundersAmount(9889)).toBe(4944);
    expect(intervalSuffix('month')).toBe('/month');
    expect(intervalSuffix('year')).toBe('/year');
  });

  it('words the code lines with and without a date', () => {
    expect(codeOverlapNote('March 1, 2027')).toBe(
      'Your code already covers Supersonic until March 1, 2027. Subscribing starts billing today.'
    );
    expect(alsoFromCode(undefined)).toBe('Also Supersonic from a code.');
    expect(alsoFromCode('March 1, 2027')).toBe('Also Supersonic from a code until March 1, 2027.');
  });

  it('explains both mixed plan and interval changes', () => {
    expect(planChangeSummary('supersonic', 'year', 'wing', 'month')).toContain(
      'Wing yearly starts now'
    );
    expect(planChangeSummary('supersonic', 'year', 'wing', 'month')).toContain(
      'Wing monthly starts at the end'
    );
    expect(planChangeSummary('wing', 'month', 'supersonic', 'year')).toContain(
      'end of your current paid period'
    );
  });
});
