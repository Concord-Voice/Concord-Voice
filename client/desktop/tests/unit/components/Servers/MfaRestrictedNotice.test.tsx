import { useRef } from 'react';
import { render, screen, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { gracefulReset } from '@/renderer/services/system/resetService';
import MfaRestrictedNotice from '@/renderer/components/Servers/MfaRestrictedNotice';

// The banner under the channel sidebar's server header (#3456 §3.6). Real
// stores, real openVerificationSetup and real gracefulReset; nothing is mocked.
//
// "Mutant:" comments name the production change each case exists to turn red.

const SENTENCE = 'Some of your permissions on this server need MFA.';

/** The sidebar header's server-name control, as MainView renders it, beside the notice. */
function Harness({
  serverId = 'server-1',
  compact = false,
}: {
  serverId?: string;
  compact?: boolean;
}) {
  const nameRef = useRef<HTMLHeadingElement>(null);
  return (
    <>
      <h3 ref={nameRef} tabIndex={-1}>
        Server name
      </h3>
      <MfaRestrictedNotice serverId={serverId} compact={compact} returnFocusRef={nameRef} />
    </>
  );
}

function flag(...serverIds: string[]) {
  usePermissionStore.setState({
    mfaRestrictedByServer: Object.fromEntries(serverIds.map((id) => [id, true as const])),
  });
}

const dismissButton = () => screen.getByRole('button', { name: 'Dismiss MFA notice' });
const setupLink = () => screen.getByRole('button', { name: 'Set up verification' });

beforeEach(() => {
  resetAllStores();
});

describe('MfaRestrictedNotice', () => {
  // Mutant: the `restricted` guard removed from the early return.
  it('renders nothing while the flag is absent', () => {
    render(<Harness />);

    expect(screen.queryByText(SENTENCE)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Dismiss MFA notice' })).not.toBeInTheDocument();
  });

  // Mutant: the selector reading any key instead of this server's.
  it('renders nothing when only another server is flagged', () => {
    flag('server-2');

    render(<Harness />);

    expect(screen.queryByText(SENTENCE)).not.toBeInTheDocument();
  });

  // Mutant: the sentence or the link wording changed.
  it('says the sentence and offers the set-up link when the flag is set', () => {
    flag('server-1');

    render(<Harness />);

    expect(screen.getByText(SENTENCE)).toBeInTheDocument();
    expect(setupLink()).toBeInTheDocument();
  });

  // Mutant: the `compact` guard removed, so the narrow rail grows a sentence it has no room for.
  it('renders nothing in the compact rail even when flagged', () => {
    flag('server-1');

    render(<Harness compact />);

    expect(screen.queryByText(SENTENCE)).not.toBeInTheDocument();
  });

  // Mutant: `returnTo` changed to `serverSettings`, or the call dropped from onClick.
  it('the link opens App Settings at the MFA section with a chat return', async () => {
    flag('server-1');
    render(<Harness />);

    await userEvent.click(setupLink());

    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'privacy',
      controlId: 'section-mfa',
    });
  });

  // Mutant: the dismiss handler not calling `dismissMfaNotice`, or the `dismissed` guard dropped.
  it('dismissing hides the banner', async () => {
    flag('server-1');
    render(<Harness />);

    await userEvent.click(dismissButton());

    expect(screen.queryByText(SENTENCE)).not.toBeInTheDocument();
  });

  // Mutant: `returnFocusRef.current?.focus()` removed: focus falls to <body> with the removed button.
  it('dismissing moves focus to the server-name control', async () => {
    flag('server-1');
    render(<Harness />);

    await userEvent.click(dismissButton());

    expect(screen.getByRole('heading', { name: 'Server name' })).toHaveFocus();
  });

  // Mutant: dismissal stored as a single boolean, so one server's dismissal hides every server's notice.
  it('dismissal is per server: another flagged server keeps its banner', async () => {
    flag('server-1', 'server-2');
    const { rerender } = render(<Harness serverId="server-1" />);

    await userEvent.click(dismissButton());
    rerender(<Harness serverId="server-2" />);

    expect(screen.getByText(SENTENCE)).toBeInTheDocument();
  });

  // Mutant: dismissal held in component state, so a remount (switching away and back) brings the banner back.
  it('stays dismissed for the session across a remount', async () => {
    flag('server-1');
    const first = render(<Harness />);
    await userEvent.click(dismissButton());
    first.unmount();

    render(<Harness />);

    expect(screen.queryByText(SENTENCE)).not.toBeInTheDocument();
  });

  // Mutant: `clearMfaNoticeDismissals()` missing from gracefulReset: the next account inherits the dismissal.
  it('returns after an account switch', async () => {
    flag('server-1');
    const first = render(<Harness />);
    await userEvent.click(dismissButton());
    first.unmount();

    gracefulReset();
    flag('server-1');
    render(<Harness />);

    expect(screen.getByText(SENTENCE)).toBeInTheDocument();
  });
});
