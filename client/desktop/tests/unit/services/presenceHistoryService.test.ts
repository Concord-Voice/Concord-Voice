import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  ACTIVITY_HISTORY_RETENTION_DAYS,
  PresenceHistoryRequestError,
  deletePresenceHistory,
  getPresenceHistoryPage,
  getPresenceHistorySettings,
  patchPresenceHistorySettings,
  type PresenceHistorySettingsMutation,
} from '@/renderer/services/presenceHistoryService';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';

const API_BASE = 'http://localhost:8080';
const HISTORY_ENDPOINT = `${API_BASE}/api/v1/users/me/presence-history`;
const SETTINGS_ENDPOINT = `${HISTORY_ENDPOINT}/settings`;
const VALID_HASH = 'a'.repeat(64);
const OTHER_VALID_HASH = 'b'.repeat(64);
const HISTORY_ID = '11111111-1111-4111-8111-111111111111';
const STARTED_AT = '2026-07-12T14:00:00.000000000Z';
const ENDED_AT = '2026-07-12T14:45:00+00:00';
const EXPIRES_AT = '2026-08-11T14:00:00Z';

function validDisclosure(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    version: 1,
    copy_hash: VALID_HASH,
    operator_name: 'Concord Voice LLC',
    required_text:
      'Persistent activity history is stored on Concord servers. This data may be subject to legal subpoena. Disable to delete all history.',
    details: ['History starts with your next Custom Status change.', 'History is server-readable.'],
    privacy_policy_url: 'https://concordvoice.com/privacy-policy',
    acknowledgement_label:
      'I understand and consent to server-readable Activity History under the terms above.',
    ...overrides,
  };
}

function validSettings(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    available: true,
    enabled: false,
    reconsent_required: false,
    retention_days: 30,
    consent_version: null,
    consent_copy_hash: null,
    consented_at: null,
    required_consent: validDisclosure(),
    ...overrides,
  };
}

function validEnabledSettings(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return validSettings({
    enabled: true,
    consent_version: 1,
    consent_copy_hash: VALID_HASH,
    consented_at: STARTED_AT,
    ...overrides,
  });
}

function supportedItem(
  payload: Record<string, unknown> = { text: 'Reviewing a pull request', emoji: '🔍' },
  overrides: Record<string, unknown> = {}
): Record<string, unknown> {
  return {
    status: 'supported',
    id: HISTORY_ID,
    category: 'custom_text',
    payload_version: 1,
    payload,
    started_at: STARTED_AT,
    ended_at: ENDED_AT,
    recorded_at: STARTED_AT,
    expires_at: EXPIRES_AT,
    ...overrides,
  };
}

function unsupportedItem(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    status: 'unsupported',
    id: HISTORY_ID,
    category: 'games',
    payload_version: 2,
    payload: null,
    started_at: STARTED_AT,
    ended_at: null,
    recorded_at: STARTED_AT,
    expires_at: EXPIRES_AT,
    ...overrides,
  };
}

function page(items: Record<string, unknown>[] = [supportedItem()]): Record<string, unknown> {
  return { items, next_cursor: null };
}

function serveSettings(body: unknown, status = 200, headers?: Record<string, string>): void {
  server.use(
    http.get(SETTINGS_ENDPOINT, () => HttpResponse.json(body, { status, headers })),
    http.patch(SETTINGS_ENDPOINT, () => HttpResponse.json(body, { status, headers }))
  );
}

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
});

afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
});

