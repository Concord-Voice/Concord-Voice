import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import {
  hasMalformedExpirationPolicyListRow,
  mergeExpirationPolicy,
  parseExpirationPolicyFromListRow,
  parseExpirationPolicyResponse,
  updateExpirationPolicy,
  type ExpirationPolicy,
} from '@/renderer/services/messaging/expirationPolicyApi';

const API_BASE = 'http://localhost:8080';
const policyWire = {
  window_seconds: 86400,
  updated_at: '2026-09-08T05:00:00Z',
  revision: 4,
  backfill_pending: false,
};

beforeAll(() => server.listen({ onUnhandledFrame: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => resetAllStores());

describe('expiration policy wire contract', () => {
  it.each([3600, 86400, 604800, 2592000, null])(
    'accepts supported window %s and null metadata',
    (windowSeconds) => {
      expect(
        parseExpirationPolicyResponse({
          ...policyWire,
          window_seconds: windowSeconds,
          updated_at: null,
        })
      ).toEqual({ windowSeconds, updatedAt: null, revision: 4, backfillPending: false });
    }
  );
  it.each([
    ['response', parseExpirationPolicyResponse, policyWire],
    [
      'list row',
      parseExpirationPolicyFromListRow,
      {
        expiration_window_seconds: 86400,
        expiration_updated_at: policyWire.updated_at,
        expiration_revision: 4,
        expiration_backfill_pending: false,
      },
    ],
  ])('maps a valid %s and strips unknown fields', (_name, parse, raw) => {
    expect(parse({ ...raw, future_metadata: true })).toEqual({
      windowSeconds: 86400,
      updatedAt: policyWire.updated_at,
      revision: 4,
      backfillPending: false,
    });
  });

  it.each([
    { ...policyWire, revision: -1 },
    { ...policyWire, revision: 1.5 },
    { ...policyWire, window_seconds: 300 },
    { ...policyWire, updated_at: 'yesterday' },
    { ...policyWire, backfill_pending: 'false' },
    { ...policyWire, window_seconds: undefined },
  ])('rejects malformed response policy %j', (raw) => {
    expect(parseExpirationPolicyResponse(raw)).toBeUndefined();
  });

  it('orders whole policies by revision, then completed over pending at equality', () => {
    const pending = {
      windowSeconds: 86400 as const,
      updatedAt: policyWire.updated_at,
      revision: 4,
      backfillPending: true,
    };
    const completed = { ...pending, backfillPending: false };
    expect(mergeExpirationPolicy(completed, pending)).toEqual(completed);
    expect(mergeExpirationPolicy(pending, completed)).toEqual(completed);
    expect(mergeExpirationPolicy(completed, { ...completed, revision: 3 })).toEqual(completed);
    expect(mergeExpirationPolicy(completed, { ...completed, revision: 5 })).toEqual({
      ...completed,
      revision: 5,
    });
  });
});

// ── #3459: pin-aware expiry ────────────────────────────────────────────────
describe('include_pinned on the policy wire contract (#3459)', () => {
  const listRow = {
    expiration_window_seconds: 86400,
    expiration_updated_at: policyWire.updated_at,
    expiration_revision: 4,
    expiration_backfill_pending: false,
  };

  it.each([true, false])('maps include_pinned %s on a response and on a list row', (value) => {
    expect(parseExpirationPolicyResponse({ ...policyWire, include_pinned: value })).toEqual(
      expect.objectContaining({ includePinned: value })
    );
    expect(
      parseExpirationPolicyFromListRow({ ...listRow, expiration_include_pinned: value })
    ).toEqual(expect.objectContaining({ includePinned: value }));
  });

  it('keeps an ABSENT field absent: "the server did not say" is one shape, not includePinned: undefined', () => {
    // toStrictEqual distinguishes a missing key from an explicit undefined, which toEqual would not.
    expect(parseExpirationPolicyResponse(policyWire)).toStrictEqual({
      windowSeconds: 86400,
      updatedAt: policyWire.updated_at,
      revision: 4,
      backfillPending: false,
    });
    expect(parseExpirationPolicyFromListRow(listRow)).toStrictEqual({
      windowSeconds: 86400,
      updatedAt: policyWire.updated_at,
      revision: 4,
      backfillPending: false,
    });
  });

  it.each([null, 'true', 1, {}])(
    'rejects a non-boolean include_pinned %j on a response, a list row, and as a malformed row',
    (value) => {
      expect(
        parseExpirationPolicyResponse({ ...policyWire, include_pinned: value })
      ).toBeUndefined();
      expect(
        parseExpirationPolicyFromListRow({ ...listRow, expiration_include_pinned: value })
      ).toBeUndefined();
      expect(
        hasMalformedExpirationPolicyListRow({ ...listRow, expiration_include_pinned: value })
      ).toBe(true);
    }
  );

  it('treats a row carrying ONLY a malformed include field as malformed, and a valid one as not', () => {
    // The field must be in the "has a policy field" set, or a row whose only policy field is a bad
    // include would read as "no policy on this row" instead of a failure.
    expect(hasMalformedExpirationPolicyListRow({ expiration_include_pinned: 'yes' })).toBe(true);
    expect(
      hasMalformedExpirationPolicyListRow({ ...listRow, expiration_include_pinned: false })
    ).toBe(false);
    expect(hasMalformedExpirationPolicyListRow({ name: 'no policy fields at all' })).toBe(false);
  });
});

describe('mergeExpirationPolicy at an equal revision (#3459, X14)', () => {
  const base: ExpirationPolicy = {
    windowSeconds: 86400,
    updatedAt: policyWire.updated_at,
    revision: 4,
    backfillPending: false,
  };
  const kept = { ...base, includePinned: false };
  const expiring = { ...base, includePinned: true };

  it('DROPS the pin claim when the incoming read cannot repeat it', () => {
    // After a rollback an older server answers at the SAME revision without the field. A held
    // "except pinned messages" would then outlive the binary that deletes newly pinned rows.
    expect(mergeExpirationPolicy(kept, base)).toStrictEqual(base);
    expect(mergeExpirationPolicy(expiring, base)).toStrictEqual(base);
  });

  it('drops ONLY the claim: a stale pending read does not undo a held completion', () => {
    // An older server's delayed read at the same revision can still report the backfill pending.
    // The claim goes; the completion held already stands.
    const stalePending = { ...base, backfillPending: true };
    expect(mergeExpirationPolicy(kept, stalePending)).toStrictEqual(base);
    expect(mergeExpirationPolicy(expiring, stalePending)).toStrictEqual(base);
  });

  it('never ADDS the claim at an equal revision', () => {
    expect(mergeExpirationPolicy(base, kept)).toStrictEqual(base);
    expect(mergeExpirationPolicy(base, expiring)).toStrictEqual(base);
  });

  it('keeps a held claim against an incoming one at the same revision (no churn)', () => {
    expect(mergeExpirationPolicy(kept, expiring)).toStrictEqual(kept);
    expect(mergeExpirationPolicy(kept, { ...kept })).toStrictEqual(kept);
  });

  it('lets a completed backfill supersede a pending one that carried the claim', () => {
    const pendingKept = { ...kept, backfillPending: true };
    expect(mergeExpirationPolicy(pendingKept, kept)).toStrictEqual(kept);
    expect(mergeExpirationPolicy(pendingKept, expiring)).toStrictEqual(expiring);
    expect(mergeExpirationPolicy(pendingKept, base)).toStrictEqual(base);
  });

  it('takes a completion over a pending read that had no claim, without ADDING one', () => {
    // The pending policy came from a server that predates #3459 (a rollback, or an out-of-order
    // read). Taking the completed form whole would bring "except pinned messages" back at the same
    // revision while that older server may still be the one deleting pins.
    const pending = { ...base, backfillPending: true };
    expect(mergeExpirationPolicy(pending, kept)).toStrictEqual(base);
    expect(mergeExpirationPolicy(pending, expiring)).toStrictEqual(base);
    expect(mergeExpirationPolicy(pending, base)).toStrictEqual(base);
  });

  it('leaves the revision ordering alone: a newer revision wins with or without the field', () => {
    expect(mergeExpirationPolicy(kept, { ...base, revision: 5 })).toStrictEqual({
      ...base,
      revision: 5,
    });
    expect(mergeExpirationPolicy(base, { ...expiring, revision: 5 })).toStrictEqual({
      ...expiring,
      revision: 5,
    });
    expect(mergeExpirationPolicy(kept, { ...base, revision: 3 })).toStrictEqual(kept);
  });
});

describe('updateExpirationPolicy include_pinned (#3459)', () => {
  const channelPath = `${API_BASE}/api/v1/channels/channel-1/expiration`;

  async function sendAndCapture(request: Parameters<typeof updateExpirationPolicy>[1]) {
    let body: unknown;
    server.use(
      http.patch(channelPath, async ({ request: req }) => {
        body = await req.json();
        return HttpResponse.json({ ...policyWire, include_pinned: false, revision: 5 });
      })
    );
    const result = await updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request);
    return { body, result };
  }

  it.each([true, false])(
    'puts include_pinned %s on the wire when the caller sent it',
    async (v) => {
      const { body, result } = await sendAndCapture({
        mode: 'set',
        window_seconds: 86400,
        retroactive: 'new_only',
        include_pinned: v,
      });
      expect(body).toEqual({
        mode: 'set',
        window_seconds: 86400,
        retroactive: 'new_only',
        include_pinned: v,
      });
      // And the answer's claim reaches the caller, so the OUTCOME of the field is observable.
      expect(result).toMatchObject({ kind: 'ok', policy: { includePinned: false, revision: 5 } });
    }
  );

  it('puts NO include_pinned key on the wire when the caller omitted it (old-server skew)', async () => {
    const { body } = await sendAndCapture({
      mode: 'set',
      window_seconds: 86400,
      retroactive: 'new_only',
    });
    expect(Object.keys(body as object)).not.toContain('include_pinned');
  });

  it.each([
    ['null', { mode: 'set', window_seconds: 86400, retroactive: 'new_only', include_pinned: null }],
    [
      'a non-boolean',
      { mode: 'set', window_seconds: 86400, retroactive: 'new_only', include_pinned: 'true' },
    ],
    ['on clear', { mode: 'clear', retroactive: 'leave_pending', include_pinned: true }],
    ['on resume', { mode: 'resume', revision: 4, include_pinned: true }],
  ])('refuses include_pinned %s before dispatch, sending nothing', async (_name, request) => {
    let called = false;
    server.use(
      http.patch(channelPath, () => {
        called = true;
        return HttpResponse.json(policyWire);
      })
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request as never)
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
    expect(called).toBe(false);
  });

  it('sends include_pinned on the DM route too', async () => {
    let body: unknown;
    server.use(
      http.patch(`${API_BASE}/api/v1/dm/conversations/conv-1/expiration`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(policyWire);
      })
    );
    await updateExpirationPolicy(
      { kind: 'dm', id: 'conv-1' },
      { mode: 'set', window_seconds: 3600, retroactive: 'apply', include_pinned: true }
    );
    expect(body).toMatchObject({ include_pinned: true });
  });
});

