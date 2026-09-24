import React from 'react';
import { render, screen, fireEvent, act } from '../../test-utils';
import { vi } from 'vitest';
import Modal from '@/renderer/components/ui/Modal';
import { resetAllStores } from '../../helpers/store-helpers';
import {
  installTopLayerEmulation,
  installRootHarness,
  SettingsStandIn,
  DialogInsideRoot,
  topDialog,
} from '../../helpers/topLayerEmulation';

import ForceUpdateOverlay from '@/renderer/components/ui/ForceUpdateOverlay';
import ConnectionLostOverlay from '@/renderer/components/ui/ConnectionLostOverlay';
import { AttestationFailedModal } from '@/renderer/components/AttestationFailedModal';
import { ChangelogModal } from '@/renderer/components/ChangelogModal/ChangelogModal';
import { useAttestationFailureStore } from '@/renderer/stores/auth/attestationFailureStore';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';

// jsdom-gap emulation (showModal()/inert focus blocking) and the #root /
// SettingsStandIn harness are shared with the founding MFA regression test —
// see tests/helpers/topLayerEmulation.tsx and [internal]rules/frontend.md §
// "A global overlay must be reachable over the Settings dialog".
installTopLayerEmulation();
const getRoot = installRootHarness();

// ForceUpdateOverlay reads getDesktopClientDisplayVersion()/getDesktopClientVersion()
// on mount via globalThis.electron.getVersion — tests/setup.ts already stubs a
// resolving getVersion, so no additional bridge mocking is needed here; none
// of these tests click an action button, so no update-IPC mocks are required.

// Each case isolates ONE emulation rule, so deleting that rule turns it red:
// a fixture that two rules can both satisfy would pin neither.
describe('emulation fidelity (positive controls)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  // A ui/Modal makes #root inert and opens no <dialog>, so only the inert
  // rule (a) can refuse this focus: rule (b) has no top dialog to apply.
  function InertRoot({ children }: Readonly<{ children?: React.ReactNode }>) {
    return (
      <>
        <Modal isOpen onClose={vi.fn()} title="Plain modal">
          <button type="button">In modal</button>
        </Modal>
        {children}
      </>
    );
  }

  it('fact (a): a plain button under an inert #root refuses focus', () => {
    render(
      <InertRoot>
        <button type="button">Outside</button>
      </InertRoot>,
      { container: getRoot() }
    );
    expect(getRoot(), 'precondition: the ui/Modal made #root inert').toHaveAttribute('inert');
    expect(topDialog(), 'precondition: no showModal() dialog is open').toBeUndefined();
    const outside = screen.getByText('Outside');
    outside.focus();
    expect(document.activeElement).not.toBe(outside);
  });

  it('fact (c): a showModal() dialog inside an inert #root takes focus', () => {
    render(
      <InertRoot>
        <DialogInsideRoot
          label="Nested dialog"
          leadingButtonLabel="Nested first"
          buttonLabel="Nested dialog button"
        />
      </InertRoot>,
      { container: getRoot() }
    );
    expect(getRoot(), 'precondition: the ui/Modal made #root inert').toHaveAttribute('inert');
    const target = screen.getByRole('button', { name: 'Nested dialog button' });
    expect(
      document.activeElement,
      'precondition: the focusing steps focused the first control, not the target'
    ).not.toBe(target);
    target.focus();
    expect(document.activeElement).toBe(target);
  });

  // Only a MODAL dialog escapes an inert ancestor (HTML spec). An open
  // non-modal dialog (show(), or the open attribute) stays inert.
  it('an open non-modal dialog inside an inert #root refuses focus', () => {
    render(
      <InertRoot>
        <dialog open aria-label="Non-modal">
          <button type="button">Non-modal button</button>
        </dialog>
      </InertRoot>,
      { container: getRoot() }
    );
    const button = screen.getByText('Non-modal button');
    button.focus();
    expect(document.activeElement).not.toBe(button);
  });

  describe('dialog methods behave as the HTML spec says', () => {
    const made: HTMLDialogElement[] = [];
    function control(tag: 'a' | 'button', text: string, attrs: Record<string, string> = {}) {
      const el = document.createElement(tag);
      el.textContent = text;
      Object.entries(attrs).forEach(([name, value]) => el.setAttribute(name, value));
      return el;
    }
    function dialog(...children: HTMLElement[]): HTMLDialogElement {
      const d = document.createElement('dialog');
      d.append(...children);
      document.body.append(d);
      made.push(d);
      return d;
    }
    afterEach(() => {
      made.splice(0).forEach((d) => d.remove());
    });

    it('showModal() on a dialog that is already modal changes nothing', () => {
      const a = dialog(control('button', 'a'));
      const b = dialog(control('button', 'b'));
      a.showModal();
      b.showModal();
      a.showModal();
      expect(topDialog()).toBe(b);
    });

    it('close() on a closed dialog fires no close event', async () => {
      const d = dialog(control('button', 'd'));
      const onClose = vi.fn();
      d.addEventListener('close', onClose);
      d.close();
      await Promise.resolve();
      expect(onClose).not.toHaveBeenCalled();
    });

    it('showModal() focuses the first focusable control, skipping disabled ones', () => {
      const d = dialog(
        control('button', 'off', { disabled: '' }),
        control('a', 'link', { href: 'https://example.test' }),
        control('button', 'on')
      );
      d.showModal();
      expect(document.activeElement).toBe(d.querySelector('a'));
    });
  });
});

