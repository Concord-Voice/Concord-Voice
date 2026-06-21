import { test, expect } from '@playwright/test';
import { registerUser, createServer } from './helpers';

test.describe('Servers', () => {
  test.beforeEach(async ({ page }) => {
    await registerUser(page);
  });

  test('create a server', async ({ page }) => {
    await createServer(page, 'E2E Test Server');

    // The active server's channel-panel header (<h3>) shows its name. (We can't
    // assert the bar icon by name: an ACTIVE server's icon is relabeled
    // "Toggle channel panel", so `${name} server` only matches inactive servers.)
    await expect(page.getByRole('heading', { name: 'E2E Test Server', level: 3 })).toBeVisible();
  });

  test('server appears in server bar', async ({ page }) => {
    await createServer(page, 'Bar Test Server');

    // Confirm the created server is active and rendered (its channel-panel <h3>).
    // The active server's bar icon is relabeled "Toggle channel panel", so a
    // name-based bar-icon assertion isn't viable for a single active server.
    await expect(page.getByRole('heading', { name: 'Bar Test Server', level: 3 })).toBeVisible({
      timeout: 5_000,
    });
  });

  test('can switch between servers', async ({ page }) => {
    await createServer(page, 'Server Alpha');
    await createServer(page, 'Server Beta');

    // Server Beta is now active (its rail icon is relabeled "Toggle channel panel");
    // Server Alpha is inactive, with aria-label "Server Alpha server" (ServerBar.tsx).
    // Switch to Alpha. Unconditional: a logged-in user with two servers always has
    // this affordance, so the old `if (visible)` guard hid both regressions and any
    // selector drift behind a silent green.
    const alphaIcon = page.getByLabel('Server Alpha server');
    await expect(alphaIcon).toBeVisible();
    await alphaIcon.click();

    // Alpha's channel-panel header (<h3>, level 3) confirms the switch landed.
    await expect(page.getByRole('heading', { name: 'Server Alpha', level: 3 })).toBeVisible({
      timeout: 5_000,
    });
  });

  test('delete a server', async ({ page }) => {
    await createServer(page, 'Delete Me Server');

    // The created server is active; its rail icon is "Toggle channel panel" and
    // right-clicking it opens the server context menu (ServerBar.tsx handleContextMenu,
    // which is wired on the active-server icon too). The old test right-clicked a
    // by-name locator that never matches the ACTIVE server, then bailed silently
    // through three nested `if (visible)` guards — a fully vacuous pass.
    await page.getByLabel('Toggle channel panel').click({ button: 'right' });

    // Owner-only "Delete Server" context-menu item (ServerContextMenu.tsx) opens
    // DeleteServerModal; the menu closes as the modal opens.
    await page.getByRole('button', { name: 'Delete Server' }).click();

    // DeleteServerModal is a type-to-confirm dialog (ConfirmActionModal.tsx): the
    // input's placeholder is the server name, and the confirm button enables only
    // once the typed value matches.
    await page.getByPlaceholder('Delete Me Server').fill('Delete Me Server');
    await page.getByRole('button', { name: 'Delete Server' }).click();

    // Deleting the only server leaves no active server → the rail renders the
    // "No active server" placeholder (ServerBar.tsx), confirming the delete took.
    await expect(page.getByLabel('No active server')).toBeVisible({ timeout: 10_000 });
  });
});
