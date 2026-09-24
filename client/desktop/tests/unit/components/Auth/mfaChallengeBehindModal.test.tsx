import React, { useCallback, useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { vi } from 'vitest';
import { useMFAChallengeStore } from '@/renderer/stores/auth/mfaChallengeStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import Modal from '@/renderer/components/ui/Modal';
import { ModalPortalHostContext } from '@/renderer/components/ui/ModalContext';
import MFAChallengeModal from '@/renderer/components/Auth/MFAChallengeModal';
import AttestationFailedModalHost from '@/renderer/components/AttestationFailedModal';
import { useAttestationFailureStore } from '@/renderer/stores/auth/attestationFailureStore';

// MFAChallengeModal reaches these services at import/verify time; mock only
// what's needed to import cleanly (mirrors MFAChallengeModal.test.tsx). This
// regression test never submits a code, so none of these are actually
// invoked — they exist purely so the module graph resolves in jsdom.
vi.mock('@/renderer/services/system/ssoService', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/ssoService')>();
  return { ...actual, completeSSOMFA: vi.fn(), abandonSSOReservation: vi.fn() };
});

vi.mock('@/renderer/services/system/apiClient', () => ({
  API_BASE: 'http://localhost:8080',
  ensureMachineId: vi.fn().mockResolvedValue('mock-machine-id'),
  safeJson: async <T,>(res: { json: () => Promise<T> }): Promise<T> => res.json(),
}));

// ── jsdom gap emulation ──────────────────────────────────────────────────
// jsdom implements neither the `inert` attribute's focus-blocking behavior
// nor the browser's top-layer / showModal() modality. Both are load-bearing
// for this regression: ModalContext's syncInert sets `inert` on #root and on
// non-topmost modal overlays, and SettingsOverlayHost opens its <dialog> with
// showModal(), which in a real browser blocks focus/Tab/Escape from reaching
// anything outside the top-layer dialog. Without emulating both rules here,
// every assertion in this file would trivially pass regardless of whether the
// production bug is present.
let dialogStack: HTMLDialogElement[];
let originalShowModal: typeof HTMLDialogElement.prototype.showModal;
let originalFocus: typeof HTMLElement.prototype.focus;

function topDialog(): HTMLDialogElement | undefined {
  for (let i = dialogStack.length - 1; i >= 0; i--) {
    const d = dialogStack[i];
    if (d.isConnected && d.open) return d;
  }
  return undefined;
}

function isFocusBlocked(el: HTMLElement): boolean {
  // (a) an [inert] ancestor-or-self blocks focus, but a modal dialog escapes
  // an inert ancestor (whatwg/html#7808): only an inert found between the
  // element and its nearest open modal dialog counts
  const inert = el.closest('[inert]');
  const modal = el.closest('dialog');
  if (inert && !(modal?.matches(':modal') && !modal.contains(inert))) return true;
  // (a2) a closed <dialog> is not rendered, so nothing inside it can take
  // focus (React's autoFocus fires before the effect that opens the dialog)
  if (el.closest('dialog:not([open])')) return true;
  // (b) while any showModal()-opened <dialog> is open and connected, every
  // element NOT inside the most-recently-shown such dialog is blocked
  const top = topDialog();
  if (top && !top.contains(el)) return true;
  return false;
}