describe('Activity History settings schema', () => {
  it('exposes the exact retention choices', () => {
    expect(ACTIVITY_HISTORY_RETENTION_DAYS).toEqual([7, 30, 90, 365]);
  });

  it('strictly parses and camel-cases a valid available settings disclosure', async () => {
    serveSettings(validSettings());

    await expect(getPresenceHistorySettings()).resolves.toEqual({
      available: true,
      enabled: false,
      reconsentRequired: false,
      retentionDays: 30,
      consentVersion: null,
      consentCopyHash: null,
      consentedAt: null,
      requiredConsent: {
        version: 1,
        copyHash: VALID_HASH,
        operatorName: 'Concord Voice LLC',
        requiredText:
          'Persistent activity history is stored on Concord servers. This data may be subject to legal subpoena. Disable to delete all history.',
        details: [
          'History starts with your next Custom Status change.',
          'History is server-readable.',
        ],
        privacyPolicyUrl: 'https://concordvoice.com/privacy-policy',
        acknowledgementLabel:
          'I understand and consent to server-readable Activity History under the terms above.',
      },
    });
  });

  it('accepts unavailable settings only without a disclosure or consent metadata', async () => {
    const unavailable = validSettings({ available: false });
    delete unavailable.required_consent;
    serveSettings(unavailable);

    await expect(getPresenceHistorySettings()).resolves.toMatchObject({
      available: false,
      enabled: false,
      reconsentRequired: false,
      requiredConsent: null,
    });
  });

  it('accepts an enabled row bound to the current returned disclosure', async () => {
    serveSettings(validEnabledSettings());

    await expect(getPresenceHistorySettings()).resolves.toMatchObject({
      available: true,
      enabled: true,
      reconsentRequired: false,
      consentVersion: 1,
      consentCopyHash: VALID_HASH,
      consentedAt: STARTED_AT,
    });
  });

  it('accepts a paused re-consent row while retaining the selected retention', async () => {
    serveSettings(validSettings({ reconsent_required: true, retention_days: 7 }));

    await expect(getPresenceHistorySettings()).resolves.toMatchObject({
      enabled: false,
      reconsentRequired: true,
      retentionDays: 7,
      consentVersion: null,
      consentCopyHash: null,
      consentedAt: null,
    });
  });

  it.each([
    ['HTTPS', 'https://privacy.example.com/activity-history'],
    ['uppercase HTTPS', 'HTTPS://privacy.example.com/activity-history'],
    ['localhost HTTP', 'http://localhost:8080/privacy'],
    ['mixed-case localhost HTTP', 'HtTp://LOCALHOST:8080/privacy'],
    ['IPv4 loopback HTTP', 'http://127.0.0.2:8080/privacy'],
    ['IPv6 loopback HTTP', 'http://[::1]:8080/privacy'],
    ['IPv4-mapped IPv6 loopback HTTP', 'http://[::ffff:127.0.0.2]/privacy'],
  ])('accepts a backend-valid %s privacy policy URL', async (_name, privacyPolicyUrl) => {
    serveSettings(
      validSettings({
        required_consent: validDisclosure({ privacy_policy_url: privacyPolicyUrl }),
      })
    );

    await expect(getPresenceHistorySettings()).resolves.toMatchObject({
      requiredConsent: { privacyPolicyUrl },
    });
  });

  it.each([
    ['JavaScript URL', 'javascript:alert(1)'],
    ['mailto URL', 'mailto:privacy@example.com'],
    ['FTP URL', 'ftp://privacy.example.com/policy'],
    ['opaque HTTPS URL', 'https:privacy.example.com/policy'],
    ['arbitrary HTTP host', 'http://privacy.example.com/policy'],
    ['non-loopback HTTP IP', 'http://192.168.1.8/privacy'],
    ['mapped non-loopback HTTP IP', 'http://[::ffff:192.168.1.8]/privacy'],
    ['username credentials', 'https://reader@privacy.example.com/policy'],
    [
      'username and password credentials',
      'https://reader:secret@privacy.example.com/policy', // pragma: allowlist secret
    ],
    ['empty userinfo', 'https://@privacy.example.com/policy'],
  ])('rejects a backend-invalid %s privacy policy URL', async (_name, privacyPolicyUrl) => {
    serveSettings(
      validSettings({
        required_consent: validDisclosure({ privacy_policy_url: privacyPolicyUrl }),
      })
    );

    await expect(getPresenceHistorySettings()).rejects.toThrow('Invalid Activity History response');
  });

  it.each([
    ['tab in host', 'https://privacy.\texample.com/policy'],
    ['line feed in path', 'https://privacy.example.com/poli\ncy'],
    ['carriage return in query', 'https://privacy.example.com/policy?version=\r1'],
    ['U+0001 in path', 'https://privacy.example.com/policy\u0001notice'],
    ['DEL in query', 'https://privacy.example.com/policy?version=\u007f1'],
  ])('rejects raw ASCII control bytes: %s', async (_name, privacyPolicyUrl) => {
    serveSettings(
      validSettings({
        required_consent: validDisclosure({ privacy_policy_url: privacyPolicyUrl }),
      })
    );

    await expect(getPresenceHistorySettings()).rejects.toThrow('Invalid Activity History response');
  });

  it.each(ACTIVITY_HISTORY_RETENTION_DAYS)('accepts retention_days=%s', async (retentionDays) => {
    serveSettings(validSettings({ retention_days: retentionDays }));

    await expect(getPresenceHistorySettings()).resolves.toMatchObject({ retentionDays });
  });

  it.each([
    {
      name: 'available response without required disclosure',
      build: () => {
        const value = validSettings();
        delete value.required_consent;
        return value;
      },
    },
    {
      name: 'unavailable response with required disclosure',
      build: () => validSettings({ available: false }),
    },
    {
      name: 'unavailable response claiming recording is enabled',
      build: () => validEnabledSettings({ available: false }),
    },
    {
      name: 'enabled response requiring re-consent',
      build: () => validEnabledSettings({ reconsent_required: true }),
    },
    {
      name: 'enabled response without consent version',
      build: () => validEnabledSettings({ consent_version: null }),
    },
    {
      name: 'enabled response without consent hash',
      build: () => validEnabledSettings({ consent_copy_hash: null }),
    },
    {
      name: 'enabled response without consent time',
      build: () => validEnabledSettings({ consented_at: null }),
    },
    {
      name: 'enabled response with stale consent version',
      build: () => validEnabledSettings({ consent_version: 2 }),
    },
    {
      name: 'enabled response with stale consent hash',
      build: () => validEnabledSettings({ consent_copy_hash: OTHER_VALID_HASH }),
    },
    {
      name: 'disabled response retaining consent version',
      build: () => validSettings({ consent_version: 1 }),
    },
    {
      name: 'disabled response retaining consent hash',
      build: () => validSettings({ consent_copy_hash: VALID_HASH }),
    },
    {
      name: 'disabled response retaining consent time',
      build: () => validSettings({ consented_at: STARTED_AT }),
    },
  ])('rejects the cross-field invariant: $name', async ({ build }) => {
    serveSettings(build());

    await expect(getPresenceHistorySettings()).rejects.toThrow('Invalid Activity History response');
  });

  it.each([
    { name: 'unknown settings field', build: () => validSettings({ extra: true }) },
    {
      name: 'unknown disclosure field',
      build: () => validSettings({ required_consent: validDisclosure({ extra: true }) }),
    },
    { name: 'invalid retention', build: () => validSettings({ retention_days: 8 }) },
    {
      name: 'uppercase or malformed hash',
      build: () =>
        validEnabledSettings({
          consent_copy_hash: 'A'.repeat(64),
          required_consent: validDisclosure({ copy_hash: 'A'.repeat(64) }),
        }),
    },
    {
      name: 'malformed consent timestamp',
      build: () => validEnabledSettings({ consented_at: '2026-07-12 14:00:00' }),
    },
    {
      name: 'non-URL policy',
      build: () =>
        validSettings({ required_consent: validDisclosure({ privacy_policy_url: 'not a URL' }) }),
    },
  ])('rejects $name', async ({ build }) => {
    serveSettings(build());

    await expect(getPresenceHistorySettings()).rejects.toThrow('Invalid Activity History response');
  });
});

