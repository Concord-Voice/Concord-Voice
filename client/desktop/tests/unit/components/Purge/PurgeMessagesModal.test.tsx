import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';
import { formatRetryAfter } from '@/renderer/components/Purge/PurgeResult';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';

const noop = () => {};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
});

function channelModal(isOpen: boolean) {
  return (
    <PurgeMessagesModal
      context="channel"
      isOpen={isOpen}
      scopeId="c1"
      scopeName="general"
      onClose={noop}
    />
  );
}

function renderChannelModal() {
  return render(channelModal(true));
}

describe('PurgeMessagesModal — configure stage', () => {
  it('preselects no range and disables confirm until one is chosen', async () => {
    const user = userEvent.setup();
    renderChannelModal();

    for (const radio of screen.getAllByRole('radio')) {
      expect(radio).not.toBeChecked();
    }
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeDisabled();

    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeEnabled();
  });

  it('offers the nine server-accepted ranges and no others', () => {
    renderChannelModal();
    expect(screen.getAllByRole('radio')).toHaveLength(9);
  });

  it('echoes scope and range qualitatively, with no count', async () => {
    const user = userEvent.setup();
    renderChannelModal();
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));

    expect(screen.getByText(/purge all messages from the last 7 days in/i)).toBeInTheDocument();
    // No count-preview endpoint exists; a number here would be invented.
    expect(screen.queryByText(/\d+\s+messages\?/)).not.toBeInTheDocument();
  });

  it('names the last option with its "no time limit" qualifier', () => {
    renderChannelModal();
    expect(screen.getByRole('radio', { name: /^All messages/ })).toHaveAccessibleName(
      'All messages — no time limit'
    );
  });

  it('requires typing PURGE when the range is all', async () => {
    const user = userEvent.setup();
    renderChannelModal();
    await user.click(screen.getByRole('radio', { name: /^All messages/ }));

    const confirm = screen.getByRole('button', { name: 'Purge Messages' });
    expect(confirm).toBeDisabled();

    await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    expect(confirm).toBeEnabled();
  });

  it('rejects a typed confirmation that is not an exact match', async () => {
    const user = userEvent.setup();
    renderChannelModal();
    await user.click(screen.getByRole('radio', { name: /^All messages/ }));

    await user.type(screen.getByLabelText(/type purge to confirm/i), 'purge');
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeDisabled();
  });

  it('re-arms the typed confirmation when a cancelled dialog is reopened', async () => {
    const user = userEvent.setup();
    // Two entry points render this modal unconditionally behind a boolean
    // isOpen, so it survives close — a satisfied PURGE would otherwise make the
    // reopened first paint one click away from deleting an entire history.
    const { rerender } = render(channelModal(true));
    await user.click(screen.getByRole('radio', { name: /^All messages/ }));
    await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeEnabled();

    rerender(channelModal(false));
    rerender(channelModal(true));

    for (const radio of screen.getAllByRole('radio')) {
      expect(radio).not.toBeChecked();
    }
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeDisabled();
    expect(screen.queryByLabelText(/type purge to confirm/i)).not.toBeInTheDocument();
  });

  it('returns a reopened dialog to the configure stage, not the last result', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 4, hidden_count: 0 })
      )
    );
    const user = userEvent.setup();
    const { rerender } = render(channelModal(true));
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await screen.findByText('Purged 4 messages.');

    rerender(channelModal(false));
    rerender(channelModal(true));

    // The result stage has no route back to configure, so a stale result would
    // strand the reopened dialog on the previous run's outcome.
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
    expect(screen.getAllByRole('radio')).toHaveLength(9);
  });

  it('requires typing PURGE in server context at any range', async () => {
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="server"
        isOpen
        scopeId="s1"
        scopeName="Test Server"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last hour' }));
    expect(screen.getByRole('button', { name: 'Purge Messages' })).toBeDisabled();
  });
});

