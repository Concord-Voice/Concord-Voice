import { Page, expect } from '@playwright/test';
import Redis from 'ioredis';

const API_BASE = 'http://localhost:8080';

/**
 * Fetch a pending-registration's verification code from Redis.
 *
 * The control-plane writes the plaintext 6-digit code to
 * `test_only:<pending_id>` when `CONCORD_ENV=test`, alongside the hashed
 * code stored at `email_verify:<pending_id>` (see
 * services/control-plane/internal/auth/handlers.go `sendInitialCode`).
 *
 * This helper is the e2e equivalent of Go integration tests'
 * `testhelpers.FetchVerificationCode`. It polls the key for up to
 * `timeoutMs` before giving up, since the backend's Redis write happens
 * concurrently with the API response.
 *
 * REQUIRES: control-plane started with `CONCORD_ENV=test`. (The removed
 * CI workflow's "Start control-plane (background)" step used to set this —
 * CI enforcement was removed in #1435; these specs run manually via
 * `npm run test:e2e`.) Local-dev backends typically do NOT set it, so start
 * the backend with CONCORD_ENV=test before a manual run.
 *
 * @throws if the key is absent after `timeoutMs`, with a hint about
 *         the most likely cause (CONCORD_ENV not set).
 */
async function fetchVerificationCode(pendingId: string, timeoutMs = 5000): Promise<string> {
  // Connection URL: prefer REDIS_URL from environment so the workflow can
  // pass redis://localhost:6379 (no password — CI service container is
  // auth-less) and local dev can pass redis://:<password>@localhost:6379.
  // Defaults to localhost auth-less for environments that match the CI
  // shape without explicit env config.
  const redisUrl = process.env.REDIS_URL ?? 'redis://localhost:6379';
  // connectTimeout + commandTimeout: bound any individual Redis operation
  // so the polling loop's `timeoutMs` is the genuine deadline, not just
  // the time-budget-after-connect-succeeds. Without these, ioredis would
  // retry connect/auth on its own schedule (default ~10s+) before any
  // command timed out, masking the polling-loop deadline.
  const client = new Redis(redisUrl, {
    maxRetriesPerRequest: 1,
    lazyConnect: false,
    connectTimeout: 2000,
    commandTimeout: 1000,
  });
  try {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const code = await client.get(`test_only:${pendingId}`);
      if (code) return code;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    throw new Error(
      `Verification code not found at Redis key test_only:${pendingId} within ${timeoutMs}ms. ` +
        `This likely means the control-plane is not running with CONCORD_ENV=test (see ` +
        `services/control-plane/internal/auth/handlers.go isTestEnv()). The Playwright workflow ` +
        `sets this env var; local dev backends typically do not.`
    );
  } finally {
    // Best-effort cleanup: if quit() throws (e.g., connection never
    // established), don't mask the original error from the polling loop.
    try {
      await client.quit();
    } catch {
      // Intentional: cleanup failure should not shadow the real test failure.
    }
  }
}

/** Generate a unique username for test isolation */
export function uniqueUsername(prefix = 'e2e'): string {
  const suffix = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  return `${prefix}_${suffix}`;
}

/** Generate a unique email for test isolation */
export function uniqueEmail(prefix = 'e2e'): string {
  return `${uniqueUsername(prefix)}@test.concord.chat`;
}

const TEST_PASSWORD = 'E2ETestPassword123!';

/**
 * Navigate past the SPA's initial ConnectionSelector step.
 *
 * The SPA opens to a 3-option ConnectionSelector ("Sign In to Existing
 * Account", "Create New Account", "Connect to Self-Hosted Server"). This
 * helper selects the appropriate option and clicks Continue, landing on
 * Register or Login as requested.
 *
 * Implementation note: ConnectionSelector.tsx puts the `<input type="radio">`
 * in a visually-hidden wrapper. Playwright's actionability check would refuse
 * to click a hidden element. We click the visible option-title text instead;
 * HTML's `<label>` semantics forward the click to the underlying radio, which
 * triggers React's onChange. Accessibility-first per [internal]rules/tests.md.
 *
 * @param page Playwright page (must be at the connection-select step,
 *             i.e., just after `page.goto('/')` for an unauthenticated
 *             session).
 * @param mode 'register' lands on Register; 'signin' lands on Login.
 */
async function selectConnectionMode(page: Page, mode: 'register' | 'signin'): Promise<void> {
  const optionName = mode === 'register' ? 'Create New Account' : 'Sign In to Existing Account';
  await page.getByText(optionName, { exact: true }).click();
  await page.getByRole('button', { name: 'Continue' }).click();
}

/**
 * Register a new user via the UI.
 * Navigates ConnectionSelector → Register form, fills it out, and submits.
 * Returns the username and email.
 */