describe('Activity History settings transport', () => {
  it.each([
    {
      name: 'enable',
      mutation: {
        kind: 'enable',
        retentionDays: 30,
        consentVersion: 1,
        consentCopyHash: VALID_HASH,
      } satisfies PresenceHistorySettingsMutation,
      expected: {
        enabled: true,
        retention_days: 30,
        acknowledged: true,
        consent_version: 1,
        consent_copy_hash: VALID_HASH,
      },
    },
    {
      name: 'disable',
      mutation: { kind: 'disable' } satisfies PresenceHistorySettingsMutation,
      expected: { enabled: false },
    },
    {
      name: 'retention',
      mutation: {
        kind: 'retention',
        retentionDays: 90,
      } satisfies PresenceHistorySettingsMutation,
      expected: { retention_days: 90 },
    },
  ])('sends the exact $name PATCH body', async ({ mutation, expected }) => {
    let body: unknown;
    let contentType: string | null = null;
    server.use(
      http.patch(SETTINGS_ENDPOINT, async ({ request }) => {
        body = await request.json();
        contentType = request.headers.get('Content-Type');
        return HttpResponse.json(validSettings());
      })
    );

    await patchPresenceHistorySettings(mutation);

    expect(body).toEqual(expected);
    expect(contentType).toBe('application/json');
  });

  it('rejects an invalid runtime mutation before transport', async () => {
    let calls = 0;
    server.use(
      http.patch(SETTINGS_ENDPOINT, () => {
        calls += 1;
        return HttpResponse.json(validSettings());
      })
    );

    await expect(
      patchPresenceHistorySettings({
        kind: 'retention',
        retentionDays: 8,
      } as unknown as PresenceHistorySettingsMutation)
    ).rejects.toThrow('Invalid Activity History settings mutation');
    expect(calls).toBe(0);
  });
});

