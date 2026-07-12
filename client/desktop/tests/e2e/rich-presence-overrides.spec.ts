import { expect, test, type Page, type Response as PlaywrightResponse } from '@playwright/test';
import { loginUser, registerAndLogin } from './helpers';

const E2E_API_PORT = process.env.E2E_API_PORT ?? process.env.VITE_API_PORT ?? '8080';
const API_BASE = `http://localhost:${E2E_API_PORT}`;
const E2E_WS_URL = `ws://localhost:${E2E_API_PORT}/api/v1/ws`;
const E2E_UI_PORT = process.env.E2E_UI_PORT ?? '3001';
const E2E_UI_BASE = `http://localhost:${E2E_UI_PORT}`;
const OVERRIDE_PATH = '/api/v1/users/me/presence-overrides/custom_text';

/**
 * This is a genuine two-user integration spec. It intentionally does not mock
 * the control-plane, PostgreSQL, Redis, E2EE, or WebSocket fan-out.
 *
 * Manual execution therefore requires the same environment as the rest of the
 * desktop E2E suite: a control-plane on the localhost port selected by
 * E2E_API_PORT/VITE_API_PORT (8080 by default), backed by migrated PostgreSQL +
 * Redis and started with CONCORD_ENV=test so registration codes are recoverable
 * through the test-only Redis key. Playwright starts only Vite.
 */

interface AuthSession {
  accessToken: string;
  userId: string;
  username: string;
  email: string;
}

interface WireMessage {
  type: string;
  data?: Record<string, unknown>;
}

interface CapturedFrame {
  sequence: number;
  socket: number;
  message: WireMessage;
}

function parseWireMessage(payload: string | Buffer): WireMessage | null {
  try {
    const value = JSON.parse(
      typeof payload === 'string' ? payload : payload.toString('utf8')
    ) as unknown;
    if (typeof value !== 'object' || value === null || !('type' in value)) return null;
    const type = (value as { type?: unknown }).type;
    if (typeof type !== 'string') return null;
    const data = (value as { data?: unknown }).data;
    return {
      type,
      data:
        typeof data === 'object' && data !== null ? (data as Record<string, unknown>) : undefined,
    };
  } catch {
    return null;
  }
}

class WebSocketRecorder {
  private readonly frames: CapturedFrame[] = [];
  readonly closedSockets = new Set<number>();
  socketCount = 0;

  constructor(page: Page) {
    page.on('websocket', (socket) => {
      // Never retain socket.url(): it contains the single-use authentication ticket.
      const socketNumber = ++this.socketCount;
      socket.on('framereceived', (frame) => {
        const message = parseWireMessage(frame.payload);
        if (!message) return;
        this.frames.push({
          sequence: this.frames.length,
          socket: socketNumber,
          message,
        });
      });
      socket.on('close', () => this.closedSockets.add(socketNumber));
    });
  }

  mark(): number {
    return this.frames.length;
  }

  async waitForFrame(
    after: number,
    predicate: (frame: CapturedFrame) => boolean,
    message: string,
    timeout = 15_000
  ): Promise<CapturedFrame> {
    let match: CapturedFrame | undefined;
    await expect
      .poll(
        () => {
          match = this.frames.slice(after).find(predicate);
          return match !== undefined;
        },
        { message, timeout }
      )
      .toBe(true);
    if (!match) throw new Error(message);
    return match;
  }

  through(socket: number, sequence: number): readonly CapturedFrame[] {
    return this.frames.filter((frame) => frame.socket === socket && frame.sequence <= sequence);
  }
}

function isMessageType(frame: CapturedFrame, type: string): boolean {
  return frame.message.type === type;
}

function isCustomTextFrame(frame: CapturedFrame, type: string, senderId: string): boolean {
  return (
    frame.message.type === type &&
    frame.message.data?.user_id === senderId &&
    frame.message.data?.category === 'custom_text'
  );
}

