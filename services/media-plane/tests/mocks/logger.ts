import { vi } from 'vitest';

/**
 * Silent logger mock — import this file in any test that transitively
 * imports @/lib/logger to suppress console output.
 */
vi.mock('@/lib/logger', () => ({
  logger: {
    info: vi.fn(),
    warn: vi.fn(),
    error: vi.fn(),
    debug: vi.fn(),
    log: vi.fn(),
  },
}));