describe('Activity History page schema and transport', () => {
  it.each([
    { text: 'x', emoji: '' },
    { text: '😀'.repeat(140), emoji: '😀'.repeat(32) },
  ])('accepts Custom Status Unicode boundaries %#', async ({ text, emoji }) => {
    server.use(
      http.get(HISTORY_ENDPOINT, () => HttpResponse.json(page([supportedItem({ text, emoji })])))
    );

    const result = await getPresenceHistoryPage();

    expect(result.items[0]).toMatchObject({
      status: 'supported',
      category: 'custom_text',
      payloadVersion: 1,
      payload: { text, emoji },
    });
  });

  it('accepts a supported payload with an omitted emoji', async () => {
    server.use(
      http.get(HISTORY_ENDPOINT, () =>
        HttpResponse.json(page([supportedItem({ text: 'No emoji' })]))
      )
    );

    await expect(getPresenceHistoryPage()).resolves.toMatchObject({
      items: [{ status: 'supported', payload: { text: 'No emoji' } }],
    });
  });

  it.each([
    { name: 'empty text', payload: { text: '', emoji: '' } },
    { name: 'over-140 text', payload: { text: '😀'.repeat(141), emoji: '' } },
    { name: 'over-32 emoji', payload: { text: 'valid', emoji: '😀'.repeat(33) } },
  ])('rejects $name', async ({ payload: invalidPayload }) => {
    server.use(
      http.get(HISTORY_ENDPOINT, () => HttpResponse.json(page([supportedItem(invalidPayload)])))
    );

    await expect(getPresenceHistoryPage()).rejects.toThrow('Invalid Activity History response');
  });

  it('returns unsupported records with metadata and a null payload only', async () => {
    server.use(http.get(HISTORY_ENDPOINT, () => HttpResponse.json(page([unsupportedItem()]))));

    await expect(getPresenceHistoryPage()).resolves.toEqual({
      items: [
        {
          status: 'unsupported',
          id: HISTORY_ID,
          category: 'games',
          payloadVersion: 2,
          payload: null,
          startedAt: STARTED_AT,
          endedAt: null,
          recordedAt: STARTED_AT,
          expiresAt: EXPIRES_AT,
        },
      ],
      nextCursor: null,
    });
  });

  it.each([
    {
      name: 'unsupported record carrying raw payload',
      item: () => unsupportedItem({ payload: { text: 'private raw payload' } }),
    },
    {
      name: 'unsupported record carrying a raw_payload field',
      item: () => unsupportedItem({ raw_payload: { text: 'private raw payload' } }),
    },
    {
      name: 'supported payload with an unknown field',
      item: () => supportedItem({ text: 'valid', private_field: 'private raw payload' }),
    },
    {
      name: 'supported non-custom category',
      item: () => supportedItem({ text: 'valid' }, { category: 'games' }),
    },
    {
      name: 'supported future payload version',
      item: () => supportedItem({ text: 'valid' }, { payload_version: 2 }),
    },
  ])('rejects $name', async ({ item }) => {
    server.use(http.get(HISTORY_ENDPOINT, () => HttpResponse.json(page([item()]))));

    await expect(getPresenceHistoryPage()).rejects.toThrow('Invalid Activity History response');
  });

  it.each([
    { name: 'malformed UUID', overrides: { id: 'not-a-uuid' } },
    { name: 'unknown category', overrides: { category: 'private_payload' } },
    { name: 'zero payload version', overrides: { payload_version: 0 } },
    { name: 'malformed started timestamp', overrides: { started_at: 'yesterday' } },
    { name: 'local timestamp without offset', overrides: { recorded_at: '2026-07-12T14:00:00' } },
    { name: 'unknown page field', overrides: null },
  ])('rejects $name', async ({ name, overrides }) => {
    const body =
      name === 'unknown page field'
        ? { ...page(), private_page_data: true }
        : page([unsupportedItem(overrides ?? {})]);
    server.use(http.get(HISTORY_ENDPOINT, () => HttpResponse.json(body)));

    await expect(getPresenceHistoryPage()).rejects.toThrow('Invalid Activity History response');
  });

  it('uses URLSearchParams and treats request/response cursors as opaque strings', async () => {
    const opaqueCursor = 'opaque.cursor/value?keep=yes&still=opaque';
    let requestedUrl = '';
    server.use(
      http.get(HISTORY_ENDPOINT, ({ request }) => {
        requestedUrl = request.url;
        return HttpResponse.json({ items: [], next_cursor: opaqueCursor });
      })
    );

    const result = await getPresenceHistoryPage({ limit: 50, before: opaqueCursor });

    const url = new URL(requestedUrl);
    expect(url.pathname).toBe('/api/v1/users/me/presence-history');
    expect(url.searchParams.get('limit')).toBe('50');
    expect(url.searchParams.get('before')).toBe(opaqueCursor);
    expect(result.nextCursor).toBe(opaqueCursor);
  });

  it.each([0, 1.5, 101])('rejects invalid limit %s before transport', async (limit) => {
    let calls = 0;
    server.use(
      http.get(HISTORY_ENDPOINT, () => {
        calls += 1;
        return HttpResponse.json(page());
      })
    );

    await expect(getPresenceHistoryPage({ limit })).rejects.toThrow(
      'Invalid Activity History page options'
    );
    expect(calls).toBe(0);
  });
});