async function parseLoginResponse(
  responsePromise: Promise<PlaywrightResponse>,
  credentials: { username: string; email: string }
): Promise<AuthSession> {
  const response = await responsePromise;
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
    ...credentials,
  };
}

function nextLoginResponse(page: Page): Promise<PlaywrightResponse> {
  return page.waitForResponse(
    (response) =>
      response.url().includes('/api/v1/auth/login') && response.request().method() === 'POST'
  );
}

async function registerWithSession(page: Page): Promise<AuthSession> {
  const responsePromise = nextLoginResponse(page);
  const credentials = await registerAndLogin(page);
  return parseLoginResponse(responsePromise, credentials);
}

async function loginWithSession(page: Page, session: AuthSession): Promise<AuthSession> {
  const responsePromise = nextLoginResponse(page);
  await loginUser(page, session.email);
  return parseLoginResponse(responsePromise, {
    username: session.username,
    email: session.email,
  });
}

async function apiJson<T>(
  page: Page,
  method: 'POST' | 'PATCH',
  path: string,
  accessToken: string,
  data: Record<string, unknown>,
  expectedStatus: number
): Promise<T> {
  const response = await page.context().request.fetch(`${API_BASE}${path}`, {
    method,
    headers: { Authorization: `Bearer ${accessToken}` },
    data,
  });
  if (response.status() !== expectedStatus) {
    throw new Error(
      `${method} ${path} returned ${response.status()} instead of ${expectedStatus}: ${await response.text()}`
    );
  }
  return (await response.json()) as T;
}

async function captureFreshConnectionSnapshot(
  page: Page,
  accessToken: string
): Promise<WireMessage[]> {
  const ticketResponse = await page.context().request.post(`${API_BASE}/api/v1/auth/ws-ticket`, {
    headers: { Authorization: `Bearer ${accessToken}` },
  });
  expect(ticketResponse.status(), 'fresh-snapshot WS ticket request should succeed').toBe(200);
  const body = (await ticketResponse.json()) as { ticket?: unknown };
  if (typeof body.ticket !== 'string') {
    throw new Error('WS ticket response omitted ticket');
  }

  return page.evaluate(
    async ({ ticket, wsEndpoint }) =>
      new Promise<WireMessage[]>((resolve, reject) => {
        const messages: WireMessage[] = [];
        const wsUrl = new URL(wsEndpoint);
        wsUrl.searchParams.set('ticket', ticket);
        const socket = new WebSocket(wsUrl);
        const timeout = window.setTimeout(() => {
          socket.close();
          reject(new Error('fresh snapshot WebSocket did not reach connection_ready'));
        }, 15_000);

        const finish = (result: WireMessage[]): void => {
          window.clearTimeout(timeout);
          socket.close();
          resolve(result);
        };

        socket.addEventListener('open', () => {
          socket.send(
            JSON.stringify({
              type: 'connection_ready_probe',
              data: { protocol_version: 2 },
            })
          );
        });
        socket.addEventListener('message', (event) => {
          if (typeof event.data !== 'string') return;
          try {
            const value = JSON.parse(event.data) as unknown;
            if (typeof value !== 'object' || value === null) return;
            const type = (value as { type?: unknown }).type;
            if (typeof type !== 'string') return;
            const rawData = (value as { data?: unknown }).data;
            const message: WireMessage = {
              type,
              data:
                typeof rawData === 'object' && rawData !== null
                  ? (rawData as Record<string, unknown>)
                  : undefined,
            };
            messages.push(message);
            if (type === 'connection_ready') finish([...messages]);
          } catch {
            // Ignore non-JSON frames; the application protocol is JSON.
          }
        });
        socket.addEventListener('error', () => {
          window.clearTimeout(timeout);
          reject(new Error('fresh snapshot WebSocket failed'));
        });
      }),
    { ticket: body.ticket, wsEndpoint: E2E_WS_URL }
  );
}

