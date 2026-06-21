import { formatRetryAfter } from '@/renderer/utils/formatRetryAfter';

describe('formatRetryAfter', () => {
  it('formats seconds under 1 minute', () => {
    expect(formatRetryAfter(30)).toBe('30s');
    expect(formatRetryAfter(1)).toBe('1s');
    expect(formatRetryAfter(59)).toBe('59s');
  });

  it('formats minutes under 1 hour', () => {
    expect(formatRetryAfter(60)).toBe('1m');
    expect(formatRetryAfter(120)).toBe('2m');
    expect(formatRetryAfter(2700)).toBe('45m');
    expect(formatRetryAfter(3540)).toBe('59m');
  });

  it('formats hours and minutes', () => {
    expect(formatRetryAfter(3600)).toBe('1h');
    expect(formatRetryAfter(3660)).toBe('1h 1m');
    expect(formatRetryAfter(51780)).toBe('14h 23m');
    expect(formatRetryAfter(86400)).toBe('24h');
  });

  it('handles zero', () => {
    expect(formatRetryAfter(0)).toBe('0s');
  });
});
