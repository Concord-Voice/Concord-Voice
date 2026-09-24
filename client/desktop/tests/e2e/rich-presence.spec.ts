import {
  expect,
  test,
  type Browser,
  type Page,
  type Response as PlaywrightResponse,
} from '@playwright/test';
import Redis from 'ioredis';
import { createServer, loginUser, registerAndLogin } from './helpers';

const E2E_API_PORT = process.env.E2E_API_PORT ?? process.env.VITE_API_PORT ?? '8080';
const API_BASE = `http://localhost:${E2E_API_PORT}`;
const E2E_WS_URL = `ws://localhost:${E2E_API_PORT}/api/v1/ws`;
const E2E_UI_BASE = `http://localhost:${process.env.E2E_UI_PORT ?? '3001'}`;
const VIEW_VOICE_CHANNELS = 1 << 9;

type ApiMethod = 'GET' | 'POST' | 'PATCH' | 'PUT';

interface AuthSession {
  accessToken: string;
  userId: string;
  username: string;
  email: string;
}

interface WireMessage {
  type: string;
  userId?: string;
  category?: string;
  minimized?: boolean;
  payloadKeys?: readonly string[];
}

interface CapturedFrame {
  sequence: number;
  socket: number;
  message: WireMessage;
}

function payloadKeyPaths(value: object, prefix = ''): string[] {
  return Object.entries(value).flatMap(([key, child]) => {
    const path = prefix ? `${prefix}.${key}` : key;
    return [
      key,
      ...(prefix ? [path] : []),
      ...(typeof child === 'object' && child !== null ? payloadKeyPaths(child, path) : []),
    ];
  });
}

function structuralWireMessages(payload: string | Buffer): WireMessage[] {
  try {
    const raw = JSON.parse(
      typeof payload === 'string' ? payload : payload.toString('utf8')
    ) as unknown;
    if (typeof raw !== 'object' || raw === null || !('type' in raw)) return [];
    const type = (raw as { type?: unknown }).type;
    if (typeof type !== 'string') return [];
    const data = (raw as { data?: unknown }).data;
    if (typeof data !== 'object' || data === null) return [{ type }];
    const value = data as Record<string, unknown>;
    if (type === 'presence_snapshot' && Array.isArray(value.users)) {
      const updates: WireMessage[] = [];
      for (const rawUser of value.users) {
        if (typeof rawUser !== 'object' || rawUser === null) continue;
        const user = rawUser as Record<string, unknown>;
        if (typeof user.user_id !== 'string') continue;
        const richPresence = user.rich_presence;
        if (typeof richPresence !== 'object' || richPresence === null) continue;
        for (const [category, rawActivity] of Object.entries(richPresence)) {
          if (typeof rawActivity !== 'object' || rawActivity === null) continue;
          const activity = rawActivity as Record<string, unknown>;
          const payload = activity.payload;
          updates.push({
            type: 'rich_presence_update',
            userId: user.user_id,
            category,
            ...(typeof activity.minimized === 'boolean' ? { minimized: activity.minimized } : {}),
            ...(typeof payload === 'object' && payload !== null
              ? { payloadKeys: payloadKeyPaths(payload) }
              : {}),
          });
        }
      }
      return updates;
    }
    const payloadValue = value.payload;
    return [
      {
        type,
        ...(typeof value.user_id === 'string' ? { userId: value.user_id } : {}),
        ...(typeof value.category === 'string' ? { category: value.category } : {}),
        ...(typeof value.minimized === 'boolean' ? { minimized: value.minimized } : {}),
        ...(typeof payloadValue === 'object' && payloadValue !== null
          ? { payloadKeys: payloadKeyPaths(payloadValue) }
          : {}),
      },
    ];
  } catch {
    return [];
  }
}

test('structural wire metadata includes nested keys without retaining values', () => {
  const [message] = structuralWireMessages(
    JSON.stringify({
      type: 'rich_presence_update',
      data: { payload: { details: { channel_name: 'secret' } } },
    })
  );

  expect(message.payloadKeys).toContain('details.channel_name');
  expect(JSON.stringify(message)).not.toContain('secret');
});

test('denied Server Voice detection catches mislabeled payload fields', () => {
  expect(
    isForbiddenServerVoice(
      {
        type: 'rich_presence_update',
        userId: 'wrong-user',
        category: 'wrong-category',
        payloadKeys: ['details.channel_name', 'channel_name'],
      },
      'sender'
    )
  ).toBe(true);
});

class WebSocketRecorder {
  private readonly frames: CapturedFrame[] = [];
  socketCount = 0;

  constructor(page: Page) {
    page.on('websocket', (socket) => {
      // Do not retain socket.url(): it contains a single-use authentication ticket.
      const socketNumber = ++this.socketCount;
      socket.on('framereceived', (frame) => {
        for (const message of structuralWireMessages(frame.payload)) {
          this.frames.push({ sequence: this.frames.length, socket: socketNumber, message });
        }
      });
    });
  }

  mark(): number {
    return this.frames.length;
  }

