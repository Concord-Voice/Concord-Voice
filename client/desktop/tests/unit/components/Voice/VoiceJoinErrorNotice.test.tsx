import { render, screen, fireEvent, act } from '../../../test-utils';
import VoiceJoinErrorNotice from '@/renderer/components/Voice/VoiceJoinErrorNotice';
import Modal from '@/renderer/components/ui/Modal';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import {
  installTopLayerEmulation,
  installRootHarness,
  SettingsStandIn,
  topDialog,
} from '../../../helpers/topLayerEmulation';

// Emulate native modality and inert focus blocking so an ordinary in-page
// notice cannot pass the reachability and focus restoration regressions.
installTopLayerEmulation();
const getRoot = installRootHarness();

const microphoneError =
  'The selected microphone is not available. Choose another microphone, then try again.';

function setJoinError(message: string) {
  act(() => useVoiceStore.getState().setJoinError(message));
}

describe('VoiceJoinErrorNotice', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders nothing when there is no voice join error', () => {
    render(<VoiceJoinErrorNotice />, { container: getRoot() });

    expect(screen.queryByRole('dialog', { name: 'Unable to join voice' })).not.toBeInTheDocument();
    expect(HTMLDialogElement.prototype.showModal).not.toHaveBeenCalled();
  });

  it('opens a native modal dialog with the error as its accessible description', () => {
    render(<VoiceJoinErrorNotice />, { container: getRoot() });
    setJoinError(microphoneError);

    const dialog = screen.getByRole('dialog', { name: 'Unable to join voice' });
    expect(dialog).toHaveAccessibleDescription(microphoneError);
    expect(dialog).toHaveTextContent(microphoneError);
    expect(dialog.matches(':modal')).toBe(true);
    expect(topDialog()).toBe(dialog);
    expect(screen.getByRole('button', { name: 'Dismiss' })).toHaveFocus();
  });

  it('Dismiss clears the join error and lets a later error open a new dialog', () => {
    render(<VoiceJoinErrorNotice />, { container: getRoot() });
    setJoinError(microphoneError);
    screen.getByRole('dialog', { name: 'Unable to join voice' });

    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));

    expect(useVoiceStore.getState().joinError).toBeNull();
    expect(screen.queryByRole('dialog', { name: 'Unable to join voice' })).not.toBeInTheDocument();

    const nextError = 'Microphone permission was denied. Allow microphone access, then try again.';
    setJoinError(nextError);

    const nextDialog = screen.getByRole('dialog', { name: 'Unable to join voice' });
    expect(nextDialog).toHaveAccessibleDescription(nextError);
    expect(nextDialog.matches(':modal')).toBe(true);
    expect(topDialog()).toBe(nextDialog);
  });

  it('leaves programmatic native cancel unprevented and clears the error when the browser closes the dialog', async () => {
    render(<VoiceJoinErrorNotice />, { container: getRoot() });
    setJoinError(microphoneError);
    const dialog = screen.getByRole('dialog', {
      name: 'Unable to join voice',
    }) as HTMLDialogElement;

    const notCancelled = fireEvent(dialog, new Event('cancel', { cancelable: true }));

    expect(notCancelled).toBe(true);
    expect(useVoiceStore.getState().joinError).toBe(microphoneError);

    // jsdom does not implement the unprevented cancel event's native close action.
    await act(async () => {
      dialog.close();
      await Promise.resolve();
    });

    expect(useVoiceStore.getState().joinError).toBeNull();
    expect(screen.queryByRole('dialog', { name: 'Unable to join voice' })).not.toBeInTheDocument();
  });

  it('closing the native dialog clears the store so the error cannot remain hidden', async () => {
    render(<VoiceJoinErrorNotice />, { container: getRoot() });
    setJoinError(microphoneError);
    const dialog = screen.getByRole('dialog', {
      name: 'Unable to join voice',
    }) as HTMLDialogElement;

    await act(async () => {
      dialog.close();
      await Promise.resolve(); // Chromium queues the native close event.
    });

    expect(useVoiceStore.getState().joinError).toBeNull();
    expect(screen.queryByRole('dialog', { name: 'Unable to join voice' })).not.toBeInTheDocument();
  });

  it('a queued close from an earlier opening does not dismiss a reopened dialog', async () => {
    render(<VoiceJoinErrorNotice />, { container: getRoot() });
    setJoinError(microphoneError);
    const dialog = screen.getByRole('dialog', {
      name: 'Unable to join voice',
    }) as HTMLDialogElement;
    const nextError = 'Microphone permission was denied. Allow microphone access, then try again.';

    await act(async () => {
      dialog.close();
      dialog.showModal();
      useVoiceStore.getState().setJoinError(nextError);
      await Promise.resolve(); // The earlier close event arrives after reopening.
    });

    expect(useVoiceStore.getState().joinError).toBe(nextError);
    expect(
      screen.getByRole('dialog', { name: 'Unable to join voice' })
    ).toHaveAccessibleDescription(nextError);
    expect(dialog.matches(':modal')).toBe(true);
    expect(topDialog()).toBe(dialog);
  });

  it('returns focus to the control that was focused before the error opened', () => {
    render(
      <>
        <button type="button">Join voice</button>
        <VoiceJoinErrorNotice />
      </>,
      { container: getRoot() }
    );
    const joinButton = screen.getByRole('button', { name: 'Join voice' });
    joinButton.focus();
    expect(joinButton).toHaveFocus();

    setJoinError(microphoneError);
    expect(screen.getByRole('button', { name: 'Dismiss' })).toHaveFocus();

    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));

    expect(joinButton).toHaveFocus();
  });

  it.each([false, true])(
    'Escape dismisses only the reachable notice over a ui/Modal (inside Settings: %s)',
    (insideSettings) => {
      const onModalClose = vi.fn();
      const underlyingModal = (
        <Modal isOpen onClose={onModalClose} title="Audio settings">
          <button type="button">Choose microphone</button>
        </Modal>
      );
      render(
        <>
          {insideSettings ? <SettingsStandIn>{underlyingModal}</SettingsStandIn> : underlyingModal}
          <VoiceJoinErrorNotice />
        </>,
        { container: getRoot() }
      );
      const chooseButton = screen.getByRole('button', { name: 'Choose microphone' });
      chooseButton.focus();
      expect(chooseButton).toHaveFocus();

      setJoinError(microphoneError);
      const dialog = screen.getByRole('dialog', {
        name: 'Unable to join voice',
      }) as HTMLDialogElement;
      const dismissButton = screen.getByRole('button', { name: 'Dismiss' });
      expect(topDialog()).toBe(dialog);
      expect(dismissButton).toHaveFocus();
      chooseButton.focus();
      expect(dismissButton).toHaveFocus();

      expect(fireEvent.keyDown(dismissButton, { key: 'Tab' })).toBe(true);
      const onDocumentKeyDown = vi.fn();
      document.addEventListener('keydown', onDocumentKeyDown);
      try {
        const escape = new KeyboardEvent('keydown', {
          key: 'Escape',
          bubbles: true,
          cancelable: true,
        });
        fireEvent(dismissButton, escape);

        // Async dialogs may share Chromium close watchers with Settings. Preventing
        // native Escape stops the browser from closing the underlying dialog too.
        expect(escape.defaultPrevented).toBe(true);
        expect(onDocumentKeyDown).not.toHaveBeenCalled();
        expect(onModalClose).not.toHaveBeenCalled();
        expect(useVoiceStore.getState().joinError).toBeNull();
        expect(
          screen.queryByRole('dialog', { name: 'Unable to join voice' })
        ).not.toBeInTheDocument();
        expect(screen.getByRole('dialog', { name: 'Audio settings' })).toBeInTheDocument();
        if (insideSettings) {
          expect(screen.getByRole('dialog', { name: 'Settings' }).matches(':modal')).toBe(true);
        }
        expect(chooseButton).toHaveFocus();

        // Positive control: the underlying modal owns Escape again after dismissal.
        fireEvent.keyDown(chooseButton, { key: 'Escape' });
        expect(onModalClose).toHaveBeenCalledOnce();
        // The modal consumes Escape in capture, so use an ordinary key to prove
        // the document probe can observe bubbling events after the notice closes.
        fireEvent.keyDown(chooseButton, { key: 'Enter' });
        expect(onDocumentKeyDown).toHaveBeenCalledOnce();
      } finally {
        document.removeEventListener('keydown', onDocumentKeyDown);
      }
    }
  );
});
