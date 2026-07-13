import { createHash } from 'node:crypto';
import {
  expect,
  test,
  type APIResponse,
  type Locator,
  type Page,
  type Response as PlaywrightResponse,
} from '@playwright/test';
import Redis from 'ioredis';
import { registerAndLogin } from './helpers';

const E2E_API_PORT = process.env.E2E_API_PORT ?? process.env.VITE_API_PORT ?? '8080';
const API_BASE = 'http://localhost:' + E2E_API_PORT;
const E2E_UI_PORT = process.env.E2E_UI_PORT ?? '3001';
const E2E_UI_BASE = 'http://localhost:' + E2E_UI_PORT;
const DEFAULT_PASSWORD = 'E2ETestPassword123!'; // pragma: allowlist secret
const RECOVERED_PASSWORD = 'E2ERecoveredPassword456!'; // pragma: allowlist secret
const RESET_PASSWORD = 'E2EResetPassword789!'; // pragma: allowlist secret
const ACKNOWLEDGEMENT =
  'I understand and consent to server-readable Activity History under the terms above.';

interface AuthSession {
  accessToken: string;
  userId: string;
  username: string;
  email: string;
}

interface RequiredConsent {
  version: number;
  copy_hash: string;
}

interface HistorySettings {
  available: boolean;
  enabled: boolean;
  reconsent_required: boolean;
  retention_days: 7 | 30 | 90 | 365;
  required_consent?: RequiredConsent;
}

interface HistoryPayload {
  text: string;
  emoji?: string;
}

interface HistoryItem {
  id: string;
  status: 'supported' | 'unsupported';
  category: string;
  payload_version: number;
  payload: HistoryPayload | null;
  started_at: string;
  ended_at: string | null;
  recorded_at: string;
  expires_at: string;
}

interface HistoryPage {
  items: HistoryItem[];
  next_cursor: string | null;
}

interface PresenceSettings {
  custom_text_tier: number;
  custom_text: string | null;
  custom_text_emoji: string | null;
}

interface ApiOptions {
  bearer?: string;
  data?: Record<string, unknown>;
}

type ApiMethod = 'GET' | 'PATCH' | 'PUT' | 'POST' | 'DELETE';

function nextLoginResponse(page: Page): Promise<PlaywrightResponse> {
  return page.waitForResponse(
    (response) =>
      response.url().includes('/api/v1/auth/login') && response.request().method() === 'POST'
  );
}

async function registerWithSession(page: Page): Promise<AuthSession> {
  const loginResponsePromise = nextLoginResponse(page);
  const credentials = await registerAndLogin(page);
  const response = await loginResponsePromise;
  expect(response.status(), 'login setup request should succeed').toBe(200);
  const body = (await response.json()) as {
    access_token?: unknown;
    user?: { id?: unknown };
  };
  if (typeof body.access_token !== 'string' || typeof body.user?.id !== 'string') {
    throw new Error('Login response omitted access_token or user.id');
  }
  return {
    accessToken: body.access_token,
    userId: body.user.id,
    username: credentials.username,
    email: credentials.email,
  };
}

async function apiResponse(
  page: Page,
  method: ApiMethod,
  path: string,
  expectedStatus: number,
  options: ApiOptions = {}
): Promise<APIResponse> {
  const headers: Record<string, string> = {};
  if (options.bearer !== undefined) {
    headers.Authorization = 'Bearer ' + options.bearer;
  }
  const response = await page.context().request.fetch(API_BASE + path, {
    method,
    headers,
    data: options.data,
  });
  expect(
    response.status(),
    method + ' request should return HTTP ' + expectedStatus + ' without exposing response data'
  ).toBe(expectedStatus);
  return response;
}

async function apiJson<T>(
  page: Page,
  method: ApiMethod,
  path: string,
  expectedStatus: number,
  options: ApiOptions = {}
): Promise<T> {
  const response = await apiResponse(page, method, path, expectedStatus, options);
  return (await response.json()) as T;
}