  async waitForFrame(
    after: number,
    predicate: (frame: CapturedFrame) => boolean,
    message: string,
    timeout = 20_000
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

  hasFrame(after: number, predicate: (frame: CapturedFrame) => boolean): boolean {
    return this.frames.slice(after).some(predicate);
  }
}

function hasActivity(
  frame: CapturedFrame,
  type: string,
  senderId: string,
  category: string
): boolean {
  return (
    frame.message.type === type &&
    frame.message.userId === senderId &&
    frame.message.category === category
  );
}

function isForbiddenServerVoice(message: WireMessage, senderId: string): boolean {
  return (
    (['rich_presence_update', 'rich_presence_clear'].includes(message.type) &&
      message.userId === senderId &&
      message.category === 'server_voice') ||
    (message.payloadKeys?.some((key) =>
      ['channel_id', 'server_id', 'channel_name', 'server_name'].includes(key)
    ) ??
      false)
  );
}

async function nextLoginResponse(page: Page): Promise<PlaywrightResponse> {
  return page.waitForResponse(
    (response) =>
      response.url().includes('/api/v1/auth/login') && response.request().method() === 'POST'
  );
}

async function sessionFromLogin(
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
    throw new Error('login response omitted required session metadata');
  }
  return {
    accessToken: body.access_token,
    userId: body.user.id,
    ...credentials,
  };
}

async function registerWithSession(page: Page): Promise<AuthSession> {
  const response = nextLoginResponse(page);
  const credentials = await registerAndLogin(page);
  return sessionFromLogin(response, credentials);
}

async function loginWithSession(page: Page, session: AuthSession): Promise<AuthSession> {
  const response = nextLoginResponse(page);
  await loginUser(page, session.email);
  return sessionFromLogin(response, session);
}

async function newParticipant(
  browser: Browser,
  forceLegacyE2EE = false
): Promise<{
  context: Awaited<ReturnType<Browser['newContext']>>;
  page: Page;
  wire: WebSocketRecorder;
  session: AuthSession;
}> {
  const context = await browser.newContext({ baseURL: E2E_UI_BASE });
  try {
    if (forceLegacyE2EE) {
      await context.addInitScript(() => localStorage.setItem('concord.forceLegacyE2EE', '1'));
    }
    const page = await context.newPage();
    const wire = new WebSocketRecorder(page);
    const session = await registerWithSession(page);
    return { context, page, wire, session };
  } catch (error) {
    await context.close().catch(() => undefined);
    throw error;
  }
}

async function apiJson<T>(
  page: Page,
  method: ApiMethod,
  path: string,
  accessToken: string,
  data: Record<string, unknown> | undefined,
  expectedStatus: number
): Promise<T> {
  const response = await page.context().request.fetch(`${API_BASE}${path}`, {
    method,
    headers: { Authorization: `Bearer ${accessToken}` },
    ...(data ? { data } : {}),
  });
  expect(response.status(), `${method} ${path} returned an unexpected status`).toBe(expectedStatus);
  return (await response.json()) as T;
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
  await apiJson<Record<string, unknown>>(
    senderPage,
    'PATCH',
    `/api/v1/friends/request/${request.id}`,
    viewer.accessToken,
    { action: 'accept' },
    200
  );
}

async function createServerWithId(page: Page, name: string): Promise<string> {
  const created = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === '/api/v1/servers' &&
      response.request().method() === 'POST' &&
      response.status() === 201
  );
  await createServer(page, name);
  const body = (await (await created).json()) as { server?: { id?: unknown } };
  if (typeof body.server?.id !== 'string') throw new Error('server setup response omitted id');
  return body.server.id;
}

async function createUnlimitedInvite(
  page: Page,
  accessToken: string,
  serverId: string
): Promise<string> {
  const body = await apiJson<{ invite?: { code?: unknown } }>(
    page,
    'POST',
    `/api/v1/servers/${serverId}/invites`,
    accessToken,
    { max_uses: 0, expires_in: 3600 },
    201
  );
  if (typeof body.invite?.code !== 'string') throw new Error('invite setup response omitted code');
  return body.invite.code;
}

async function joinWithInvite(page: Page, inviteCode: string, serverName: string): Promise<void> {
  await page.getByLabel('Add Server').click();
  await page.getByRole('button', { name: 'Join a Server' }).click();
  await page.getByLabel('Invite Code').fill(inviteCode);
  const join = page.getByRole('button', { name: 'Join Server' });
  await expect(join).toBeEnabled({ timeout: 10_000 });
  await join.click();
  const server = page.getByLabel(`${serverName} server`);
  await expect(server).toBeVisible({ timeout: 15_000 });
  await server.click();
  await expect(page.getByRole('heading', { name: serverName, level: 3 })).toBeVisible();
}

