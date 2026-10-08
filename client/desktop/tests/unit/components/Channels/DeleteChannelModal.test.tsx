import { useState } from 'react';
import { render, screen, fireEvent, waitFor, userEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { server as mswServer } from '../../../mocks/server';
import { http, HttpResponse } from 'msw';
import { mockChannel } from '../../../mocks/fixtures';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import DeleteChannelModal from '@/renderer/components/Channels/DeleteChannelModal';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
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

describe('DeleteChannelModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useAuthStore.getState().setAccessToken('mock-token');
    useChannelStore.getState().addChannel(mockChannel);
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <DeleteChannelModal isOpen={false} channel={mockChannel} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders warning with channel name', () => {
    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);
    expect(screen.getByText(/#general/)).toBeInTheDocument();
    expect(screen.getByText(/cannot be undone/)).toBeInTheDocument();
  });

  it('deletes channel on confirmation', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/channels/channel-1`, () =>
        HttpResponse.json({ message: 'Deleted' })
      )
    );

    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(useChannelStore.getState().channels).toHaveLength(0);
  });

  it('shows error on API failure', async () => {
    mswServer.use(
      http.delete(`${API_BASE}/api/v1/channels/channel-1`, () =>
        HttpResponse.json({ error: 'Forbidden' }, { status: 403 })
      )
    );

    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);

    fireEvent.click(getConfirmBtn());

    await waitFor(() => {
      expect(screen.getByText('Forbidden')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<DeleteChannelModal isOpen={true} channel={mockChannel} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});

// The delete swap (#3456 §3.3, §3.4): a server that enforces MFA refuses the
// code-less DELETE, and the confirmation gives way to the shared step-up dialog.
// The dialog, the factor hook and the adapter are real; only the network is
// stubbed. "Mutant:" comments name the production change each case turns red.
describe('DeleteChannelModal on an MFA-enforcing server (#3456)', () => {
  const ROUTE = `${GATED_API_BASE}/api/v1/channels/channel-1`;
  const mockOnClose = vi.fn();

  /** Mounted and unmounted the way `MainViewModals` does: `onClose` removes the host. */
  function Host({ keepTrigger = true }: Readonly<{ keepTrigger?: boolean }>) {
    const [open, setOpen] = useState(false);
    const [triggerShown, setTriggerShown] = useState(true);
    return (
      <>
        <nav className="channel-list">
          <button type="button">General row</button>
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
          <DeleteChannelModal
            isOpen
            channel={mockChannel}
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
  const stepUpPrimary = () => screen.getByRole('button', { name: 'Delete Channel' });

  async function openAndConfirm(props: { keepTrigger?: boolean } = {}) {
    render(<Host {...props} />);
    await userEvent.click(screen.getByRole('button', { name: 'Open delete' }));
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
    useChannelStore.getState().addChannel(mockChannel);
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
      'This server asks you to verify before you delete #general.'
    );
    // The first send is the host's own code-less request, exactly as before.
    expect(requests).toEqual([{ body: null, contentType: null }]);
    expect(useChannelStore.getState().channels).toHaveLength(1);
    expect(mockOnClose).not.toHaveBeenCalled();
  });

  // Mutant: the confirmation's own close read as the action ending (`handingOffRef` unset),
  // which unmounts the host and takes the dialog with it.
  it('keeps the host mounted while the dialog is up', async () => {
    stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm();
    await code();

    expect(mockOnClose).not.toHaveBeenCalled();
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
    expect(useChannelStore.getState().channels).toHaveLength(0);
    expect(stepUpDialog()).not.toBeInTheDocument();
  });

  // Mutant: the re-send built from the `channel` prop instead of the request frozen at the
  // first send; or the intro and the store removal read from the live prop (the dialog
  // names one channel while the code deletes another).
  it('re-sends, words and removes the frozen channel even when the prop changes meanwhile', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE });
    const other = stubGatedRoute({
      method: 'delete',
      url: `${GATED_API_BASE}/api/v1/channels/channel-2`,
    });
    const random = { ...mockChannel, id: 'channel-2', name: 'random' };
    useChannelStore.getState().addChannel(random);
    const { rerender } = render(
      <DeleteChannelModal isOpen channel={mockChannel} onClose={mockOnClose} />
    );
    fireEvent.click(getConfirmBtn());
    await code();

    rerender(<DeleteChannelModal isOpen channel={random} onClose={mockOnClose} />);
    expect(stepUpDialog()).toHaveAccessibleDescription(
      'This server asks you to verify before you delete #general.'
    );
    await typeCodeAndConfirm();

    await waitFor(() => expect(requests).toHaveLength(2));
    expect(other).toHaveLength(0);
    await waitFor(() =>
      expect(useChannelStore.getState().channels.map((c) => c.id)).toEqual(['channel-2'])
    );
  });

  // Mutant: a wrong code treated as success, or the dialog dropped on a field refusal.
  it('a refused code keeps the dialog and the channel, and a second code deletes', async () => {
    const requests = stubGatedRoute({
      method: 'delete',
      url: ROUTE,
      retries: [INVALID_CODE, { status: 200 }],
    });
    await openAndConfirm();
    await typeCodeAndConfirm();

    await waitFor(() => expect(requests).toHaveLength(2));
    expect(stepUpDialog()).toBeInTheDocument();
    expect(useChannelStore.getState().channels).toHaveLength(1);
    expect(mockOnClose).not.toHaveBeenCalled();

    await typeCodeAndConfirm('271828');
    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(requests).toHaveLength(3);
    expect(requests[2].body).toEqual({ mfa_code: '271828' });
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
    expect(useChannelStore.getState().channels).toHaveLength(1);
  });

  // Mutant: the adapter widened to own an unflagged 429 or a 500, hiding the host's own sentence.
  it.each([
    [
      'an unflagged 429',
      { status: 429, body: { error: 'Rate limit exceeded' } },
      'Rate limit exceeded',
    ],
    ['a 500 with a sentence', { status: 500, body: { error: 'Database down' } }, 'Database down'],
    ['a 500 with no sentence', { status: 500, body: {} }, 'Failed to delete channel'],
  ])('%s keeps the confirmation and its own error', async (_name, first, text) => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE, first });
    await openAndConfirm();

    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(stepUpDialog()).not.toBeInTheDocument();
    expect(requests).toHaveLength(1);
    expect(useChannelStore.getState().channels).toHaveLength(1);
  });

  // Mutant: Cancel wired to a second request, or `onClose` not forwarded (`endStepUp` unwired).
  it('Cancel sends nothing more and focus returns to the trigger', async () => {
    const requests = stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm();
    await code();

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(requests).toHaveLength(1);
    expect(useChannelStore.getState().channels).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Open delete' })).toHaveFocus();
  });

  // Mutant: `onEnd` run in the same commit as the close (the host unmounted before the fallback
  // ran), or `focusFallback` aimed at nothing: focus drops to <body>.
  it('with the trigger gone, Cancel puts focus on the channel list, never <body>', async () => {
    stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm({ keepTrigger: false });
    await code();

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(screen.getByRole('button', { name: 'General row' })).toHaveFocus();
    expect(document.body).not.toHaveFocus();
  });

  // Mutant: the success path skipping `endStepUp`, so the host's `onClose` never runs.
  it('with the trigger gone, success also lands focus on the channel list', async () => {
    stubGatedRoute({ method: 'delete', url: ROUTE });
    await openAndConfirm({ keepTrigger: false });
    await typeCodeAndConfirm();

    await waitFor(() => expect(mockOnClose).toHaveBeenCalledTimes(1));
    expect(screen.getByRole('button', { name: 'General row' })).toHaveFocus();
  });
});