describe('global overlay reachability while Settings + a ui/Modal are open (regression)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('ForceUpdateOverlay action button — regression: global overlay unreachable over Settings/ui/Modal', () => {
    const onClose = vi.fn();

    render(
      <>
        <SettingsStandIn>
          <Modal isOpen onClose={onClose} title="Reset TOTP">
            <button type="button">Disable</button>
          </Modal>
        </SettingsStandIn>
        <button type="button">Outside</button>
        <ForceUpdateOverlay />
      </>,
      { container: getRoot() }
    );

    const disableButton = screen.getByRole('button', { name: 'Disable' });
    disableButton.focus();
    expect(
      document.activeElement,
      'the underlying Reset TOTP modal button must remain focusable before the overlay opens'
    ).toBe(disableButton);

    const outsideButton = screen.getByRole('button', { name: 'Outside' });
    outsideButton.focus();
    expect(
      document.activeElement,
      'a plain element inside #root outside any dialog must not be focusable while a modal makes #root inert'
    ).not.toBe(outsideButton);

    act(() => {
      useAttestationFailureStore.getState().showFailure({
        code: 'CLIENT_VERSION_TOO_OLD',
      });
    });

    const actionButton = screen.getByRole('button', { name: 'Update Now' });

    actionButton.focus();
    expect
      .soft(
        document.activeElement,
        'ForceUpdateOverlay\'s "Update Now" button must be focusable while Settings + a ui/Modal are open'
      )
      .toBe(actionButton);

    const tabNotCancelled = fireEvent.keyDown(actionButton, { key: 'Tab' });
    expect
      .soft(
        tabNotCancelled,
        'Tab pressed inside ForceUpdateOverlay must not be captured by the ui/Modal underneath'
      )
      .toBe(true);

    fireEvent.keyDown(actionButton, { key: 'Escape' });
    expect
      .soft(
        onClose,
        'Escape pressed inside ForceUpdateOverlay must not dismiss the ui/Modal underneath'
      )
      .not.toHaveBeenCalled();
  });

  it('ConnectionLostOverlay action button — regression: global overlay unreachable over Settings/ui/Modal', () => {
    const onClose = vi.fn();

    render(
      <>
        <SettingsStandIn>
          <Modal isOpen onClose={onClose} title="Reset TOTP">
            <button type="button">Disable</button>
          </Modal>
        </SettingsStandIn>
        <button type="button">Outside</button>
        <ConnectionLostOverlay />
      </>,
      { container: getRoot() }
    );

    const disableButton = screen.getByRole('button', { name: 'Disable' });
    disableButton.focus();
    expect(
      document.activeElement,
      'the underlying Reset TOTP modal button must remain focusable before the overlay opens'
    ).toBe(disableButton);

    const outsideButton = screen.getByRole('button', { name: 'Outside' });
    outsideButton.focus();
    expect(
      document.activeElement,
      'a plain element inside #root outside any dialog must not be focusable while a modal makes #root inert'
    ).not.toBe(outsideButton);

    act(() => {
      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: {
          internet: 'ok',
          serverReachable: 'failed',
          tokenValid: 'failed',
          sessionRevoked: false,
          rendererStable: 'ok',
        },
      });
    });

    const actionButton = screen.getByRole('button', { name: 'Retry' });

    actionButton.focus();
    expect
      .soft(
        document.activeElement,
        'ConnectionLostOverlay\'s "Retry" button must be focusable while Settings + a ui/Modal are open'
      )
      .toBe(actionButton);

    const tabNotCancelled = fireEvent.keyDown(actionButton, { key: 'Tab' });
    expect
      .soft(
        tabNotCancelled,
        'Tab pressed inside ConnectionLostOverlay must not be captured by the ui/Modal underneath'
      )
      .toBe(true);

    fireEvent.keyDown(actionButton, { key: 'Escape' });
    expect
      .soft(
        onClose,
        'Escape pressed inside ConnectionLostOverlay must not dismiss the ui/Modal underneath'
      )
      .not.toHaveBeenCalled();
  });

  it('AttestationFailedModal action button — regression: global overlay unreachable over Settings/ui/Modal', () => {
    const onClose = vi.fn();
    const onDismiss = vi.fn();

    const { rerender } = render(
      <>
        <SettingsStandIn>
          <Modal isOpen onClose={onClose} title="Reset TOTP">
            <button type="button">Disable</button>
          </Modal>
        </SettingsStandIn>
        <button type="button">Outside</button>
      </>,
      { container: getRoot() }
    );

    const disableButton = screen.getByRole('button', { name: 'Disable' });
    disableButton.focus();
    expect(
      document.activeElement,
      'the underlying Reset TOTP modal button must remain focusable before the overlay opens'
    ).toBe(disableButton);

    const outsideButton = screen.getByRole('button', { name: 'Outside' });
    outsideButton.focus();
    expect(
      document.activeElement,
      'a plain element inside #root outside any dialog must not be focusable while a modal makes #root inert'
    ).not.toBe(outsideButton);

    rerender(
      <>
        <SettingsStandIn>
          <Modal isOpen onClose={onClose} title="Reset TOTP">
            <button type="button">Disable</button>
          </Modal>
        </SettingsStandIn>
        <button type="button">Outside</button>
        <AttestationFailedModal code="ATTESTATION_REVOKED" onDismiss={onDismiss} />
      </>
    );

    const actionButton = screen.getByRole('button', { name: 'Dismiss' });

    actionButton.focus();
    expect
      .soft(
        document.activeElement,
        'AttestationFailedModal\'s "Dismiss" button must be focusable while Settings + a ui/Modal are open'
      )
      .toBe(actionButton);

    const tabNotCancelled = fireEvent.keyDown(actionButton, { key: 'Tab' });
    expect
      .soft(
        tabNotCancelled,
        'Tab pressed inside AttestationFailedModal must not be captured by the ui/Modal underneath'
      )
      .toBe(true);

    fireEvent.keyDown(actionButton, { key: 'Escape' });
    expect
      .soft(
        onClose,
        'Escape pressed inside AttestationFailedModal must not dismiss the ui/Modal underneath'
      )
      .not.toHaveBeenCalled();
  });

  it('ChangelogModal action button — regression: global overlay unreachable over Settings/ui/Modal', () => {
    const onClose = vi.fn();
    const onDismiss = vi.fn();

    const { rerender } = render(
      <>
        <SettingsStandIn>
          <Modal isOpen onClose={onClose} title="Reset TOTP">
            <button type="button">Disable</button>
          </Modal>
        </SettingsStandIn>
        <button type="button">Outside</button>
      </>,
      { container: getRoot() }
    );

    const disableButton = screen.getByRole('button', { name: 'Disable' });
    disableButton.focus();
    expect(
      document.activeElement,
      'the underlying Reset TOTP modal button must remain focusable before the overlay opens'
    ).toBe(disableButton);

    const outsideButton = screen.getByRole('button', { name: 'Outside' });
    outsideButton.focus();
    expect(
      document.activeElement,
      'a plain element inside #root outside any dialog must not be focusable while a modal makes #root inert'
    ).not.toBe(outsideButton);

    rerender(
      <>
        <SettingsStandIn>
          <Modal isOpen onClose={onClose} title="Reset TOTP">
            <button type="button">Disable</button>
          </Modal>
        </SettingsStandIn>
        <button type="button">Outside</button>
        <ChangelogModal currentVersion="0.2.21" sections={[]} onDismiss={onDismiss} />
      </>
    );

    const actionButton = screen.getByRole('button', { name: /got it/i });

    actionButton.focus();
    expect
      .soft(
        document.activeElement,
        'ChangelogModal\'s "Got it" button must be focusable while Settings + a ui/Modal are open'
      )
      .toBe(actionButton);

    const tabNotCancelled = fireEvent.keyDown(actionButton, { key: 'Tab' });
    expect
      .soft(
        tabNotCancelled,
        'Tab pressed inside ChangelogModal must not be captured by the ui/Modal underneath'
      )
      .toBe(true);

    fireEvent.keyDown(actionButton, { key: 'Escape' });
    expect
      .soft(
        onClose,
        'Escape pressed inside ChangelogModal must not dismiss the ui/Modal underneath'
      )
      .not.toHaveBeenCalled();
  });
});