async function createVoiceChannel(page: Page, name: string): Promise<string> {
  await page.getByRole('button', { name: 'Add', exact: true }).click();
  await page.getByRole('button', { name: 'Channel', exact: true }).click();
  await page.getByLabel('Channel Name').fill(name);
  await page
    .getByRole('button', {
      name: 'Voice Voice and video conversations',
      exact: true,
    })
    .click();
  const created = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === '/api/v1/channels' &&
      response.request().method() === 'POST' &&
      response.status() === 201,
    { timeout: 40_000 }
  );
  const channelButton = page.getByRole('button', { name, exact: true });
  const [response] = await Promise.all([
    created,
    expect(async () => {
      await page.evaluate(() => {
        document.querySelector<HTMLFormElement>('form.create-channel-form')?.requestSubmit();
      });
      await expect(channelButton).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 30_000 }),
  ]);
  const body = (await response.json()) as { channel?: { id?: unknown } };
  if (typeof body.channel?.id !== 'string')
    throw new Error('voice-channel setup response omitted id');
  return body.channel.id;
}

async function openSettings(page: Page): Promise<void> {
  await page.locator('button.user-panel-settings-btn').click();
  await expect(page.getByRole('heading', { name: 'Settings', level: 1 })).toBeVisible();
  await page
    .locator('nav.settings-nav')
    .getByRole('button', { name: 'Rich Presence', exact: true })
    .click();
  await expect(page.getByRole('heading', { name: 'Server Voice', level: 3 })).toBeVisible();
}

async function freshSnapshot(page: Page, accessToken: string): Promise<WireMessage[]> {
  return page.evaluate(
    async ({ accessToken, wsEndpoint }) =>
      new Promise<WireMessage[]>((resolve, reject) => {
        let socket: WebSocket | undefined;
        let settled = false;
        const cleanup = (): void => {
          window.clearTimeout(timeout);
          if (socket) {
            try {
              socket.close();
            } catch {
              // The socket may already be closed by the browser.
            }
          }
        };
        const finish = (messages: WireMessage[]): void => {
          if (settled) return;
          settled = true;
          cleanup();
          resolve(messages);
        };
        const fail = (error: unknown): void => {
          if (settled) return;
          settled = true;
          cleanup();
          reject(error);
        };
        const payloadKeyPaths = (value: object, prefix = ''): string[] => {
          return Object.entries(value).flatMap(([key, child]) => {
            const path = prefix ? `${prefix}.${key}` : key;
            return [
              key,
              ...(prefix ? [path] : []),
              ...(typeof child === 'object' && child !== null ? payloadKeyPaths(child, path) : []),
            ];
          });
        };

        const structural = (raw: unknown): WireMessage[] => {
          if (typeof raw !== 'object' || raw === null || !('type' in raw)) return [];
          const type = (raw as { type?: unknown }).type;
          if (typeof type !== 'string') return [];
          const data = (raw as { data?: unknown }).data;
          if (typeof data !== 'object' || data === null) return [{ type }];
          const value = data as Record<string, unknown>;
          if (type === 'presence_snapshot' && Array.isArray(value.users)) {
            const updates: WireMessage[] = [];
            for (const rawUser of value.users) {
              if (typeof rawUser !== 'object' || rawUser === null) continue;
              const user = rawUser as Record<string, unknown>;
              if (typeof user.user_id !== 'string') continue;
              const richPresence = user.rich_presence;
              if (typeof richPresence !== 'object' || richPresence === null) continue;
              for (const [category, rawActivity] of Object.entries(richPresence)) {
                if (typeof rawActivity !== 'object' || rawActivity === null) continue;
                const activity = rawActivity as Record<string, unknown>;
                const payload = activity.payload;
                updates.push({
                  type: 'rich_presence_update',
                  userId: user.user_id,
                  category,
                  ...(typeof activity.minimized === 'boolean'
                    ? { minimized: activity.minimized }
                    : {}),
                  ...(typeof payload === 'object' && payload !== null
                    ? { payloadKeys: payloadKeyPaths(payload) }
                    : {}),
                });
              }
            }
            return updates;
          }
          const payload = value.payload;
          return [
            {
              type,
              ...(typeof value.user_id === 'string' ? { userId: value.user_id } : {}),
              ...(typeof value.category === 'string' ? { category: value.category } : {}),
              ...(typeof value.minimized === 'boolean' ? { minimized: value.minimized } : {}),
              ...(typeof payload === 'object' && payload !== null
                ? { payloadKeys: payloadKeyPaths(payload) }
                : {}),
            },
          ];
        };
        const start = async (): Promise<void> => {
          const ticketUrl = new URL('/api/v1/auth/ws-ticket', wsEndpoint);
          ticketUrl.protocol = ticketUrl.protocol === 'wss:' ? 'https:' : 'http:';
          const ticketResponse = await fetch(ticketUrl, {
            method: 'POST',
            headers: { Authorization: `Bearer ${accessToken}` },
          });
          if (!ticketResponse.ok) throw new Error('fresh snapshot ticket request failed');
          const ticketBody = (await ticketResponse.json()) as {
            ticket?: unknown;
          };
          if (typeof ticketBody.ticket !== 'string')
            throw new Error('fresh snapshot ticket response invalid');
          if (settled) return;
          const wsUrl = new URL(wsEndpoint);
          wsUrl.searchParams.set('ticket', ticketBody.ticket);
          socket = new WebSocket(wsUrl);
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
              const incoming = structural(JSON.parse(event.data));
              messages.push(...incoming);
              if (incoming.some((message) => message.type === 'connection_ready')) finish(messages);
            } catch {
              // Ignore non-JSON frames; application frames are JSON.
            }
          });
          socket.addEventListener('error', () =>
            fail(new Error('fresh snapshot WebSocket failed'))
          );
        };

        const messages: WireMessage[] = [];
        const timeout = window.setTimeout(
          () => fail(new Error('fresh snapshot did not reach connection_ready')),
          20_000
        );
        void start().catch(fail);
      }),
    { accessToken, wsEndpoint: E2E_WS_URL }
  );
}