describe('updateExpirationPolicy', () => {
  it.each([
    [{ mode: 'set', window_seconds: 3600, retroactive: 'apply' }, 3600],
    [{ mode: 'set', window_seconds: 2592000, retroactive: 'new_only' }, 2592000],
    [{ mode: 'clear', retroactive: 'clear_pending' }, null],
    [{ mode: 'clear', retroactive: 'leave_pending' }, null],
    [{ mode: 'resume', revision: 8 }, 86400],
  ])('sends the exact closed request body %j', async (request, windowSeconds) => {
    let body: unknown;
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, async ({ request: req }) => {
        body = await req.json();
        return HttpResponse.json({ ...policyWire, window_seconds: windowSeconds, revision: 8 });
      })
    );

    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request)
    ).resolves.toMatchObject({ kind: 'ok' });
    expect(body).toEqual(request);
  });

  it.each([
    { mode: 'set', window_seconds: 604800, retroactive: 'apply' },
    { mode: 'set', window_seconds: 86400, retroactive: 'new_only' },
  ])('accepts the remaining supported set window %j', async (request) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json(policyWire)
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request)
    ).resolves.toMatchObject({ kind: 'ok' });
  });

  it.each([
    { mode: 'set', window_seconds: 0, retroactive: 'apply' },
    { mode: 'set', window_seconds: 3600, retroactive: 'other' },
    { mode: 'clear', retroactive: 'other' },
    { mode: 'resume', revision: -1 },
    { mode: 'resume', revision: Number.MAX_SAFE_INTEGER + 1 },
  ])('rejects missing or invalid request values %j', async (request) => {
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request as never)
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
  });

  it('rejects an extra request property before dispatch', async () => {
    let called = false;
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () => {
        called = true;
        return HttpResponse.json(policyWire);
      })
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, {
        mode: 'clear',
        retroactive: 'clear_pending',
        future: true,
      } as never)
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
    expect(called).toBe(false);
  });

  it.each([
    [400, { kind: 'rejected', reason: 'invalidRequest' }],
    [401, { kind: 'rejected', reason: 'sessionExpired' }],
    [403, { kind: 'rejected', reason: 'forbidden' }],
    [404, { kind: 'rejected', reason: 'notFound' }],
    [429, { kind: 'rejected', reason: 'rateLimited', retryAfterSeconds: 12 }],
    [500, { kind: 'rejected', reason: 'unavailable' }],
  ])('maps HTTP %i to its semantic result', async (status, expected) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/dm/conversations/conv-1/expiration`, () =>
        HttpResponse.json(
          {},
          {
            status,
            headers: status === 429 ? { 'Retry-After': '12' } : undefined,
          }
        )
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'dm', id: 'conv-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual(expected);
  });

  it('maps a valid 409 policy to conflict', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/dm/conversations/conv-1/expiration`, () =>
        HttpResponse.json({ ...policyWire, revision: 5 }, { status: 409 })
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'dm', id: 'conv-1' }, { mode: 'resume', revision: 4 })
    ).resolves.toEqual({
      kind: 'conflict',
      policy: {
        windowSeconds: 86400,
        updatedAt: policyWire.updated_at,
        revision: 5,
        backfillPending: false,
      },
    });
  });

  it('maps 413 to invalidRequest', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({}, { status: 413 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
  });

  it('maps an unknown status to ambiguous even with a valid-looking body', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json(policyWire, { status: 418 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it.each([
    [{ ...policyWire, revision: -1 }],
    {
      revision: 4,
      window_seconds: 300,
      updated_at: policyWire.updated_at,
      backfill_pending: false,
    },
  ])('maps malformed 409 policy to conflict without a candidate', async (body) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/dm/conversations/conv-1/expiration`, () =>
        HttpResponse.json(body, { status: 409 })
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'dm', id: 'conv-1' }, { mode: 'resume', revision: 4 })
    ).resolves.toEqual({ kind: 'conflict' });
  });

  it('keeps a valid 503 candidate partial and never treats it as current success', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({ ...policyWire, backfill_pending: true }, { status: 503 })
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, { mode: 'resume', revision: 4 })
    ).resolves.toEqual({
      kind: 'partial',
      candidate: {
        windowSeconds: 86400,
        updatedAt: policyWire.updated_at,
        revision: 4,
        backfillPending: true,
      },
    });
  });

  // Mutant: the 503 arm of `mutationResult` returns `{ kind: 'partial' }` for a body that is not a
  // policy (the pre-#3456 mapping), which blocks the editor on an ordinary outage.
  it.each([undefined, { ...policyWire, revision: -1 }])(
    'maps a 503 whose body is not a policy to a retryable failure, not a partial commit (%j)',
    async (body) => {
      server.use(
        http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
          body === undefined
            ? HttpResponse.json({}, { status: 503 })
            : HttpResponse.json(body, { status: 503 })
        )
      );
      await expect(
        updateExpirationPolicy(
          { kind: 'channel', id: 'channel-1' },
          { mode: 'resume', revision: 4 }
        )
      ).resolves.toEqual({ kind: 'rejected', reason: 'unavailable' });
    }
  );

  // Mutant: the body read once and not tolerated when it is not JSON (a proxy's HTML 503).
  it('maps a 503 with an unreadable body to the same retryable failure', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.text('<html>bad gateway</html>', { status: 503 })
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, { mode: 'resume', revision: 4 })
    ).resolves.toEqual({ kind: 'rejected', reason: 'unavailable' });
  });

  it('maps an unreadable success body to ambiguous', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.text('<html>proxy</html>', { status: 200 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it('maps a readable but incomplete success body to ambiguous', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({ revision: 4 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it('maps a network failure to ambiguous', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () => HttpResponse.error())
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it.each(['-1', '1.5', '12x'])('does not expose invalid Retry-After %s', async (retryAfter) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({}, { status: 429, headers: { 'Retry-After': retryAfter } })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'rejected', reason: 'rateLimited' });
  });
});

// The dangerous-action gate on a channel's policy route (#3456 §3.4, F11): the gate's answer is
// read before the status decides what the body is, so a 503 means three different things.
describe('updateExpirationPolicy on a channel whose server enforces MFA (#3456)', () => {
  const CHANNEL = { kind: 'channel', id: 'channel-1' } as const;
  const DM = { kind: 'dm', id: 'conv-1' } as const;
  const SHORTEN = { mode: 'set', window_seconds: 3600, retroactive: 'apply' } as const;
  const channelUrl = `${API_BASE}/api/v1/channels/channel-1/expiration`;
  const dmUrl = `${API_BASE}/api/v1/dm/conversations/conv-1/expiration`;

  const MFA_REQUIRED = { error: 'MFA verification required', mfa_required: true };
  const ENROLMENT = { error: 'MFA enrollment required', mfa_enrollment_required: true };
  const BUDGET_EXHAUSTED = { error: 'Too many attempts', step_up_budget_exhausted: true };
  const BUDGET_UNAVAILABLE = { error: 'Busy', step_up_budget_unavailable: true };
  const LOCK_CONFLICT = { error: 'Try again', lock_conflict: true };

  const answerWith = (url: string, status: number, body: unknown) =>
    server.use(http.patch(url, () => HttpResponse.json(body as never, { status })));

  // Mutant: `gated` dropped (the 403 falls to `forbidden`) or `status`/`body` not carried.
  it.each([
    ['a plain mfa_required 403', 403, MFA_REQUIRED],
    ['an mfa_enrollment_required 403', 403, ENROLMENT],
    ['a budget-exhausted 429', 429, BUDGET_EXHAUSTED],
    ['a budget-unavailable 503', 503, BUDGET_UNAVAILABLE],
    ['a lock-conflict 503', 503, LOCK_CONFLICT],
  ])(
    'hands %s to the host as gated, with the response as received',
    async (_name, status, body) => {
      answerWith(channelUrl, status, body);
      await expect(updateExpirationPolicy(CHANNEL, SHORTEN)).resolves.toEqual({
        kind: 'gated',
        status,
        body,
      });
    }
  );

  // Mutant: the gate check placed after the 503 policy branch, so a flagged 503 that also parses as
  // a policy becomes `partial` and the dialog never opens.
  it('reads the gate flag before the body: a flagged 503 that also parses as a policy is gated', async () => {
    const body = { ...policyWire, lock_conflict: true };
    answerWith(channelUrl, 503, body);
    await expect(updateExpirationPolicy(CHANNEL, SHORTEN)).resolves.toEqual({
      kind: 'gated',
      status: 503,
      body,
    });
  });

  // Mutant: the 503 policy branch removed (every 503 retryable), or the policy parse skipped.
  it('keeps a 503 whose body parses as a policy as the partial commit it is', async () => {
    answerWith(channelUrl, 503, { ...policyWire, window_seconds: 3600, backfill_pending: true });
    await expect(updateExpirationPolicy(CHANNEL, SHORTEN)).resolves.toEqual({
      kind: 'partial',
      candidate: {
        windowSeconds: 3600,
        updatedAt: policyWire.updated_at,
        revision: 4,
        backfillPending: true,
      },
    });
  });

  // Mutant: the adapter's `null` for an unflagged 429 widened, so an ordinary rate limit opens the dialog.
  it('leaves an unflagged 429 and an unflagged 500 to their ordinary results', async () => {
    answerWith(channelUrl, 429, { error: 'Rate limit exceeded' });
    await expect(updateExpirationPolicy(CHANNEL, SHORTEN)).resolves.toEqual({
      kind: 'rejected',
      reason: 'rateLimited',
    });
    answerWith(channelUrl, 500, { error: 'Database down' });
    await expect(updateExpirationPolicy(CHANNEL, SHORTEN)).resolves.toEqual({
      kind: 'rejected',
      reason: 'unavailable',
    });
  });

  // Mutant: `status !== 401` removed from `mutationResult`, so a dead session reads as the gate's.
  it('keeps a 401 as the session, whatever its body says', async () => {
    answerWith(channelUrl, 401, MFA_REQUIRED);
    await expect(updateExpirationPolicy(CHANNEL, SHORTEN)).resolves.toEqual({
      kind: 'rejected',
      reason: 'sessionExpired',
    });
  });

  // Mutant: `scope.kind === 'channel'` dropped, so a direct-message conversation's flagged answers are gated.
  it.each([
    ['an mfa_required 403', 403, MFA_REQUIRED, { kind: 'rejected', reason: 'forbidden' }],
    ['an enrolment 403', 403, ENROLMENT, { kind: 'rejected', reason: 'forbidden' }],
    ['a lock-conflict 503', 503, LOCK_CONFLICT, { kind: 'rejected', reason: 'unavailable' }],
  ])('never gates a direct-message conversation: %s', async (_name, status, body, expected) => {
    answerWith(dmUrl, status, body);
    await expect(updateExpirationPolicy(DM, SHORTEN)).resolves.toEqual(expected);
  });

  // Mutant: `mfa_code` dropped from the closed request schema, or its length bounds loosened.
  it('sends mfa_code in the PATCH body on the re-send and nothing else new', async () => {
    let body: unknown;
    server.use(
      http.patch(channelUrl, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(policyWire);
      })
    );
    await updateExpirationPolicy(CHANNEL, { ...SHORTEN, mfa_code: '314159' });
    expect(body).toEqual({ ...SHORTEN, mfa_code: '314159' });
  });

  it.each(['', 'x'.repeat(257)])(
    'refuses an unusable mfa_code before dispatch (length %#)',
    async (code) => {
      let called = false;
      server.use(
        http.patch(channelUrl, () => {
          called = true;
          return HttpResponse.json(policyWire);
        })
      );
      await expect(
        updateExpirationPolicy(CHANNEL, { ...SHORTEN, mfa_code: code })
      ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
      expect(called).toBe(false);
    }
  );

  // Mutant: `mfa_code` accepted on the clear and resume modes (the schema is a strictObject per mode).
  it('does not admit mfa_code on a mode that cannot be gated', async () => {
    await expect(
      updateExpirationPolicy(CHANNEL, {
        mode: 'clear',
        retroactive: 'leave_pending',
        mfa_code: '314159',
      } as never)
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
  });
});

// The step-up re-send is admitted against the capture its factor was proven under (C82): a code
// proven for one server is never sent to another.
describe('updateExpirationPolicy against a captured request context (#3456)', () => {
  const CHANNEL = { kind: 'channel', id: 'channel-1' } as const;
  const SHORTEN = {
    mode: 'set',
    window_seconds: 3600,
    retroactive: 'apply',
    mfa_code: '314159',
  } as const;
  const channelUrl = `${API_BASE}/api/v1/channels/channel-1/expiration`;

  afterEach(() => resetRuntimeServerBase());

  function recordPatches(): string[] {
    const sent: string[] = [];
    server.use(
      http.patch(channelUrl, async ({ request }) => {
        sent.push(JSON.stringify(await request.json()));
        return HttpResponse.json(policyWire);
      })
    );
    return sent;
  }

  // Mutant: the context is dropped (bare apiFetch), so the code goes out as whoever is current.
  it('sends nothing, and says aborted, once the server moved since the capture', async () => {
    const sent = recordPatches();
    const context = captureApiRequestContext();
    setRuntimeServerBase('https://other-server.example.test');

    await expect(updateExpirationPolicy(CHANNEL, SHORTEN, context)).resolves.toEqual({
      kind: 'aborted',
    });
    expect(sent).toEqual([]);
  });

  // Mutant: `aborted` reported as ambiguous, which tells the user to refresh a change that never left.
  it('reads an unsent request as aborted, not as the ambiguity a lost connection is', async () => {
    recordPatches();
    const context = captureApiRequestContext();
    setRuntimeServerBase('https://other-server.example.test');

    const result = await updateExpirationPolicy(CHANNEL, SHORTEN, context);

    expect(result.kind).not.toBe('ambiguous');
  });

  it('sends under a capture that is still current', async () => {
    const sent = recordPatches();

    await expect(
      updateExpirationPolicy(CHANNEL, SHORTEN, captureApiRequestContext())
    ).resolves.toMatchObject({ kind: 'ok' });
    expect(sent).toEqual([JSON.stringify(SHORTEN)]);
  });
});
