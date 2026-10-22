import { hostname, tmpdir } from 'node:os';
import { createServer } from 'node:net';
import { mkdtemp, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const providerMocks = vi.hoisted(() => ({
  open: vi.fn(),
}));

vi.mock('node:fs/promises', async (importOriginal) => {
  const actual = await importOriginal<Record<string, unknown>>();
  return { ...actual, open: providerMocks.open };
});
import {
  acknowledgeMediaCredential,
  readExactMediaCredential,
  type AppliedAck,
  type SecretScope,
} from '../src/lib/managedRedis.js';

const scope: SecretScope = {
  projectId: 'synthetic-project',
  environment: 'test',
  path: '/synthetic/redis',
  name: 'REDIS_MEDIA_PLANE_CREDENTIAL',
};
const syntheticLeaf = 'synthetic-client-secret-only';
const accessToken = 'synthetic-access-token-only';

function credentialValue(): string {
  return JSON.stringify({
    version: 1,
    scope,
    rotationId: 'rotation-test-12',
    service: 'media-plane',
    generation: 12,
    username: 'cv_mp_12',
    password: 'synthetic-redis-password-only', // pragma: allowlist secret -- test fixture
    validFrom: '2026-10-17T00:00:00Z',
    validUntil: '2026-10-18T00:00:00Z',
  });
}

function fileHandle(leaf = syntheticLeaf) {
  return {
    stat: vi.fn().mockResolvedValue({
      uid: process.geteuid?.() ?? 501,
      gid: process.getegid?.() ?? 20,
      mode: 0o100600,
      size: Buffer.byteLength(leaf),
      isFile: () => true,
    }),
    read: vi.fn(async (target: Buffer) => {
      const bytes = Buffer.from(leaf);
      bytes.copy(target);
      return { bytesRead: bytes.length };
    }),
    close: vi.fn().mockResolvedValue(undefined),
  };
}

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function oversizedResponse(onCancel: () => void, onPull: () => void): Response {
  let emitted = 0;
  return new Response(
    new ReadableStream<Uint8Array>({
      pull(controller) {
        if (emitted++ === 1024) {
          controller.close();
          return;
        }
        onPull();
        controller.enqueue(new Uint8Array(1024));
      },
      cancel: onCancel,
    })
  );
}

describe('readExactMediaCredential', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let cache: { token: string; renewAt: number; expiresAt: number };

  beforeEach(() => {
    cache = { token: '', renewAt: 0, expiresAt: 0 };
    providerMocks.open.mockResolvedValue(fileHandle());
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('reads the protected leaf and makes one Universal Auth login plus one exact named v4 GET', async () => {
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      );

    const result = await readExactMediaCredential(
      scope,
      'https://infisical.synthetic.test',
      'synthetic-client-id',
      new AbortController().signal,
      cache
    );

    expect(providerMocks.open).toHaveBeenCalledWith(
      '/run/secrets/redis-infisical-client-secret',
      expect.any(Number)
    );
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const [loginUrl, loginInit] = fetchMock.mock.calls[0] as [URL, RequestInit];
    expect(String(loginUrl)).toBe(
      'https://infisical.synthetic.test/api/v1/auth/universal-auth/login'
    );
    expect(loginInit).toEqual(
      expect.objectContaining({
        method: 'POST',
        redirect: 'error',
        body: JSON.stringify({ clientId: 'synthetic-client-id', clientSecret: syntheticLeaf }),
      })
    );
    const [secretUrl, secretInit] = fetchMock.mock.calls[1] as [URL, RequestInit];
    const parsed = new URL(String(secretUrl));
    expect(parsed.pathname).toBe('/api/v4/secrets/REDIS_MEDIA_PLANE_CREDENTIAL');
    expect([...parsed.searchParams.entries()].sort()).toEqual(
      [
        ['environment', 'test'],
        ['expandSecretReferences', 'false'],
        ['includeImports', 'false'],
        ['projectId', 'synthetic-project'],
        ['secretPath', '/synthetic/redis'],
        ['type', 'shared'],
      ].sort()
    );
    expect(secretInit).toEqual(
      expect.objectContaining({
        redirect: 'error',
        headers: { authorization: `Bearer ${accessToken}` },
      })
    );
    expect(result).toEqual({ value: credentialValue(), returnedName: scope.name });
  });

  it('starts token deadlines before a slow login response is read', async () => {
    let clock = 1_000;
    vi.spyOn(Date, 'now').mockImplementation(() => clock);
    fetchMock
      .mockImplementationOnce(async () => {
        clock += 4_000;
        return response({ accessToken, tokenType: 'Bearer', expiresIn: 10, accessTokenMaxTTL: 10 });
      })
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      );

    await readExactMediaCredential(
      scope,
      'https://infisical.synthetic.test',
      'synthetic-client-id',
      new AbortController().signal,
      cache
    );

    expect(cache.renewAt).toBe(6_000);
    expect(cache.expiresAt).toBe(11_000);
  });

  it('does not use a token that expired during login response delivery', async () => {
    let clock = 1_000;
    vi.spyOn(Date, 'now').mockImplementation(() => clock);
    fetchMock.mockImplementationOnce(async () => {
      clock += 11_000;
      return response({ accessToken, tokenType: 'Bearer', expiresIn: 10, accessTokenMaxTTL: 10 });
    });

    await expect(
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'synthetic-client-id',
        new AbortController().signal,
        cache
      )
    ).rejects.toThrow();
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('bounds a cold login to 10 seconds and the named read to a separate 5 seconds', async () => {
    const timeoutControllers: AbortController[] = [];
    const timeout = vi.spyOn(AbortSignal, 'timeout').mockImplementation((_ms) => {
      const controller = new AbortController();
      timeoutControllers.push(controller);
      return controller.signal;
    });
    const waitForAbort = (init?: RequestInit): Promise<Response> => {
      const signal = init?.signal;
      if (!signal) throw new Error('missing provider timeout signal');
      return new Promise((_, reject) => {
        signal.addEventListener('abort', () => reject(signal.reason), { once: true });
      });
    };
    fetchMock.mockImplementationOnce((_url: URL, init?: RequestInit) => waitForAbort(init));

    const coldLogin = readExactMediaCredential(
      scope,
      'https://infisical.synthetic.test',
      'synthetic-client-id',
      new AbortController().signal,
      cache
    );
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    expect(timeout).toHaveBeenNthCalledWith(1, 10_000);
    timeoutControllers[0].abort(new DOMException('timed out', 'TimeoutError'));
    await expect(coldLogin).rejects.toThrow('managed_redis_provider_timeout');

    fetchMock.mockReset();
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockImplementationOnce((_url: URL, init?: RequestInit) => waitForAbort(init));
    const namedRead = readExactMediaCredential(
      scope,
      'https://infisical.synthetic.test',
      'synthetic-client-id',
      new AbortController().signal,
      cache
    );
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(timeout).toHaveBeenNthCalledWith(2, 10_000);
    expect(timeout).toHaveBeenNthCalledWith(3, 5_000);
    timeoutControllers[2].abort(new DOMException('timed out', 'TimeoutError'));
    await expect(namedRead).rejects.toThrow('managed_redis_provider_timeout');
  });

  it('reuses a valid token across polls and renews it at half-life', async () => {
    let now = 1_000;
    vi.spyOn(Date, 'now').mockImplementation(() => now);
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      )
      .mockResolvedValueOnce(
        response({
          accessToken: 'renewed-token',
          tokenType: 'Bearer',
          expiresIn: 600,
          accessTokenMaxTTL: 3600,
        })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      );

    const read = () =>
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'client',
        new AbortController().signal,
        cache
      );
    await read();
    now += 30_000;
    await read();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(providerMocks.open).toHaveBeenCalledOnce();
    now += 270_000;
    await read();
    expect(fetchMock).toHaveBeenCalledTimes(5);
    expect(providerMocks.open).toHaveBeenCalledTimes(2);
    expect((fetchMock.mock.calls[4][1] as RequestInit).headers).toEqual({
      authorization: 'Bearer renewed-token',
    });
  });

  it('uses a still-valid token when scheduled renewal is temporarily unavailable', async () => {
    let now = 1_000;
    vi.spyOn(Date, 'now').mockImplementation(() => now);
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      )
      .mockResolvedValueOnce(response({}, 503))
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      );
    const read = () =>
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'client',
        new AbortController().signal,
        cache
      );
    await read();
    now += 300_000;
    await expect(read()).resolves.toEqual({ value: credentialValue(), returnedName: scope.name });
    expect((fetchMock.mock.calls[3][1] as RequestInit).headers).toEqual({
      authorization: `Bearer ${accessToken}`,
    });
  });

  it('refuses a provider outage after the cached token expires', async () => {
    let now = 1_000;
    vi.spyOn(Date, 'now').mockImplementation(() => now);
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      )
      .mockResolvedValueOnce(response({}, 503));
    const read = () =>
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'client',
        new AbortController().signal,
        cache
      );
    await read();
    now += 600_000;
    await expect(read()).rejects.toThrow('managed_redis_provider_unavailable');
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('invalidates a token rejected by the named secret GET before the next poll', async () => {
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(response({}, 401))
      .mockResolvedValueOnce(
        response({
          accessToken: 'renewed-token',
          tokenType: 'Bearer',
          expiresIn: 600,
          accessTokenMaxTTL: 3600,
        })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      );
    const read = () =>
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'client',
        new AbortController().signal,
        cache
      );
    await expect(read()).rejects.toThrow('managed_redis_provider_forbidden');
    await expect(read()).resolves.toEqual({ value: credentialValue(), returnedName: scope.name });
    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect((fetchMock.mock.calls[3][1] as RequestInit).headers).toEqual({
      authorization: 'Bearer renewed-token',
    });
  });

  it.each([401, 403])('clears token on login %i before a later outage', async (status) => {
    cache = { token: accessToken, renewAt: 0, expiresAt: Date.now() + 600_000 };
    fetchMock.mockResolvedValueOnce(response({}, status)).mockResolvedValueOnce(response({}, 429));
    const read = () =>
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'client',
        new AbortController().signal,
        cache
      );

    await expect(read()).rejects.toThrow('managed_redis_provider_forbidden');
    expect(cache).toEqual({ token: '', renewAt: 0, expiresAt: 0 });
    await expect(read()).rejects.toThrow('managed_redis_provider_rate_limited');
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('preserves a configured Infisical URL path prefix for login and the named secret', async () => {
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      );

    await readExactMediaCredential(
      scope,
      'https://infisical.synthetic.test/tenant/proxy/',
      'synthetic-client-id',
      new AbortController().signal,
      cache
    );

    expect(new URL(String(fetchMock.mock.calls[0][0])).pathname).toBe(
      '/tenant/proxy/api/v1/auth/universal-auth/login'
    );
    expect(new URL(String(fetchMock.mock.calls[1][0])).pathname).toBe(
      `/tenant/proxy/api/v4/secrets/${scope.name}`
    );
  });

  it('normalizes repeated slashes in the Infisical path prefix like the Go provider', async () => {
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(
        response({ secret: { secretKey: scope.name, secretValue: credentialValue() } })
      );

    await readExactMediaCredential(
      scope,
      'https://infisical.synthetic.test/tenant//proxy/',
      'synthetic-client-id',
      new AbortController().signal,
      cache
    );

    expect(new URL(String(fetchMock.mock.calls[0][0])).pathname).toBe(
      '/tenant/proxy/api/v1/auth/universal-auth/login'
    );
    expect(new URL(String(fetchMock.mock.calls[1][0])).pathname).toBe(
      `/tenant/proxy/api/v4/secrets/${scope.name}`
    );
  });

  it.each([
    ['missing expiresIn', { accessToken, tokenType: 'Bearer' }],
    ['zero expiresIn', { accessToken, tokenType: 'Bearer', expiresIn: 0 }],
    ['negative expiresIn', { accessToken, tokenType: 'Bearer', expiresIn: -1 }],
    ['non-numeric expiresIn', { accessToken, tokenType: 'Bearer', expiresIn: '600' }],
    ['missing accessTokenMaxTTL', { accessToken, tokenType: 'Bearer', expiresIn: 600 }],
    [
      'zero accessTokenMaxTTL',
      { accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 0 },
    ],
  ])('refuses login response with %s before issuing the secret GET', async (_name, body) => {
    fetchMock.mockResolvedValueOnce(response(body));

    await expect(
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'synthetic-client-id',
        new AbortController().signal,
        cache
      )
    ).rejects.toThrow();

    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('rejects a returned secret name mismatch and never exposes payload values in the error', async () => {
    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(
        response({
          secret: { secretKey: 'REDIS_CONTROL_PLANE_CREDENTIAL', secretValue: 'synthetic-value' }, // pragma: allowlist secret -- test fixture
        })
      );

    let error: unknown;
    try {
      await readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'synthetic-client-id',
        new AbortController().signal,
        cache
      );
    } catch (caught) {
      error = caught;
    }
    expect(error).toBeInstanceOf(Error);
    expect(String(error)).not.toContain('synthetic-value');
    expect(String(error)).not.toContain(syntheticLeaf);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('rejects redirects and malformed provider responses with redacted failures', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(null, { status: 302, headers: { location: 'https://attacker.invalid' } })
    );
    await expect(
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'synthetic-client-id',
        new AbortController().signal,
        cache
      )
    ).rejects.toThrow();
    expect(fetchMock).toHaveBeenCalledOnce();

    fetchMock
      .mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      )
      .mockResolvedValueOnce(new Response('synthetic-provider-body-secret'));
    let error: unknown;
    try {
      await readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'synthetic-client-id',
        new AbortController().signal,
        cache
      );
    } catch (caught) {
      error = caught;
    }
    expect(String(error)).not.toContain('synthetic-provider-body-secret');
    expect(String(error)).not.toContain(accessToken);
  });

  it.each(['login', 'named secret'])('stops reading an oversized %s response', async (target) => {
    const cancelled = vi.fn();
    const pulled = vi.fn();
    if (target === 'named secret') {
      fetchMock.mockResolvedValueOnce(
        response({ accessToken, tokenType: 'Bearer', expiresIn: 600, accessTokenMaxTTL: 3600 })
      );
    }
    fetchMock.mockResolvedValueOnce(oversizedResponse(cancelled, pulled));

    await expect(
      readExactMediaCredential(
        scope,
        'https://infisical.synthetic.test',
        'synthetic-client-id',
        new AbortController().signal,
        cache
      )
    ).rejects.toThrow('managed_redis_provider_invalid');

    expect(cancelled).toHaveBeenCalledOnce();
    expect(pulled.mock.calls.length).toBeLessThan(100);
    expect(fetchMock).toHaveBeenCalledTimes(target === 'login' ? 1 : 2);
  });
});

