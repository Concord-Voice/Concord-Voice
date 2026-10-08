import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW } from '../../../helpers/stepUpTokenWire';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import StepUpCredentials from '@/renderer/components/Auth/StepUpCredentials';
import { purgeMessages } from '@/renderer/services/messaging/purgeApi';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';

// #3456 (C82): a purge stage opened by a challenge works against the account
// and server the challenged purge went out as. Its re-send and its "Set up
// verification" use that capture, never one its factor hook takes when the
// stage mounts, so an account or server change between the challenge and that
// render cannot send the old purge with the new session's token and code.
//
// Both spies pass through: the real purge and the real credential fields run,
// and only what they were handed becomes observable. The same capture OBJECT
// is the claim: one the hook took for itself would be equal but not identical.

vi.mock('@/renderer/services/messaging/purgeApi', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/messaging/purgeApi')>();
  return { ...actual, purgeMessages: vi.fn(actual.purgeMessages) };
});
vi.mock('@/renderer/components/Auth/StepUpCredentials', async (importOriginal) => {
  const actual =
    await importOriginal<typeof import('@/renderer/components/Auth/StepUpCredentials')>();
  return { ...actual, default: vi.fn(actual.default) };
});
const purgeSpy = vi.mocked(purgeMessages);
const credentialsSpy = vi.mocked(StepUpCredentials);

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  purgeSpy.mockClear();
  credentialsSpy.mockClear();
  server.use(readAnswers(['totp']));
});

function readAnswers(methods: string[]) {
  return http.get('*/api/v1/mfa/step-up', () =>
    HttpResponse.json({
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: false,
    })
  );
}

const CHANNEL_PATH = '*/api/v1/channels/:id/messages';
const DM_PATH = '*/api/v1/dm/conversations/:id/messages';
const CODE = '123456';
const D1_MFA = { error: 'MFA verification required', mfa_required: true, methods: ['totp'] };
const SOFT_LOCK = { ...D1_MFA, delete_rate_limited: true };

/** Refuses the first purge with `challenge`, then purges. */
function challengeThenPurge(path: string, challenge: object) {
  let sent = 0;
  server.use(
    http.delete(path, () => {
      sent += 1;
      return sent === 1
        ? HttpResponse.json(challenge, { status: 403 })
        : HttpResponse.json({ deleted_count: 2, hidden_count: 0 });
    })
  );
}

async function startChannelPurge() {
  const user = userEvent.setup();
  render(
    <PurgeMessagesModal
      context="channel"
      isOpen
      scopeId="c1"
      scopeName="general"
      onClose={() => {}}
    />
  );
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
  return user;
}

const submit = () => screen.getByRole('button', { name: /^(Confirm and Purge|Waiting|Purging)/ });
const firstSendCapture = () => purgeSpy.mock.calls[0][1];
const credentialsCapture = () => credentialsSpy.mock.calls.at(-1)?.[0].capture;

describe('PurgeMessagesModal: a challenge stage keeps the challenged purge capture', () => {
  // Mutation: dropping `capture` from the dangerous or soft-lock stepUpActivation in PurgeMessagesModal.tsx re-sends against the hook's own capture (red).
  // Mutation: dropping `capture={capture}` from StepUpForm's StepUpCredentials leaves "Set up verification" checking the hook's own capture (red).
  it.each([
    ['dangerous-action', D1_MFA],
    ['soft-lock', SOFT_LOCK],
  ])('the %s stage re-sends, and offers setup, against it', async (_stage, challenge) => {
    challengeThenPurge(CHANNEL_PATH, challenge);
    const user = await startChannelPurge();

    await user.type(await screen.findByLabelText('Authenticator app code'), CODE);
    expect(credentialsCapture()).toBe(firstSendCapture());
    await user.click(submit());

    expect(await screen.findByText('Purged 2 messages.')).toBeInTheDocument();
    expect(purgeSpy).toHaveBeenCalledTimes(2);
    expect(purgeSpy.mock.calls[1][1]).toBe(firstSendCapture());
  });

  // Mutation: dropping `setStageCapture(capture)` from showStepUpChallenge in PurgeMessagesModal.tsx (red).
  it('the DM stage a refusal opened re-sends against that refusal', async () => {
    // Protection off locally, on at the server: the code-less purge draws the challenge.
    usePrivacyStore.setState((st) => ({
      settings: { ...st.settings, requireAuthBeforePurge: false },
    }));
    server.use(readAnswers([]));
    challengeThenPurge(DM_PATH, D1_MFA);
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={() => {}} />
    );
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    expect(credentialsCapture()).toBe(firstSendCapture());
    await user.click(submit());

    expect(await screen.findByText('Purged 2 messages.')).toBeInTheDocument();
    expect(purgeSpy.mock.calls[1][1]).toBe(firstSendCapture());
  });

  // Mutation: setting stageCapture on the proactive path would hand the DM stage a capture no purge went out as.
  it('the DM stage opened before anything was sent takes none', async () => {
    usePrivacyStore.setState((st) => ({
      settings: { ...st.settings, requireAuthBeforePurge: true },
    }));
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={() => {}} />
    );
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    expect(await screen.findByText(/confirm your identity first/)).toBeInTheDocument();
    expect(purgeSpy).not.toHaveBeenCalled();
    expect(credentialsSpy).toHaveBeenCalled();
    expect(credentialsCapture()).toBeUndefined();
  });
});