beforeEach(() => {
  dialogStack = [];
  originalShowModal = HTMLDialogElement.prototype.showModal;
  originalFocus = HTMLElement.prototype.focus;

  // A showModal() adds the dialog at the TOP of the top layer, including one
  // that was already in it (HTML "add an element to the top layer").
  vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (
    this: HTMLDialogElement
  ) {
    originalShowModal.call(this);
    const i = dialogStack.indexOf(this);
    if (i !== -1) dialogStack.splice(i, 1);
    dialogStack.push(this);
    // The dialog focusing steps: showModal() moves focus into the dialog.
    this.querySelector<HTMLElement>('input, button, [tabindex]:not([tabindex="-1"])')?.focus();
  });
  // Chromium QUEUES the close event (HTML "close the dialog"); the setup.ts
  // polyfill fires it synchronously, which no browser does.
  vi.spyOn(HTMLDialogElement.prototype, 'close').mockImplementation(function (
    this: HTMLDialogElement
  ) {
    this.removeAttribute('open');
    queueMicrotask(() => this.dispatchEvent(new Event('close')));
  });

  vi.spyOn(HTMLElement.prototype, 'focus').mockImplementation(function (
    this: HTMLElement,
    ...args: Parameters<typeof HTMLElement.prototype.focus>
  ) {
    if (isFocusBlocked(this)) return;
    originalFocus.apply(this, args);
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

// ── #root harness ────────────────────────────────────────────────────────
// ModalContext's syncInert targets document.getElementById('root'), which
// test-utils' default RTL container does not provide.
let root: HTMLDivElement;

beforeEach(() => {
  root = document.createElement('div');
  root.id = 'root';
  document.body.appendChild(root);
});

afterEach(() => {
  root.remove();
});

// ── S1 stand-in for SettingsOverlayHost ─────────────────────────────────
// A minimal native-<dialog> host that mirrors SettingsOverlayHost's shape
// (callback ref -> useState -> ModalPortalHostContext), WITHOUT its
// jsdom-only Escape fallback (SettingsOverlayHost only installs a manual
// Escape listener when `dialogCancelsOnEscape` is false, which would muddy
// the Escape assertion this test cares about — the real
// showModal()-opened <dialog> already owns Escape in a real browser).
function SettingsStandIn({ children }: { children: React.ReactNode }) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);
  const [portalHost, setPortalHost] = useState<HTMLDialogElement | null>(null);
  const setDialogRef = useCallback((dialog: HTMLDialogElement | null) => {
    dialogRef.current = dialog;
    setPortalHost(dialog);
  }, []);

  useEffect(() => {
    const dlg = dialogRef.current;
    if (dlg && !dlg.open) dlg.showModal();
  }, [portalHost]);

  return createPortal(
    <dialog ref={setDialogRef} aria-label="Settings">
      {/* eslint-disable-next-line @eslint-react/no-context-provider -- test stand-in mirrors SettingsOverlayHost's own provider usage */}
      <ModalPortalHostContext.Provider value={portalHost}>
        {portalHost ? children : null}
      </ModalPortalHostContext.Provider>
    </dialog>,
    document.body
  );
}

function openChallenge() {
  act(() => {
    useMFAChallengeStore.setState({
      challengeToken: 'tok',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      purpose: 'suspicious_refresh',
    });
  });
}

describe('MFA challenge reachability while a ui/Modal is open (regression)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('challenge opened over Settings+Reset TOTP is focusable and owns Escape/Tab — regression: MFA upgrade challenge deadlock', () => {
    const onClose = vi.fn();

    render(
      <>
        <SettingsStandIn>
          <Modal isOpen onClose={onClose} title="Reset TOTP">
            <button type="button">Disable</button>
          </Modal>
        </SettingsStandIn>
        <button type="button">Outside</button>
        <MFAChallengeModal />
      </>,
      { container: root }
    );

    // Positive controls first — prove the emulation is not blocking
    // everything, and does block what it should, BEFORE the challenge opens.
    const disableButton = screen.getByRole('button', { name: 'Disable' });
    disableButton.focus();
    expect(
      document.activeElement,
      'the underlying Reset TOTP modal button must remain focusable before the MFA challenge opens'
    ).toBe(disableButton);

    const outsideButton = screen.getByRole('button', { name: 'Outside' });
    outsideButton.focus();
    expect(
      document.activeElement,
      'a plain element inside #root outside any dialog must not be focusable while a modal makes #root inert'
    ).not.toBe(outsideButton);

    openChallenge();

    screen.getByText('Verify Your Identity');
    const input = screen.getByLabelText('Digit 1');

    input.focus();
    expect
      .soft(document.activeElement, 'challenge input must be focusable while a ui/Modal is open')
      .toBe(input);

    const tabNotCancelled = fireEvent.keyDown(input, { key: 'Tab' });
    expect
      .soft(
        tabNotCancelled,
        'Tab pressed inside the MFA challenge must not be captured by the modal underneath'
      )
      .toBe(true);

    fireEvent.keyDown(input, { key: 'Escape' });
    expect
      .soft(onClose, 'Escape inside the challenge must not dismiss the modal underneath')
      .not.toHaveBeenCalled();
  });
});

