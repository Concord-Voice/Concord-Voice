import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { act } from 'react';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { answerCapabilityRefresh } from '../../../helpers/capabilityRefresh';
import { clientConfigService } from '@/renderer/services/system/clientConfigService';

// Opening a purge dialog refreshes the capability and offers the pin choice
// only once that answer lands (#3552 review). These tests set the capability
// in the store directly, so the stub answers with what the store holds.
beforeEach(() => {
  answerCapabilityRefresh();
});

// #3458: "Include pinned messages". The option, its copy and the wire value
// all derive from one PinMode, sampled from features.purgeKeepsPinned on open.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  // A DM or group step-up reads GET /api/v1/mfa/step-up. Answer it for an
  // account with no MFA (the password alone), so the read never reaches the
  // network: unanswered, a local dev server's 404 passes and CI's refused
  // connection blocks the stage.
  server.use(
    http.get('*/api/v1/mfa/step-up', () =>
      HttpResponse.json({ methods: [], default_method: null, backup_code_available: false })
    )
  );
});

const noop = () => {};
const CODE = '123456';
const FIXTURE_PW = 'fixture-password-pins';
const CHANNEL_PATH = '*/api/v1/channels/:id/messages';
const SERVER_PATH = '*/api/v1/servers/:id/messages';
const DM_PATH = '*/api/v1/dm/conversations/:id/messages';
const OPTION = { name: 'Include pinned messages' };

type Sent = Record<string, unknown>;

function setKeepsPinned(value: boolean | undefined): void {
  useClientConfigStore.setState({
    serverCapabilities: {
      auth: { oauthProviders: [] },
      features: value === undefined ? {} : { purgeKeepsPinned: value },
    },
  });
}

function scriptedPurge(path: string, responses: Array<() => Response>): Sent[] {
  const bodies: Sent[] = [];
  server.use(
    http.delete(path, async ({ request }) => {
      bodies.push((await request.json()) as Sent);
      return responses[Math.min(bodies.length - 1, responses.length - 1)]();
    })
  );
  return bodies;
}

const ok =
  (deleted = 4, hidden = 0) =>
  () =>
    HttpResponse.json({ deleted_count: deleted, hidden_count: hidden });

async function pickRange(user: ReturnType<typeof userEvent.setup>) {
  await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
}

