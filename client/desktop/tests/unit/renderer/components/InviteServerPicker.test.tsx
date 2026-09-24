import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach, vi } from 'vitest';
import { render, screen, fireEvent, act, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { server as mswServer } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { clearMutualServersCache } from '@/renderer/services/system/mutualServers';
import {
  ADMIN_PERMISSIONS,
  ADMINISTRATOR,
  BASE_PERMISSIONS,
  INVITE,
} from '@/renderer/utils/policy/permissions';
import { InviteServerPicker } from '@/renderer/components/Chat/InviteServerPicker';

const API_BASE = 'http://localhost:8080';

/** Decimal encoding of the INVITE bit, as `GET /api/v1/servers` sends it. */
const INVITE_WIRE = INVITE.toString();

describe('InviteServerPicker', () => {
  beforeEach(() => {
    resetAllStores();
    // Module-scope and therefore shared between tests. Without this a probe
    // settled in one case answers the next one, which is the exact cross-account
    // shape resetService clears it for.
    clearMutualServersCache();
    // Rows WITHOUT `permissions`, answered by permissionStore. This is the
    // legacy shape — a control plane predating #2372 — and the default here so
    // the fallback stays exercised by the cases that do not name a source.
    useServerStore.setState({
      servers: [
        { id: 's1', name: 'Has Invite', icon_url: null },
        { id: 's2', name: 'No Invite', icon_url: null },
      ] as never,
    });
    usePermissionStore.setState({ serverPermissions: { s1: INVITE, s2: 0n } as never });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('falls back to permissionStore for a row carrying no permissions field', () => {
    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);
    expect(screen.getByText('Has Invite')).toBeInTheDocument();
    expect(screen.queryByText('No Invite')).not.toBeInTheDocument();
  });

  // THE REGRESSION TEST FOR #2372 defect 1. This is the assertion that fails
  // before the fix.
  //
  // `serverPermissions` is EMPTY on purpose, and that is the whole point: it is
  // populated only by MainView's `activeServerId` effect, and MainView is not
  // mounted in the DM view where this picker renders. Seeding it here would
  // reproduce nothing — the picker would answer from the fallback and pass
  // whether or not it reads the wire field at all.
  it('reads servers[].permissions when permissionStore knows nothing about the server', () => {
    useServerStore.setState({
      servers: [
        { id: 's1', name: 'Background Invitable', icon_url: null, permissions: INVITE_WIRE },
        { id: 's2', name: 'Background Plain', icon_url: null, permissions: '0' },
      ] as never,
    });
    usePermissionStore.setState({ serverPermissions: {} as never });

    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);

    expect(screen.getByText('Background Invitable')).toBeInTheDocument();
    expect(screen.queryByText('Background Plain')).not.toBeInTheDocument();
  });

  // Both directions, because one alone cannot tell "field first" from "either
  // source grants". The second row is the discriminating one: a store-first
  // reader would list it.
  it('prefers the permissions field over permissionStore in both directions', () => {
    useServerStore.setState({
      servers: [
        { id: 's1', name: 'Field Grants', icon_url: null, permissions: INVITE_WIRE },
        { id: 's2', name: 'Field Denies', icon_url: null, permissions: '0' },
      ] as never,
    });
    usePermissionStore.setState({ serverPermissions: { s1: 0n, s2: INVITE } as never });

    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);

    expect(screen.getByText('Field Grants')).toBeInTheDocument();
    expect(screen.queryByText('Field Denies')).not.toBeInTheDocument();
  });

  // `parsePermissions` returns 0n for anything it cannot parse, so a corrupt or
  // truncated field hides the server rather than showing one the user cannot
  // invite to. It must NOT silently fall through to permissionStore either —
  // that would make a malformed value read as "no field present" and quietly
  // resurrect the bug this component was fixed for.
  it('fails closed on a malformed permissions field rather than falling back', () => {
    useServerStore.setState({
      servers: [{ id: 's1', name: 'Corrupt Field', icon_url: null, permissions: 'not-a-number' }],
    } as never);
    usePermissionStore.setState({ serverPermissions: { s1: INVITE } as never });

    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);

    expect(screen.queryByText('Corrupt Field')).not.toBeInTheDocument();
    expect(screen.getByText(/no servers you can invite to/i)).toBeInTheDocument();
  });

  // THE DISCRIMINATING CASE for this component, and the suite had no equivalent
  // until now. Every other wire value here is INVITE alone ("64") or "0", so
  // `hasPermission(parse(x), INVITE)` and `parse(x) === INVITE` are
  // indistinguishable — and the second hides every owner and every admin in
  // production, which is the defect this component was written to fix. Each row
  // below carries INVITE as one bit among many, or reaches it by the
  // ADMINISTRATOR bypass, so an equality reader fails all three.
  it('grants invite from a composite bitfield, not just the bare INVITE bit', () => {
    useServerStore.setState({
      servers: [
        // An admin preset: INVITE is one bit among many, not the whole value.
        {
          id: 's1',
          name: 'Composite Server',
          icon_url: null,
          permissions: ADMIN_PERMISSIONS.toString(),
        },
        // The load-bearing NEGATIVE: non-zero, yet deliberately WITHOUT INVITE.
        // It is the only case that can separate a bit test from a truthiness
        // test — every other negative in this file is "0".
        { id: 's2', name: 'Base Member', icon_url: null, permissions: BASE_PERMISSIONS.toString() },
        // Reaches invite through the ADMINISTRATOR bypass in `hasPermission`,
        // carrying no INVITE bit of its own.
        { id: 's3', name: 'Admin Server', icon_url: null, permissions: ADMINISTRATOR.toString() },
      ] as never,
    });
    usePermissionStore.setState({ serverPermissions: {} as never });

    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);

    expect(screen.getByText('Composite Server')).toBeInTheDocument();
    expect(screen.getByText('Admin Server')).toBeInTheDocument();
    expect(screen.queryByText('Base Member')).not.toBeInTheDocument();
  });

  // An empty string is a THIRD state the tri-state contract did not name:
  // `'' !== undefined`, so it takes the field branch, and `BigInt('')` is 0n
  // rather than a throw — so the row would hide with no fallback. The Go side
  // now carries `,omitempty` so the wire never produces it; this pins the
  // client's behaviour if it ever does.
  it('treats an empty permissions field as "not computed" and falls back', () => {
    useServerStore.setState({
      servers: [{ id: 's1', name: 'Uncomputed Field', icon_url: null, permissions: '' }] as never,
    });
    usePermissionStore.setState({ serverPermissions: { s1: INVITE } as never });

    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);

    expect(screen.getByText('Uncomputed Field')).toBeInTheDocument();
  });

  it('renders through the shared context-menu layer at the requested viewport position', () => {
    render(<InviteServerPicker position={{ x: 16, y: 32 }} onPick={vi.fn()} onClose={vi.fn()} />);

    expect(document.querySelector('.ctx-menu-overlay')).toBeInTheDocument();
    expect(document.querySelector('.ctx-menu')).toHaveStyle({ left: '16px', top: '32px' });
  });

  it('calls onPick with the chosen server id', () => {
    const onPick = vi.fn();
    render(<InviteServerPicker onPick={onPick} onClose={vi.fn()} />);
    fireEvent.click(screen.getByText('Has Invite'));
    expect(onPick).toHaveBeenCalledWith('s1');
  });

  it('shows an empty state when no server is invitable', () => {
    usePermissionStore.setState({ serverPermissions: { s1: 0n, s2: 0n } as never });
    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);
    expect(screen.getByText(/no servers you can invite to/i)).toBeInTheDocument();
  });

  it('calls onClose on Escape', () => {
    vi.useFakeTimers();
    const onClose = vi.fn();
    render(<InviteServerPicker onPick={vi.fn()} onClose={onClose} />);
    fireEvent.keyDown(document, { key: 'Escape' });
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// --- #2372 defect 2: grey a server the recipients are already in ---

const SERVERS_WITH_INVITE = [
  { id: 's1', name: 'Alpha', icon_url: null, permissions: INVITE.toString() },
  { id: 's2', name: 'Beta', icon_url: null, permissions: INVITE.toString() },
] as never;

describe('InviteServerPicker — already-a-member greying', () => {
  beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
  afterAll(() => mswServer.close());
  afterEach(() => mswServer.resetHandlers());

  beforeEach(() => {
    resetAllStores();
    clearMutualServersCache();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({ user: { id: 'me' } } as never);
    useServerStore.setState({ servers: SERVERS_WITH_INVITE });
    usePermissionStore.setState({ serverPermissions: {} as never });
  });

  function seedConversation(participantIds: readonly string[]) {
    useDMStore.setState({
      conversations: [
        {
          id: 'conv-1',
          participants: [{ userId: 'me' }, ...participantIds.map((userId) => ({ userId }))],
        },
      ],
    } as never);
  }

  /** Answer the probe per user id, so a group DM can disagree with itself. */
  function mockMutual(byUser: Record<string, string[]>) {
    mswServer.use(
      http.get(`${API_BASE}/api/v1/users/:id/mutual-servers`, ({ params }) =>
        HttpResponse.json({ server_ids: byUser[params.id as string] ?? [] })
      )
    );
  }

  // The load-bearing half of the aria-disabled swap, and the half a green suite
  // did not have: `aria-disabled` leaves the button FOCUSABLE and CLICKABLE, so
  // the activation guard is the only thing stopping a click. Without this case a
  // dropped guard survives the whole suite — and a click on a greyed row would
  // create an invite to a server the recipient is already in, which is the exact
  // outcome this feature exists to prevent.
  //
  // The ungreyed sibling is the positive control: it proves clicks reach the
  // handler at all, so a green result here cannot come from a picker that is
  // simply inert.
  it('ignores a click on a greyed server, while its sibling still picks', async () => {
    const onPick = vi.fn();
    seedConversation(['friend']);
    mockMutual({ friend: ['s1'] });

    render(<InviteServerPicker conversationId="conv-1" onPick={onPick} onClose={vi.fn()} />);

    const greyed = await screen.findByRole('button', { name: /Alpha — already a member/i });
    await waitFor(() => expect(greyed).toHaveAttribute('aria-disabled', 'true'));

    fireEvent.click(greyed);
    expect(onPick).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'Beta' }));
    expect(onPick).toHaveBeenCalledTimes(1);
  });

  // THE REGRESSION TEST. Before the fix every invitable server was offered
  // identically, so the only way to discover the recipient was already in one
  // was to send them an invite they could not use.
  it('greys a server the sole recipient is already in', async () => {
    seedConversation(['friend']);
    mockMutual({ friend: ['s1'] });

    render(<InviteServerPicker conversationId="conv-1" onPick={vi.fn()} onClose={vi.fn()} />);

    // `aria-disabled`, and deliberately NOT `toBeDisabled()` — that matcher reads
    // the native attribute only, which is exactly the distinction being pinned:
    // a natively disabled button leaves the focus order and takes its "already a
    // member" label with it, so the one place that explanation is given becomes
    // unreachable to the keyboard and screen-reader users who most need it.
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Alpha — already a member/i })).toHaveAttribute(
        'aria-disabled',
        'true'
      )
    );
    // The other half, and the one that would silently regress if someone "fixed"
    // this back to a native attribute: the item is still natively ENABLED, i.e.
    // still focusable. Without this line the aria assertion above passes just as
    // happily on a button carrying BOTH.
    expect(screen.getByRole('button', { name: /Alpha — already a member/i })).toBeEnabled();
    // Its sibling is the control: same list, same permissions, not greyed.
    expect(screen.getByRole('button', { name: 'Beta' })).toBeEnabled();
  });

  // EVERY recipient, not ANY. A server one member of a group DM is already in
  // is still worth inviting the others to, so greying it would remove a working
  // action. This is the case that separates the two rules — under "any" it
  // would grey.
  it('does not grey a server only SOME group-DM recipients are in', async () => {
    seedConversation(['friend-a', 'friend-b']);
    mockMutual({ 'friend-a': ['s1'], 'friend-b': [] });

    render(<InviteServerPicker conversationId="conv-1" onPick={vi.fn()} onClose={vi.fn()} />);

    // Settle the probes before asserting an absence, or this passes on the first
    // poll whether or not the intersection ever ran.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Beta' })).toBeEnabled());
    expect(screen.getByRole('button', { name: 'Alpha' })).toBeEnabled();
  });

  it('greys a server EVERY group-DM recipient is already in', async () => {
    seedConversation(['friend-a', 'friend-b']);
    mockMutual({ 'friend-a': ['s1'], 'friend-b': ['s1'] });

    render(<InviteServerPicker conversationId="conv-1" onPick={vi.fn()} onClose={vi.fn()} />);

    // `aria-disabled`, and deliberately NOT `toBeDisabled()` — that matcher reads
    // the native attribute only, which is exactly the distinction being pinned:
    // a natively disabled button leaves the focus order and takes its "already a
    // member" label with it, so the one place that explanation is given becomes
    // unreachable to the keyboard and screen-reader users who most need it.
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Alpha — already a member/i })).toHaveAttribute(
        'aria-disabled',
        'true'
      )
    );
    // The other half, and the one that would silently regress if someone "fixed"
    // this back to a native attribute: the item is still natively ENABLED, i.e.
    // still focusable. Without this line the aria assertion above passes just as
    // happily on a button carrying BOTH.
    expect(screen.getByRole('button', { name: /Alpha — already a member/i })).toBeEnabled();
  });

  // Degrade OPEN. A control plane predating this route 404s, and greying on a
  // non-answer would hide a working action with nothing to explain it.
  it('greys nothing when the route is unavailable', async () => {
    seedConversation(['friend']);
    mswServer.use(
      http.get(`${API_BASE}/api/v1/users/:id/mutual-servers`, () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 })
      )
    );

    render(<InviteServerPicker conversationId="conv-1" onPick={vi.fn()} onClose={vi.fn()} />);

    const alpha = await screen.findByRole('button', { name: 'Alpha' });
    expect(alpha).toBeEnabled();
  });

  // No conversation means no recipient to ask about — the picker must not probe
  // at all, and must render exactly as it did before this feature.
  it('probes nothing and greys nothing outside a conversation', async () => {
    let probed = 0;
    mswServer.use(
      http.get(`${API_BASE}/api/v1/users/:id/mutual-servers`, () => {
        probed += 1;
        return HttpResponse.json({ server_ids: ['s1'] });
      })
    );

    render(<InviteServerPicker onPick={vi.fn()} onClose={vi.fn()} />);

    const alpha = await screen.findByRole('button', { name: 'Alpha' });
    expect(alpha).toBeEnabled();
    expect(probed).toBe(0);
  });
});