describe('MFA challenge reachability while a plain ui/Modal is open (no Settings)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('challenge opened over a plain body-level ui/Modal is focusable and owns Escape/Tab — regression: MFA upgrade challenge deadlock', () => {
    const onClose = vi.fn();

    render(
      <>
        <Modal isOpen onClose={onClose} title="Reset TOTP">
          <button type="button">Disable</button>
        </Modal>
        <MFAChallengeModal />
      </>,
      { container: root }
    );

    const disableButton = screen.getByRole('button', { name: 'Disable' });
    disableButton.focus();
    expect(
      document.activeElement,
      'the underlying modal button must remain focusable before the MFA challenge opens'
    ).toBe(disableButton);

    openChallenge();

    screen.getByText('Verify Your Identity');
    const input = screen.getByLabelText('Digit 1');

    input.focus();
    expect
      .soft(document.activeElement, 'challenge input must be focusable while a ui/Modal is open')
      .toBe(input);

    const tabNotCancelled = fireEvent.keyDown(input, { key: 'Tab' });
    expect
      .soft(
        tabNotCancelled,
        'Tab pressed inside the MFA challenge must not be captured by the modal underneath'
      )
      .toBe(true);

    fireEvent.keyDown(input, { key: 'Escape' });
    expect
      .soft(onClose, 'Escape inside the challenge must not dismiss the modal underneath')
      .not.toHaveBeenCalled();
  });
});

