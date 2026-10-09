import { describe, expect, it } from 'vitest';
import {
  addCalendarMonthsUTC,
  addElapsedSeconds,
} from '../../../src/renderer/utils/time/calendarMonths';

describe('addCalendarMonthsUTC', () => {
  it.each([
    ['2025-11-30T16:45:00.000Z', 3, '2026-02-28T16:45:00.000Z'],
    ['2023-08-31T09:15:00.000Z', 6, '2024-02-29T09:15:00.000Z'],
    ['2026-01-15T23:59:00.000Z', 6, '2026-07-15T23:59:00.000Z'],
  ])('clamps %s plus %i months to %s', (start, months, expected) => {
    expect(addCalendarMonthsUTC(start, months)?.toISOString()).toBe(expected);
  });

  it.each([
    ['malformed date', 'not-a-date', 6],
    ['zero months', '2026-01-31T23:00:00.000Z', 0],
    ['negative months', '2026-01-31T23:00:00.000Z', -1],
    ['fractional months', '2026-01-31T23:00:00.000Z', 1.5],
    ['nonfinite months', '2026-01-31T23:00:00.000Z', Infinity],
    ['calendar overflow', '2026-01-31T23:00:00.000Z', Number.MAX_SAFE_INTEGER],
  ])('returns null for %s', (_name, start, months) => {
    expect(addCalendarMonthsUTC(start, months)).toBeNull();
  });
});

describe('addElapsedSeconds', () => {
  it('preserves the fixed-day cadence published by an older server', () => {
    expect(addElapsedSeconds('2026-01-31T23:00:00.000Z', 91 * 86_400)?.toISOString()).toBe(
      '2026-05-02T23:00:00.000Z'
    );
  });

  it.each([
    ['malformed date', 'not-a-date', 91 * 86_400],
    ['zero seconds', '2026-01-31T23:00:00.000Z', 0],
    ['negative seconds', '2026-01-31T23:00:00.000Z', -1],
    ['fractional seconds', '2026-01-31T23:00:00.000Z', 1.5],
    ['nonfinite seconds', '2026-01-31T23:00:00.000Z', Infinity],
    ['date overflow', '2026-01-31T23:00:00.000Z', Number.MAX_SAFE_INTEGER],
  ])('returns null for %s', (_name, start, seconds) => {
    expect(addElapsedSeconds(start, seconds)).toBeNull();
  });
});
