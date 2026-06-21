import { test, expect } from '@playwright/test';
import { registerUser, createServer } from './helpers';

test.describe('Invites', () => {
  test('create an invite code', async ({ page }) => {
    await registerUser(page);
    await createServer(page, 'Invite Test Server');

    // The Invite button lives in ServerActionBar (visible to the owner via canInvite).
    // Target it by role+exact name — `getByText(/invite/i)` also matched the
    // server-name heading ("Invite Test Server"), a flaky multi-match. Unconditional
    // now: the owner always has this affordance, so the old `if (visible)` guard was
    // a vacuous pass that would go silently green on any selector drift.
    const inviteBtn = page.getByRole('button', { name: 'Invite', exact: true });
    await expect(inviteBtn).toBeVisible();
    await inviteBtn.click();

    // The invite popup offers a "Generate Code" button (a fresh server has no active
    // invite yet). Assert the button specifically — a broad regex multi-matches the
    // popup's "Invite Code" header too.
    await expect(page.getByRole('button', { name: 'Generate Code' })).toBeVisible({
      timeout: 5_000,
    });
  });

  test('join server with invite code', async ({ page, browser }) => {
    // A real end-to-end join: user 1 generates a code, user 2 (separate context)
    // joins with it. The old version of this test only re-opened user 1's invite
    // popup and asserted nothing about *joining* — a vacuous duplicate of the
    // "create an invite code" test above.

    // User 1: create a server and generate a real invite code.
    await registerUser(page);
    await createServer(page, 'Joinable Server');
    await page.getByRole('button', { name: 'Invite', exact: true }).click();
    await page.getByRole('button', { name: 'Generate Code' }).click();
    const codeEl = page.locator('.invite-popup-code');
    await expect(codeEl).toBeVisible({ timeout: 10_000 });
    const inviteCode = ((await codeEl.textContent()) ?? '').trim();
    expect(inviteCode).not.toBe('');

    // User 2: a separate browser context (isolated session/token) joins via the code.
    const ctx2 = await browser.newContext();
    const page2 = await ctx2.newPage();
    try {
      await registerUser(page2);
      await page2.getByLabel('Add Server').click();
      await page2.getByRole('button', { name: 'Join a Server' }).click();
      // JoinServerModal's field is labeled "Invite Code"; the submit button enables
      // only once the code resolves to a valid server preview.
      await page2.getByLabel('Invite Code').fill(inviteCode);
      const joinBtn = page2.getByRole('button', { name: 'Join Server' });
      await expect(joinBtn).toBeEnabled({ timeout: 10_000 });
      await joinBtn.click();

      // User 2 has now joined: the server appears in their rail as the inactive icon
      // "Joinable Server server". Join does NOT auto-select the server (the active view
      // stays on the DM/welcome screen — confirmed via the failure snapshot), so select
      // it and confirm its channel panel renders, proving the join is fully functional.
      const joinedIcon = page2.getByLabel('Joinable Server server');
      await expect(joinedIcon).toBeVisible({ timeout: 15_000 });
      await joinedIcon.click();
      await expect(page2.getByRole('heading', { name: 'Joinable Server', level: 3 })).toBeVisible({
        timeout: 10_000,
      });
    } finally {
      await ctx2.close();
    }
  });

  test('join server form accepts invite code', async ({ page }) => {
    await registerUser(page);

    // Click add server (opens the ServerActionModal chooser). The button's
    // aria-label is "Add Server" (was stale "Add a Server" pre-#1274).
    await page.getByLabel('Add Server').click();

    // Choose "Join a Server" (a button in the chooser; targeting it by role avoids
    // the old getByText(/join.*server/i) strict-mode multi-match against the option's
    // title + description). This opens JoinServerModal. Unconditional now — the
    // chooser always offers this option, so the old `if (visible)` guard was vacuous.
    await page.getByRole('button', { name: 'Join a Server' }).click();

    // JoinServerModal's field is labeled "Invite Code" (JoinServerModal.tsx). The old
    // getByPlaceholder(/invite|code/i) NEVER matched — the placeholder is "AbCd1234" —
    // so this assertion only ever ran inside a guard that silently skipped it.
    await expect(page.getByLabel('Invite Code')).toBeVisible({ timeout: 5_000 });
  });
});
