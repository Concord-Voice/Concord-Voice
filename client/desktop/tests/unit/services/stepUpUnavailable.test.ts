import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { purgeMessages } from '@/renderer/services/messaging/purgeApi';
import { clearDMHistory } from '@/renderer/services/messaging/dmVisibilityApi';

// Codex on #3509 (P2): an account with neither a password nor inline MFA
// cannot confirm a soft-locked self-purge, and the server says how to fix
// that in a 400 flagged step_up_unavailable. The desktop showed "Something
// went wrong" instead. The flag, not the status, tells it apart from a
// malformed-body 400.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => resetAllStores());

const CHANNEL = '11111111-1111-4111-8111-111111111111';
const CONVERSATION = '44444444-4444-4444-8444-444444444444';
const NO_FACTORS =
  'Deleting messages this quickly needs verification, but this account has no password and no MFA method. Set a password or enable MFA to continue.';

function channelPurgeAnswers(status: number, body: Record<string, unknown>) {
  server.use(
    http.delete('*/api/v1/channels/:id/messages', () => HttpResponse.json(body, { status }))
  );
}

describe('step_up_unavailable refusals', () => {
  it("shows a channel self-purge's remedy from a flagged 400", async () => {
    channelPurgeAnswers(400, { error: NO_FACTORS, step_up_unavailable: true });

    await expect(
      purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '1h' })
    ).resolves.toEqual({ kind: 'softLockFailed', message: NO_FACTORS });
  });

  it('keeps an unflagged 400 generic', async () => {
    channelPurgeAnswers(400, { error: 'Invalid range' });

    await expect(
      purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '1h' })
    ).resolves.toEqual({ kind: 'unexpectedError' });
  });

  it('reads the flag, not the wording, on DM Clear', async () => {
    server.use(
      http.post('*/api/v1/dm/conversations/:id/clear', () =>
        HttpResponse.json(
          { error: 'Verification is not possible for this account.', step_up_unavailable: true },
          { status: 400 }
        )
      )
    );

    await expect(clearDMHistory(CONVERSATION)).resolves.toEqual({ kind: 'stepUpImpossible' });
  });
});
