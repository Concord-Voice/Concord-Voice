import { constants } from 'node:fs';
import { open } from 'node:fs/promises';
import { connect as connectSocket } from 'node:net';
import { hostname } from 'node:os';
import { posix } from 'node:path';

const PROVIDER_LOGIN_TIMEOUT_MS = 10_000;
const PROVIDER_NAMED_READ_TIMEOUT_MS = 5_000;
export const PROVIDER_CREDENTIAL_FETCH_TIMEOUT_MS =
  PROVIDER_LOGIN_TIMEOUT_MS + PROVIDER_NAMED_READ_TIMEOUT_MS;

export interface SecretScope {
  projectId: string;
  environment: string;
  path: string;
  name: string;
}

export interface ManagedCredential {
  version: 1;
  scope: SecretScope;
  rotationId: string;
  service: 'media-plane';
  generation: number;
  username: string;
  password: string;
  validFrom: string;
  validUntil: string;
}

export type ExactSecretReader = (
  scope: SecretScope
) => Promise<{ value: string; returnedName?: string }>;

export interface AppliedAck {
  version: 1;
  rotationId: string;
  service: 'media-plane';
  instanceId: string;
  generation: number;
  username: string;
}

function fail(): never {
  throw new Error('managed_redis_credential_invalid');
}

function exactKeys(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const actual = Object.keys(value);
  return actual.length === keys.length && actual.every((key) => keys.includes(key));
}

function parsedTimestamp(value: unknown): number {
  if (typeof value !== 'string') return Number.NaN;
  const match =
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.exec(
      value
    );
  if (!match) return Number.NaN;
  const [, yearText, monthText, dayText, hourText, minuteText, secondText] = match;
  const year = Number(yearText);
  const month = Number(monthText);
  const day = Number(dayText);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (
    month < 1 ||
    month > 12 ||
    day < 1 ||
    day > days[month - 1] ||
    Number(hourText) > 23 ||
    Number(minuteText) > 59 ||
    Number(secondText) > 59
  )
    return Number.NaN;
  return Date.parse(value);
}

// JSON.parse silently accepts duplicate object keys. Inspect the bounded JSON token stream first.
function rejectDuplicateKeys(raw: string): void {
  let at = 0;
  let depth = 0;
  const space = () => {
    while (/\s/.test(raw[at] ?? '')) at += 1;
  };
  const string = (): string => {
    const start = at++;
    while (at < raw.length) {
      if (raw[at] === '\\') {
        at += 2;
      } else if (raw[at++] === '"') {
        return JSON.parse(raw.slice(start, at)) as string;
      }
    }
    return fail();
  };
  const object = (): void => {
    at += 1;
    const seen = new Set<string>();
    space();
    while (raw[at] !== '}') {
      if (raw[at] !== '"') fail();
      const key = string();
      if (seen.has(key)) fail();
      seen.add(key);
      space();
      if (raw[at++] !== ':') fail();
      value();
      space();
      if (raw[at] !== ',') break;
      at += 1;
      space();
    }
    if (raw[at++] !== '}') fail();
  };
  const array = (): void => {
    at += 1;
    space();
    while (raw[at] !== ']') {
      value();
      space();
      if (raw[at] !== ',') break;
      at += 1;
    }
    if (raw[at++] !== ']') fail();
  };
  const primitive = (): void => {
    const start = at;
    while (at < raw.length && !/[\s,}\]]/.test(raw[at])) at += 1;
    if (start === at) fail();
  };
  const value = (): void => {
    space();
    if (++depth > 16) fail();
    if (raw[at] === '{') object();
    else if (raw[at] === '[') array();
    else if (raw[at] === '"') string();
    else primitive();
    depth -= 1;
  };
  value();
  space();
  if (at !== raw.length) fail();
}

export function parseManagedCredential(
  raw: string,
  scope: SecretScope,
  now: Date
): ManagedCredential {
  try {
    if (Buffer.byteLength(raw, 'utf8') > 16_384) fail();
    rejectDuplicateKeys(raw);
    const credential: unknown = JSON.parse(raw);
    if (
      !exactKeys(credential, [
        'version',
        'scope',
        'rotationId',
        'service',
        'generation',
        'username',
        'password',
        'validFrom',
        'validUntil',
      ])
    )
      fail();
    const payloadScope = credential.scope;
    if (!exactKeys(payloadScope, ['projectId', 'environment', 'path', 'name'])) fail();
    if (
      Object.keys(scope).length !== 4 ||
      !Object.entries(scope).every(
        ([key, val]) => payloadScope[key] === val && typeof val === 'string' && val.length > 0
      )
    )
      fail();
    if (credential.version !== 1 || credential.service !== 'media-plane') fail();
    const generation = credential.generation;
    if (typeof generation !== 'number' || !Number.isSafeInteger(generation) || generation <= 0)
      fail();
    if (
      typeof credential.rotationId !== 'string' ||
      !/^[A-Za-z0-9_-]{1,64}$/.test(credential.rotationId)
    )
      fail();
    if (
      typeof credential.username !== 'string' ||
      credential.username !== `cv_mp_${credential.generation}`
    )
      fail();
    if (
      typeof credential.password !== 'string' || // pragma: allowlist secret -- runtime field, no literal
      credential.password.length === 0
    )
      fail();
    if (typeof credential.validFrom !== 'string' || typeof credential.validUntil !== 'string')
      fail();
    const from = parsedTimestamp(credential.validFrom);
    const until = parsedTimestamp(credential.validUntil);
    if (
      !Number.isFinite(from) ||
      !Number.isFinite(until) ||
      from > now.getTime() ||
      until <= now.getTime()
    )
      fail();
    return credential as unknown as ManagedCredential;
  } catch {
    return fail();
  }
}