describe('Activity History request failures', () => {
  it('retains only stable status, code, and parsed Retry-After metadata', async () => {
    server.use(
      http.get(SETTINGS_ENDPOINT, () =>
        HttpResponse.json(
          {
            code: 'activity_history_consent_mismatch',
            required_consent: validDisclosure({
              required_text: 'sentinel-private-disclosure-text',
            }),
          },
          { status: 409, headers: { 'Retry-After': '17' } }
        )
      )
    );

    const error = await getPresenceHistorySettings().catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(PresenceHistoryRequestError);
    expect(error).toMatchObject({
      status: 409,
      code: 'activity_history_consent_mismatch',
      retryAfter: 17,
    });
    expect(String(error)).not.toContain('sentinel');
    expect(JSON.stringify(error)).not.toContain('sentinel');
    expect(Object.keys(error as object).sort()).toEqual(['code', 'retryAfter', 'status'].sort());
  });

  it('uses stable fallbacks for malformed error bodies and Retry-After values', async () => {
    server.use(
      http.get(SETTINGS_ENDPOINT, () =>
        HttpResponse.json(
          { error: 'sentinel-private-server-detail' },
          { status: 500, headers: { 'Retry-After': 'tomorrow' } }
        )
      )
    );

    const error = await getPresenceHistorySettings().catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(PresenceHistoryRequestError);
    expect(error).toMatchObject({
      status: 500,
      code: 'activity_history_request_failed',
      retryAfter: null,
    });
    expect(String(error)).not.toContain('sentinel');
  });

  it('does not log malformed history payloads or server error details', async () => {
    const log = vi.spyOn(console, 'log').mockImplementation(() => {});
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const error = vi.spyOn(console, 'error').mockImplementation(() => {});
    server.use(
      http.get(HISTORY_ENDPOINT, () =>
        HttpResponse.json(
          page([
            supportedItem({
              text: '',
              private_detail: 'sentinel-private-history-payload',
            }),
          ])
        )
      )
    );

    const caught = await getPresenceHistoryPage().catch((reason: unknown) => reason);

    expect(String(caught)).not.toContain('sentinel');
    expect(log).not.toHaveBeenCalled();
    expect(warn).not.toHaveBeenCalled();
    expect(error).not.toHaveBeenCalled();
  });
});