async function makeFriends(
  senderPage: Page,
  sender: AuthSession,
  viewer: AuthSession
): Promise<void> {
  const request = await apiJson<{ id: string }>(
    senderPage,
    'POST',
    '/api/v1/friends/request',
    sender.accessToken,
    { user_id: viewer.userId },
    201
  );
  expect(request.id).toMatch(/^[0-9a-f-]{36}$/i);
  await apiJson<{ message: string }>(
    senderPage,
    'PATCH',
    `/api/v1/friends/request/${request.id}`,
    viewer.accessToken,
    { action: 'accept' },
    200
  );
}

function waitForOverrideWrite(page: Page): Promise<PlaywrightResponse> {
  return page.waitForResponse((response) => {
    const url = new URL(response.url());
    return url.pathname === OVERRIDE_PATH && response.request().method() === 'PUT';
  });
}

async function expectSuccessfulOverrideWrite(
  responsePromise: Promise<PlaywrightResponse>
): Promise<void> {
  const response = await responsePromise;
  expect(response.status(), `override write failed: ${await response.text()}`).toBe(200);
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

async function openExceptions(senderPage: Page): Promise<void> {
  await senderPage.getByRole('button', { name: 'Settings', exact: true }).click();
  await expect(senderPage.getByRole('heading', { name: 'Settings', level: 1 })).toBeVisible();
  const settingsNav = senderPage.locator('nav.settings-nav');
  await settingsNav.getByRole('button', { name: 'Custom Status', exact: true }).click();
  const summary = senderPage.getByText('Exceptions - 0 people', { exact: true });
  await expect(summary).toBeVisible();
  await summary.click();
  await expect(senderPage.getByRole('button', { name: 'Add exceptions' })).toBeVisible();
}

async function addException(senderPage: Page, viewerUsername: string): Promise<void> {
  await senderPage.getByRole('button', { name: 'Add exceptions' }).click();
  const dialog = senderPage.getByRole('dialog', { name: 'Custom Status exceptions' });
  await expect(dialog).toBeVisible();
  await dialog
    .getByRole('checkbox', { name: new RegExp(`@${escapeRegExp(viewerUsername)}$`) })
    .check();
  const responsePromise = waitForOverrideWrite(senderPage);
  await dialog.getByRole('button', { name: 'Save exceptions' }).click();
  await expectSuccessfulOverrideWrite(responsePromise);
  await expect(dialog).toBeHidden();
  await expect(senderPage.getByText('Exceptions - 1 person', { exact: true })).toBeVisible();
}

test.describe('Custom Status recipient exceptions', () => {
  test('add clears, removal restores, and reconnect snapshot stays excluded', async ({
    page: senderPage,
    browser,
  }) => {
    test.slow();

    const viewerContext = await browser.newContext({ baseURL: E2E_UI_BASE });
    const viewerPage = await viewerContext.newPage();
    const viewerWire = new WebSocketRecorder(viewerPage);

    try {
      let sender = await registerWithSession(senderPage);
      let viewer = await registerWithSession(viewerPage);
      const viewerPreviousSocket = viewerWire.socketCount;
      viewer = await loginWithSession(viewerPage, viewer);
      await viewerWire.waitForFrame(
        0,
        (frame) => frame.socket > viewerPreviousSocket && isMessageType(frame, 'connection_ready'),
        'viewer fresh-login WebSocket never reached connection_ready'
      );

      await makeFriends(senderPage, sender, viewer);
      // A fresh sender login deterministically hydrates the accepted friend into
      // the picker instead of relying on the timing of a setup-only WS event.
      sender = await loginWithSession(senderPage, sender);

      const baselineMark = viewerWire.mark();
      await apiJson<Record<string, unknown>>(
        senderPage,
        'PATCH',
        '/api/v1/users/me/presence-settings',
        sender.accessToken,
        {
          custom_text_tier: 1,
          custom_text: 'E2E recipient exception',
          custom_text_emoji: '🔒',
        },
        200
      );
      await viewerWire.waitForFrame(
        baselineMark,
        (frame) => isCustomTextFrame(frame, 'rich_presence_update', sender.userId),
        'friend viewer did not receive the baseline Custom Status update'
      );

      await openExceptions(senderPage);

      const addMark = viewerWire.mark();
      await addException(senderPage, viewer.username);
      await viewerWire.waitForFrame(
        addMark,
        (frame) => isCustomTextFrame(frame, 'rich_presence_clear', sender.userId),
        'adding the viewer as an exception did not clear the live Custom Status'
      );

      const removeMark = viewerWire.mark();
      const removeResponse = waitForOverrideWrite(senderPage);
      await senderPage.getByRole('button', { name: `Remove ${viewer.username}` }).click();
      await expectSuccessfulOverrideWrite(removeResponse);
      await expect(senderPage.getByText('Exceptions - 0 people', { exact: true })).toBeVisible();
      await viewerWire.waitForFrame(
        removeMark,
        (frame) => isCustomTextFrame(frame, 'rich_presence_update', sender.userId),
        'removing the exception did not restore the live Custom Status'
      );

      const permittedSnapshotWindow = await captureFreshConnectionSnapshot(
        viewerPage,
        viewer.accessToken
      );
      const permittedPresenceIndex = permittedSnapshotWindow.findIndex(
        (message) => message.type === 'presence_snapshot'
      );
      const permittedUpdateIndex = permittedSnapshotWindow.findIndex(
        (message) =>
          message.type === 'rich_presence_update' &&
          message.data?.user_id === sender.userId &&
          message.data?.category === 'custom_text'
      );
      const permittedReadyIndex = permittedSnapshotWindow.findIndex(
        (message) => message.type === 'connection_ready'
      );
      expect(
        permittedPresenceIndex,
        'permitted viewer fresh socket omitted presence_snapshot'
      ).toBeGreaterThanOrEqual(0);
      expect(
        permittedUpdateIndex,
        'permitted viewer fresh snapshot omitted sender Custom Status'
      ).toBeGreaterThan(permittedPresenceIndex);
      expect(permittedReadyIndex).toBeGreaterThan(permittedUpdateIndex);

      const readdMark = viewerWire.mark();
      await addException(senderPage, viewer.username);
      await viewerWire.waitForFrame(
        readdMark,
        (frame) => isCustomTextFrame(frame, 'rich_presence_clear', sender.userId),
        're-adding the exception did not clear the live Custom Status'
      );

      // Open a fresh ticket-authenticated socket and place an explicit v2
      // connection-ready barrier after the server's connect-time snapshot.
      // This avoids relying on browser reload/offline timing while exercising
      // the same Hub registration and snapshot path used by reconnects.
      const reconnectSnapshotWindow = await captureFreshConnectionSnapshot(
        viewerPage,
        viewer.accessToken
      );
      const snapshotIndex = reconnectSnapshotWindow.findIndex(
        (message) => message.type === 'presence_snapshot'
      );
      const readyIndex = reconnectSnapshotWindow.findIndex(
        (message) => message.type === 'connection_ready'
      );
      expect(snapshotIndex, 'fresh viewer socket omitted presence_snapshot').toBeGreaterThanOrEqual(
        0
      );
      expect(readyIndex).toBeGreaterThan(snapshotIndex);

      // sendCustomTextSnapshot queues every permitted rich_presence_update
      // immediately after presence_snapshot. connection_ready is queued later,
      // so the interval through that barrier is a deterministic snapshot window,
      // not a timing-based negative assertion.
      expect(
        reconnectSnapshotWindow.some(
          (message) =>
            message.type === 'rich_presence_update' &&
            message.data?.user_id === sender.userId &&
            message.data?.category === 'custom_text'
        ),
        'excluded viewer received the sender Custom Status during reconnect snapshot delivery'
      ).toBe(false);
    } finally {
      await viewerContext.close();
    }
  });
});
