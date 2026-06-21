import { vi } from 'vitest';

/**
 * Creates a mocked e2eeService with all methods stubbed.
 * Use vi.mock() to replace the real service module in tests.
 */
export function createMockE2EEService() {
  return {
    isInitialized: true,
    initialize: vi.fn().mockResolvedValue(undefined),
    getChannelKey: vi.fn().mockResolvedValue({} as CryptoKey),
    encryptForChannel: vi
      .fn()
      .mockImplementation((_channelId: string, plaintext: string) =>
        Promise.resolve(`encrypted:${plaintext}`)
      ),
    decryptForChannel: vi
      .fn()
      .mockImplementation((_channelId: string, ciphertext: string) =>
        Promise.resolve(ciphertext.replace('encrypted:', ''))
      ),
    createChannelKeys: vi.fn().mockResolvedValue(new Map()),
    wrapKeyForMember: vi.fn().mockResolvedValue('mock-wrapped-key'),
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    clearKeys: vi.fn(),
    invalidateChannelKey: vi.fn(),
  };
}

/**
 * Factory for the e2eeService vi.mock() callback.
 * Usage:
 *   vi.mock('../../src/renderer/services/e2eeService', () => mockE2EEModule());
 */
export function mockE2EEModule() {
  const mock = createMockE2EEService();
  return {
    e2eeService: mock,
    default: mock,
  };
}