export async function registerUser(
  page: Page,
  options?: { username?: string; email?: string }
): Promise<{ username: string; email: string }> {
  const username = options?.username || uniqueUsername();
  const email = options?.email || uniqueEmail();

  await page.goto('/');

  // SPA opens at ConnectionSelector (post-#808). Select "Create New
  // Account" → Continue → lands on Register's "Create Your Account".
  await selectConnectionMode(page, 'register');

  // Wait for register form
  await expect(page.getByText('Create Your Account')).toBeVisible({ timeout: 10_000 });

  // Fill form
  await page.getByPlaceholder('you@example.com').fill(email);
  await page.getByPlaceholder('your_username').fill(username);
  await page.getByPlaceholder('Create a strong password').fill(TEST_PASSWORD);
  await page.getByPlaceholder('Confirm your password').fill(TEST_PASSWORD);

  // Check age confirmation. Playwright's .check()/.setChecked() dispatches
  // a DOM click that React's synthetic event system doesn't pick up for
  // this particular controlled-component pattern (verified). The simplest
  // robust workaround is to call HTMLInputElement.click() directly in
  // page.evaluate — browsers automatically fire the change event after a
  // checkbox click, which React DOES recognize. See #1263 investigation.
  await page.evaluate(() => {
    // Scope to the register-form to avoid first-match fragility if a future
    // Register-form change introduces another checkbox (e.g., remember-me,
    // marketing opt-in). Without the form scope, `input[type="checkbox"]`
    // would retarget to whatever appeared first in document order and
    // submit-with-age-unconfirmed would silently break.
    const cb = document.querySelector<HTMLInputElement>(
      'form.register-form input[type="checkbox"]'
    );
    if (!cb) throw new Error('Age-confirmation checkbox not found');
    cb.click();
  });

  // Set up response capture BEFORE submit so we don't race the network round-trip.
  // The register endpoint returns { pending_id, email, code_expires_at } for the
  // post-#1058 pending-registration flow. We need pending_id to recover the
  // verification code from Redis.
  const registerResponsePromise = page.waitForResponse(
    (resp) =>
      resp.url().includes('/api/v1/auth/register') &&
      !resp.url().includes('/register/confirm') &&
      !resp.url().includes('/register/resend') &&
      !resp.url().includes('/register/change-email') &&
      resp.request().method() === 'POST'
  );

  // Submit. Note: Playwright's click on the <button type="submit"> doesn't
  // reach the form's onSubmit handler under React 19 in this codebase
  // (verified empirically: both getByText().click() and
  // getByRole('button').click() silently no-op; React's handleSubmit
  // never runs and no /api/v1/auth/register request fires). The
  // workaround is to call form.requestSubmit() programmatically, which
  // does fire the synthetic React submit event correctly. See #1263.
  await page.evaluate(() => {
    const form = document.querySelector<HTMLFormElement>('form.register-form');
    if (!form) throw new Error('Register form not found');
    form.requestSubmit();
  });

  // Capture pending_id from the register response.
  const registerResponse = await registerResponsePromise;
  if (registerResponse.status() < 200 || registerResponse.status() >= 300) {
    throw new Error(
      `Register API returned HTTP ${registerResponse.status()}: ${await registerResponse.text()}`
    );
  }
  const registerBody = (await registerResponse.json()) as { pending_id?: string };
  if (!registerBody.pending_id) {
    throw new Error(`Register response missing pending_id: ${JSON.stringify(registerBody)}`);
  }
  const pendingId = registerBody.pending_id;

  // The SPA transitions to the email-verification step (AuthFlow.tsx
  // handleRegistrationSuccess sets step='email-verification'). The
  // EmailVerification component renders TOTPInput with autoFocus, so we can
  // type the 6 digits and the form submits on completion.
  await expect(page.getByText('Verify your email')).toBeVisible({ timeout: 10_000 });

  const verificationCode = await fetchVerificationCode(pendingId);
  await page.keyboard.type(verificationCode);

  // Wait for navigation to app — happens after the SPA's confirm POST
  // completes and the auth store flips emailVerified=true.
  await expect(page).toHaveURL(/\/app/, { timeout: 30_000 });

  return { username, email };
}

/**
 * Login an existing user via the UI.
 * Navigates ConnectionSelector → Login form, fills it out, and submits.
 */
export async function loginUser(
  page: Page,
  email: string,
  password = TEST_PASSWORD
): Promise<void> {
  await page.goto('/');

  // SPA opens at ConnectionSelector (post-#808). Select "Sign In to
  // Existing Account" → Continue → lands on Login's "Welcome Back".
  await selectConnectionMode(page, 'signin');

  // Wait for login form
  await expect(page.getByText('Welcome Back')).toBeVisible({ timeout: 10_000 });

  await page.getByPlaceholder('you@example.com').fill(email);
  await page.getByPlaceholder('Enter your password').fill(password);

  // See registerUser for the form.requestSubmit() rationale — clicking
  // the <button type="submit"> doesn't reach React 19's onSubmit handler
  // for these forms; programmatic submission does.
  await page.evaluate(() => {
    const form = document.querySelector<HTMLFormElement>('form.login-form');
    if (!form) throw new Error('Login form not found');
    form.requestSubmit();
  });

  // Wait for navigation to app
  await expect(page).toHaveURL(/\/app/, { timeout: 30_000 });
}

