import { render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import ContextMenu from '@/renderer/components/ui/ContextMenu';
import MfaRestrictedMenuItem from '@/renderer/components/Servers/MfaRestrictedMenuItem';

// The "Set up verification" item the three context menus share (#3456 §3.6).
// Real stores and the real openVerificationSetup; nothing is mocked.
//
// "Mutant:" comments name the production change each case exists to turn red.

const SERVER_ID = 'server-1';
const onClose = vi.fn();

function renderItem(serverId = SERVER_ID) {
  return render(
    <ContextMenu position={{ x: 0, y: 0 }} onClose={onClose}>
      <MfaRestrictedMenuItem serverId={serverId} onClose={onClose} />
    </ContextMenu>
  );
}

const item = () => screen.queryByRole('button', { name: 'Set up verification' });

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
});

describe('MfaRestrictedMenuItem', () => {
  // Mutant: the `restricted` guard removed, so the item renders for everyone.
  it('renders nothing while the flag is absent', () => {
    renderItem();

    expect(item()).not.toBeInTheDocument();
  });

  // Mutant: the selector reading any key (`Object.keys(...).length > 0`) instead of this server's.
  it('renders nothing when only another server is flagged', () => {
    usePermissionStore.setState({ mfaRestrictedByServer: { 'server-2': true } });

    renderItem();

    expect(item()).not.toBeInTheDocument();
  });

  // Mutant: the selector keyed on a constant, so a flag on this server never shows the item.
  it('renders the item when the flag is set for its server', () => {
    usePermissionStore.setState({ mfaRestrictedByServer: { [SERVER_ID]: true } });

    renderItem();

    expect(item()).toBeInTheDocument();
  });

  // Mutant: the item reading `dismissedMfaNotices`, making the menu answer dismissible like the banner.
  it('is not dismissible: a dismissed banner leaves the item in place', () => {
    usePermissionStore.setState({ mfaRestrictedByServer: { [SERVER_ID]: true } });
    useSettingsOverlayStore.getState().dismissMfaNotice(SERVER_ID);

    renderItem();

    expect(item()).toBeInTheDocument();
  });

  // Mutant: `returnTo` changed to `serverSettings`, or the call dropped from onClick.
  it('opens App Settings at the MFA section with a chat return, then closes the menu', async () => {
    usePermissionStore.setState({ mfaRestrictedByServer: { [SERVER_ID]: true } });
    renderItem();

    await userEvent.click(item() as HTMLElement);

    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'privacy',
      controlId: 'section-mfa',
    });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