function isolatedRedis(): Redis {
  const value = process.env.REDIS_URL;
  if (!value) throw new Error('rich-presence E2E requires an isolated authenticated REDIS_URL');
  const url = new URL(value);
  if (url.protocol !== 'redis:' || !['127.0.0.1', 'localhost'].includes(url.hostname)) {
    throw new Error('rich-presence E2E requires a loopback Redis URL');
  }
  if (!url.password || Number(url.pathname.slice(1)) <= 0) {
    throw new Error('rich-presence E2E requires a non-default authenticated Redis database');
  }
  return new Redis(value, {
    connectTimeout: 2_000,
    commandTimeout: 1_000,
    maxRetriesPerRequest: 1,
  });
}

test.use({ trace: 'off' });

test.describe('Rich Presence cross-stack privacy acceptance', () => {
  test.beforeEach(async () => {
    const redis = isolatedRedis();
    try {
      await redis.ping();
    } finally {
      redis.disconnect();
    }
  });

  test('fresh settings show the documented default audience and disabled external category', async ({
    browser,
  }) => {
    const { context, page, session } = await newParticipant(browser);
    try {
      const settings = await apiJson<{
        master_enabled: boolean;
        server_voice_tier: number;
        private_call_tier: number;
        custom_text_tier: number;
      }>(page, 'GET', '/api/v1/users/me/presence-settings', session.accessToken, undefined, 200);
      expect(settings.master_enabled).toBe(true);
      expect(settings.server_voice_tier).toBe(1);
      expect(settings.private_call_tier).toBe(0);
      expect(settings.custom_text_tier).toBe(0);
      await openSettings(page);
      const serverVoice = page.locator('.presence-activity-card-serverVoice');
      const privateCall = page.locator('.presence-activity-card-privateCall');
      await expect(
        serverVoice.getByRole('button', {
          name: 'Friends in server',
          exact: true,
        })
      ).toHaveAttribute('aria-pressed', 'true');
      await expect(privateCall.getByRole('button', { name: 'Off', exact: true })).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(page.getByRole('switch', { name: 'Share Rich Presence' })).toBeChecked();
      await expect(
        page.getByRole('group', { name: 'Custom status visibility' }).getByRole('button', {
          name: 'Off',
          exact: true,
        })
      ).toHaveAttribute('aria-pressed', 'true');
      for (const category of ['Games', 'Music', 'Streaming']) {
        await expect(page.getByText(category, { exact: true })).toHaveCount(0);
      }
    } finally {
      await context.close();
    }
  });

  test('server voice enforces audience, minimization, live tier changes, TTL, and leave clearing', async ({
    browser,
  }) => {
    test.slow();
    let sender!: Awaited<ReturnType<typeof newParticipant>>;
    let allowed!: Awaited<ReturnType<typeof newParticipant>>;
    let crossServer!: Awaited<ReturnType<typeof newParticipant>>;
    let denied!: Awaited<ReturnType<typeof newParticipant>>;
    let serverOnly!: Awaited<ReturnType<typeof newParticipant>>;
    const contexts: Array<Awaited<ReturnType<typeof newParticipant>>['context']> = [];
    try {
      sender = await newParticipant(browser);
      contexts.push(sender.context);
      allowed = await newParticipant(browser);
      contexts.push(allowed.context);
      crossServer = await newParticipant(browser);
      contexts.push(crossServer.context);
      denied = await newParticipant(browser);
      contexts.push(denied.context);
      serverOnly = await newParticipant(browser);
      contexts.push(serverOnly.context);
      const voiceServer = `Presence Voice ${Date.now()}`;
      const voiceChannel = `voice-${Date.now()}`;
      const voiceServerId = await createServerWithId(sender.page, voiceServer);
      const channelId = await createVoiceChannel(sender.page, voiceChannel);
      const voiceInvite = await createUnlimitedInvite(
        sender.page,
        sender.session.accessToken,
        voiceServerId
      );
      await Promise.all([
        joinWithInvite(allowed.page, voiceInvite, voiceServer),
        joinWithInvite(denied.page, voiceInvite, voiceServer),
        joinWithInvite(serverOnly.page, voiceInvite, voiceServer),
      ]);

      const crossServerName = `Presence Cross ${Date.now()}`;
      const crossServerId = await createServerWithId(sender.page, crossServerName);
      const crossInvite = await createUnlimitedInvite(
        sender.page,
        sender.session.accessToken,
        crossServerId
      );
      await joinWithInvite(crossServer.page, crossInvite, crossServerName);
      await makeFriends(sender.page, sender.session, allowed.session);
      await makeFriends(sender.page, sender.session, crossServer.session);
      await apiJson<Record<string, unknown>>(
        sender.page,
        'PUT',
        `/api/v1/channels/${channelId}/overrides`,
        sender.session.accessToken,
        {
          target_type: 'user',
          target_id: denied.session.userId,
          allow: 0,
          deny: VIEW_VOICE_CHANNELS,
        },
        200
      );

      const appSockets = [sender, allowed, crossServer, denied, serverOnly].map((participant) => ({
        participant,
        mark: participant.wire.mark(),
        socketCount: participant.wire.socketCount,
      }));
      sender.session = await loginWithSession(sender.page, sender.session);
      allowed.session = await loginWithSession(allowed.page, allowed.session);
      crossServer.session = await loginWithSession(crossServer.page, crossServer.session);
      denied.session = await loginWithSession(denied.page, denied.session);
      serverOnly.session = await loginWithSession(serverOnly.page, serverOnly.session);
      await Promise.all(
        appSockets.map(({ participant, mark, socketCount }) =>
          participant.wire.waitForFrame(
            mark,
            (frame) => frame.socket > socketCount && frame.message.type === 'connection_ready',
            `${participant.session.username} did not reach its new app-socket readiness barrier`
          )
        )
      );
      await sender.page
        .locator(`button.server-bar-icon[aria-label="${voiceServer} server"]`)
        .click();
      await Promise.all([
        allowed.page.getByLabel(`${voiceServer} server`).click(),
        denied.page.getByLabel(`${voiceServer} server`).click(),
        crossServer.page.getByLabel(`${crossServerName} server`).click(),
      ]);

      const defaultFriendsMark = allowed.wire.mark();
      const deniedLiveMark = denied.wire.mark();
      const crossLiveMark = crossServer.wire.mark();
      await sender.page.getByRole('button', { name: voiceChannel, exact: true }).click();
      await sender.page.getByRole('button', { name: 'Join Voice', exact: true }).click();
      await expect(sender.page.getByText('In voice', { exact: true })).toBeVisible({
        timeout: 20_000,
      });
      const initialAllowedUpdate = await allowed.wire.waitForFrame(
        defaultFriendsMark,
        (frame) =>
          hasActivity(frame, 'rich_presence_update', sender.session.userId, 'server_voice'),
        'same-server friend did not receive default Server Voice'
      );
      expect(initialAllowedUpdate.message.minimized).toBe(false);
      expect(initialAllowedUpdate.message.payloadKeys).toContain('channel_id');
      expect(initialAllowedUpdate.message.payloadKeys).toContain('server_id');
      expect(initialAllowedUpdate.message.payloadKeys).toContain('channel_name');
      expect(initialAllowedUpdate.message.payloadKeys).toContain('server_name');
      const forbiddenServerVoice = (frame: CapturedFrame): boolean =>
        isForbiddenServerVoice(frame.message, sender.session.userId);
      const senderMenu = sender.page.getByRole('button', { name: /^User menu for / });
      const selfActivity = senderMenu.getByText('In voice', { exact: true });
      await expect(selfActivity).toBeVisible();

      const allowedReconnect = allowed.wire.mark();
      const serverOnlyReconnect = serverOnly.wire.mark();
      await apiJson<Record<string, unknown>>(
        sender.page,
        'PATCH',
        '/api/v1/users/me/presence-settings',
        sender.session.accessToken,
        { server_voice_tier: 2, server_voice_show_details: false },
        200
      );
      await Promise.all([
        allowed.wire.waitForFrame(
          allowedReconnect,
          (frame) => frame.message.type === 'connection_ready',
          'same-server friend did not finish reconnecting after a live policy change'
        ),
        serverOnly.wire.waitForFrame(
          serverOnlyReconnect,
          (frame) => frame.message.type === 'connection_ready',
          'same-server viewer did not finish reconnecting after a live policy change'
        ),
      ]);
      await openSettings(sender.page);
      const serverVoiceSettings = sender.page.locator('.presence-activity-card-serverVoice');
      await expect(
        serverVoiceSettings.getByRole('switch', { name: 'Server Voice show details' })
      ).not.toBeChecked();
      await expect(serverVoiceSettings.getByText('In voice', { exact: true })).toBeVisible();
      await sender.page.getByRole('button', { name: 'Back to app' }).click();
      await senderMenu.hover();
      await expect(senderMenu.locator('.user-panel-activity-policy')).toHaveCSS('opacity', '1');
      await expect(senderMenu).toContainText(
        'Eligible audience: People in this server who can view this voice channel.'
      );
      await expect(selfActivity).toBeVisible();

      let allowedUpdate: WireMessage | undefined;
      let serverOnlyUpdate: WireMessage | undefined;
      await expect(async () => {
        const [allowedSnapshot, serverOnlySnapshot] = await Promise.all([
          freshSnapshot(allowed.page, allowed.session.accessToken),
          freshSnapshot(serverOnly.page, serverOnly.session.accessToken),
        ]);
        allowedUpdate = allowedSnapshot.find(
          (message) =>
            message.type === 'rich_presence_update' &&
            message.userId === sender.session.userId &&
            message.category === 'server_voice'
        );
        serverOnlyUpdate = serverOnlySnapshot.find(
          (message) =>
            message.type === 'rich_presence_update' &&
            message.userId === sender.session.userId &&
            message.category === 'server_voice'
        );
        expect(allowedUpdate, 'same-server friend snapshot omitted Server Voice').toBeDefined();
        expect(
          serverOnlyUpdate,
          'same-server viewer snapshot omitted Servers-tier Server Voice'
        ).toBeDefined();
      }).toPass({ timeout: 20_000 });
      if (!allowedUpdate || !serverOnlyUpdate)
        throw new Error('authorized Server Voice snapshot missing');
      expect(allowedUpdate.minimized).toBe(true);
      expect(allowedUpdate.payloadKeys).toContain('channel_id');
      expect(allowedUpdate.payloadKeys).toContain('server_id');
      expect(allowedUpdate.payloadKeys).not.toContain('channel_name');
      expect(allowedUpdate.payloadKeys).not.toContain('server_name');
      expect(serverOnlyUpdate.minimized).toBe(true);
      expect(serverOnlyUpdate.payloadKeys).toContain('channel_id');
      expect(serverOnlyUpdate.payloadKeys).toContain('server_id');
      expect(serverOnlyUpdate.payloadKeys).not.toContain('channel_name');
      expect(serverOnlyUpdate.payloadKeys).not.toContain('server_name');
      await expect(allowed.page.getByText('In voice', { exact: true })).toBeVisible({
        timeout: 20_000,
      });

      const deniedSnapshot = await freshSnapshot(denied.page, denied.session.accessToken);
      const crossSnapshot = await freshSnapshot(crossServer.page, crossServer.session.accessToken);
      for (const snapshot of [deniedSnapshot, crossSnapshot]) {
        const ready = snapshot.findIndex((message) => message.type === 'connection_ready');
        expect(ready, 'fresh snapshot did not reach connection_ready').toBeGreaterThanOrEqual(0);
        expect(
          snapshot
            .slice(0, ready + 1)
            .some((message) => isForbiddenServerVoice(message, sender.session.userId))
        ).toBe(false);
      }
      const deniedBarrier = denied.wire.mark();
      const crossBarrier = crossServer.wire.mark();
      [denied.session, crossServer.session] = await Promise.all([
        loginWithSession(denied.page, denied.session),
        loginWithSession(crossServer.page, crossServer.session),
      ]);
      await Promise.all([
        denied.wire.waitForFrame(
          deniedBarrier,
          (frame) => frame.message.type === 'connection_ready',
          'denied viewer did not reach its live-negative snapshot barrier'
        ),
        crossServer.wire.waitForFrame(
          crossBarrier,
          (frame) => frame.message.type === 'connection_ready',
          'cross-server viewer did not reach its live-negative snapshot barrier'
        ),
      ]);
      expect(
        denied.wire.hasFrame(deniedLiveMark, forbiddenServerVoice),
        'denied viewer received live Server Voice before its app-socket snapshot barrier'
      ).toBe(false);
      expect(
        crossServer.wire.hasFrame(crossLiveMark, forbiddenServerVoice),
        'cross-server viewer received live Server Voice before its app-socket snapshot barrier'
      ).toBe(false);
      for (const page of [denied.page, crossServer.page]) {
        await expect(page.getByText('In voice', { exact: true })).toHaveCount(0);
      }
      const downgradeClear = serverOnly.wire.mark();
      const retainedReconnect = allowed.wire.mark();
      await apiJson<Record<string, unknown>>(
        sender.page,
        'PATCH',
        '/api/v1/users/me/presence-settings',
        sender.session.accessToken,
        { server_voice_tier: 1 },
        200
      );
      const tierDownClear = await serverOnly.wire.waitForFrame(
        downgradeClear,
        (frame) => hasActivity(frame, 'rich_presence_clear', sender.session.userId, 'server_voice'),
        'Servers-to-Friends tier-down did not deliver a rich_presence_clear'
      );
      await serverOnly.wire.waitForFrame(
        tierDownClear.sequence + 1,
        (frame) => frame.message.type === 'connection_ready',
        'Servers-to-Friends tier-down did not finish the privacy reconnect'
      );
      await allowed.wire.waitForFrame(
        retainedReconnect,
        (frame) => frame.message.type === 'connection_ready',
        'retained friend did not finish the Servers-to-Friends privacy reconnect'
      );
      const retainedSnapshot = await freshSnapshot(allowed.page, allowed.session.accessToken);
      expect(
        retainedSnapshot.some(
          (message) =>
            message.type === 'rich_presence_update' &&
            message.userId === sender.session.userId &&
            message.category === 'server_voice'
        ),
        'retained friend snapshot omitted Server Voice after tier-down'
      ).toBe(true);
      await expect(serverOnly.page.getByText('In voice', { exact: true })).toHaveCount(0);
      expect(
        serverOnly.wire.hasFrame(tierDownClear.sequence + 1, (frame) =>
          hasActivity(frame, 'rich_presence_update', sender.session.userId, 'server_voice')
        ),
        'Servers-to-Friends reconnect snapshot retained Server Voice for a non-friend'
      ).toBe(false);

      const redis = isolatedRedis();
      try {
        const key = `presence:rich:${sender.session.userId}:server_voice`;
        await expect.poll(() => redis.ttl(key), { timeout: 20_000 }).toBeGreaterThan(0);
        expect(await redis.ttl(key)).toBeLessThanOrEqual(90);

        const leaveMark = allowed.wire.mark();
        await sender.page.getByRole('button', { name: 'Leave', exact: true }).click();
        const leaveClear = await allowed.wire.waitForFrame(
          leaveMark,
          (frame) =>
            hasActivity(frame, 'rich_presence_clear', sender.session.userId, 'server_voice'),
          'leaving voice did not deliver the viewer rich_presence_clear'
        );
        await allowed.wire.waitForFrame(
          leaveClear.sequence + 1,
          (frame) => frame.message.type === 'connection_ready',
          'leaving voice did not finish the viewer privacy reconnect'
        );
        await expect(selfActivity).toHaveCount(0);
        await expect(allowed.page.getByText('In voice', { exact: true })).toHaveCount(0);
        await expect.poll(() => redis.exists(key), { timeout: 20_000 }).toBe(0);

        const rejoinMark = allowed.wire.mark();
        await sender.page.getByRole('button', { name: voiceChannel, exact: true }).click();
        await sender.page.getByRole('button', { name: 'Join Voice', exact: true }).click();
        await expect(sender.page.getByText('In voice', { exact: true })).toBeVisible({
          timeout: 20_000,
        });
        await expect.poll(() => redis.exists(key), { timeout: 20_000 }).toBe(1);
        const minimizedRejoin = await allowed.wire.waitForFrame(
          rejoinMark,
          (frame) =>
            hasActivity(frame, 'rich_presence_update', sender.session.userId, 'server_voice'),
          'viewer did not receive Server Voice after the sender rejoined'
        );
        expect(minimizedRejoin.message.minimized).toBe(true);
        expect(minimizedRejoin.message.payloadKeys).toContain('channel_id');
        expect(minimizedRejoin.message.payloadKeys).toContain('server_id');
        expect(minimizedRejoin.message.payloadKeys).not.toContain('channel_name');
        expect(minimizedRejoin.message.payloadKeys).not.toContain('server_name');
        const allowedSenderRow = allowed.page
          .locator('button.member-item')
          .filter({ hasText: sender.session.username });
        await expect(allowedSenderRow.locator('.member-rich-presence-headline')).toHaveText(
          'In voice'
        );
        await expect(allowedSenderRow.locator('.member-rich-presence-detail')).toHaveCount(0);
        const contextCloseMark = allowed.wire.mark();
        await sender.context.close();
        const contextCloseClear = await allowed.wire.waitForFrame(
          contextCloseMark,
          (frame) =>
            hasActivity(frame, 'rich_presence_clear', sender.session.userId, 'server_voice'),
          'closing the sender context did not deliver the viewer rich_presence_clear'
        );
        await allowed.wire.waitForFrame(
          contextCloseClear.sequence + 1,
          (frame) => frame.message.type === 'connection_ready',
          'closing the sender context did not finish the viewer privacy reconnect'
        );
        await expect(allowed.page.getByText('In voice', { exact: true })).toHaveCount(0);
        await expect.poll(() => redis.exists(key), { timeout: 20_000 }).toBe(0);
        const postDisconnect = await freshSnapshot(allowed.page, allowed.session.accessToken);
        expect(
          postDisconnect.some(
            (message) =>
              message.type === 'rich_presence_update' &&
              message.userId === sender.session.userId &&
              message.category === 'server_voice'
          )
        ).toBe(false);
      } finally {
        try {
          await redis.quit();
        } catch {
          redis.disconnect();
        }
      }
    } finally {
      await Promise.all(contexts.map((context) => context.close()));
    }
  });

  test('private-call Tier Off reaches participants but never a friend/server peer', async ({
    browser,
  }) => {
    test.slow();
    let caller!: Awaited<ReturnType<typeof newParticipant>>;
    let participant!: Awaited<ReturnType<typeof newParticipant>>;
    let observer!: Awaited<ReturnType<typeof newParticipant>>;
    const contexts: Array<Awaited<ReturnType<typeof newParticipant>>['context']> = [];
    try {
      caller = await newParticipant(browser, true);
      contexts.push(caller.context);
      participant = await newParticipant(browser, true);
      contexts.push(participant.context);
      observer = await newParticipant(browser);
      contexts.push(observer.context);
      const serverName = `Private Presence ${Date.now()}`;
      const serverId = await createServerWithId(caller.page, serverName);
      const invite = await createUnlimitedInvite(caller.page, caller.session.accessToken, serverId);
      await joinWithInvite(observer.page, invite, serverName);
      await makeFriends(caller.page, caller.session, participant.session);
      await makeFriends(caller.page, caller.session, observer.session);
      await apiJson<{ conversation?: { id?: string } }>(
        caller.page,
        'POST',
        '/api/v1/dm/conversations',
        caller.session.accessToken,
        { user_id: participant.session.userId },
        201
      );
      const callerReady = caller.wire.mark();
      const participantReady = participant.wire.mark();
      const observerReady = observer.wire.mark();
      caller.session = await loginWithSession(caller.page, caller.session);
      participant.session = await loginWithSession(participant.page, participant.session);
      observer.session = await loginWithSession(observer.page, observer.session);
      await Promise.all([
        caller.wire.waitForFrame(
          callerReady,
          (frame) => frame.message.type === 'connection_ready',
          'caller Rich Presence connection did not become ready'
        ),
        participant.wire.waitForFrame(
          participantReady,
          (frame) => frame.message.type === 'connection_ready',
          'participant Rich Presence connection did not become ready'
        ),
        observer.wire.waitForFrame(
          observerReady,
          (frame) => frame.message.type === 'connection_ready',
          'observer Rich Presence connection did not become ready'
        ),
      ]);
      await caller.page
        .locator('button.conversation-item')
        .filter({ hasText: participant.session.username })
        .click();
      await participant.page
        .locator('button.conversation-item')
        .filter({ hasText: caller.session.username })
        .click();

      const callerLiveMark = caller.wire.mark();
      const participantLiveMark = participant.wire.mark();
      const observerWireStart = observer.wire.mark();
      await caller.page
        .locator('button.conversation-item')
        .filter({ hasText: participant.session.username })
        .click({ button: 'right' });
      await caller.page.getByText('Voice Call', { exact: true }).click();
      await participant.page.getByRole('button', { name: 'Accept call' }).click();
      await expect(participant.page.locator('.voice-view')).toBeVisible({ timeout: 20_000 });
      await Promise.all([
        caller.wire.waitForFrame(
          callerLiveMark,
          (frame) =>
            hasActivity(frame, 'rich_presence_update', participant.session.userId, 'private_call'),
          'caller did not receive the participant Private Call activity'
        ),
        participant.wire.waitForFrame(
          participantLiveMark,
          (frame) =>
            hasActivity(frame, 'rich_presence_update', caller.session.userId, 'private_call'),
          'participant did not receive the caller Private Call activity'
        ),
      ]);
      const redis = isolatedRedis();
      try {
        await expect
          .poll(
            async () =>
              redis.exists(
                `presence:rich:${caller.session.userId}:private_call`,
                `presence:rich:${participant.session.userId}:private_call`
              ),
            { timeout: 65_000 }
          )
          .toBe(2);
      } finally {
        try {
          await redis.quit();
        } catch {
          redis.disconnect();
        }
      }
      const [callerSnapshot, participantSnapshot] = await Promise.all([
        freshSnapshot(caller.page, caller.session.accessToken),
        freshSnapshot(participant.page, participant.session.accessToken),
      ]);
      const callerUpdate = callerSnapshot.find(
        (message) =>
          message.type === 'rich_presence_update' &&
          message.userId === participant.session.userId &&
          message.category === 'private_call'
      );
      const participantUpdate = participantSnapshot.find(
        (message) =>
          message.type === 'rich_presence_update' &&
          message.userId === caller.session.userId &&
          message.category === 'private_call'
      );
      for (const update of [callerUpdate, participantUpdate]) {
        expect(update?.minimized).toBe(true);
        expect(update?.payloadKeys).toEqual(['call_type']);
      }
      const observerSnapshot = await freshSnapshot(observer.page, observer.session.accessToken);
      const observerBarrier = observer.wire.mark();
      observer.session = await loginWithSession(observer.page, observer.session);
      await observer.wire.waitForFrame(
        observerBarrier,
        (frame) => frame.message.type === 'connection_ready',
        'observer did not reach its live-negative snapshot barrier'
      );
      expect(
        observer.wire.hasFrame(
          observerWireStart,
          (frame) =>
            frame.message.category === 'private_call' &&
            ['rich_presence_update', 'rich_presence_clear'].includes(frame.message.type)
        ),
        'private-call activity leaked to the non-participant observer before its app-socket snapshot barrier'
      ).toBe(false);
      expect(
        observerSnapshot.some(
          (message) =>
            message.type === 'rich_presence_update' && message.category === 'private_call'
        )
      ).toBe(false);
      await expect(observer.page.getByText(/In a (private|group) call/)).toHaveCount(0);
    } finally {
      await Promise.all(contexts.map((context) => context.close()));
    }
  });
});
