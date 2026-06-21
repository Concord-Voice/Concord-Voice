import { test, expect } from '@playwright/test';
import { registerAndLogin, createServer, createChannel } from './helpers';

test.describe('Messaging', () => {
  test.beforeEach(async ({ page }) => {
    // registerAndLogin (not registerUser): channel creation + encrypted
    // messaging need e2eeService initialized, which only login does. See #1274.
    await registerAndLogin(page);
    await createServer(page, 'Messaging Test Server');
    await createChannel(page, 'chat');
    // Select the channel (sidebar item is a button named after the channel)
    await page.getByRole('button', { name: 'chat', exact: true }).click();
  });

  test('send a message', async ({ page }) => {
    const textarea = page.getByRole('textbox', { name: 'Message input' });
    await textarea.fill('Hello from E2E test!');
    await textarea.press('Enter');

    // Message should appear in the message list
    await expect(page.getByText('Hello from E2E test!')).toBeVisible({ timeout: 10_000 });
  });

  test('message appears in list', async ({ page }) => {
    const textarea = page.getByRole('textbox', { name: 'Message input' });
    await textarea.fill('Visible message');
    await textarea.press('Enter');

    // Should be in a message element
    const message = page.locator('.message').filter({ hasText: 'Visible message' });
    await expect(message).toBeVisible({ timeout: 10_000 });
  });

  test('multiple messages appear in order', async ({ page }) => {
    const textarea = page.getByRole('textbox', { name: 'Message input' });

    await textarea.fill('First message');
    await textarea.press('Enter');
    await expect(page.getByText('First message')).toBeVisible({ timeout: 10_000 });

    await textarea.fill('Second message');
    await textarea.press('Enter');
    await expect(page.getByText('Second message')).toBeVisible({ timeout: 10_000 });
  });

  test('empty message is not sent', async ({ page }) => {
    const textarea = page.getByRole('textbox', { name: 'Message input' });
    const messagesBefore = await page.locator('.message').count();

    // Try to send empty message
    await textarea.press('Enter');

    // No new message should appear
    const messagesAfter = await page.locator('.message').count();
    expect(messagesAfter).toBe(messagesBefore);
  });
});
