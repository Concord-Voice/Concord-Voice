import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH, MINTED_TOKEN } from '../../helpers/stepUpTokenWire';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { purgeMessages } from '@/renderer/services/messaging/purgeApi';
import { clearDMHistory } from '@/renderer/services/messaging/dmVisibilityApi';

// Codex on #3509 (P1): a password exchange and the request that spends its
// token are two requests. If another account signs in between them, the
// second must not go out as that account: a self-scoped purge confirmed by
// account A would otherwise run as account B, whose permissions may widen it
// to every author's messages.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => resetAllStores());

const CHANNEL = '11111111-1111-4111-8111-111111111111';
const CONVERSATION = '44444444-4444-4444-8444-444444444444';

/** The mint answers, and — when `switchAccount` — another account signs in first. */
function mintThat(switchAccount: boolean) {
  server.use(
    http.post(`*${MINT_PATH}`, () => {
      if (switchAccount) {
        useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
      }
      return HttpResponse.json({ step_up_token: MINTED_TOKEN, expires_in: 60 });
    })
  );
}

describe('a step-up token is spent only by the account that minted it', () => {
  it.each([
    ['another account signed in', true, 0],
    ['control: the same account', false, 1],
  ])('channel self-purge — %s', async (_name, switchAccount, purges) => {
    mintThat(switchAccount);
    let calls = 0;
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () => {
        calls += 1;
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );

    await purgeMessages({
      context: 'channel',
      scopeId: CHANNEL,
      range: '1h',
      currentPassword: FIXTURE_PW,
      softLockPrior: { view: 'password' },
    }).catch(() => undefined);

    expect(calls).toBe(purges);
  });

  it.each([
    ['another account signed in', true, 0],
    ['control: the same account', false, 1],
  ])('DM Clear — %s', async (_name, switchAccount, clears) => {
    mintThat(switchAccount);
    let calls = 0;
    server.use(
      http.post('*/api/v1/dm/conversations/:id/clear', () => {
        calls += 1;
        return HttpResponse.json({
          conversation_id: CONVERSATION,
          cleared_at: '2026-10-01T05:00:00Z',
        });
      })
    );

    await clearDMHistory(CONVERSATION, { kind: 'password', value: FIXTURE_PW }).catch(
      () => undefined
    );

    expect(calls).toBe(clears);
  });
});