// ── A dialog opened AFTER the challenge (#3423 red-team) ────────────────────
// The top layer is last-showModal()-wins, so anything shown after the challenge
// buries it again — Settings from Ctrl/Cmd+, or a network-driven attestation
// failure. The request is still waiting on the challenge, so it must take the
// top back.
describe('MFA challenge stays on top of a dialog opened after it', () => {
  beforeEach(() => {
    resetAllStores();
    useAttestationFailureStore.getState().dismiss();
  });

  async function flushObservers() {
    await act(async () => {
      await Promise.resolve();
    });
  }

  it('retakes the top layer from a network-driven dialog and stays focusable', async () => {
    render(
      <>
        <MFAChallengeModal />
        <AttestationFailedModalHost />
      </>,
      { container: root }
    );
    openChallenge();
    const challenge = document.querySelector('dialog.mfa-challenge-dialog') as HTMLDialogElement;
    const showModal = vi.mocked(HTMLDialogElement.prototype.showModal);
    const challengeShows = () => showModal.mock.contexts.filter((d) => d === challenge).length;
    const shownBefore = challengeShows();
    let closes = 0;
    challenge.addEventListener('close', () => {
      closes += 1;
    });

    act(() => {
      useAttestationFailureStore.getState().showFailure({ code: 'ATTESTATION_REVOKED' });
    });
    await flushObservers();
    await flushObservers();

    const attestation = [...document.querySelectorAll('dialog')].find((d) => d !== challenge);
    expect(attestation?.open, 'the attestation dialog must actually have opened').toBe(true);
    expect(challengeShows(), 'the retake shows the challenge exactly once more').toBe(
      shownBefore + 1
    );
    expect(closes, "the retake's queued close event must arrive").toBeGreaterThanOrEqual(1);
    expect(topDialog(), 'the challenge must be the topmost modal dialog').toBe(challenge);
    const input = screen.getByLabelText('Digit 1');
    input.focus();
    expect(document.activeElement).toBe(input);
    expect(
      useMFAChallengeStore.getState().challengeToken,
      'retaking the top must not cancel the challenge'
    ).toBe('tok');
  });

  it('ignores <details> toggling, which shares the open attribute', async () => {
    render(
      <>
        <MFAChallengeModal />
        <details>
          <summary>More</summary>
        </details>
      </>,
      { container: root }
    );
    openChallenge();
    const showModal = vi.mocked(HTMLDialogElement.prototype.showModal);
    const shown = showModal.mock.calls.length;

    act(() => {
      document.querySelector('details')?.setAttribute('open', '');
    });
    await flushObservers();

    expect(showModal.mock.calls.length, 'a <details> is not a dialog').toBe(shown);
  });

  it('ignores a non-modal dialog, which does not enter the top layer', async () => {
    render(
      <>
        <MFAChallengeModal />
        <dialog id="non-modal">note</dialog>
      </>,
      { container: root }
    );
    openChallenge();
    const showModal = vi.mocked(HTMLDialogElement.prototype.showModal);
    const shown = showModal.mock.calls.length;

    act(() => {
      // What show() does: open, but not modal.
      document.getElementById('non-modal')?.setAttribute('open', '');
    });
    await flushObservers();

    expect(showModal.mock.calls.length, 'a non-modal dialog buries nothing').toBe(shown);
  });

  it('keeps focus on the control the user was typing in when it retakes the top', async () => {
    // The file's showModal() runs the dialog focusing steps, as Chromium does,
    // so the burying dialog has taken focus before the observer's callback.
    render(<MFAChallengeModal />, { container: root });
    openChallenge();
    const typing = screen.getByLabelText('Digit 3');
    typing.focus();

    const burying = document.createElement('dialog');
    burying.append(document.createElement('button'));
    document.body.append(burying);
    act(() => {
      burying.showModal();
    });
    expect(document.activeElement, 'the burying dialog took focus first').toBe(
      burying.querySelector('button')
    );
    await flushObservers();

    expect(document.activeElement, 'focus must return to the digit being typed').toBe(typing);
    burying.remove();
  });

  it('stops watching once the challenge settles', async () => {
    render(
      <>
        <MFAChallengeModal />
        <dialog id="later">later</dialog>
      </>,
      { container: root }
    );
    openChallenge();
    act(() => {
      useMFAChallengeStore.getState().clearChallenge();
    });
    const showModal = vi.mocked(HTMLDialogElement.prototype.showModal);
    const shown = showModal.mock.calls.length;

    act(() => {
      (document.getElementById('later') as HTMLDialogElement).showModal();
    });
    await flushObservers();

    expect(showModal.mock.calls.length, 'only the later dialog is shown').toBe(shown + 1);
  });

  it('returns focus to each challenge its own invoker, not an earlier one', () => {
    render(
      <>
        <button type="button">first</button>
        <button type="button">second</button>
        <MFAChallengeModal />
      </>,
      { container: root }
    );
    const first = screen.getByRole('button', { name: 'first' });
    const second = screen.getByRole('button', { name: 'second' });
    first.focus();
    openChallenge();
    act(() => {
      useMFAChallengeStore.getState().clearChallenge();
    });
    expect(document.activeElement).toBe(first);

    second.focus();
    act(() => {
      void useMFAChallengeStore.getState().showChallenge('tok-3', ['totp'], 'suspicious_refresh');
    });
    act(() => {
      useMFAChallengeStore.getState().clearChallenge();
    });
    expect(document.activeElement, 'the second challenge returns to its own invoker').toBe(second);
  });

  it('returns focus to the element the user was on, across a replacement challenge', () => {
    render(
      <>
        <button type="button">invoker</button>
        <MFAChallengeModal />
      </>,
      { container: root }
    );
    const invoker = screen.getByRole('button', { name: 'invoker' });
    invoker.focus();
    openChallenge();
    screen.getByLabelText('Digit 1').focus();

    act(() => {
      void useMFAChallengeStore.getState().showChallenge('tok-2', ['totp'], 'suspicious_refresh');
    });
    act(() => {
      useMFAChallengeStore.getState().clearChallenge();
    });

    expect(document.activeElement).toBe(invoker);
  });
});

