import { ApiError, ConcordApi, checkBaseUrl } from '../core/ConcordApi';
import { DmSocket, type SocketLike } from '../core/DmSocket';

const ME = 'user-mobile';
const PEER = 'user-peer';
const CONV = 'conv-1';
const MACHINE = '6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b';

type Route = (body: Record<string, unknown> | undefined) => [number, unknown];

function fakeFetch(routes: Record<string, Route>) {
  const calls: Array<{
    key: string;
    headers: Record<string, string>;
    body?: Record<string, unknown>;
  }> = [];
  const impl = jest.fn(
    async (
      url: string,
      init: { method: string; headers: Record<string, string>; body?: string },
    ) => {
      const path = url.replace(/^https?:\/\/[^/]+\/api\/v1/, '');
      const key = `${init.method} ${path}`;
      const body = init.body
        ? (JSON.parse(init.body) as Record<string, unknown>)
        : undefined;
      calls.push({ key, headers: init.headers, body });
      const route = routes[key];
      const [status, json] = route ? route(body) : [404, { code: 'not_found' }];
      return {
        ok: status >= 200 && status < 300,
        status,
        json: async () => json,
      } as Response;
    },
  );
  return { impl: impl as unknown as typeof fetch, calls };
}

class FakeSocket implements SocketLike {
  sent: Array<{ type: string; data: Record<string, unknown> }> = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  closed = false;
  constructor(
    private readonly respond: (
      frame: { type: string; data: Record<string, unknown> },
      socket: FakeSocket,
    ) => void,
    autoOpen = true,
  ) {
    if (autoOpen) setTimeout(() => this.onopen?.(), 0);
  }
  send(raw: string) {
    // Like a real WebSocket, sending on a closed socket throws.
    if (this.closed) throw new Error('Socket is not open');
    const frame = JSON.parse(raw);
    this.sent.push(frame);
    setTimeout(() => this.respond(frame, this), 0);
  }
  emit(type: string, data: Record<string, unknown>) {
    this.onmessage?.({ data: JSON.stringify({ type, data }) });
  }
  close() {
    this.closed = true;
  }
}

