/**
 * Search budgets per platform (#3875). Data only. The server's bulk limit
 * (20/min/user) stays the hard ceiling, because the server cannot trust a
 * client's claim about its platform.
 */
export interface SearchProfile {
  /** Days one chunk reaches back from where the previous chunk stopped. */
  readonly chunkWindowDays: number;
  /** Most messages one chunk fetches; the chunk ends at whichever limit comes first. */
  readonly chunkMaxMessages: number;
  /** Bulk page size. The server allows at most 200. */
  readonly pageSize: number;
  /** Pacer refill rate for bulk requests. */
  readonly requestsPerMinute: number;
  /** Pacer bucket size. The most sent in any 60 s window is burst + rate. */
  readonly requestBurst: number;
  /** Decrypts in flight per slice. */
  readonly decryptConcurrency: number;
  /** Main-thread time per run of slices before yielding. */
  readonly sliceBudgetMs: number;
  /** Panel heights from the end of the results at which the next chunk starts. */
  readonly prefetchViewports: number;
  readonly indexMaxMessages: number;
  /** Counts index overhead plus the stored result text. */
  readonly indexMaxBytes: number;
}

export const DESKTOP_SEARCH_PROFILE: SearchProfile = Object.freeze({
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

export const MOBILE_SEARCH_PROFILE: SearchProfile = Object.freeze({
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

// ponytail: the desktop renderer is the only client today; the mobile client (#204) selects MOBILE_SEARCH_PROFILE.
export const ACTIVE_SEARCH_PROFILE: SearchProfile = DESKTOP_SEARCH_PROFILE;
