import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { clearInvitePreviewCache } from '@/renderer/hooks/messaging/useInvitePreview';
import { InviteEmbed } from '@/renderer/components/Chat/InviteEmbed';

const API_BASE = 'http://localhost:8080';
// `server_id` deliberately differs from the ids the join handlers below return.
// The invite's server and the joined server are of course the same server in
// production, but if this fixture matched them, `joinServer`'s `addServer` write
// would flip `alreadyMember` true and the join-success cases would show "Joined"
// whether or not `setJoined` ran — passing for the wrong reason. Keeping them
// distinct leaves `setJoined` as the only thing those cases can be measuring.
const INVITE_SERVER_ID = 'server-acme';
const validPreview = {
  server_id: INVITE_SERVER_ID,
  server_name: 'Acme HQ',
  server_icon: '/api/v1/media/server-icons/abc',
  server_banner: null,
  member_count: 7,
  valid: true,
};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('InviteEmbed', () => {
  beforeEach(() => {
    resetAllStores();
    clearInvitePreviewCache();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('shows a neutral skeleton while loading (never sender-controlled text)', () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, async () => {
        await new Promise((r) => setTimeout(r, 50));
        return HttpResponse.json(validPreview);
      })
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    expect(screen.getByText(/loading invite/i)).toBeInTheDocument();
  });

  it('renders the authoritative server name + Join once resolved', async () => {
    server.use(http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)));
    render(<InviteEmbed code="GHJKMNPQ" />);
    await waitFor(() => expect(screen.getByText('Acme HQ')).toBeInTheDocument());
    expect(screen.getByText(/7 members/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /join/i })).toBeInTheDocument();
  });

  it('shows an invalid state for valid:false', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () =>
        HttpResponse.json({ ...validPreview, valid: false })
      )
    );
    render(<InviteEmbed code="KKKKMNPQ" />);
    await waitFor(() => expect(screen.getByText(/invalid or has expired/i)).toBeInTheDocument());
  });

  it('joins on click and shows Joined', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, () =>
        HttpResponse.json({ server: { id: 'server-1', name: 'Acme HQ' }, role: 'member' })
      )
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    const joinBtn = await screen.findByRole('button', { name: /join/i });
    fireEvent.click(joinBtn);
    await waitFor(() => expect(screen.getByText(/joined/i)).toBeInTheDocument());
  });

  // regression for #2363: InviteEmbed.tsx must NOT be edited to make this
  // pass — reconciliation belongs in inviteStore.joinServer, not here.
  it('T3d: adds the joined server to serverStore.servers on click', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, () =>
        HttpResponse.json(
          {
            server: {
              id: 'server-2363-embed',
              name: 'Acme HQ',
              owner_id: 'user-2',
              allow_embedded_content: true,
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
            role: 'member',
          },
          { status: 200 }
        )
      )
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    const joinBtn = await screen.findByRole('button', { name: /join/i });
    fireEvent.click(joinBtn);

    await waitFor(() => expect(screen.getByText(/joined/i)).toBeInTheDocument());

    const ids = useServerStore.getState().servers.map((s) => s.id);
    expect(
      ids,
      `serverStore.servers should contain id "server-2363-embed" after joining via InviteEmbed, got [${ids.join(', ')}]`
    ).toContain('server-2363-embed');
  });

  // The fixture reason is deliberately something NO fallback could contain. It
  // used to be 'expired', asserted with /expired/i — which also matches the
  // pre-fix hardcoded 'Could not join — the invite may have expired.', so the
  // assertion passed identically against the bug this PR removes. A negative on
  // that old string is what makes the case a failure-path guard rather than a
  // restatement of it.
  it('shows an error when the join fails', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, () =>
        HttpResponse.json({ error: 'Server is at capacity' }, { status: 503 })
      )
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    const joinBtn = await screen.findByRole('button', { name: /join/i });
    fireEvent.click(joinBtn);
    await waitFor(() => expect(screen.getByText(/at capacity/i)).toBeInTheDocument());
    expect(screen.queryByText(/may have expired/i)).not.toBeInTheDocument();
  });

  // THE GUARD THAT WAS ON THE WRONG COMPONENT. JoinServerModal pins
  // "read the outcome, never the shared store field"; InviteEmbed did not — and
  // InviteEmbed is the ONLY place the race is reachable, because a chat holds
  // many invite cards while exactly one modal exists at a time. Reverting
  // `setJoinError(outcome.reason)` to a `useInviteStore.getState().error` read
  // left every other test in this file green.
  //
  // Two cards, two codes, two different reasons, and the OLDER join released
  // last — the shape that makes the shared field hold the newer card's message
  // when the older one reads it.
  it('shows each card its OWN failure reason when two joins race', async () => {
    let releaseFirst!: () => void;
    const firstGate = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, async ({ request }) => {
        const body = (await request.json()) as { code: string };
        if (body.code === 'AAAAAAAA') {
          await firstGate;
          return HttpResponse.json({ error: 'First invite was revoked' }, { status: 410 });
        }
        return HttpResponse.json({ error: 'Second invite is full' }, { status: 410 });
      })
    );

    render(
      <>
        <InviteEmbed code="AAAAAAAA" />
        <InviteEmbed code="BBBBBBBB" />
      </>
    );
    const buttons = await screen.findAllByRole('button', { name: /join/i });
    expect(buttons).toHaveLength(2);
    fireEvent.click(buttons[0]);
    fireEvent.click(buttons[1]);

    // The NEWER join settles first and writes the shared store field.
    await waitFor(() => expect(screen.getByText(/Second invite is full/i)).toBeInTheDocument());
    // Now the older one lands. Reading the shared field here yields the newer
    // card's message, or the null its start wrote — never its own.
    releaseFirst();
    await waitFor(() => expect(screen.getByText(/First invite was revoked/i)).toBeInTheDocument());
    expect(screen.getByText(/Second invite is full/i)).toBeInTheDocument();
    expect(screen.queryByText(/may have expired/i)).not.toBeInTheDocument();
  });

  // `abandoned` must render NOTHING — it means a different account owns the
  // session, so the join happened for someone who is no longer here. Nothing at
  // the caller level pinned that; only the store's own T3d/T3j did.
  it('shows no error when the outcome is abandoned', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, () => {
        // Switch ACCOUNT mid-flight by advancing authGeneration — NOT by
        // clearing the access token. `rotateAuthCredentials` replaces both
        // credentials while deliberately preserving the generation, so a cleared
        // token is an ordinary refresh and the outcome would still be `failed`.
        // The generation is the only field that means "a different account".
        useAuthStore.getState().beginAuthLifecycle('token-next', 'session-next');
        return HttpResponse.json({ error: 'should never be shown' }, { status: 410 });
      })
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    fireEvent.click(await screen.findByRole('button', { name: /join/i }));
    await waitFor(() => expect(screen.getByRole('button', { name: /join/i })).not.toBeDisabled());
    expect(screen.queryByText(/should never be shown/i)).not.toBeInTheDocument();
  });

  // --- #2372 defect 3: already a member ---

  // THE REGRESSION TEST. Fails before the fix: the embed rendered Join
  // unconditionally, so the only way to learn you were already in the server
  // was to press it and read a 409.
  it('renders Joined and offers no Join button when already a member', async () => {
    useServerStore.setState({
      servers: [{ id: INVITE_SERVER_ID, name: 'Acme HQ' }],
    } as never);
    server.use(http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)));

    render(<InviteEmbed code="GHJKMNPQ" />);

    await waitFor(() => expect(screen.getByText(/joined/i)).toBeInTheDocument());
    // Absence of the button is the load-bearing half — a Joined label beside a
    // live Join button would still let the user fire the 409.
    expect(screen.queryByRole('button', { name: /join/i })).not.toBeInTheDocument();
  });

  it('offers Join when the invited server is not in the membership list', async () => {
    useServerStore.setState({
      servers: [{ id: 'some-other-server', name: 'Elsewhere' }],
    } as never);
    server.use(http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)));

    render(<InviteEmbed code="GHJKMNPQ" />);

    expect(await screen.findByRole('button', { name: /join/i })).toBeInTheDocument();
  });

  // A control plane predating #2372 sends no `server_id`, and "cannot tell" must
  // not collapse into "already a member" — that would hide Join on every invite
  // against an older self-hosted server. The membership list is deliberately
  // NON-empty so the case cannot pass merely because there was nothing to match.
  it('offers Join when the response carries no server_id at all', async () => {
    useServerStore.setState({
      servers: [{ id: INVITE_SERVER_ID, name: 'Acme HQ' }],
    } as never);
    const { server_id: _omitted, ...legacyPreview } = validPreview;
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(legacyPreview))
    );

    render(<InviteEmbed code="GHJKMNPQ" />);

    expect(await screen.findByRole('button', { name: /join/i })).toBeInTheDocument();
  });

  // The join route answers 409 "You are already a member of this server" — the
  // exact case defect 3 is about, reached by a stale client or another device.
  // This component used to overwrite every failure with an expiry guess, telling
  // the user to go find a fresh invite for a server they are already in.
  it('surfaces the server reason for a failed join rather than guessing at expiry', async () => {
    useServerStore.setState({ servers: [] } as never);
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, () =>
        HttpResponse.json({ error: 'You are already a member of this server' }, { status: 409 })
      )
    );

    render(<InviteEmbed code="GHJKMNPQ" />);
    fireEvent.click(await screen.findByRole('button', { name: /join/i }));

    await waitFor(() =>
      expect(screen.getByText(/already a member of this server/i)).toBeInTheDocument()
    );
    expect(screen.queryByText(/may have expired/i)).not.toBeInTheDocument();
  });

  it('disables the Join button while joining', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => HttpResponse.json(validPreview)),
      http.post(`${API_BASE}/api/v1/invites/join`, async () => {
        await new Promise((r) => setTimeout(r, 50));
        return HttpResponse.json({ server: { id: 'server-1', name: 'Acme HQ' }, role: 'member' });
      })
    );
    render(<InviteEmbed code="GHJKMNPQ" />);
    const joinBtn = await screen.findByRole('button', { name: /join/i });
    fireEvent.click(joinBtn);
    await waitFor(() => expect(screen.getByRole('button', { name: /joining/i })).toBeDisabled());
    await waitFor(() => expect(screen.getByText(/joined/i)).toBeInTheDocument());
  });
});