describe('ConcordApi', () => {
  it('signs up, logs in and distributes a DM key with recipient versions', async () => {
    const { impl, calls } = fakeFetch({
      'POST /auth/register': () => [201, { pending_id: 'pending-1' }],
      'POST /auth/register/confirm': () => [
        200,
        { access_token: 'a1', user: { id: ME } },
      ],
      'POST /auth/login': () => [
        200,
        {
          access_token: 'a2',
          user: { id: ME },
          e2ee_keys: {
            wrapped_private_key: 'V1JBUA==', // pragma: allowlist secret
            key_derivation_salt: 'U0FMVA==',
            key_derivation_alg: 'argon2id',
          },
        },
      ],
      'GET /users/search?q=peer': () => [
        200,
        { users: [{ id: PEER, username: 'peer' }] },
      ],
      'POST /dm/conversations': () => [201, { conversation: { id: CONV } }],
      'GET /users/user-peer/public-key': () => [
        200,
        { public_key: 'UEs=', key_version: 3 },
      ],
      'POST /e2ee/keys/conv-1': () => [200, {}],
      'GET /dm/conversations/conv-1/messages': () => [
        200,
        { messages: [{ id: 'm1' }] },
      ],
      'POST /auth/ws-ticket': () => [200, { ticket: 'tkt' }],
    });
    const api = new ConcordApi('https://api.example.test', MACHINE, impl);
    const pending = await api.register({
      email: 'm@example.test',
      username: 'mobile',
      password: 'synthetic-pass', // pragma: allowlist secret
      publicKey: 'UEs=',
      wrappedPrivateKey: 'V1JBUA==', // pragma: allowlist secret
      keyDerivationSalt: 'U0FMVA==',
    });
    await api.confirm(pending, '123456');
    expect(api.userId).toBe(ME);
    expect(await api.login('m@example.test', 'synthetic-pass')).toEqual({
      wrappedPrivateKey: 'V1JBUA==', // pragma: allowlist secret
      keyDerivationSalt: 'U0FMVA==',
      keyDerivationAlg: 'argon2id',
    });
    const peer = await api.findUserId('peer');
    const conv = await api.openDm(peer);
    expect(await api.publicKey(peer)).toEqual({
      publicKey: 'UEs=',
      keyVersion: 3,
    });
    await api.distributeDmKey(conv, { [peer]: 'd3JhcA==' }, { [peer]: 3 });
    expect(await api.dmMessages(conv)).toEqual([{ id: 'm1' }]);
    expect(await api.wsTicket()).toBe('tkt');

    expect(calls.find(c => c.key === 'POST /e2ee/keys/conv-1')?.body).toEqual({
      wrapped_keys: { [PEER]: 'd3JhcA==' },
      wrapped_key_versions: { [PEER]: 3 },
    });
    expect(calls.every(c => c.headers['X-Machine-Id'] === MACHINE)).toBe(true);
    expect(
      calls.find(c => c.key === 'POST /auth/ws-ticket')?.headers.Authorization,
    ).toBe('Bearer a2');
  });

  it('refuses MFA logins and an invalid key version', async () => {
    const { impl } = fakeFetch({
      'POST /auth/login': () => [200, { mfa_required: true }],
      'GET /users/u/public-key': () => [
        200,
        { public_key: 'UEs=', key_version: 0 },
      ],
    });
    const api = new ConcordApi('https://api.example.test', MACHINE, impl);
    await expect(api.login('e', 'p')).rejects.toThrow('MFA');
    await expect(api.publicKey('u')).rejects.toThrow('Invalid API response');
  });

  it('allows plain http only for localhost', () => {
    expect(checkBaseUrl('http://localhost:8080')).toBe('http://localhost:8080');
    expect(checkBaseUrl('https://api.example.test')).toBe(
      'https://api.example.test',
    );
    expect(() => checkBaseUrl('http://10.0.0.5:8080')).toThrow(
      'local dev stack',
    );
    expect(() => checkBaseUrl('ftp://x')).toThrow('Invalid');
    expect(() => checkBaseUrl('https://a@b')).toThrow('Invalid');
    expect(() => checkBaseUrl('https://evil.test?x')).toThrow('Invalid');
    expect(() => checkBaseUrl('http://localhost@evil.test')).toThrow('Invalid');
  });

  it('aborts a request that stalls past its deadline', async () => {
    const stalled = jest.fn(
      (_url: string, init: { signal: AbortSignal }) =>
        new Promise((_resolve, reject) =>
          init.signal.addEventListener('abort', () =>
            reject(new Error('aborted')),
          ),
        ),
    );
    const api = new ConcordApi(
      'http://localhost:8080',
      MACHINE,
      stalled as unknown as typeof fetch,
      10,
    );
    await expect(api.findUserId('anyone')).rejects.toThrow('timed out');
  });

  it('times out when headers arrive but the body stalls', async () => {
    const stalledBody = jest.fn(
      async (_url: string, init: { signal: AbortSignal }) =>
        ({
          ok: true,
          status: 200,
          json: () =>
            new Promise((_resolve, reject) =>
              init.signal.addEventListener('abort', () =>
                reject(new Error('aborted')),
              ),
            ),
        } as unknown as Response),
    );
    const api = new ConcordApi(
      'http://localhost:8080',
      MACHINE,
      stalledBody as unknown as typeof fetch,
      10,
    );
    await expect(api.distributeDmKey('c', {}, {})).rejects.toThrow('timed out');
  });

  it('surfaces a failed body read instead of treating it as empty', async () => {
    const broken = jest.fn(
      async () =>
        ({
          ok: true,
          status: 200,
          json: () => Promise.reject(new TypeError('network lost')),
        } as unknown as Response),
    );
    const api = new ConcordApi(
      'http://localhost:8080',
      MACHINE,
      broken as unknown as typeof fetch,
    );
    await expect(api.distributeDmKey('c', {}, {})).rejects.toThrow(
      'network lost',
    );
  });

  it('builds the WS URL, reports API errors by status and refuses unknown peers', async () => {
    const { impl } = fakeFetch({
      'GET /users/search?q=nobody': () => [200, { users: [] }],
      'POST /auth/ws-ticket': () => [401, { code: 'unauthorized' }],
    });
    const api = new ConcordApi('https://api.example.test', MACHINE, impl);
    expect(api.wsUrl('a b')).toBe(
      'wss://api.example.test/api/v1/ws?ticket=a%20b',
    );
    expect(() => api.userId).toThrow('Not signed in');
    await expect(api.findUserId('nobody')).rejects.toThrow('User not found');
    const error = await api.wsTicket().catch(e => e);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 401, code: 'unauthorized' });
    expect(error.message).toBe('Concord API 401 unauthorized');
  });

  it('treats a malformed body as empty and rejects invalid shapes', async () => {
    const malformed = (status: number) =>
      jest.fn(
        async () =>
          ({
            ok: status < 300,
            status,
            json: () => Promise.reject(new SyntaxError('Unexpected token')),
          } as unknown as Response),
      ) as unknown as typeof fetch;
    const failing = new ConcordApi(
      'http://localhost:8080',
      MACHINE,
      malformed(500),
    );
    const error = await failing.wsTicket().catch(e => e);
    expect(error).toMatchObject({ status: 500, code: undefined });
    const empty = new ConcordApi(
      'http://localhost:8080',
      MACHINE,
      malformed(200),
    );
    await expect(empty.distributeDmKey('c', {}, {})).resolves.toBeUndefined();

    const { impl, calls } = fakeFetch({
      'POST /auth/login': () => [200, { access_token: '', user: { id: ME } }],
      'GET /users/search?q=x': () => [200, { users: ['not-an-object'] }],
    });
    const api = new ConcordApi('https://api.example.test', MACHINE, impl);
    await expect(api.login('e', 'p')).rejects.toThrow('Invalid API response');
    await expect(api.findUserId('x')).rejects.toThrow('Invalid API response');
    expect(calls.every(c => c.headers.Authorization === undefined)).toBe(true);
  });
});

