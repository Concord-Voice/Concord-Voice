/**
 * Control-plane client for the mobile app, carried over from the #663 probe.
 * Holds tokens in memory only; key material never passes through here.
 * Not yet covered: MFA login, token refresh and persisted sessions.
 */

export type Fetch = typeof fetch;

export type Session = { accessToken: string; userId: string };

export type LoginKeys = {
  wrappedPrivateKey: string;
  keyDerivationSalt: string;
  keyDerivationAlg: string;
};

export class ApiError extends Error {
  constructor(readonly status: number, readonly code: string | undefined) {
    // Status and error code only: response bodies may echo user data.
    super(`Concord API ${status}${code ? ` ${code}` : ''}`);
  }
}

const LOCAL_HOSTS = new Set(['localhost', '127.0.0.1']);

/** http only for the local dev stack; anything else must be https. */
export function checkBaseUrl(base: string): string {
  const match = /^(https?):\/\/([A-Za-z0-9.-]+)(:\d+)?$/.exec(base);
  if (!match) throw new Error('Invalid API base URL');
  if (match[1] === 'http' && !LOCAL_HOSTS.has(match[2])) {
    throw new Error('Plain http is only allowed for the local dev stack');
  }
  return base;
}

function record(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('Invalid API response');
  }
  return value as Record<string, unknown>;
}

function text(value: unknown): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error('Invalid API response');
  }
  return value;
}

export class ConcordApi {
  private session: Session | null = null;
  private readonly base: string;

  constructor(
    base: string,
    private readonly machineId: string,
    private readonly fetchImpl: Fetch = fetch,
    private readonly requestTimeoutMs = 15000,
  ) {
    this.base = `${checkBaseUrl(base)}/api/v1`;
  }

  get userId(): string {
    if (!this.session) throw new Error('Not signed in');
    return this.session.userId;
  }

  wsUrl(ticket: string): string {
    return `${this.base.replace(/^http/, 'ws')}/ws?ticket=${encodeURIComponent(
      ticket,
    )}`;
  }

  private async call(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<Record<string, unknown>> {
    const headers: Record<string, string> = {
      Accept: 'application/json',
      'X-Machine-Id': this.machineId,
    };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (this.session)
      headers.Authorization = `Bearer ${this.session.accessToken}`;
    // A stalled server must not hold a caller (and its native key handles) forever:
    // the abort covers the headers and the body read.
    const abort = new AbortController();
    const timer = setTimeout(() => abort.abort(), this.requestTimeoutMs);
    let response: Response;
    let parsed: unknown;
    try {
      response = await this.fetchImpl(`${this.base}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: abort.signal,
      });
      // Only a malformed body counts as empty; an abort or a failed body read must surface.
      parsed = await response.json().catch((error: unknown) => {
        if (error instanceof SyntaxError) return {};
        throw error;
      });
    } catch (error) {
      if (abort.signal.aborted)
        throw new Error('Concord API request timed out');
      throw error;
    } finally {
      clearTimeout(timer);
    }
    if (!response.ok) {
      const code = (parsed as { code?: unknown } | null)?.code;
      throw new ApiError(
        response.status,
        typeof code === 'string' ? code : undefined,
      );
    }
    return record(parsed);
  }

  private adopt(result: Record<string, unknown>): void {
    this.session = {
      accessToken: text(result.access_token),
      userId: text(record(result.user).id),
    };
  }

  async register(input: {
    email: string;
    username: string;
    password: string;
    publicKey: string;
    wrappedPrivateKey: string;
    keyDerivationSalt: string;
  }): Promise<string> {
    const result = await this.call('POST', '/auth/register', {
      email: input.email,
      username: input.username,
      password: input.password,
      age_confirmation: true,
      public_key: input.publicKey,
      wrapped_private_key: input.wrappedPrivateKey,
      key_derivation_salt: input.keyDerivationSalt,
      key_derivation_alg: 'argon2id',
    });
    return text(result.pending_id);
  }

  async confirm(pendingId: string, code: string): Promise<void> {
    this.adopt(
      await this.call('POST', '/auth/register/confirm', {
        pending_id: pendingId,
        code,
      }),
    );
  }

  async login(email: string, password: string): Promise<LoginKeys> {
    const result = await this.call('POST', '/auth/login', { email, password });
    if (result.mfa_required === true)
      throw new Error('MFA accounts are out of scope');
    this.adopt(result);
    const keys = record(result.e2ee_keys);
    return {
      wrappedPrivateKey: text(keys.wrapped_private_key),
      keyDerivationSalt: text(keys.key_derivation_salt),
      keyDerivationAlg: text(keys.key_derivation_alg),
    };
  }

  async findUserId(username: string): Promise<string> {
    const result = await this.call(
      'GET',
      `/users/search?q=${encodeURIComponent(username)}`,
    );
    const users = Array.isArray(result.users) ? result.users.map(record) : [];
    const hit = users.find(u => u.username === username);
    if (!hit) throw new Error('User not found');
    return text(hit.id);
  }

  /** The current public key and its key_version, which a wrap must be reported against. */
  async publicKey(
    userId: string,
  ): Promise<{ publicKey: string; keyVersion: number }> {
    const result = await this.call(
      'GET',
      `/users/${encodeURIComponent(userId)}/public-key`,
    );
    const keyVersion = result.key_version;
    if (
      typeof keyVersion !== 'number' ||
      !Number.isSafeInteger(keyVersion) ||
      keyVersion < 1
    )
      throw new Error('Invalid API response');
    return { publicKey: text(result.public_key), keyVersion };
  }

  async openDm(userId: string): Promise<string> {
    const result = await this.call('POST', '/dm/conversations', {
      user_id: userId,
    });
    return text(record(result.conversation).id);
  }

  async distributeDmKey(
    conversationId: string,
    wrappedKeys: Record<string, string>,
    wrappedKeyVersions: Record<string, number>,
  ): Promise<void> {
    // The versions let the server refuse a wrap made against a key that rotated meanwhile.
    await this.call(
      'POST',
      `/e2ee/keys/${encodeURIComponent(conversationId)}`,
      {
        wrapped_keys: wrappedKeys,
        wrapped_key_versions: wrappedKeyVersions,
      },
    );
  }

  async dmMessages(
    conversationId: string,
  ): Promise<Array<Record<string, unknown>>> {
    const result = await this.call(
      'GET',
      `/dm/conversations/${encodeURIComponent(conversationId)}/messages`,
    );
    return Array.isArray(result.messages) ? result.messages.map(record) : [];
  }

  async wsTicket(): Promise<string> {
    return text((await this.call('POST', '/auth/ws-ticket')).ticket);
  }
}
