// @vitest-environment node
// This suite reads a file off disk and imports playwright.config — both pure
// Node concerns with no DOM in sight. It inherited the global jsdom
// environment (vite.config.ts) only by default.
//
// Not load-bearing for correctness any more, but kept deliberately. It was
// added when `import('@playwright/test')` deadlocked here: jsdom 30 declares
// undici ^8.9.0 and a stale global override was forcing the whole tree onto
// undici 7, so jsdom ran a major below its own dependency. Removing that
// override is the actual fix and this import works under jsdom again. Running
// in node keeps a Node-only concern from being coupled to the DOM environment
// at all, which is what made that dependency skew surface here as an
// unexplained 10s timeout rather than anywhere informative.
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

describe('rich-presence E2E endpoint configuration', () => {
  it('does not pin the fresh-snapshot WebSocket to a numeric localhost port', () => {
    const specSource = readFileSync(
      resolve(process.cwd(), 'tests/e2e/rich-presence-overrides.spec.ts'),
      'utf8'
    );

    expect(specSource).not.toMatch(/ws:\/\/localhost:\d+\/api\/v1\/ws/);
  });

  it('passes E2E_API_PORT through to the Vite renderer as VITE_API_PORT', async () => {
    const originalE2EPort = process.env.E2E_API_PORT;
    const originalVitePort = process.env.VITE_API_PORT;
    process.env.E2E_API_PORT = '18081';
    delete process.env.VITE_API_PORT;
    vi.resetModules();

    try {
      const config = (await import('../../../playwright.config')).default;
      const webServer = Array.isArray(config.webServer) ? config.webServer[0] : config.webServer;

      expect(webServer?.env?.VITE_API_PORT).toBe('18081');
    } finally {
      if (originalE2EPort === undefined) delete process.env.E2E_API_PORT;
      else process.env.E2E_API_PORT = originalE2EPort;
      if (originalVitePort === undefined) delete process.env.VITE_API_PORT;
      else process.env.VITE_API_PORT = originalVitePort;
      vi.resetModules();
    }
  });
});
