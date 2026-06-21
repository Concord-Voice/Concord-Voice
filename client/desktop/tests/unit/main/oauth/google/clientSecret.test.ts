// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// process.resourcesPath is undefined in Node test environment (Electron sets it at runtime).
// Set a fake value so the module's guard passes and reaches readFileSync.
const FAKE_RESOURCES = '/fake/resources';

beforeEach(() => {
  vi.resetModules();
  Object.defineProperty(process, 'resourcesPath', { value: FAKE_RESOURCES, writable: true, configurable: true });
  delete process.env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP;
});

afterEach(() => {
  vi.restoreAllMocks();
  // Restore process.resourcesPath to undefined (test cleanup)
  Object.defineProperty(process, 'resourcesPath', { value: undefined, writable: true, configurable: true });
  delete process.env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP;
});

describe('loadGoogleClientSecret', () => {
  it('reads the resource JSON when present', async () => {
    vi.doMock('node:fs', () => ({ readFileSync: () => JSON.stringify({ clientSecret: 'from-resource' }) }));
    const { loadGoogleClientSecret } = await import('../../../../../src/main/oauth/google/clientSecret');
    expect(loadGoogleClientSecret()).toBe('from-resource');
  });
  it('falls back to env when the resource is absent', async () => {
    vi.doMock('node:fs', () => ({ readFileSync: () => { throw new Error('ENOENT'); } }));
    process.env.GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP = 'from-env';
    const { loadGoogleClientSecret } = await import('../../../../../src/main/oauth/google/clientSecret');
    expect(loadGoogleClientSecret()).toBe('from-env');
  });
  it('returns empty string when neither present', async () => {
    vi.doMock('node:fs', () => ({ readFileSync: () => { throw new Error('ENOENT'); } }));
    const { loadGoogleClientSecret } = await import('../../../../../src/main/oauth/google/clientSecret');
    expect(loadGoogleClientSecret()).toBe('');
  });
});
