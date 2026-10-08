import { useState } from 'react';
import { render, screen, fireEvent, waitFor, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import { mockServer } from '../../../mocks/fixtures';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import DeleteServerModal from '@/renderer/components/Servers/DeleteServerModal';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
  FIXTURE_OTP_2,
  GATED_API_BASE,
  INVALID_CODE,
  SETUP_LINK,
  stubGatedRoute,
  stubStepUpRead,
} from '../../../helpers/gatedRoute';

const API_BASE = 'http://localhost:8080';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const getConfirmBtn = () =>
  document.querySelector<HTMLButtonElement>('button.delete-server-confirm-btn')!;

describe('DeleteServerModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useServerStore.getState().addServer(mockServer);
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <DeleteServerModal isOpen={false} server={mockServer} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders warning message with server name', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    expect(screen.getByText(/cannot be undone/)).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Test Server')).toBeInTheDocument();
  });

  it('disables delete button until name is confirmed', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    expect(getConfirmBtn()).toBeDisabled();

    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    expect(getConfirmBtn()).not.toBeDisabled();
  });

  it('does not delete with wrong confirmation name', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Wrong Name' },
    });
    expect(getConfirmBtn()).toBeDisabled();
  });

  it('deletes server on confirmation', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1`, () =>
        HttpResponse.json({ message: 'Deleted' })
      )
    );

    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(useServerStore.getState().servers).toHaveLength(0);
  });

  it('shows error on API failure', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/servers/server-1`, () =>
        HttpResponse.json({ error: 'Forbidden' }, { status: 403 })
      )
    );

    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(screen.getByText('Forbidden')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<DeleteServerModal isOpen={true} server={mockServer} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});