describe('acknowledgeMediaCredential', () => {
  const ack: AppliedAck = {
    version: 1,
    rotationId: 'rotation-test-12',
    service: 'media-plane',
    instanceId: 'synthetic-hostname',
    generation: 12,
    username: 'cv_mp_12',
  };

  async function withAckServer(
    reply: string | null | 'hold',
    verifyRequest: (received: string) => void = () => {}
  ): Promise<{ path: string; close: () => Promise<void> }> {
    const dir = await mkdtemp(join(tmpdir(), 'cv-ack-'));
    const path = join(dir, 'ack.sock');
    const server = createServer((socket) => {
      let received = '';
      socket.on('data', (chunk) => {
        received += chunk.toString('utf8');
        if (!received.endsWith('\n')) return;
        verifyRequest(received);
        if (reply === 'hold') return;
        if (reply === null) socket.end();
        else socket.end(reply);
      });
    });
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject);
      server.listen(path, resolve);
    });
    return {
      path,
      close: async () => {
        await new Promise<void>((resolve) => server.close(() => resolve()));
        await rm(dir, { recursive: true, force: true });
      },
    };
  }

  it('writes the exact applied ack and resolves only on a positive socket response', async () => {
    const liveAck = { ...ack, instanceId: hostname() };
    const server = await withAckServer('{"ok":true}\n', (received) => {
      expect(received).toBe(`${JSON.stringify(liveAck)}\n`);
    });
    try {
      await expect(acknowledgeMediaCredential(liveAck, server.path)).resolves.toBeUndefined();
    } finally {
      await server.close();
    }
  });

  it.each([
    ['negative acknowledgment', '{"ok":false}\n'],
    ['malformed response', 'not-json\n'],
    ['oversized response', `${'x'.repeat(257)}\n`],
    ['EOF without acknowledgment', null],
    ['acknowledgment timeout', 'hold'],
  ])('rejects %s', async (_name, reply) => {
    const liveAck = { ...ack, instanceId: hostname() };
    const server = await withAckServer(reply);
    try {
      await expect(acknowledgeMediaCredential(liveAck, server.path)).rejects.toThrow();
    } finally {
      await server.close();
    }
  });

  it('refuses an ack claiming a different container hostname before connecting', async () => {
    const wrongHost = { ...ack, instanceId: 'other-container' };
    await expect(
      acknowledgeMediaCredential(wrongHost, '/private/tmp/unused.sock')
    ).rejects.toThrow();
  });
});
