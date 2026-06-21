import { test, expect } from '@playwright/test';
import { registerAndLogin, createServer, createChannel } from './helpers';

test.describe('Channels', () => {
  test.beforeEach(async ({ page }) => {
    // registerAndLogin (not registerUser): channel creation needs e2eeService
    // initialized, which only login does. See helpers.ts / #1274.
    await registerAndLogin(page);
    await createServer(page, 'Channel Test Server');
  });

  test('create a channel', async ({ page }) => {
    await createChannel(page, 'test-channel');

    // Channel should appear in the sidebar as a button named after the channel.
    await expect(page.getByRole('button', { name: 'test-channel', exact: true })).toBeVisible();
  });

  test('channel appears in channel list', async ({ page }) => {
    await createChannel(page, 'visible-channel');

    // Should be in the TEXT CHANNELS group
    await expect(page.getByRole('button', { name: 'visible-channel', exact: true })).toBeVisible();
  });

  test('click channel to select it', async ({ page }) => {
    await createChannel(page, 'clickable-channel');

    // Click the channel (the sidebar item is a button named after the channel)
    await page.getByRole('button', { name: 'clickable-channel', exact: true }).click();

    // The channel item should have an active class or visual indicator
    const channelItem = page.locator('.channel-item.active');
    await expect(channelItem).toBeVisible({ timeout: 5_000 });
  });
});