describe('PurgeMessagesModal — include pinned messages (#3458)', () => {
  it('an older server gets no option, no pin copy, and include_pinned false', async () => {
    setKeepsPinned(undefined);
    const bodies = scriptedPurge(CHANNEL_PATH, [ok(4)]);
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );

    await pickRange(user);
    expect(screen.queryByRole('checkbox', OPTION)).not.toBeInTheDocument();
    expect(screen.queryByText(/pinned/i)).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(await screen.findByText('Purged 4 messages.')).toBeInTheDocument();
    expect(bodies).toEqual([{ range: '7d', include_pinned: false }]);
    expect(screen.queryByText(/pinned/i)).not.toBeInTheDocument();
  });

  it('keeps pins by default and says so from the echo to the result', async () => {
    setKeepsPinned(true);
    const bodies = scriptedPurge(CHANNEL_PATH, [ok(4)]);
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );

    const option = screen.getByRole('checkbox', OPTION);
    expect(option).not.toBeChecked();
    expect(option).toHaveAccessibleDescription('Pinned messages are kept unless you include them.');
    await pickRange(user);
    expect(screen.getByText('Pinned messages are kept.')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(
      await screen.findByText('Purged 4 messages. Pinned messages were kept.')
    ).toBeInTheDocument();
    expect(bodies).toEqual([{ range: '7d', include_pinned: false }]);
  });

  it('including pins bolds the echo, sends true, and the result makes no pin claim', async () => {
    setKeepsPinned(true);
    const bodies = scriptedPurge(CHANNEL_PATH, [ok(4)]);
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );

    await user.click(screen.getByRole('checkbox', OPTION));
    await pickRange(user);
    expect(screen.getByText('Pinned messages will be deleted too.').tagName).toBe('STRONG');

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(await screen.findByText('Purged 4 messages.')).toBeInTheDocument();
    expect(bodies).toEqual([{ range: '7d', include_pinned: true }]);
  });

  it.each([
    [
      'channel',
      'member',
      false,
      'Pinned messages are kept.',
      'Pinned messages will be deleted too.',
    ],
    [
      'channel',
      'member',
      true,
      'Your pinned messages are kept.',
      'Your pinned messages will be deleted too.',
    ],
    [
      'server',
      'member',
      false,
      'Pinned messages are kept.',
      'Pinned messages will be deleted too.',
    ],
    [
      'group',
      'admin',
      false,
      'Pinned messages are kept for everyone.',
      'Pinned messages will be deleted for everyone.',
    ],
    [
      'group',
      'member',
      false,
      'Your pinned messages are kept. Pinned messages from others stay visible to you.',
      'Your pinned messages will be deleted too. Pinned messages from others stay visible. Unpin them to hide them.',
    ],
    [
      'dm',
      'member',
      false,
      'Your pinned messages are kept. Pinned messages from Alex stay visible to you.',
      'Your pinned messages will be deleted too. Pinned messages from Alex stay visible. Unpin them to hide them.',
    ],
  ] as const)(
    '%s (%s, self-scope %s) reads the right pin sentence',
    async (context, role, selfScopeOnly, off, on) => {
      setKeepsPinned(true);
      const user = userEvent.setup();
      render(
        <PurgeMessagesModal
          context={context}
          role={role}
          selfScopeOnly={selfScopeOnly}
          isOpen
          scopeId="x1"
          scopeName="Alex"
          onClose={noop}
        />
      );
      await pickRange(user);
      expect(screen.getByText(off)).toBeInTheDocument();
      await user.click(screen.getByRole('checkbox', OPTION));
      expect(screen.getByText(on)).toBeInTheDocument();
    }
  );

  it('the DM step-up carries the flag and recaps the choice', async () => {
    setKeepsPinned(true);
    const bodies = scriptedPurge(DM_PATH, [ok(1, 2)]);
    const user = userEvent.setup();
    render(<PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={noop} />);

    await user.click(screen.getByRole('checkbox', OPTION));
    await pickRange(user);
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await screen.findByRole('heading', { name: 'Confirm it is you' });
    expect(
      screen.getByText(
        'Your pinned messages will be deleted too. Pinned messages from Alex stay visible. Unpin them to hide them.'
      )
    ).toBeInTheDocument();

    await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ range: '7d', include_pinned: true, current_password: FIXTURE_PW });
  });

  // A peer's pinned message is never hidden by a purge, so an include-mode
  // result says they stay and how to hide them. A keep-mode result makes no
  // such claim: the pins were kept, which "Pinned messages were kept" covers.
  it.each([
    ['dm', true, 'Alex'],
    ['dm', false, 'Alex'],
    ['group', true, 'others'],
    ['group', false, 'others'],
  ] as const)(
    "the %s result with pins included=%s says the peers' pins stay visible only when included (%s)",
    async (context, include, peers) => {
      setKeepsPinned(true);
      scriptedPurge(DM_PATH, [ok(1, 2)]);
      const note = `Pinned messages from ${peers} stay visible. Unpin them to hide them.`;
      const user = userEvent.setup();
      render(
        <PurgeMessagesModal
          context={context}
          role="member"
          isOpen
          scopeId="d1"
          scopeName="Alex"
          onClose={noop}
        />
      );

      if (include) await user.click(screen.getByRole('checkbox', OPTION));
      await pickRange(user);
      await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
      await screen.findByRole('heading', { name: 'Confirm it is you' });
      await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
      await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

      const counts = 'Purged 1 message. 2 more hidden from you.';
      const result = await screen.findByText(new RegExp(`^${counts}`));
      if (include) {
        expect(result).toHaveTextContent(`${counts} ${note}`);
      } else {
        expect(result).toHaveTextContent(`${counts} Pinned messages were kept.`);
        expect(screen.queryByText(/stay visible/)).not.toBeInTheDocument();
      }
    }
  );

  // "No messages matched that range" is false when a peer's pinned message in
  // that range matched and stayed visible, so an include-mode zero result with
  // a peer note drops that clause, as keep mode already does, and the note
  // takes its place. A channel purge has no peer pins to name, so its zero
  // result reads as it always has.
  it.each([
    [
      'dm',
      'member',
      'Nothing to purge. Pinned messages from Alex stay visible. Unpin them to hide them.',
    ],
    [
      'group',
      'member',
      'Nothing to purge. Pinned messages from others stay visible. Unpin them to hide them.',
    ],
    ['channel', 'member', 'No messages matched that range. Nothing to purge.'],
  ] as const)(
    'the %s (%s) include-mode zero result reads exactly as the peer-pin copy says',
    async (context, role, copy) => {
      setKeepsPinned(true);
      scriptedPurge(context === 'channel' ? CHANNEL_PATH : DM_PATH, [ok(0, 0)]);
      const user = userEvent.setup();
      render(
        <PurgeMessagesModal
          context={context}
          role={role}
          isOpen
          scopeId="d1"
          scopeName="Alex"
          onClose={noop}
        />
      );

      await user.click(screen.getByRole('checkbox', OPTION));
      await pickRange(user);
      await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
      if (context !== 'channel') {
        await screen.findByRole('heading', { name: 'Confirm it is you' });
        await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
        await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));
      }

      const result = await screen.findByText(/Nothing to purge\./);
      expect(result.textContent).toBe(copy);
      if (context === 'channel') {
        expect(screen.queryByText(/stay visible/)).not.toBeInTheDocument();
      } else {
        expect(screen.queryByText(/No messages matched/)).not.toBeInTheDocument();
      }
    }
  );

  it('the soft-lock retry carries the flag and recaps the choice', async () => {
    setKeepsPinned(true);
    const bodies = scriptedPurge(SERVER_PATH, [
      () =>
        HttpResponse.json(
          {
            error: 'Confirm it is you',
            delete_rate_limited: true,
            mfa_required: true,
            methods: ['totp'],
          },
          { status: 403 }
        ),
      ok(20),
    ]);
    // The soft-lock stage reads the requirements again; this account has TOTP.
    server.use(
      http.get('*/api/v1/mfa/step-up', () =>
        HttpResponse.json({
          methods: ['totp'],
          default_method: 'totp',
          backup_code_available: false,
        })
      )
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal context="server" isOpen scopeId="s1" scopeName="Guild" onClose={noop} />
    );

    await user.click(screen.getByRole('checkbox', OPTION));
    await pickRange(user);
    await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await screen.findByRole('heading', { name: 'Confirm it is you' });
    expect(screen.getByText('Pinned messages will be deleted too.')).toBeInTheDocument();

    await user.type(await screen.findByLabelText('Authenticator app code'), CODE);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies).toEqual([
      { range: '7d', include_pinned: true },
      { range: '7d', include_pinned: true, mfa_code: CODE },
    ]);
  });

  it('closing resets the choice', async () => {
    setKeepsPinned(true);
    const user = userEvent.setup();
    const { rerender } = render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('checkbox', OPTION));
    expect(screen.getByRole('checkbox', OPTION)).toBeChecked();

    rerender(
      <PurgeMessagesModal
        context="channel"
        isOpen={false}
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    rerender(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    expect(screen.getByRole('checkbox', OPTION)).not.toBeChecked();
  });

  it('a server that stops reporting the capability mid-dialog loses the pin promise', async () => {
    setKeepsPinned(true);
    const bodies = scriptedPurge(CHANNEL_PATH, [ok(4)]);
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    await pickRange(user);
    expect(screen.getByText('Pinned messages are kept.')).toBeInTheDocument();

    // A rolled-back server ignores include_pinned, so the dialog must stop
    // promising kept pins rather than send a choice nobody honours.
    act(() => setKeepsPinned(undefined));
    expect(screen.queryByRole('checkbox', OPTION)).not.toBeInTheDocument();
    expect(screen.queryByText('Pinned messages are kept.')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(await screen.findByText('Purged 4 messages.')).toBeInTheDocument();
    expect(screen.queryByText(/Pinned messages were kept/)).not.toBeInTheDocument();
    expect(bodies).toEqual([{ range: '7d', include_pinned: false }]);
  });

  // A cached capability can predate a rollback, and a rolled-back server
  // deletes pins it was asked to keep, so the dialog promises nothing until the
  // refresh that opening starts has answered (#3552 review).
  it('makes no pin promise until the open refresh answers', async () => {
    setKeepsPinned(true);
    vi.mocked(clientConfigService.refreshServerCapabilities).mockResolvedValue();
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    await pickRange(user);
    expect(screen.queryByRole('checkbox', OPTION)).not.toBeInTheDocument();
    expect(screen.queryByText(/pinned/i)).not.toBeInTheDocument();

    act(() => setKeepsPinned(true));
    expect(screen.getByRole('checkbox', OPTION)).toBeInTheDocument();
    expect(screen.getByText('Pinned messages are kept.')).toBeInTheDocument();
  });

  // A rollback after the open answer would delete pins under "kept" copy, so
  // confirming rechecks the capability and sends nothing on a withdrawal
  // (#3552 review).
  it('a capability withdrawn between open and confirm stops the send', async () => {
    setKeepsPinned(true);
    vi.mocked(clientConfigService.refreshServerCapabilities)
      .mockImplementationOnce(async () => setKeepsPinned(true))
      .mockImplementationOnce(async () => setKeepsPinned(undefined));
    const bodies = scriptedPurge(CHANNEL_PATH, [ok(4)]);
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    await pickRange(user);
    expect(screen.getByText('Pinned messages are kept.')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(
      await screen.findByText('Temporarily unavailable. Try again shortly.')
    ).toBeInTheDocument();
    expect(bodies).toEqual([]);
  });

  it('samples the capability on open: a failed fetch or a new capability mid-dialog changes nothing', async () => {
    setKeepsPinned(true);
    const { rerender } = render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    act(() => useClientConfigStore.setState({ serverCapabilities: null }));
    expect(screen.getByRole('checkbox', OPTION)).toBeInTheDocument();
    act(() => setKeepsPinned(undefined));

    rerender(
      <PurgeMessagesModal
        context="channel"
        isOpen={false}
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    rerender(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={noop}
      />
    );
    expect(screen.queryByRole('checkbox', OPTION)).not.toBeInTheDocument();

    act(() => setKeepsPinned(true));
    expect(screen.queryByRole('checkbox', OPTION)).not.toBeInTheDocument();
  });

  it.each([
    ['channel', CHANNEL_PATH, ok(0, 0), 'Nothing to purge. Pinned messages are kept.'],
    [
      'server',
      SERVER_PATH,
      ok(0, 0),
      'Messages purged. Pinned messages were kept. Channels you cannot moderate were skipped.',
    ],
  ] as const)('the %s result says pins were kept', async (context, path, answer, copy) => {
    setKeepsPinned(true);
    scriptedPurge(path, [answer]);
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal context={context} isOpen scopeId="x1" scopeName="Guild" onClose={noop} />
    );
    await pickRange(user);
    if (context === 'server')
      await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    expect(await screen.findByText(copy)).toBeInTheDocument();
  });
});
