import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';

// RT4: a purge sent from the configure stage carries no credentials, and a
// challenge in its answer opens a credential stage. That challenge belongs to
// the account and server that sent it: after a runtime-server switch it opens
// nothing, so a password is never typed for one server into a stage another
// will receive. The dialog is freed, and nothing is reported, because a
// challenge is answered before any batch.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
});
afterAll(() => server.close());

const SWITCHED_TO = 'https://other-server.example.test';
const noop = () => {};

beforeEach(() => {
  resetAllStores();
  server.use(
    http.get('*/api/v1/mfa/step-up', () =>
      HttpResponse.json({ methods: ['totp'], default_method: 'totp', backup_code_available: false })
    )
  );
});

/** A purge route whose one answer waits until the test releases it. */
function heldRoute(path: string, answer: () => Response) {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  let requests = 0;
  server.use(
    http.delete(path, async () => {
      requests += 1;
      await gate;
      return answer();
    })
  );
  return { release, requests: () => requests };
}

const challenge = (body: object) => () => HttpResponse.json(body, { status: 403 });

const CASES = [
  {
    name: 'the soft-lock challenge of a channel self-purge',
    path: '*/api/v1/channels/:id/messages',
    answer: challenge({
      error: 'Confirm it is you',
      delete_rate_limited: true,
      mfa_required: true,
      methods: ['totp'],
    }),
    open: () => (
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    ),
  },
  {
    // Protection off locally, on at the server: the factor-less purge is refused.
    name: 'the step-up challenge of a DM purge',
    path: '*/api/v1/dm/conversations/:id/messages',
    answer: challenge({ error: 'Current password required', password_required: true }),
    open: () => (
      <PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Momo" onClose={noop} />
    ),
  },
] as const;

async function sendHeld(c: (typeof CASES)[number]) {
  usePrivacyStore.setState((s) => ({ settings: { ...s.settings, requireAuthBeforePurge: false } }));
  const route = heldRoute(c.path, c.answer);
  const user = userEvent.setup();
  render(c.open());
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
  await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
  await waitFor(() => expect(route.requests()).toBe(1));
  return route;
}

describe('PurgeMessagesModal: a challenge answered after a server switch (RT4)', () => {
  // Mutant: runPurge checking only the open generation before applying a challenge.
  it.each(CASES)('$name opens no stage, and the dialog is freed', async (c) => {
    const route = await sendHeld(c);

    setRuntimeServerBase(SWITCHED_TO);
    route.release();

    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeEnabled()
    );
    expect(screen.queryByRole('heading', { name: 'Confirm it is you' })).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Range' })).toHaveValue('7d');
  });

  // Positive control: the same answer with no switch opens the stage.
  it.each(CASES)('$name opens its stage when nothing changed', async (c) => {
    const route = await sendHeld(c);

    route.release();

    expect(await screen.findByRole('heading', { name: 'Confirm it is you' })).toBeInTheDocument();
  });
});
