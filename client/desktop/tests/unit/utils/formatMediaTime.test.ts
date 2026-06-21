import { describe, it, expect } from 'vitest';
import { formatMediaTime } from '@/renderer/utils/formatMediaTime';

describe('formatMediaTime', () => {
  it('formats zero', () => expect(formatMediaTime(0)).toBe('0:00'));
  it('formats seconds with leading zero', () => expect(formatMediaTime(5)).toBe('0:05'));
  it('formats minutes:seconds', () => expect(formatMediaTime(65)).toBe('1:05'));
  it('formats hours:minutes:seconds', () => expect(formatMediaTime(3661)).toBe('1:01:01'));
  it('floors fractional seconds', () => expect(formatMediaTime(9.9)).toBe('0:09'));
  it('clamps NaN to 0:00', () => expect(formatMediaTime(NaN)).toBe('0:00'));
  it('clamps Infinity to 0:00', () => expect(formatMediaTime(Infinity)).toBe('0:00'));
  it('clamps negative to 0:00', () => expect(formatMediaTime(-3)).toBe('0:00'));
});