const CLIENT_SECRET_PATH = '/run/secrets/redis-infisical-client-secret';

async function readClientSecret(): Promise<string> {
  const file = await open(CLIENT_SECRET_PATH, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const leaf = await file.stat();
    const allowedOwner = leaf.uid === 0 || leaf.uid === process.geteuid?.();
    const groupReadable = (leaf.mode & 0o040) !== 0;
    const allowedGroup =
      !groupReadable ||
      leaf.gid === process.getegid?.() ||
      process.getgroups?.().includes(leaf.gid);
    if (
      !leaf.isFile() ||
      !allowedOwner ||
      !allowedGroup ||
      (leaf.mode & 0o037) !== 0 ||
      leaf.size < 1 ||
      leaf.size > 8192
    )
      fail();
    const data = Buffer.alloc(8193);
    const { bytesRead } = await file.read(data, 0, data.length, 0);
    if (bytesRead < 1 || bytesRead > 8192) fail();
    return data.subarray(0, bytesRead).toString('utf8').trim();
  } finally {
    await file.close();
  }
}

function providerStatus(status: number): never {
  if (status === 401 || status === 403) throw new Error('managed_redis_provider_forbidden');
  if (status === 429) throw new Error('managed_redis_provider_rate_limited');
  throw new Error('managed_redis_provider_unavailable');
}

async function boundedProviderBody(response: Response): Promise<string> {
  if (!response.body) fail();
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > 65_536) fail();
      chunks.push(value);
    }
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
  } finally {
    reader.releaseLock();
  }
  return Buffer.concat(chunks, size).toString('utf8');
}

interface ProviderTokenCache {
  token: string;
  renewAt: number;
  expiresAt: number;
}

function providerBase(siteUrl: string): { base: URL; prefix: string } {
  const base = new URL(siteUrl);
  if (
    base.protocol !== 'https:' ||
    base.username ||
    base.password ||
    base.search ||
    base.hash ||
    !/^\/[A-Za-z0-9._/-]*$/.test(base.pathname)
  )
    fail();
  return { base, prefix: posix.normalize(base.pathname).replace(/\/$/, '') };
}

function parseProviderLogin(raw: string): { token: string; ttlMs: number } {
  if (Buffer.byteLength(raw, 'utf8') > 65_536) fail();
  const auth: unknown = JSON.parse(raw);
  if (
    typeof auth !== 'object' ||
    auth === null ||
    !('accessToken' in auth) ||
    typeof auth.accessToken !== 'string' ||
    auth.accessToken.length === 0 ||
    !('expiresIn' in auth) ||
    typeof auth.expiresIn !== 'number' ||
    !Number.isFinite(auth.expiresIn) ||
    auth.expiresIn <= 0 ||
    !('accessTokenMaxTTL' in auth) ||
    typeof auth.accessTokenMaxTTL !== 'number' ||
    !Number.isFinite(auth.accessTokenMaxTTL) ||
    auth.accessTokenMaxTTL <= 0 ||
    !('tokenType' in auth) ||
    auth.tokenType !== 'Bearer'
  )
    fail();
  const ttlMs = Math.min(auth.expiresIn, auth.accessTokenMaxTTL) * 1000;
  if (!Number.isSafeInteger(ttlMs)) fail();
  return { token: auth.accessToken, ttlMs };
}

