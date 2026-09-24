import { render, screen, fireEvent, act } from '../../../test-utils';
import { vi } from 'vitest';
import { useMFAChallengeStore } from '@/renderer/stores/auth/mfaChallengeStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import Modal from '@/renderer/components/ui/Modal';
import MFAChallengeModal from '@/renderer/components/Auth/MFAChallengeModal';
import AttestationFailedModalHost from '@/renderer/components/AttestationFailedModal';
import { useAttestationFailureStore } from '@/renderer/stores/auth/attestationFailureStore';
import {
  installTopLayerEmulation,
  installRootHarness,
  SettingsStandIn,
  topDialog,
} from '../../../helpers/topLayerEmulation';

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

// jsdom-gap emulation (showModal()/inert focus blocking) and the #root /
// SettingsStandIn harness are shared with the rest of the "global overlay
// reachable over Settings" regression family — see
// tests/helpers/topLayerEmulation.tsx and [internal]rules/frontend.md § "A
// global overlay must be reachable over the Settings dialog".
installTopLayerEmulation();
const getRoot = installRootHarness();

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
      { container: getRoot() }
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
      { container: getRoot() }
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
      { container: getRoot() }
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
      { container: getRoot() }
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
      { container: getRoot() }
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
    // The shared emulation's showModal() runs the dialog focusing steps, as Chromium does,
    // so the burying dialog has taken focus before the observer's callback.
    render(<MFAChallengeModal />, { container: getRoot() });
    openChallenge();
    const typing = screen.getByLabelText('Digit 3');
    typing.focus();

    const burying = document.createElement('dialog');
    burying.append(document.createElement('button'));
    document.body.append(burying);
    try {
      act(() => {
        burying.showModal();
      });
      expect(document.activeElement, 'the burying dialog took focus first').toBe(
        burying.querySelector('button')
      );
      await flushObservers();

      expect(document.activeElement, 'focus must return to the digit being typed').toBe(typing);
    } finally {
      burying.remove(); // a failed assertion must not leak an open modal into the next test
    }
  });

  it('stops watching once the challenge settles', async () => {
    render(
      <>
        <MFAChallengeModal />
        <dialog id="later">later</dialog>
      </>,
      { container: getRoot() }
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
      { container: getRoot() }
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
      { container: getRoot() }
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
    render(<MFAChallengeModal />, { container: getRoot() });
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
    render(<MFAChallengeModal />, { container: getRoot() });
    openChallenge();
    expect(getRoot().hasAttribute('inert')).toBe(true);

    act(() => {
      useMFAChallengeStore.setState({ challengeToken: '' });
    });
    expect(document.querySelector('dialog.mfa-challenge-dialog')).toBeNull();
    expect(getRoot().hasAttribute('inert'), 'nothing is on screen, so nothing may be inert').toBe(
      false
    );
  });

  it('a real token after an empty one is shown, not rendered into a closed dialog', () => {
    render(<MFAChallengeModal />, { container: getRoot() });
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
    render(<MFAChallengeModal />, { container: getRoot() });
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
