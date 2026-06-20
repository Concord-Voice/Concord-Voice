import { defineConfig, devices } from '@playwright/test';

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
    baseURL: 'http://localhost:3001',
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
    command: 'npx vite --port 3001',
    port: 3001,
    reuseExistingServer: !process.env.CI,
    timeout: 30_000,
  },
});