// ── One definition of "a challenge is open" (#3423 red-team) ─────────────
describe('focus after a failed proof', () => {
  beforeEach(() => {
    resetAllStores();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  // A proof in flight disables the digits, and Chromium drops focus to <body>.
  // Without a refocus, keys after a wrong code come from outside the dialog
  // and the next digits typed are lost (#3423 red-team).
  it('puts focus back on a code input when the proof fails', async () => {
    let failProof!: () => void;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        () =>
          new Promise((resolve) => {
            failProof = () =>
              resolve({ ok: false, json: async () => ({ error: 'Invalid MFA code' }) });
          })
      )
    );
    render(<MFAChallengeModal />, { container: root });
    openChallenge();
    const first = screen.getByLabelText('Digit 1');
    fireEvent.paste(first, { clipboardData: { getData: () => '123456' } });
    await act(async () => {
      await Promise.resolve();
    });
    // Chromium's focus fixup moves focus to <body> when the focused control is
    // disabled; jsdom has no fixup and refuses blur() on a disabled element,
    // so lift the attribute for the blur and put it back.
    const focused = document.activeElement as HTMLInputElement;
    expect(focused.disabled, 'the proof in flight disables the digits').toBe(true);
    focused.disabled = false;
    focused.blur();
    focused.disabled = true;
    expect(document.activeElement).toBe(document.body);

    await act(async () => {
      failProof();
      await Promise.resolve();
      await Promise.resolve();
    });

    const dialog = document.querySelector('dialog.mfa-challenge-dialog') as HTMLDialogElement;
    expect(dialog.contains(document.activeElement), 'focus must be back inside the challenge').toBe(
      true
    );
    expect(document.activeElement?.tagName).toBe('INPUT');
  });
});

describe('an empty challenge token is no challenge', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('a live challenge replaced by an empty token leaves nothing inert', () => {
    render(<MFAChallengeModal />, { container: root });
    openChallenge();
    expect(root.hasAttribute('inert')).toBe(true);

    act(() => {
      useMFAChallengeStore.setState({ challengeToken: '' });
    });
    expect(document.querySelector('dialog.mfa-challenge-dialog')).toBeNull();
    expect(root.hasAttribute('inert'), 'nothing is on screen, so nothing may be inert').toBe(false);
  });

  it('a real token after an empty one is shown, not rendered into a closed dialog', () => {
    render(<MFAChallengeModal />, { container: root });
    act(() => {
      useMFAChallengeStore.setState({
        challengeToken: '',
        methods: ['totp'],
        recoveryOnlyMethods: [],
        purpose: 'suspicious_refresh',
      });
    });
    openChallenge();
    const dialog = document.querySelector('dialog.mfa-challenge-dialog') as HTMLDialogElement;
    expect(dialog.open, 'the challenge must be on screen').toBe(true);
  });
});

// ── Default method for a challenge (#3423) ───────────────────────────────
// The modal is mounted before any challenge arrives, so the default for EVERY
// challenge comes from the per-token reset, not from the first render.
describe('the default verification method', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('is never a recovery-only method', () => {
    render(<MFAChallengeModal />, { container: root });
    act(() => {
      void useMFAChallengeStore
        .getState()
        .showChallenge('tok', ['webauthn', 'totp'], 'suspicious_refresh', ['webauthn']);
    });
    expect(
      screen.getByText('Enter the 6-digit code from your authenticator app'),
      'webauthn is recovery-only here, so TOTP must be the default'
    ).toBeInTheDocument();
  });
});
