import { describe, it, expect } from 'vitest';
import {
  ACTIVE_SEARCH_PROFILE,
  DESKTOP_SEARCH_PROFILE,
  MOBILE_SEARCH_PROFILE,
} from '@/renderer/services/messaging/searchProfile';

const SERVER_BULK_LIMIT_PER_MINUTE = 20;
const SERVER_MAX_PAGE = 200;

describe('search profiles (#3875)', () => {
  it.each([
    ['desktop', DESKTOP_SEARCH_PROFILE],
    ['mobile', MOBILE_SEARCH_PROFILE],
  ])('%s stays inside the server limits', (_name, p) => {
    expect(p.requestBurst + p.requestsPerMinute).toBeLessThan(SERVER_BULK_LIMIT_PER_MINUTE);
    expect(p.pageSize).toBeLessThanOrEqual(SERVER_MAX_PAGE);
    expect(p.chunkMaxMessages).toBeGreaterThanOrEqual(p.pageSize);
    expect(Object.isFrozen(p)).toBe(true);
  });

  it('pins the approved desktop and mobile budgets', () => {
    expect(DESKTOP_SEARCH_PROFILE).toEqual({
      chunkWindowDays: 90,
      chunkMaxMessages: 600,
      pageSize: 200,
      requestsPerMinute: 12,
      requestBurst: 3,
      decryptConcurrency: 4,
      sliceBudgetMs: 8,
      prefetchViewports: 1.5,
      indexMaxMessages: 50_000,
      indexMaxBytes: 8 * 1024 * 1024,
    });
    expect(MOBILE_SEARCH_PROFILE).toEqual({
      chunkWindowDays: 90,
      chunkMaxMessages: 200,
      pageSize: 100,
      requestsPerMinute: 6,
      requestBurst: 2,
      decryptConcurrency: 2,
      sliceBudgetMs: 4,
      prefetchViewports: 2,
      indexMaxMessages: 10_000,
      indexMaxBytes: 2 * 1024 * 1024,
    });
  });

  it('the desktop renderer uses the desktop profile', () => {
    expect(ACTIVE_SEARCH_PROFILE).toBe(DESKTOP_SEARCH_PROFILE);
  });
});
