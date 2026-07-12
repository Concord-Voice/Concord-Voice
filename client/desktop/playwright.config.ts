import { defineConfig, devices } from '@playwright/test';

const E2E_API_PORT = process.env.E2E_API_PORT ?? process.env.VITE_API_PORT ?? '8080';
const E2E_UI_PORT = Number(process.env.E2E_UI_PORT ?? '3001');
const E2E_UI_BASE = `http://localhost:${E2E_UI_PORT}`;
const E2E_BROWSER_EXECUTABLE = process.env.E2E_BROWSER_EXECUTABLE;

/**
 * Playwright E2E test configuration for Concord Voice Desktop.
 *
 * Tests run against the Vite dev server (renderer only, not full Electron).
 * Requires the backend to be running separately (API + PostgreSQL + Redis).
 *
 * Usage:
 *   1. Start backend: cd services/control-plane && go run ./cmd/server
 *   2. Run tests: cd client/desktop && npm run test:e2e
 *
 * The webServer config below auto-starts the Vite dev server on port 3001.
 */
export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: false, // Run tests sequentially (they share DB state)
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: 'html',
  timeout: 60_000,

  use: {
    baseURL: E2E_UI_BASE,
    launchOptions: E2E_BROWSER_EXECUTABLE ? { executablePath: E2E_BROWSER_EXECUTABLE } : undefined,
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],

  webServer: {
    command: `npx vite --port ${E2E_UI_PORT}`,
    port: E2E_UI_PORT,
    env: { VITE_API_PORT: E2E_API_PORT },
    reuseExistingServer: !process.env.CI,
    timeout: 30_000,
  },
});