describe('PurgeMessagesModal — per-context body copy', () => {
  it('tells a 1:1 DM actor that the deletion is asymmetric', async () => {
    const user = userEvent.setup();
    render(<PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={noop} />);
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));

    expect(screen.getByText(/your own messages are removed for both of you/i)).toBeInTheDocument();
    expect(screen.getByText(/hidden only for you/i)).toBeInTheDocument();
  });

  it('warns a server actor that unmoderatable channels are skipped', async () => {
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="server"
        isOpen
        scopeId="s1"
        scopeName="Test Server"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last hour' }));

    expect(
      screen.getAllByText(/channels you cannot moderate will be skipped/i).length
    ).toBeGreaterThan(0);
  });

  it('tells a group admin the removal is for everyone', async () => {
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="group"
        role="admin"
        isOpen
        scopeId="g1"
        scopeName="Weekend Crew"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));

    expect(screen.getByText(/messages are removed for everyone in the group/i)).toBeInTheDocument();
  });

  it('states the asymmetry to a group member, which is also the default', async () => {
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="group"
        isOpen
        scopeId="g1"
        scopeName="Weekend Crew"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));

    // Defaulting to the member sentence is the safe direction: it never
    // promises symmetric deletion the backend would not perform.
    expect(screen.getByText(/your own messages are removed for everyone/i)).toBeInTheDocument();
    expect(
      screen.queryByText(/messages are removed for everyone in the group/i)
    ).not.toBeInTheDocument();
  });
});

describe('formatRetryAfter', () => {
  it('renders sub-minute, minute and hour delays, rounding up', () => {
    expect(formatRetryAfter(1)).toBe('1 second');
    expect(formatRetryAfter(45)).toBe('45 seconds');
    expect(formatRetryAfter(61)).toBe('2 minutes');
    expect(formatRetryAfter(60)).toBe('1 minute');
    expect(formatRetryAfter(3600)).toBe('1 hour');
    expect(formatRetryAfter(5400)).toBe('2 hours');
  });

  it('returns no countdown for a nonsensical delay', () => {
    expect(formatRetryAfter(0)).toBeNull();
    expect(formatRetryAfter(-30)).toBeNull();
    expect(formatRetryAfter(Number.NaN)).toBeNull();
  });
});

describe('PurgeMessagesModal — step-up handoff', () => {
  it('does not render a step-up response as a terminal error', async () => {
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () =>
        HttpResponse.json({ error: 'password_required', password_required: true }, { status: 403 })
      )
    );
    const user = userEvent.setup();
    render(<PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={noop} />);
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    // The credential challenge is a stage, not an outcome — the generic
    // "no permission" copy would be a false explanation.
    await waitFor(() => expect(screen.queryByRole('radio')).not.toBeInTheDocument());
    expect(screen.queryByText(/you may not have permission/i)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Done' })).not.toBeInTheDocument();
  });
});