describe('DmSocket', () => {
  it('subscribes, sends with an ack and receives the peer message', async () => {
    let fake!: FakeSocket;
    const dm = await DmSocket.open(
      'ws://x',
      () =>
        (fake = new FakeSocket((frame, socket) => {
          if (frame.type === 'subscribe_dm') {
            // A reply for another conversation must not settle this subscribe.
            socket.emit('dm_subscribed', { conversation_id: 'other' });
            socket.emit('dm_subscribed', frame.data);
          }
          if (frame.type === 'dm_message') {
            socket.emit('dm_message_ack', { nonce: 'n0', id: 'wrong' });
            socket.emit('dm_message_ack', {
              nonce: frame.data.nonce,
              id: 'm1',
            });
            socket.emit('dm_message', {
              id: 'spoof',
              conversation_id: CONV,
              user_id: 'intruder',
              content: 'cipher',
              key_version: 1,
            });
            socket.emit('dm_message', {
              id: 'm2',
              conversation_id: CONV,
              user_id: PEER,
              content: 'cipher',
              key_version: 1,
            });
          }
        })),
    );
    await dm.subscribe(CONV);
    const reply = dm.nextMessage(CONV, PEER);
    expect(await dm.send(CONV, 'c', 1, 'n1')).toBe('m1');
    expect(await reply).toEqual({
      id: 'm2',
      conversationId: CONV,
      userId: PEER,
      content: 'cipher',
      keyVersion: 1,
    });
    const pending = dm.nextMessage(CONV, PEER);
    dm.close();
    await expect(pending).rejects.toThrow('Socket closed');
    expect(fake.closed).toBe(true);
    // Sending after close rejects with the socket's error and leaves no waiter behind.
    await expect(dm.send(CONV, 'c', 1, 'n2')).rejects.toThrow('not open');
    await expect(dm.subscribe(CONV)).rejects.toThrow('not open');
  });

  it('closes and silences a socket that never opens', async () => {
    jest.useFakeTimers();
    try {
      const socket = new FakeSocket(() => undefined, false);
      const opening = DmSocket.open('ws://x', () => socket, 50);
      const lateOpen = socket.onopen;
      expect(lateOpen).not.toBeNull();
      const failure = opening.catch(e => e);
      jest.advanceTimersByTime(50);
      expect(String(await failure)).toContain('timed out');
      expect(socket.closed).toBe(true);
      expect(socket.onopen).toBeNull();
      expect(socket.onerror).toBeNull();
      // A late open from the old handler cannot turn the failure into a socket.
      lateOpen?.();
      expect(String(await opening.catch(e => e))).toContain('timed out');
    } finally {
      jest.useRealTimers();
    }
  });

  it('ignores malformed and unrelated frames and fails waiters on error frames', async () => {
    let socket!: FakeSocket;
    const dm = await DmSocket.open(
      'ws://x',
      () => (socket = new FakeSocket(() => undefined)),
    );
    const pending = dm.nextMessage(CONV, PEER);
    socket.onmessage?.({ data: 'not json' });
    socket.onmessage?.({ data: 42 });
    socket.emit('dm_message', {
      id: 'x',
      conversation_id: CONV,
      user_id: PEER,
      content: 'c',
      key_version: 0,
    });
    socket.emit('dm_message', {
      id: 'x',
      conversation_id: 'other',
      user_id: PEER,
      content: 'c',
      key_version: 1,
    });
    socket.emit('error', { message: 'nope' });
    await expect(pending).rejects.toThrow('Server error frame');
    const closed = dm.nextMessage(CONV, PEER);
    socket.onclose?.();
    await expect(closed).rejects.toThrow('Socket closed');
  });

  it('times out waits and failed opens', async () => {
    const dm = await DmSocket.open(
      'ws://x',
      () => new FakeSocket(() => undefined),
      10,
    );
    await expect(dm.nextMessage(CONV, PEER)).rejects.toThrow(
      'Timed out waiting for dm_message',
    );
    let failingClosed = false;
    const failing = {
      send() {},
      close() {
        failingClosed = true;
      },
      onopen: null,
      onmessage: null,
      onerror: null,
      onclose: null,
    } as SocketLike;
    const opening = DmSocket.open('ws://x', () => failing, 1000);
    failing.onerror?.();
    await expect(opening).rejects.toThrow('failed to open');
    expect(failingClosed).toBe(true);
    await expect(
      DmSocket.open('ws://x', () => ({ ...failing }), 5),
    ).rejects.toThrow('open timed out');
  });
});
