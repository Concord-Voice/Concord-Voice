import type { ReactElement } from 'react';
import { render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockChannel, mockMember2, mockServer } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { ADMIN_PERMISSIONS } from '@/renderer/utils/policy/permissions';
import ServerContextMenu from '@/renderer/components/Servers/ServerContextMenu';
import ChannelContextMenu from '@/renderer/components/Channels/ChannelContextMenu';
import MemberContextMenu from '@/renderer/components/Members/MemberContextMenu';

// The "Set up verification" item as the three context menus place it (#3456
// §3.6): only when the flag is set for that server, in the upper group before
// the destructive separator, calling openVerificationSetup with a chat return.
// Real stores, real ContextMenu and a real click; the only seam is the menus'
// own callbacks.
//
// "Mutant:" comments name the production change each case exists to turn red.

const SERVER_ID = 'server-1';
const position = { x: 0, y: 0 };
const onClose = vi.fn();

// Administrator on every menu: it turns on each destructive control a menu has
// (Delete/Leave Server, Delete Channel, Kick/Ban), so there is a destructive
// separator for the item to sit before.
const noop = vi.fn();
const MENUS: Array<{ name: string; element: () => ReactElement }> = [
  {
    name: 'ServerContextMenu',
    element: () => (
      <ServerContextMenu
        server={mockServer}
        position={position}
        onClose={onClose}
        onEditServer={noop}
        onDeleteServer={noop}
        onLeaveServer={noop}
        onInvite={noop}
        onPurgeMessages={noop}
      />
    ),
  },
  {
    name: 'ChannelContextMenu',
    element: () => (
      <ChannelContextMenu
        channel={mockChannel}
        position={position}
        serverId={SERVER_ID}
        onClose={onClose}
        onEditChannel={noop}
        onDeleteChannel={noop}
        onPurgeMessages={noop}
      />
    ),
  },
  {
    name: 'MemberContextMenu',
    element: () => (
      <MemberContextMenu
        member={mockMember2}
        position={position}
        serverId={SERVER_ID}
        ownerUserId="user-1"
        onClose={onClose}
        onViewProfile={noop}
        onBan={noop}
        onKick={noop}
      />
    ),
  },
];

const item = () => screen.queryByRole('button', { name: 'Set up verification' });

function flag(...serverIds: string[]) {
  usePermissionStore.setState({
    mfaRestrictedByServer: Object.fromEntries(serverIds.map((id) => [id, true as const])),
  });
}

/** The menu's children in order: items and separators, header excluded. */
function menuChildren(): Element[] {
  const menu = document.querySelector('.ctx-menu');
  return Array.from(menu?.querySelectorAll('.ctx-menu-item, .ctx-menu-separator') ?? []);
}

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  // Neither the server's owner (user-1) nor the member menu's target (user-2).
  useUserStore.setState({ user: { id: 'viewer-1', username: 'viewer' } } as never);
  usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: ADMIN_PERMISSIONS } });
});

describe.each(MENUS)('$name', ({ element }) => {
  // Mutant: the menu's <MfaRestrictedMenuItem> removed.
  it('shows the item when the flag is set for its server', () => {
    flag(SERVER_ID);

    render(element());

    expect(item()).toBeInTheDocument();
  });

  // Mutant: the menu passes a constant or wrong serverId to the item.
  it('omits the item while the flag is absent', () => {
    render(element());

    expect(item()).not.toBeInTheDocument();
  });

  // Mutant: the menu passes a constant or wrong serverId to the item.
  it('omits the item when only another server is flagged', () => {
    flag('server-2');

    render(element());

    expect(item()).not.toBeInTheDocument();
  });

  // Mutant: the item moved below the destructive cluster (after Delete/Leave/Kick/Ban).
  it('places the item in the upper group, before the destructive separator', () => {
    flag(SERVER_ID);
    render(element());

    const children = menuChildren();
    const firstDanger = children.findIndex((c) => c.classList.contains('ctx-menu-item-danger'));
    const destructiveSeparator = children
      .slice(0, firstDanger)
      .map((c, i) => (c.classList.contains('ctx-menu-separator') ? i : -1))
      .filter((i) => i >= 0)
      .pop();
    const at = children.findIndex((c) => c.textContent === 'Set up verification');

    expect(firstDanger).toBeGreaterThan(-1);
    expect(destructiveSeparator).toBeDefined();
    expect(at).toBeGreaterThan(-1);
    expect(at).toBeLessThan(destructiveSeparator as number);
  });

  // Mutant: the menu's onClose not forwarded to the item, or returnTo changed from chat.
  it('click opens verification setup with a chat return and closes the menu', async () => {
    flag(SERVER_ID);
    render(element());

    await userEvent.click(item() as HTMLElement);

    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