async function getHistorySettings(page: Page, session: AuthSession): Promise<HistorySettings> {
  return apiJson<HistorySettings>(page, 'GET', '/api/v1/users/me/presence-history/settings', 200, {
    bearer: session.accessToken,
  });
}

async function getHistory(page: Page, session: AuthSession, query = ''): Promise<HistoryPage> {
  return apiJson<HistoryPage>(page, 'GET', '/api/v1/users/me/presence-history' + query, 200, {
    bearer: session.accessToken,
  });
}

async function enableHistoryByApi(
  page: Page,
  session: AuthSession,
  retentionDays: 7 | 30 | 90 | 365 = 30
): Promise<void> {
  const settings = await getHistorySettings(page, session);
  expect(settings.available).toBe(true);
  const disclosure = settings.required_consent;
  if (disclosure === undefined) {
    throw new Error('Activity History disclosure is unavailable');
  }
  await apiJson<HistorySettings>(page, 'PATCH', '/api/v1/users/me/presence-history/settings', 200, {
    bearer: session.accessToken,
    data: {
      enabled: true,
      retention_days: retentionDays,
      acknowledged: true,
      consent_version: disclosure.version,
      consent_copy_hash: disclosure.copy_hash,
    },
  });
}

async function patchPresence(
  page: Page,
  session: AuthSession,
  data: Record<string, unknown>
): Promise<PresenceSettings> {
  return apiJson<PresenceSettings>(page, 'PATCH', '/api/v1/users/me/presence-settings', 200, {
    bearer: session.accessToken,
    data,
  });
}

async function getPresence(page: Page, session: AuthSession): Promise<PresenceSettings> {
  return apiJson<PresenceSettings>(page, 'GET', '/api/v1/users/me/presence-settings', 200, {
    bearer: session.accessToken,
  });
}

async function waitForHistoryItem(
  page: Page,
  session: AuthSession,
  text: string
): Promise<HistoryItem> {
  let match: HistoryItem | undefined;
  await expect
    .poll(
      async () => {
        const history = await getHistory(page, session);
        match = history.items.find(
          (item) => item.status === 'supported' && item.payload?.text === text
        );
        return match !== undefined;
      },
      { message: 'semantic Custom Status change should appear in self history', timeout: 15_000 }
    )
    .toBe(true);
  if (match === undefined) {
    throw new Error('Expected history item was not returned');
  }
  return match;
}

async function openHistoryInterval(
  page: Page,
  session: AuthSession,
  text: string
): Promise<HistoryItem> {
  await enableHistoryByApi(page, session);
  await patchPresence(page, session, {
    custom_text_tier: 0,
    custom_text: text,
    custom_text_emoji: '🔐',
  });
  const item = await waitForHistoryItem(page, session, text);
  expect(item.category).toBe('custom_text');
  expect(item.ended_at).toBeNull();
  return item;
}

async function expectHistoryClosed(
  page: Page,
  session: AuthSession,
  original: HistoryItem
): Promise<void> {
  const history = await getHistory(page, session);
  const item = history.items.find((candidate) => candidate.id === original.id);
  expect(item, 'destructive key operation should preserve the archived interval').toBeDefined();
  expect(history.items).toHaveLength(1);
  expect(item?.payload?.text === original.payload?.text).toBe(true);
  expect(item?.ended_at).not.toBeNull();
  const presence = await getPresence(page, session);
  expect(presence.custom_text).toBeNull();
  expect(presence.custom_text_emoji).toBeNull();
}

function settingsNav(page: Page): Locator {
  return page.locator('nav.settings-nav');
}

function activityHistoryCard(page: Page): Locator {
  return page
    .getByRole('article')
    .filter({ has: page.getByRole('heading', { name: 'Activity History', level: 3 }) });
}

