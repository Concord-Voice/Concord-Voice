// @vitest-environment node
/**
 * Main-process API base resolution tests (#974, plan deviation D1).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const mocked = vi.hoisted(() => ({
  isPackaged: false,
  persistedApiBase: null as string | null,
}));

vi.mock('electron', () => ({
  app: {
    get isPackaged() {
      return mocked.isPackaged;
    },
  },
}));

vi.mock('../../../src/main/tokenManager', () => ({
  getPersistedApiBase: () => mocked.persistedApiBase,
}));

import { getApiBaseUrl } from '@/main/apiBaseUrl';

describe('getApiBaseUrl', () => {
  beforeEach(() => {
    mocked.isPackaged = false;
    mocked.persistedApiBase = null;
    vi.unstubAllEnvs();
  });
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it('prefers the persisted apiBase from tokenManager when present', () => {
    mocked.persistedApiBase = 'https://staging.concordvoice.chat';
    mocked.isPackaged = true;
    expect(getApiBaseUrl()).toBe('https://staging.concordvoice.chat');
  });

  it('falls back to the production SaaS endpoint in packaged builds', () => {
    mocked.isPackaged = true;
    expect(getApiBaseUrl()).toBe('https://api.concordvoice.chat');
  });

  it('falls back to the local control plane in dev', () => {
    expect(getApiBaseUrl()).toBe('http://localhost:8080');
  });

  it('honors the CONCORD_DEV_API_BASE override in dev only', () => {
    vi.stubEnv('CONCORD_DEV_API_BASE', 'http://10.0.0.19:8080');
    expect(getApiBaseUrl()).toBe('http://10.0.0.19:8080');
    mocked.isPackaged = true;
    expect(getApiBaseUrl()).toBe('https://api.concordvoice.chat');
  });
});