/**
 * Register a fresh user AND log in, leaving e2eeService initialized.
 *
 * The registration-confirm flow does NOT call e2eeService.initialize — only
 * login does (Login.tsx). A registration-only session therefore cannot create
 * channels or send messages: CreateChannelModal guards key-wrapping behind
 * e2eeService.isInitialized and shows "Setting up secure messaging" otherwise.
 * Specs that exercise channels/messaging must use this helper, not registerUser
 * alone. See #1274.
 */
export async function registerAndLogin(page: Page): Promise<{ username: string; email: string }> {
  const creds = await registerUser(page);
  await loginUser(page, creds.email);
  return creds;
}

/**
 * Create a server via the UI.
 * Assumes user is already logged in.
 */
export async function createServer(page: Page, name: string): Promise<void> {
  // Open the create-server modal via the server-bar button. Its accessible
  // name is "Add Server" (ServerBar.tsx aria-label), NOT "Add a Server" — the
  // latter was stale and silently broke every server/channel/messaging spec
  // until the #1274 reconciliation.
  await page.getByLabel('Add Server').click();

  // "Add Server" opens a chooser (ServerActionModal: "Create a Server" /
  // "Join a Server"), not the create form directly — pick "Create a Server".
  // This intermediate step was the gap remaining after the first #1274 pass.
  await page.getByRole('button', { name: 'Create a Server' }).click();

  // The server-name field is labeled "Server Name" (its placeholder is
  // "My Awesome Server", so a placeholder-based locator no longer matches —
  // use the associated <label>).
  const nameInput = page.getByLabel('Server Name');
  await expect(nameInput).toBeVisible({ timeout: 5_000 });
  await nameInput.fill(name);

  // Submit via form.requestSubmit(): clicking <button type="submit"> does not
  // reliably fire React 19's onSubmit in this codebase (same quirk documented
  // for registerUser/loginUser above).
  await page.evaluate(() => {
    const form = document.querySelector<HTMLFormElement>('form.create-server-form');
    if (!form) throw new Error('Create-server form not found');
    form.requestSubmit();
  });

  // Ensure the new server is active. handleCreateServerSuccess calls
  // setActiveServer, but that races the server-list load: the new server is
  // sometimes auto-selected (its bar icon becomes "Toggle channel panel") and
  // sometimes not (icon stays "${name} server"). Poll for the channel-panel
  // header (<h3>, level-3 to avoid the placeholder heading); if it's not
  // showing, click the still-inactive bar icon to select it. Clicking is
  // best-effort — once active, that by-name icon no longer exists.
  const header = page.getByRole('heading', { name, level: 3 });
  const inactiveIcon = page.getByLabel(`${name} server`);
  await expect(async () => {
    if (await inactiveIcon.isVisible().catch(() => false)) {
      await inactiveIcon.click().catch(() => {});
    }
    await expect(header).toBeVisible({ timeout: 1_500 });
  }).toPass({ timeout: 20_000 });
}

/**
 * Create a channel in the current server.
 * Assumes user is already logged in and has a server selected.
 */
export async function createChannel(page: Page, name: string): Promise<void> {
  // Open the create-channel modal via the ServerActionBar two-step flow:
  // the "Add" button toggles an inline popup, then the "Channel" item opens
  // the modal. The old single getByText(/new channel/i) matched nothing.
  // `exact: true` on "Add" is required — a substring match would also hit the
  // server-bar "Add Server" button. The "Add" affordance only renders for users
  // with MANAGE_CHANNELS, which the server owner (our test user) has.
  await page.getByRole('button', { name: 'Add', exact: true }).click();
  await page.getByRole('button', { name: 'Channel', exact: true }).click();

  // The field is labeled "Channel Name" (placeholder is "general-chat", so the
  // old /channel name/i placeholder locator is stale).
  const nameInput = page.getByLabel('Channel Name');
  await expect(nameInput).toBeVisible({ timeout: 5_000 });
  await nameInput.fill(name);

  // Submit, retrying until the channel appears. Right after registration the
  // E2EE service may still be initializing; CreateChannelModal guards channel
  // creation behind `e2eeService.isInitialized` and shows "Setting up secure
  // messaging — try again in a moment." (CreateChannelModal.tsx). The modal
  // stays open on that guard, so we re-submit until E2EE is ready and the
  // channel is created. `?.requestSubmit()` no-ops once the modal closes on
  // success, so a successful create is not double-submitted.
  const channelButton = page.getByRole('button', { name, exact: true });
  await expect(async () => {
    await page.evaluate(() => {
      document.querySelector<HTMLFormElement>('form.create-channel-form')?.requestSubmit();
    });
    await expect(channelButton).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 30_000 });
}

/**
 * Clean up: delete a server directly via API.
 * Used for test teardown.
 */
export async function deleteServerViaAPI(accessToken: string, serverId: string): Promise<void> {
  await fetch(`${API_BASE}/api/v1/servers/${serverId}`, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${accessToken}` },
  });
}