async function openCustomStatusSettings(page: Page): Promise<Locator> {
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Settings', level: 1 })).toBeVisible();
  await settingsNav(page).getByRole('button', { name: 'Custom Status', exact: true }).click();
  const card = activityHistoryCard(page);
  await expect(card).toBeVisible({ timeout: 15_000 });
  return card;
}

async function enableHistoryByUi(
  page: Page,
  card: Locator,
  retentionDays: '7' | '30' | '90' | '365' = '30'
): Promise<void> {
  await card.locator('label.settings-toggle').click();
  const dialog = page.getByRole('dialog', { name: 'Enable Activity History' });
  await expect(dialog).toBeVisible();
  await dialog
    .getByRole('combobox', { name: 'Keep Activity History for' })
    .selectOption(retentionDays);
  await dialog.getByRole('checkbox', { name: ACKNOWLEDGEMENT, exact: true }).check();
  await dialog.getByRole('button', { name: 'Enable Activity History', exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(card.getByText('On', { exact: true })).toBeVisible();
  await expect(card.getByRole('switch', { name: 'Activity History' })).toBeChecked();
}

async function assertHistoryEmpty(page: Page, session: AuthSession): Promise<void> {
  await expect.poll(async () => (await getHistory(page, session)).items.length).toBe(0);
}

function validBase64(label: string): string {
  return Buffer.from(label, 'utf8').toString('base64');
}

function redisClient(): Redis {
  const rawUrl = process.env.REDIS_URL;
  if (rawUrl === undefined) {
    throw new Error('Activity History recovery E2E requires an explicit isolated REDIS_URL');
  }
  const parsed = new URL(rawUrl);
  const loopbackHosts = new Set(['localhost', '127.0.0.1', '::1']);
  const database = Number(parsed.pathname.slice(1));
  if (
    parsed.protocol !== 'redis:' ||
    !loopbackHosts.has(parsed.hostname) ||
    parsed.password.length === 0 ||
    !Number.isInteger(database) ||
    database <= 0
  ) {
    throw new Error(
      'Activity History recovery E2E requires authenticated loopback Redis on an isolated non-default database'
    );
  }
  return new Redis(rawUrl, {
    maxRetriesPerRequest: 1,
    lazyConnect: false,
    connectTimeout: 2_000,
    commandTimeout: 1_000,
  });
}

async function closeRedis(client: Redis): Promise<void> {
  try {
    await client.quit();
  } catch {
    client.disconnect();
  }
}

async function seedRecoveryCode(email: string, userId: string, code: string): Promise<string> {
  const key = 'recovery_code:' + email.trim().toLowerCase();
  const client = redisClient();
  try {
    await client.set(
      key,
      JSON.stringify({
        code_hash: createHash('sha256').update(code, 'utf8').digest('hex'),
        user_id: userId,
        attempts: 0,
      }),
      'EX',
      600
    );
  } finally {
    await closeRedis(client);
  }
  return key;
}

async function deleteRedisKey(key: string): Promise<void> {
  const client = redisClient();
  try {
    await client.del(key);
  } finally {
    await closeRedis(client);
  }
}

async function issueRecoveryToken(
  page: Page,
  session: AuthSession,
  code: string
): Promise<{ token: string; redisKey: string }> {
  const redisKey = await seedRecoveryCode(session.email, session.userId, code);
  try {
    const body = await apiJson<{ recovery_token?: unknown }>(
      page,
      'POST',
      '/api/v1/auth/recovery/verify-code',
      200,
      { data: { email: session.email, code } }
    );
    if (typeof body.recovery_token !== 'string') {
      throw new Error('Recovery verification omitted recovery_token');
    }
    return { token: body.recovery_token, redisKey };
  } catch (error) {
    await deleteRedisKey(redisKey);
    throw error;
  }
}

test.describe('Activity History lifecycle', () => {
  test.beforeAll(async () => {
    const response = await fetch(API_BASE + '/api/v1/server/capabilities');
    expect(response.status, 'Activity History E2E requires a running control plane').toBe(200);
    const capabilities = (await response.json()) as {
      features?: { activityHistorySupported?: unknown };
    };
    expect(
      capabilities.features?.activityHistorySupported,
      'Start the control plane with the Activity History gate enabled and replica count one'
    ).toBe(true);
    const client = redisClient();
    await closeRedis(client);
  });

  test('defaults off, records only after consent, keeps tier semantics, and supports both deletion paths', async ({
    page,
  }) => {
    test.slow();
    const session = await registerWithSession(page);
    const card = await openCustomStatusSettings(page);

    await expect(card.getByText('Off', { exact: true })).toBeVisible();
    await expect(card.getByRole('switch', { name: 'Activity History' })).not.toBeChecked();
    const retention = card.getByRole('combobox', { name: 'Retention period' });
    await expect(retention).toHaveValue('30');
    await expect(retention).toBeDisabled();

    const initialSettings = await getHistorySettings(page, session);
    expect(initialSettings.enabled).toBe(false);
    expect(initialSettings.retention_days).toBe(30);
    const initialHistoryResponse = await apiResponse(
      page,
      'GET',
      '/api/v1/users/me/presence-history',
      200,
      { bearer: session.accessToken }
    );
    expect(initialHistoryResponse.headers()['cache-control']).toBe('no-store');
    const initialHistory = (await initialHistoryResponse.json()) as HistoryPage;
    expect(initialHistory.items).toEqual([]);
    await assertHistoryEmpty(page, session);

    await settingsNav(page).getByRole('button', { name: 'Activity History', exact: true }).click();
    const initialFeed = page.getByRole('region', { name: 'Activity History' });
    await expect(
      initialFeed.getByRole('heading', { name: 'Activity History is off' })
    ).toBeVisible();
    await expect(
      initialFeed.getByRole('heading', { name: 'Activity History', level: 3 })
    ).toBeFocused();
    await settingsNav(page).getByRole('button', { name: 'Custom Status', exact: true }).click();
    await expect(card).toBeVisible();

    await patchPresence(page, session, {
      custom_text_tier: 0,
      custom_text: 'Before Activity History consent',
      custom_text_emoji: '🌙',
    });
    await assertHistoryEmpty(page, session);

    await enableHistoryByUi(page, card);
    await expect.poll(async () => (await getHistorySettings(page, session)).enabled).toBe(true);
    await assertHistoryEmpty(page, session);

    const recordedText = 'Recorded while visibility is Off';
    await patchPresence(page, session, {
      custom_text: recordedText,
      custom_text_emoji: '🕶️',
    });
    const recorded = await waitForHistoryItem(page, session, recordedText);
    expect(recorded.ended_at).toBeNull();
    expect((await getPresence(page, session)).custom_text_tier).toBe(0);

    await card.getByRole('button', { name: 'View history' }).click();
    const feed = page.getByRole('region', { name: 'Activity History' });
    const timeline = feed.getByRole('list', { name: 'Activity History timeline' });
    const item = timeline.getByRole('article', { name: 'Custom Status' });
    await expect(item.getByText(recordedText, { exact: true })).toBeVisible();
    await expect(item.getByText('Ongoing', { exact: true })).toBeVisible();

    await patchPresence(page, session, { custom_text_tier: 1 });
    const afterTierChange = await getHistory(page, session);
    expect(afterTierChange.items).toHaveLength(1);
    expect(afterTierChange.items[0]?.id).toBe(recorded.id);
    expect(afterTierChange.items[0]?.ended_at).toBeNull();

    await settingsNav(page).getByRole('button', { name: 'Custom Status', exact: true }).click();
    await retention.selectOption('90');
    await expect
      .poll(async () => (await getHistorySettings(page, session)).retention_days)
      .toBe(90);
    await expect(retention).toHaveValue('90');

    await retention.selectOption('7');
    const shortenDialog = page.getByRole('dialog', {
      name: 'Shorten Activity History retention?',
    });
    await expect(shortenDialog).toBeVisible();
    await shortenDialog.getByRole('button', { name: 'Delete older history' }).click();
    await expect(shortenDialog).toBeHidden();
    await expect.poll(async () => (await getHistorySettings(page, session)).retention_days).toBe(7);
    await expect(retention).toHaveValue('7');

    await card.locator('label.settings-toggle').click();
    const disableDialog = page.getByRole('dialog', { name: 'Turn off Activity History?' });
    await expect(disableDialog).toBeVisible();
    await disableDialog.getByRole('button', { name: 'Turn off and delete history' }).click();
    await expect(disableDialog).toBeHidden();
    await expect(card.getByText('Off', { exact: true })).toBeVisible();
    await expect(card.getByRole('switch', { name: 'Activity History' })).not.toBeChecked();
    await expect(retention).toBeDisabled();
    await expect.poll(async () => (await getHistorySettings(page, session)).enabled).toBe(false);
    await assertHistoryEmpty(page, session);

    await patchPresence(page, session, {
      custom_text: 'Disabled history must not repopulate',
      custom_text_emoji: '🛑',
    });
    await assertHistoryEmpty(page, session);

    await enableHistoryByUi(page, card);
    const deleteText = 'Delete-path Activity History row';
    await patchPresence(page, session, {
      custom_text_tier: 0,
      custom_text: deleteText,
      custom_text_emoji: '🗑️',
    });
    await waitForHistoryItem(page, session, deleteText);
    await card.getByRole('button', { name: 'Delete history and turn off' }).click();
    const deleteDialog = page.getByRole('dialog', { name: 'Delete Activity History?' });
    await expect(deleteDialog).toBeVisible();
    await deleteDialog
      .getByRole('button', { name: 'Delete history and turn off', exact: true })
      .click();
    await expect(deleteDialog).toBeHidden();
    await expect(card.getByText('Off', { exact: true })).toBeVisible();
    await expect(card.getByRole('switch', { name: 'Activity History' })).not.toBeChecked();
    await expect(retention).toBeDisabled();
    await expect.poll(async () => (await getHistorySettings(page, session)).enabled).toBe(false);
    await assertHistoryEmpty(page, session);

    await patchPresence(page, session, {
      custom_text: 'Delete path must remain off',
      custom_text_emoji: '⛔',
    });
    await assertHistoryEmpty(page, session);
  });

  test('a second authenticated user cannot select or infer the owner history', async ({
    page,
    browser,
  }) => {
    test.slow();
    const owner = await registerWithSession(page);
    const ownerText = 'Owner-only Activity History payload';
    await openHistoryInterval(page, owner, ownerText);

    const viewerContext = await browser.newContext({ baseURL: E2E_UI_BASE });
    try {
      const viewerPage = await viewerContext.newPage();
      const viewer = await registerWithSession(viewerPage);
      const ownHistory = await getHistory(viewerPage, viewer);
      expect(ownHistory.items).toEqual([]);

      const queryAttempt = await getHistory(
        viewerPage,
        viewer,
        '?user_id=' + encodeURIComponent(owner.userId)
      );
      expect(queryAttempt.items).toEqual([]);

      await viewerPage.getByRole('button', { name: 'Settings', exact: true }).click();
      await settingsNav(viewerPage)
        .getByRole('button', { name: 'Activity History', exact: true })
        .click();
      const viewerFeed = viewerPage.getByRole('region', { name: 'Activity History' });
      await expect(
        viewerFeed.getByRole('heading', { name: 'Activity History is off' })
      ).toBeVisible();
      await expect(viewerFeed.getByText(ownerText, { exact: true })).toHaveCount(0);

      const nonexistentRoute = await viewerPage
        .context()
        .request.get(API_BASE + '/api/v1/users/' + owner.userId + '/presence-history', {
          headers: { Authorization: 'Bearer ' + viewer.accessToken },
        });
      expect(nonexistentRoute.status()).toBe(404);
    } finally {
      await viewerContext.close();
    }
  });

  test('authenticated key replacement closes the ongoing interval', async ({ page }) => {
    test.slow();
    const session = await registerWithSession(page);
    const original = await openHistoryInterval(page, session, 'Key replacement interval');

    await apiJson<{ message: string }>(page, 'PUT', '/api/v1/users/me/keys', 200, {
      bearer: session.accessToken,
      data: {
        wrapped_private_key: validBase64('replacement-wrapped-private-key'),
        key_derivation_salt: validBase64('replacement-derivation-salt'),
        key_derivation_alg: 'argon2id',
        public_key: validBase64('replacement-public-key'),
        current_password: DEFAULT_PASSWORD,
        acknowledge_data_loss: true,
      },
    });
    await expectHistoryClosed(page, session, original);
  });

  test('recovery password reset closes the ongoing interval', async ({ page }) => {
    test.slow();
    const session = await registerWithSession(page);
    const original = await openHistoryInterval(page, session, 'Recovery password interval');
    const issued = await issueRecoveryToken(page, session, '618204');

    try {
      await apiJson<{ message: string }>(
        page,
        'POST',
        '/api/v1/auth/recovery/reset-password',
        200,
        {
          data: {
            recovery_token: issued.token,
            new_password: RECOVERED_PASSWORD,
            wrapped_private_key: validBase64('recovery-password-wrapped-private-key'),
            key_derivation_salt: validBase64('recovery-password-derivation-salt'),
            key_derivation_alg: 'argon2id',
          },
        }
      );
      await expectHistoryClosed(page, session, original);
    } finally {
      await deleteRedisKey(issued.redisKey);
    }
  });

  test('recovery account reset closes the ongoing interval', async ({ page }) => {
    test.slow();
    const session = await registerWithSession(page);
    const original = await openHistoryInterval(page, session, 'Recovery account interval');
    const issued = await issueRecoveryToken(page, session, '618205');

    try {
      await apiJson<{ message: string }>(page, 'POST', '/api/v1/auth/recovery/reset-account', 200, {
        data: {
          recovery_token: issued.token,
          new_password: RESET_PASSWORD,
          wrapped_private_key: validBase64('recovery-account-wrapped-private-key'),
          key_derivation_salt: validBase64('recovery-account-derivation-salt'),
          key_derivation_alg: 'argon2id',
          public_key: validBase64('recovery-account-public-key'),
          acknowledge_data_loss: true,
        },
      });
      await expectHistoryClosed(page, session, original);
    } finally {
      await deleteRedisKey(issued.redisKey);
    }
  });

  test('account erasure succeeds with a known history child and revokes access', async ({
    page,
  }) => {
    test.slow();
    const session = await registerWithSession(page);
    await openHistoryInterval(page, session, 'Account erasure cascade interval');

    await apiResponse(page, 'POST', '/api/v1/privacy/erase-account', 204, {
      bearer: session.accessToken,
      data: {},
    });

    const denied = await page
      .context()
      .request.get(API_BASE + '/api/v1/users/me/presence-history', {
        headers: { Authorization: 'Bearer ' + session.accessToken },
      });
    expect([401, 403]).toContain(denied.status());

    const login = await page.context().request.post(API_BASE + '/api/v1/auth/login', {
      data: { email: session.email, password: DEFAULT_PASSWORD },
    });
    expect(login.status()).toBe(401);

    // The browser cannot list rows after the owner row is gone. The 204 proves
    // PostgreSQL accepted DELETE FROM users while the known child existed; the
    // migration-87 and repository integration tests are the physical cascade oracle.
  });
});