// Rule 4: a mandatory overlay sets closedby="none", so Escape fires neither
// cancel nor close. This pins that React renders the attribute into the DOM,
// where Chromium reads it (measured by scripts/dialog-top-layer-probe).
describe('mandatory overlays render closedby="none"', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('ForceUpdateOverlay', () => {
    render(<ForceUpdateOverlay />, { container: getRoot() });
    act(() => {
      useAttestationFailureStore.getState().showFailure({ code: 'CLIENT_VERSION_TOO_OLD' });
    });
    expect(screen.getByRole('button', { name: 'Update Now' }).closest('dialog')).toHaveAttribute(
      'closedby',
      'none'
    );
  });

  it('ConnectionLostOverlay', () => {
    render(<ConnectionLostOverlay />, { container: getRoot() });
    act(() => {
      useConnectionStore.setState({
        phase: 'recovery_a',
        diagnostics: {
          internet: 'ok',
          serverReachable: 'failed',
          tokenValid: 'failed',
          sessionRevoked: false,
          rendererStable: 'ok',
        },
      });
    });
    expect(screen.getByRole('button', { name: 'Retry' }).closest('dialog')).toHaveAttribute(
      'closedby',
      'none'
    );
  });
});

// WCAG 4.1.2 and 4.1.3: each overlay dialog is described by its message, and
// the connection-lost message is a live region that survives a phase change,
// so a screen reader hears the new state instead of nothing.
describe('overlay dialogs are described, and connection changes are announced', () => {
  beforeEach(() => {
    resetAllStores();
  });

  function setPhase(phase: 'recovery_a' | 'fatal') {
    act(() => {
      useConnectionStore.setState({
        phase,
        diagnostics: {
          internet: 'ok',
          serverReachable: 'failed',
          tokenValid: 'failed',
          sessionRevoked: false,
          rendererStable: 'ok',
        },
      });
    });
  }

  it('ForceUpdateOverlay is described by its message', () => {
    render(<ForceUpdateOverlay />, { container: getRoot() });
    act(() => {
      useAttestationFailureStore.getState().showFailure({ code: 'CLIENT_VERSION_TOO_OLD' });
    });
    expect(
      screen.getByRole('button', { name: 'Update Now' }).closest('dialog')
    ).toHaveAccessibleDescription(/requires an update|below the minimum required version/);
  });

  it('AttestationFailedModal is described by its message', () => {
    render(<AttestationFailedModal code="ATTESTATION_REVOKED" onDismiss={vi.fn()} />, {
      container: getRoot(),
    });
    expect(
      screen.getByRole('button', { name: 'Dismiss' }).closest('dialog')
    ).toHaveAccessibleDescription(/requires an official Concord Voice client/);
  });

  it('ConnectionLostOverlay is described by its message and announces a phase change in place', () => {
    render(<ConnectionLostOverlay />, { container: getRoot() });
    setPhase('recovery_a');
    const status = screen.getByRole('status');
    expect(
      screen.getByRole('button', { name: 'Retry' }).closest('dialog')
    ).toHaveAccessibleDescription(status.textContent ?? '');

    setPhase('fatal');
    expect(
      screen.getByRole('status'),
      'the live region must be the same node, or the new state is not announced'
    ).toBe(status);
    expect(status).toHaveTextContent(/Unable to restore your connection/);
  });
});