async function refreshProviderToken(
  base: URL,
  prefix: string,
  clientId: string,
  signal: AbortSignal,
  cache: ProviderTokenCache
): Promise<void> {
  if (cache.token && Date.now() < cache.renewAt) return;
  try {
    const clientSecret = await readClientSecret();
    if (!clientSecret) fail();
    const loginStartedAt = Date.now();
    const login = await fetch(new URL(`${prefix}/api/v1/auth/universal-auth/login`, base), {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ clientId, clientSecret }),
      signal: AbortSignal.any([signal, AbortSignal.timeout(PROVIDER_LOGIN_TIMEOUT_MS)]),
      redirect: 'error',
    });
    if (!login.ok) {
      if (login.status === 401 || login.status === 403) {
        cache.token = '';
        cache.renewAt = 0;
        cache.expiresAt = 0;
      }
      providerStatus(login.status);
    }
    const { token, ttlMs } = parseProviderLogin(await boundedProviderBody(login));
    if (Date.now() >= loginStartedAt + ttlMs) fail();
    cache.token = token;
    cache.renewAt = loginStartedAt + ttlMs / 2;
    cache.expiresAt = loginStartedAt + ttlMs;
  } catch (error) {
    const temporary =
      error instanceof TypeError ||
      (error instanceof Error &&
        (error.name === 'TimeoutError' ||
          error.message === 'managed_redis_provider_rate_limited' ||
          error.message === 'managed_redis_provider_unavailable'));
    if (!temporary || signal.aborted || !cache.token || Date.now() >= cache.expiresAt) throw error;
  }
}

function parseNamedSecret(raw: string, name: string): { value: string; returnedName: string } {
  if (Buffer.byteLength(raw, 'utf8') > 65_536) fail();
  const response: unknown = JSON.parse(raw);
  if (
    typeof response !== 'object' ||
    response === null ||
    !('secret' in response) ||
    typeof response.secret !== 'object' || // pragma: allowlist secret -- runtime response, no literal
    response.secret === null ||
    !('secretKey' in response.secret) ||
    response.secret.secretKey !== name ||
    !('secretValue' in response.secret) ||
    typeof response.secret.secretValue !== 'string' // pragma: allowlist secret -- runtime response, no literal
  )
    fail();
  return { value: response.secret.secretValue, returnedName: response.secret.secretKey };
}

export async function readExactMediaCredential(
  scope: SecretScope,
  siteUrl: string,
  clientId: string,
  signal: AbortSignal,
  cache: ProviderTokenCache
): Promise<{ value: string; returnedName?: string }> {
  try {
    if (!clientId) fail();
    const { base, prefix } = providerBase(siteUrl);
    await refreshProviderToken(base, prefix, clientId, signal, cache);
    if (Date.now() >= cache.expiresAt) fail();
    const path = new URL(`${prefix}/api/v4/secrets/${encodeURIComponent(scope.name)}`, base);
    path.searchParams.set('projectId', scope.projectId);
    path.searchParams.set('environment', scope.environment);
    path.searchParams.set('secretPath', scope.path);
    path.searchParams.set('type', 'shared');
    path.searchParams.set('includeImports', 'false');
    path.searchParams.set('expandSecretReferences', 'false');
    const fetched = await fetch(path, {
      headers: { authorization: `Bearer ${cache.token}` },
      signal: AbortSignal.any([signal, AbortSignal.timeout(PROVIDER_NAMED_READ_TIMEOUT_MS)]),
      redirect: 'error',
    });
    if (!fetched.ok) {
      if (fetched.status === 401 || fetched.status === 403) {
        cache.token = '';
        cache.renewAt = 0;
        cache.expiresAt = 0;
      }
      providerStatus(fetched.status);
    }
    return parseNamedSecret(await boundedProviderBody(fetched), scope.name);
  } catch (error) {
    if (
      error instanceof Error &&
      /^managed_redis_provider_(forbidden|rate_limited|unavailable)$/.test(error.message)
    ) {
      throw error;
    }
    if (signal.aborted || (error instanceof Error && error.name === 'TimeoutError')) {
      // eslint-disable-next-line preserve-caught-error -- provider errors may contain credentials or raw responses
      throw new Error('managed_redis_provider_timeout');
    }
    // eslint-disable-next-line preserve-caught-error -- provider errors may contain credentials or raw responses
    throw new Error('managed_redis_provider_invalid');
  }
}

export async function acknowledgeMediaCredential(
  ack: AppliedAck,
  socketPath = '/run/redis-auth/ack-media.sock'
): Promise<void> {
  if (ack.instanceId !== hostname()) fail();
  await new Promise<void>((resolve, reject) => {
    const socket = connectSocket(socketPath);
    let response = '';
    let settled = false;
    const finish = (ok: boolean) => {
      if (settled) return;
      settled = true;
      socket.destroy();
      if (ok) resolve();
      else reject(new Error('managed_redis_ack_failed'));
    };
    socket.setTimeout(5000);
    socket.once('connect', () => socket.end(`${JSON.stringify(ack)}\n`));
    socket.on('data', (chunk: Buffer) => {
      response += chunk.toString('utf8');
      if (response.length > 256) return finish(false);
      if (response.endsWith('\n')) {
        try {
          const reply: unknown = JSON.parse(response);
          finish(exactKeys(reply, ['ok']) && reply.ok === true);
        } catch {
          finish(false);
        }
      }
    });
    socket.once('end', () => finish(false));
    socket.once('timeout', () => finish(false));
    socket.once('error', () => finish(false));
  });
}
