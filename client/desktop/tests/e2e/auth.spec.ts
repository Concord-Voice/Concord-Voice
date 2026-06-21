import { test, expect } from '@playwright/test';
import { registerUser, loginUser } from './helpers';

test.describe('Authentication', () => {
  test('register a new user', async ({ page }) => {
    const { username } = await registerUser(page);

    // Should be on the app page after registration
    await expect(page).toHaveURL(/\/app/);

    // User identity should be visible somewhere (e.g., user panel)
    await expect(page.getByText(username)).toBeVisible({ timeout: 5_000 });
  });

  test('login with existing credentials', async ({ page }) => {
    // Register first
    const { email } = await registerUser(page);

    // Log out
    await page.goto('/');

    // Login with the same credentials
    await loginUser(page, email);

    // Should be on the app page
    await expect(page).toHaveURL(/\/app/);
  });

  test('session persists across reload', async ({ page }) => {
    // Session restore requires the `globalThis.electron.restoreSession` IPC
    // bridge, which is undefined in the Playwright web context (Vite dev
    // server). The renderer's `useAuthStore` has no `persist` middleware
    // (authStore.ts:14-29) and `main.tsx:60` explicitly removes any legacy
    // `concord-auth` localStorage key at startup, so the in-memory access
    // token does not survive a page reload outside Electron.
    //
    // #1263's spec assumed Zustand persist + safeStorage rehydration, but
    // that path doesn't exist in this codebase. Skipping in web context
    // until either (a) an electron-forge end-to-end test covers the IPC
    // path, or (b) authStore gains a renderer-side persistence shim.
    test.skip(typeof globalThis.electron === 'undefined', 'session restore requires electron IPC');

    await registerUser(page);

    // Reload the page
    await page.reload();

    // Should still be on the app page (not redirected to login)
    await expect(page).toHaveURL(/\/app/, { timeout: 10_000 });
  });

  test('logout returns to login page', async ({ page }) => {
    const { username } = await registerUser(page);

    // The logout control lives in UserPopover, which only mounts while the popover
    // is open — so open the UserPanel avatar menu first (aria-label "User menu for
    // <username>", UserPanel.tsx), then click "Log Out" (UserPopover.tsx). The owner
    // always has this affordance, so the old `if (visible)` guard was a vacuous pass:
    // it would go silently green if the selector ever drifted. Unconditional now.
    await page.getByLabel(`User menu for ${username}`).click();
    await page.getByRole('button', { name: 'Log Out' }).click();

    // handleLogout navigates to '/' → the ConnectionSelector. Its "Sign In to
    // Existing Account" option is a stable anchor proving we left /app.
    await expect(page.getByText('Sign In to Existing Account')).toBeVisible({
      timeout: 10_000,
    });
  });
});