describe('PurgeMessagesModal — result stage', () => {
  async function submitChannelPurge(range = 'Last 7 days') {
    const user = userEvent.setup();
    renderChannelModal();
    await user.click(screen.getByRole('radio', { name: range }));
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    return user;
  }

  it('reports a count on success', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 12, hidden_count: 0 })
      )
    );
    await submitChannelPurge();
    expect(await screen.findByText('Purged 12 messages.')).toBeInTheDocument();
  });

  it('treats an empty scope as a success, not an error', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 0, hidden_count: 0 })
      )
    );
    await submitChannelPurge();

    expect(
      await screen.findByText('No messages matched that range. Nothing to purge.')
    ).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('renders a countdown derived from Retry-After and never a fixed budget', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json(
          { error: 'Rate limit exceeded' },
          { status: 429, headers: { 'Retry-After': '900' } }
        )
      )
    );
    await submitChannelPurge();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Purge limit reached. Try again in 15 minutes.');
    // PURGE_RATE_LIMIT is operator-tunable — a hardcoded allowance would be a
    // drift bug on any non-default deployment.
    expect(alert).not.toHaveTextContent(/\bper hour\b/);
    expect(alert).not.toHaveTextContent(/\b5\b/);
  });

  it('falls back to the countdown-less rate-limit copy when Retry-After is absent', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'Rate limit exceeded' }, { status: 429 })
      )
    );
    await submitChannelPurge();

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Purge limit reached — try again later.'
    );
  });

  it('keeps 503 distinct from 429 and never claims a limit was hit', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'service unavailable' }, { status: 503 })
      )
    );
    await submitChannelPurge();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Temporarily unavailable. Try again shortly.');
    expect(alert).not.toHaveTextContent(/limit/i);
  });

  it('never claims nothing was deleted on a 500', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'internal' }, { status: 500 })
      )
    );
    await submitChannelPurge();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/some messages may already have been purged/i);
    expect(alert).not.toHaveTextContent(/nothing was deleted/i);
    // The refetch is a notice, not an error.
    expect(screen.getByRole('status')).toHaveTextContent("We're refreshing this view.");
  });

  it('recovers from a request that never reaches the server', async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.delete('*/api/v1/channels/:id/messages', async () => {
        await gate;
        return HttpResponse.error();
      })
    );
    await submitChannelPurge();

    // While busy, every dismiss affordance is withdrawn: <fieldset disabled>
    // owns Cancel and dismissable={!busy} removes the close button, Escape and
    // the backdrop. A rejection that never cleared busy left no way out at all.
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Close' })).toBeNull());
    release?.();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/couldn't reach the server/i);
    // A rejection covers a connection dropped AFTER the server committed, so
    // claiming non-deletion on an irreversible operation would be a guess.
    expect(alert).toHaveTextContent(/some messages may already have been purged/i);
    expect(alert).not.toHaveTextContent(/nothing was purged/i);
    expect(screen.getByRole('status')).toHaveTextContent("We're refreshing this view.");
    expect(screen.getByRole('button', { name: 'Close' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Done' })).toBeEnabled();
  });

  it('asks for a refetch when the transport fails, because the outcome is unknown', async () => {
    const details: unknown[] = [];
    const listener = (e: Event) => details.push((e as CustomEvent).detail);
    globalThis.addEventListener('messages-purged', listener);
    server.use(http.delete('*/api/v1/channels/:id/messages', () => HttpResponse.error()));
    await submitChannelPurge();
    await screen.findByRole('alert');
    globalThis.removeEventListener('messages-purged', listener);

    // The refetch is what would show the user the deletion they were not told
    // about — it matters most on the outcome we cannot name.
    expect(details).toEqual([{ scopeId: 'c1' }]);
  });

  it('re-enables Cancel after a failed attempt is dismissed and reopened', async () => {
    server.use(http.delete('*/api/v1/channels/:id/messages', () => HttpResponse.error()));
    const user = userEvent.setup();
    const { rerender } = render(channelModal(true));
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await screen.findByRole('alert');

    rerender(channelModal(false));
    rerender(channelModal(true));

    expect(screen.getByRole('button', { name: 'Cancel' })).toBeEnabled();
  });

  it('asks a server purge to refetch whatever is mounted, not a channel by id', async () => {
    const details: unknown[] = [];
    const listener = (e: Event) => details.push((e as CustomEvent).detail);
    globalThis.addEventListener('messages-purged', listener);
    server.use(
      http.delete('*/api/v1/servers/:id/messages', () =>
        HttpResponse.json({ error: 'internal' }, { status: 500 })
      )
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="server"
        isOpen
        scopeId="s1"
        scopeName="Test Server"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last hour' }));
    await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await screen.findByRole('alert');
    globalThis.removeEventListener('messages-purged', listener);

    // The server context's scopeId is a SERVER id; the refetch listener matches
    // on channel id, so emitting it there would leave the recovery refetch dead.
    // `serverId` travels alongside because the two fields answer different
    // questions — null says what to REFETCH, serverId says what to CLEAR. On a
    // 500 the WebSocket echo cannot be assumed to arrive, so this dispatch is
    // the only thing that clears the server's other known channels.
    expect(details).toEqual([{ scopeId: null, serverId: 's1' }]);
  });

  it('signals its own scope on a successful purge, not only on a partial one', async () => {
    const details: unknown[] = [];
    const listener = (e: Event) => details.push((e as CustomEvent).detail);
    globalThis.addEventListener('messages-purged', listener);
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 3, hidden_count: 0 })
      )
    );
    await submitChannelPurge();
    await screen.findByText('Purged 3 messages.');
    globalThis.removeEventListener('messages-purged', listener);

    // The channel_purged echo is subscription-scoped, so the actor may never
    // receive one; without this the person who pressed the button keeps the
    // purged messages cached and searchable.
    expect(details).toEqual([{ scopeId: 'c1' }]);
  });

  it('asks a successful server purge to refetch whatever is mounted', async () => {
    const details: unknown[] = [];
    const listener = (e: Event) => details.push((e as CustomEvent).detail);
    globalThis.addEventListener('messages-purged', listener);
    server.use(
      http.delete('*/api/v1/servers/:id/messages', () =>
        HttpResponse.json({ deleted_count: 7, hidden_count: 0 })
      )
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="server"
        isOpen
        scopeId="s1"
        scopeName="Test Server"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last hour' }));
    await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    await screen.findByText(/messages purged/i);
    globalThis.removeEventListener('messages-purged', listener);

    // Same shape the partial path uses: a SERVER id matches no mounted channel,
    // so `scopeId` stays null and `serverId` carries the clear.
    expect(details).toEqual([{ scopeId: null, serverId: 's1' }]);
  });

  it('names the conversation, not a channel, when a DM scope is gone', async () => {
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 })
      )
    );
    // The step-up stage is not what is under test here; drop it so the purge is
    // single-shot (its own coverage lives in StepUp.test.tsx).
    usePrivacyStore.setState({
      settings: { ...usePrivacyStore.getState().settings, requireAuthBeforePurge: false },
    });
    const user = userEvent.setup();
    render(<PurgeMessagesModal context="dm" isOpen scopeId="d1" scopeName="Alex" onClose={noop} />);
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('This conversation no longer exists.');
    expect(alert).not.toHaveTextContent(/channel/i);
  });

  it('reports the 404 channel-gone state', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 })
      )
    );
    await submitChannelPurge();
    expect(await screen.findByRole('alert')).toHaveTextContent('This channel no longer exists.');
  });

  it('keeps the 403 copy generic', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'forbidden' }, { status: 403 })
      )
    );
    await submitChannelPurge();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "This purge couldn't be completed. You may not have permission for this scope."
    );
  });

  it('does not auto-close on success — a Done button dismisses', async () => {
    let closed = false;
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 0, hidden_count: 0 })
      )
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen
        scopeId="c1"
        scopeName="general"
        onClose={() => {
          closed = true;
        }}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last 7 days' }));
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    const done = await screen.findByRole('button', { name: 'Done' });
    expect(closed).toBe(false);
    await user.click(done);
    expect(closed).toBe(true);
  });

  it('never renders a server-wide count', async () => {
    server.use(
      http.delete('*/api/v1/servers/:id/messages', () =>
        HttpResponse.json({ deleted_count: 0, hidden_count: 0 })
      )
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="server"
        isOpen
        scopeId="s1"
        scopeName="Test Server"
        onClose={noop}
      />
    );
    await user.click(screen.getByRole('radio', { name: 'Last hour' }));
    await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    expect(
      await screen.findByText('Messages purged. Channels you cannot moderate were skipped.')
    ).toBeInTheDocument();
    expect(screen.queryByText(/purged \d+ message/i)).not.toBeInTheDocument();
  });

  it('keeps the dialog title constant across stages', async () => {
    server.use(
      http.delete('*/api/v1/servers/:id/messages', () =>
        HttpResponse.json({ deleted_count: 0, hidden_count: 0 })
      )
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="server"
        isOpen
        scopeId="s1"
        scopeName="Test Server"
        onClose={noop}
      />
    );
    expect(screen.getByRole('heading', { name: 'Purge Server Messages' })).toBeInTheDocument();

    await user.click(screen.getByRole('radio', { name: 'Last hour' }));
    await user.type(screen.getByLabelText(/type purge to confirm/i), 'PURGE');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));

    await screen.findByRole('button', { name: 'Done' });
    expect(screen.getByRole('heading', { name: 'Purge Server Messages' })).toBeInTheDocument();
  });

  it('disables the whole form while the purge is in flight', async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.delete('*/api/v1/channels/:id/messages', async () => {
        await gate;
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );
    await submitChannelPurge();

    // Native <fieldset disabled> removes descendants from the tab order for
    // free; pointer-events:none + aria-disabled would not.
    await waitFor(() => expect(screen.getByRole('radio', { name: 'Last hour' })).toBeDisabled());
    release?.();
    await screen.findByText('Purged 1 message.');
  });
});