describe('Activity History abort and delete transport', () => {
  it.each([
    ['settings GET', (signal: AbortSignal) => getPresenceHistorySettings(signal)],
    [
      'settings PATCH',
      (signal: AbortSignal) => patchPresenceHistorySettings({ kind: 'disable' }, signal),
    ],
    ['history GET', (signal: AbortSignal) => getPresenceHistoryPage({ signal })],
    ['history DELETE', (signal: AbortSignal) => deletePresenceHistory(signal)],
  ] as const)('aborts %s before starting transport', async (_name, operation) => {
    let calls = 0;
    server.use(
      http.all(`${HISTORY_ENDPOINT}/*`, () => {
        calls += 1;
        return HttpResponse.json(validSettings());
      }),
      http.all(HISTORY_ENDPOINT, () => {
        calls += 1;
        return HttpResponse.json(page());
      })
    );
    const controller = new AbortController();
    controller.abort();

    await expect(operation(controller.signal)).rejects.toMatchObject({ name: 'AbortError' });
    expect(calls).toBe(0);
  });

  it('re-checks abort after JSON parsing before returning a continuation', async () => {
    serveSettings(validSettings());
    const controller = new AbortController();
    const originalJson = Response.prototype.json;
    vi.spyOn(Response.prototype, 'json').mockImplementationOnce(async function (this: Response) {
      const value: unknown = await originalJson.call(this);
      controller.abort();
      return value;
    });

    await expect(getPresenceHistorySettings(controller.signal)).rejects.toMatchObject({
      name: 'AbortError',
    });
  });

  it('resolves only an exact 204 deletion response', async () => {
    let method = '';
    server.use(
      http.delete(HISTORY_ENDPOINT, ({ request }) => {
        method = request.method;
        return new HttpResponse(null, { status: 204 });
      })
    );

    await expect(deletePresenceHistory()).resolves.toBeUndefined();
    expect(method).toBe('DELETE');
  });

  it('rejects a non-204 successful deletion response without parsing its body', async () => {
    server.use(
      http.delete(HISTORY_ENDPOINT, () =>
        HttpResponse.json({ private_payload: 'sentinel-private-delete-response' })
      )
    );

    await expect(deletePresenceHistory()).rejects.toThrow('Invalid Activity History response');
  });
});
