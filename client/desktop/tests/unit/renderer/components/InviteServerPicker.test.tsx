import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, fireEvent, act } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import {
  ADMIN_PERMISSIONS,
  ADMINISTRATOR,
  BASE_PERMISSIONS,
  INVITE,
} from '@/renderer/utils/policy/permissions';
import { InviteServerPicker } from '@/renderer/components/Chat/InviteServerPicker';

/** Decimal encoding of the INVITE bit, as `GET /api/v1/servers` sends it. */
const INVITE_WIRE = INVITE.toString();

describe('InviteServerPicker', () => {
  beforeEach(() => {
    resetAllStores();
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
    const { container } = render(
      <InviteServerPicker position={{ x: 16, y: 32 }} onPick={vi.fn()} onClose={vi.fn()} />
    );

    expect(container.querySelector('.ctx-menu-overlay')).toBeInTheDocument();
    expect(container.querySelector('.ctx-menu')).toHaveStyle({ left: '16px', top: '32px' });
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