// The delete swap (#3456 §3.3, §3.4): a server that enforces MFA refuses the
// code-less DELETE, and the confirmation gives way to the shared step-up dialog.
// The dialog, the factor hook and the adapter are real; only the network is
// stubbed. "Mutant:" comments name the production change each case turns red.
describe('DeleteServerModal on an MFA-enforcing server (#3456)', () => {
  const ROUTE = `${GATED_API_BASE}/api/v1/servers/server-1`;
  const mockOnClose = vi.fn();

  /** Mounted and unmounted the way `MainViewModals` does: `onClose` removes the host. */
  function Host({ keepTrigger = true }: Readonly<{ keepTrigger?: boolean }>) {
    const [open, setOpen] = useState(false);
    const [triggerShown, setTriggerShown] = useState(true);
    return (
      <>
        <nav className="server-bar">
          <button type="button">Server icon</button>
        </nav>
        {triggerShown && (
          <button
            type="button"
            onClick={() => {
              if (!keepTrigger) setTriggerShown(false);
              setOpen(true);
            }}
          >
            Open delete
          </button>
        )}
        {open && (
          <DeleteServerModal
            isOpen
            server={mockServer}
            onClose={() => {
              mockOnClose();
              setOpen(false);
            }}
          />
        )}
      </>
    );
  }

  const code = () => screen.findByLabelText(CODE_LABEL);
  const stepUpDialog = () => screen.queryByRole('dialog', { name: DIALOG_TITLE });
  const stepUpPrimary = () => screen.getByRole('button', { name: 'Delete Server' });

  async function openAndConfirm(props: { keepTrigger?: boolean } = {}) {
    render(<Host {...props} />);
    await userEvent.click(screen.getByRole('button', { name: 'Open delete' }));
    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    fireEvent.click(getConfirmBtn());
  }

  async function typeCodeAndConfirm(digits = FIXTURE_OTP) {
    await userEvent.type(await code(), digits);
    await userEvent.click(stepUpPrimary());
  }

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useServerStore.getState().addServer(mockServer);
    stubStepUpRead();
  });

  // Mutant: the refusal not handed off (the error shown in the confirmation), or the
  // confirmation left open beside the dialog.
  it('swaps the confirmation for the dialog, one dialog at a time', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm();

    expect(await code()).toBeInTheDocument();
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(stepUpDialog()).toBeInTheDocument();
    expect(screen.queryByText(/cannot be undone/)).not.toBeInTheDocument();
    expect(stepUpDialog()).toHaveAccessibleDescription(
      'This server asks you to verify before you delete Test Server.'
    );
    // The first send is the host's own code-less request, exactly as before.
    expect(requests).toEqual([{ body: null, contentType: null }]);
    expect(useServerStore.getState().servers).toHaveLength(1);
    expect(mockOnClose).not.toHaveBeenCalled();
  });

  // Mutant: the typed-name gate re-asked in the dialog (the confirmation's input left mounted).
  it('does not ask for the server name again once the dialog is up', async () => {
    stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm();
    await code();

    expect(screen.queryByPlaceholderText('Test Server')).not.toBeInTheDocument();
  });

  // Mutant: `mfa_code` dropped from the re-send, sent twice, or the path not the frozen one.
  it('re-sends the same route once with the typed code, and deletes once', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm();
    await typeCodeAndConfirm();

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(requests).toHaveLength(2);
    expect(requests[1]).toEqual({
      body: { mfa_code: FIXTURE_OTP },
      contentType: 'application/json',
    });
    expect(useServerStore.getState().servers).toHaveLength(0);
    expect(stepUpDialog()).not.toBeInTheDocument();
  });

  // Mutant: the re-send built from the `server` prop instead of the request frozen at the first send.
  it('re-sends, words and removes the frozen server even when the prop changes meanwhile', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE });
    const other = stubGatedRoute({
      method: 'delete',
      url: `${GATED_API_BASE}/api/v1/servers/server-2`,
    });
    const another = { ...mockServer, id: 'server-2', name: 'Another' };
    useServerStore.getState().addServer(another);
    const { rerender } = render(
      <DeleteServerModal isOpen server={mockServer} onClose={mockOnClose} />
    );
    fireEvent.change(screen.getByPlaceholderText('Test Server'), {
      target: { value: 'Test Server' },
    });
    fireEvent.click(getConfirmBtn());
    await code();

    rerender(<DeleteServerModal isOpen server={another} onClose={mockOnClose} />);
    expect(screen.getByRole('dialog', { name: DIALOG_TITLE })).toHaveAccessibleDescription(
      'This server asks you to verify before you delete Test Server.'
    );
    await typeCodeAndConfirm();

    await waitFor(() => expect(requests).toHaveLength(2));
    expect(other).toHaveLength(0);
    await waitFor(() =>
      expect(useServerStore.getState().servers.map((s) => s.id)).toEqual(['server-2'])
    );
  });

  // Mutant: a wrong code treated as success, or the dialog dropped on a field refusal.
  it('a refused code keeps the dialog and the server, and a second code deletes', async () => {
    const requests = stubGatedRoute({
      method: 'delete',
      url: ROUTE,
      retries: [INVALID_CODE, { status: 200 }],
    });
    await openAndConfirm();
    await typeCodeAndConfirm();

    await waitFor(() => expect(requests).toHaveLength(2));
    expect(stepUpDialog()).toBeInTheDocument();
    expect(useServerStore.getState().servers).toHaveLength(1);
    expect(mockOnClose).not.toHaveBeenCalled();

    await typeCodeAndConfirm(FIXTURE_OTP_2);
    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(requests).toHaveLength(3);
    expect(requests[2].body).toEqual({ mfa_code: FIXTURE_OTP_2 });
  });

  // Mutant: the enrolment seed ignored, so the code field mounts for an actor with no factor.
  it('shows the enrolment state with the setup link, and sends nothing more', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE, first: ENROLMENT_REQUIRED });
    await openAndConfirm();

    expect(await screen.findByText(ENROLMENT_TEXT)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: SETUP_LINK })).toBeInTheDocument();
    expect(screen.queryByLabelText(CODE_LABEL)).not.toBeInTheDocument();
    expect(requests).toHaveLength(1);
  });

  // Mutant: `onSetUpVerification` unwired, `closeHost` dropped, or the return aimed anywhere but the chat.
  it('the setup link abandons the delete and records the return to chat', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE, first: ENROLMENT_REQUIRED });
    await openAndConfirm();
    await userEvent.click(await screen.findByRole('button', { name: SETUP_LINK }));

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(requests).toHaveLength(1);
    expect(useServerStore.getState().servers).toHaveLength(1);
  });

  // Mutant: the adapter widened to own an unflagged 429 or a 500, hiding the host's own sentence.
  it.each([
    [
      'an unflagged 429',
      { status: 429, body: { error: 'Rate limit exceeded' } },
      'Rate limit exceeded',
    ],
    ['a 500 with a sentence', { status: 500, body: { error: 'Database down' } }, 'Database down'],
    ['a 500 with no sentence', { status: 500, body: {} }, 'Failed to delete server'],
  ])('%s keeps the confirmation and its own error', async (_name, first, text) => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE, first });
    await openAndConfirm();

    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(stepUpDialog()).not.toBeInTheDocument();
    expect(requests).toHaveLength(1);
    expect(useServerStore.getState().servers).toHaveLength(1);
  });

  // Mutant: Cancel wired to a second request, or `onClose` not forwarded (`endStepUp` unwired).
  // The confirmation's autofocused name field is what `ui/Modal` records as the invoker, so the
  // trigger is not where focus returns for this host: the server rail is.
  it('Cancel sends nothing more and focus does not fall to <body>', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm();
    await code();

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(requests).toHaveLength(1);
    expect(useServerStore.getState().servers).toHaveLength(1);
    await waitFor(() => expect(document.body).not.toHaveFocus());
    expect(document.querySelector('.server-bar')).toContainElement(
      document.activeElement as HTMLElement
    );
  });

  // Mutant: `onEnd` run in the same commit as the close (the host unmounted before the fallback
  // ran), or `focusFallback` aimed at nothing: focus drops to <body>.
  it('with the trigger gone, Cancel puts focus on the server rail, never <body>', async () => {
    stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm({ keepTrigger: false });
    await code();

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(screen.getByRole('button', { name: 'Server icon' })).toHaveFocus();
    expect(document.body).not.toHaveFocus();
  });

  // Mutant: the success path skipping `endStepUp`, so the host's `onClose` never runs.
  it('with the trigger gone, success also lands focus on the server rail', async () => {
    stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm({ keepTrigger: false });
    await typeCodeAndConfirm();

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(screen.getByRole('button', { name: 'Server icon' })).toHaveFocus();
  });
});