describe('overlay dialog details', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it.each(['preflight', 'recovery_a', 'recovery_b', 'fatal'] as const)(
    'ConnectionLostOverlay is named by its heading in the %s phase',
    (phase) => {
      render(<ConnectionLostOverlay />, { container: getRoot() });
      act(() => {
        useConnectionStore.setState({
          phase,
          diagnostics: {
            internet: 'ok',
            serverReachable: 'failed',
            tokenValid: 'failed',
            sessionRevoked: false,
            rendererStable: 'ok',
          },
        });
      });
      const heading = screen.getByRole('heading');
      expect(screen.getByRole('dialog')).toHaveAccessibleName(heading.textContent ?? '');
    }
  );

  // The dismissible overlays must stay dismissible: closedby="none" copied
  // onto them would swallow Escape, and their close handlers would never run.
  it.each([
    [
      'AttestationFailedModal',
      () => <AttestationFailedModal code="ATTESTATION_REVOKED" onDismiss={vi.fn()} />,
    ],
    [
      'ChangelogModal',
      () => (
        <ChangelogModal
          currentVersion="0.2.47"
          sections={[
            { version: '0.2.47', label: '0.2.47', date: '2026-09-24', body: 'x', preamble: '' },
          ]}
          onDismiss={vi.fn()}
        />
      ),
    ],
  ] as const)('%s does not set closedby', (_name, Overlay) => {
    render(<Overlay />, { container: getRoot() });
    expect(screen.getByRole('dialog')).not.toHaveAttribute('closedby');
  });

  it("AttestationFailedModal's close event dismisses it", async () => {
    const onDismiss = vi.fn();
    render(<AttestationFailedModal code="ATTESTATION_REVOKED" onDismiss={onDismiss} />, {
      container: getRoot(),
    });
    (screen.getByRole('dialog') as HTMLDialogElement).close();
    await Promise.resolve(); // Chromium queues the close event.
    expect(onDismiss).toHaveBeenCalledOnce();
  });

  // showModal() focuses the first focusable control (here a link in the notes);
  // ChangelogModal then moves focus to "Got it". That only works if its focus
  // effect runs AFTER the hook's showModal(): before it, the dialog is closed
  // and refuses focus, and showModal() leaves focus on the link.
  it('ChangelogModal puts initial focus on "Got it", after showModal()', () => {
    render(
      <ChangelogModal
        currentVersion="0.2.47"
        sections={[
          {
            version: '0.2.47',
            label: '0.2.47',
            date: '2026-09-24',
            body: 'See [the notes](https://concordvoice.com/notes).',
            preamble: '',
          },
        ]}
        onDismiss={vi.fn()}
      />,
      { container: getRoot() }
    );
    expect(
      screen.getAllByRole('link').length,
      'precondition: a focusable link precedes the button'
    ).toBeGreaterThan(0);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /got it/i }));
  });
});
