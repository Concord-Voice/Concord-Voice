import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { signInRefreshableSession } from '../../../helpers/refreshReplay';
import { moderateMember, purgeNotice } from '@/renderer/components/Members/purgeOnModeration';
import { PIN_CLAIM_UNCONFIRMED_MESSAGE } from '@/renderer/services/messaging/purgeApi';
import { clientConfigService } from '@/renderer/services/system/clientConfigService';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import type { ServerMember } from '@/renderer/stores/chat/memberStore';

/**
 * The notice string alone cannot distinguish "no purge was requested" from
 * "a purge happened that this client cannot describe" — both are empty. These
 * lock the `unknownStatus` discriminator that keeps the two apart (#1354).
 */
describe('purgeNotice (#1354)', () => {
  it.each([
    ['completed', 'Alice was banned and their messages were purged.'],
    [
      'skipped_unauthorized',
      'Alice was banned. Their messages were not purged — you do not have permission to purge messages in this server.',
    ],
    [
      'skipped_rate_limited',
      'Alice was banned. Their messages were not purged — the purge limit was not available just now. You can purge them from a channel later.',
    ],
    [
      'failed',
      'Alice was banned. Their messages could not be purged. You can try again from a channel.',
    ],
  ])('describes %s as a known outcome', (status, expected) => {
    expect(purgeNotice('Alice', 'banned', status, 'unsupported')).toEqual({
      notice: expected,
      unknownStatus: false,
    });
  });

  it('substitutes the kick verb without changing the shape', () => {
    expect(purgeNotice('Alice', 'kicked', 'completed', 'unsupported')).toEqual({
      notice: 'Alice was kicked and their messages were purged.',
      unknownStatus: false,
    });
  });

  it('flags an unrecognized status rather than silently returning an empty notice', () => {
    expect(purgeNotice('Alice', 'banned', 'something_new', 'unsupported')).toEqual({
      notice: '',
      unknownStatus: true,
    });
  });

  it('never names the rate-limit budget', () => {
    expect(
      purgeNotice('Alice', 'banned', 'skipped_rate_limited', 'unsupported').notice
    ).not.toMatch(/\d/);
  });
});

describe('purgeNotice pin claim (#3458)', () => {
  it('a completed purge that kept pins says so', () => {
    expect(purgeNotice('Alice', 'banned', 'completed', 'keep').notice).toBe(
      'Alice was banned and their messages were purged. Pinned messages were kept.'
    );
  });

  it.each(['include', 'unsupported'] as const)('%s makes no pin claim', (mode) => {
    expect(purgeNotice('Alice', 'kicked', 'completed', mode).notice).toBe(
      'Alice was kicked and their messages were purged.'
    );
  });

  it('only a completed purge mentions pins', () => {
    expect(purgeNotice('Alice', 'banned', 'failed', 'keep').notice).not.toMatch(/pinned/i);
  });
});

// Codex P1s on #3552: the pin recheck is a request of its own, so the ban it
// gates stays bound to the confirming account, and a refresh does not resend it.
describe('moderateMember dispatch (#3552 review)', () => {
  const SERVER = '22222222-2222-4222-8222-222222222222';
  const target: ServerMember = {
    user_id: '33333333-3333-4333-8333-333333333333',
    username: 'alice',
    role: 'member',
    joined_at: '2026-01-01T00:00:00Z',
    roles: [],
  };
  const keepsPinned = () => ({
    auth: { oauthProviders: [] },
    features: { purgeKeepsPinned: true },
  });
  const recheckAnswers = (during?: () => void) =>
    vi.spyOn(clientConfigService, 'refreshServerCapabilities').mockImplementation(async () => {
      during?.();
      useClientConfigStore.setState({ serverCapabilities: keepsPinned() });
    });
  /** Counts the bans; the first `refusals` are answered 401, the rest succeed. */
  const bans = (refusals = 0) => {
    let hits = 0;
    server.use(
      http.post('*/api/v1/servers/:sid/bans/:uid', () => {
        hits += 1;
        return hits <= refusals
          ? new HttpResponse(null, { status: 401 })
          : HttpResponse.json({ purge: { status: 'completed' } });
      })
    );
    return () => hits;
  };

  beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
  afterEach(() => {
    server.resetHandlers();
    vi.restoreAllMocks();
  });
  afterAll(() => server.close());
  beforeEach(() => {
    resetAllStores();
    useClientConfigStore.setState({ serverCapabilities: keepsPinned() });
  });

  // Mutant: no capture before the recheck, so the ban goes out as the account
  // that signed in meanwhile.
  it.each([
    ['the account is replaced during the recheck', true, 0],
    ['control: the account stays', false, 1],
  ])('a ban with a purge: %s', async (_name, replace, sent) => {
    recheckAnswers(
      replace
        ? () => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }))
        : undefined
    );
    const hits = bans();

    const ban = moderateMember(SERVER, target, 'ban', true, 'keep');
    if (replace) await expect(ban).rejects.toMatchObject({ name: 'AbortError' });
    else await expect(ban).resolves.toMatchObject({ unknownStatus: false });
    expect(hits()).toBe(sent);
  });

  // Mutant: no dispatch guard, so the refresh resends the purge unchecked. The
  // ban without a purge is the control: no claim, so the ordinary resend happens.
  it.each([
    ['with a purge', true, 1],
    ['control: without a purge', false, 2],
  ])('a ban answered 401 %s', async (_name, alsoPurge, sent) => {
    signInRefreshableSession();
    recheckAnswers();
    const hits = bans(1);

    const ban = moderateMember(SERVER, target, 'ban', alsoPurge, 'keep');
    if (alsoPurge) await expect(ban).rejects.toThrow(PIN_CLAIM_UNCONFIRMED_MESSAGE);
    else await expect(ban).resolves.toMatchObject({ unknownStatus: false });
    expect(hits()).toBe(sent);
  });
});
