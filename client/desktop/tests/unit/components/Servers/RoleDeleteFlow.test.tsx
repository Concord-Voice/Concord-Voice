import { useState } from 'react';
import { render, screen, waitFor, within, userEvent } from '../../../test-utils';
import RoleDeleteFlow, {
  type RoleDeleteTarget,
} from '@/renderer/components/Servers/RoleDeleteFlow';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { FIRST_SEND_SESSION_CHANGED } from '@/renderer/services/system/dangerousActionRequest';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import type {
  PermissionWriteFailure,
  PermissionWriteOutcome,
} from '@/renderer/stores/chat/permissionStore';
import { server as mswServer } from '../../../mocks/server';
import {
  CODE_LABEL,
  DIALOG_TITLE,
  ENROLMENT_REQUIRED,
  ENROLMENT_TEXT,
  FIXTURE_OTP,
  INVALID_CODE,
  MFA_REQUIRED,
  SETUP_LINK,
  stubStepUpRead,
  type GatedReply,
} from '../../../helpers/gatedRoute';

beforeAll(() => mswServer.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => mswServer.close());
afterEach(() => mswServer.resetHandlers());

const OK: PermissionWriteOutcome = { ok: true };
// Captured when called, as the store captures when it sends: inside a test,
// after `beforeEach` has signed in, so the refusal belongs to that session.
type RefusedWrite = Extract<PermissionWriteFailure, { kind: 'refused' }>;

const refusedWith = ({ status, body }: GatedReply): RefusedWrite => ({
  ok: false,
  kind: 'refused',
  status,
  body,
  context: captureApiRequestContext(),
});

const RETURN_TO = { kind: 'serverSettings', serverId: 'server-1', section: 'roles' } as const;
const TARGET: RoleDeleteTarget = { id: 'role-1', name: 'Moderator' };

const onDelete = vi.fn<(...args: unknown[]) => Promise<PermissionWriteOutcome>>();
const onDeleted = vi.fn();
const onEnd = vi.fn();

// The host owns `target`, as RoleEditorPanel does: `onEnd` drops it.
function Host({ confirmDiscard }: Readonly<{ confirmDiscard?: () => boolean | Promise<boolean> }>) {
  const [target, setTarget] = useState<RoleDeleteTarget | null>(TARGET);
  return (
    <RoleDeleteFlow
      target={target}
      onDelete={onDelete}
      onDeleted={onDeleted}
      onEnd={() => {
        onEnd();
        setTarget(null);
      }}
      returnTo={RETURN_TO}
      confirmDiscard={confirmDiscard}
    />
  );
}

// The host as `RoleEditorPanel` is: a Delete button opens the flow, `onEnd` drops the target, and the
// role list sits beside it. `keepTrigger={false}` removes the button, as deleting its role does.
function TriggerHost({ keepTrigger = true }: Readonly<{ keepTrigger?: boolean }>) {
  const [target, setTarget] = useState<RoleDeleteTarget | null>(null);
  const [triggerShown, setTriggerShown] = useState(true);
  return (
    <>
      <nav className="role-hierarchy">
        <button type="button">Moderator row</button>
      </nav>
      {triggerShown && (
        <button
          type="button"
          onClick={() => {
            if (!keepTrigger) setTriggerShown(false);
            setTarget(TARGET);
          }}
        >
          Delete
        </button>
      )}
      <RoleDeleteFlow
        target={target}
        onDelete={onDelete}
        onDeleted={onDeleted}
        onEnd={() => {
          onEnd();
          setTarget(null);
        }}
        returnTo={RETURN_TO}
      />
    </>
  );
}

const confirmDialog = () => screen.getByRole('dialog', { name: 'Delete Role' });
const stepUpDialog = () => screen.getByRole('dialog', { name: DIALOG_TITLE });

async function reachStepUp(confirmDiscard?: () => boolean | Promise<boolean>) {
  render(<Host confirmDiscard={confirmDiscard} />);
  await userEvent.click(within(confirmDialog()).getByRole('button', { name: 'Delete Role' }));
  await screen.findByRole('dialog', { name: DIALOG_TITLE });
}

/** Cancels the step-up dialog, recording whether the confirmation is added to the document on the way out. */
async function cancelStepUpWatchingConfirmation(): Promise<string[]> {
  // Records, not a re-query: the re-shown confirmation is added and removed within one flush.
  const reshown: string[] = [];
  const watch = new MutationObserver((records) => {
    for (const added of records.flatMap((record) => [...record.addedNodes])) {
      if (added.textContent?.includes('Are you sure you want to delete the role')) {
        reshown.push(added.nodeName);
      }
    }
  });
  watch.observe(document.body, { childList: true, subtree: true });
  await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Cancel' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  watch.disconnect();
  return reshown;
}

async function openStepUpFromTrigger(keepTrigger: boolean) {
  render(<TriggerHost keepTrigger={keepTrigger} />);
  await userEvent.click(screen.getByRole('button', { name: 'Delete' }));
  await userEvent.click(within(confirmDialog()).getByRole('button', { name: 'Delete Role' }));
  await screen.findByRole('dialog', { name: DIALOG_TITLE });
}

describe('RoleDeleteFlow', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    stubStepUpRead();
    onDelete.mockReset().mockResolvedValue(OK);
    onDeleted.mockReset();
    onEnd.mockReset();
  });

  // Mutation: dropping `isOpen={target !== null && ...}` target gating renders the confirmation with no role (red).
  it('names the role in the confirmation and renders nothing when there is no target', () => {
    const { rerender } = render(
      <RoleDeleteFlow
        target={null}
        onDelete={onDelete}
        onDeleted={onDeleted}
        onEnd={onEnd}
        returnTo={RETURN_TO}
      />
    );
    expect(screen.queryByRole('dialog')).toBeNull();
    rerender(
      <RoleDeleteFlow
        target={TARGET}
        onDelete={onDelete}
        onDeleted={onDeleted}
        onEnd={onEnd}
        returnTo={RETURN_TO}
      />
    );
    expect(within(confirmDialog()).getByText('Moderator')).toBeInTheDocument();
  });

  // Mutation: swallowing the confirmation's `onClose` unconditionally in useStepUpHandoff.confirmClosed never ends the flow on a cancel (red).
  it('cancel ends the flow without sending anything', async () => {
    render(<Host />);
    await userEvent.click(within(confirmDialog()).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(onEnd).toHaveBeenCalledTimes(1));
    expect(onDelete).not.toHaveBeenCalled();
    expect(onDeleted).not.toHaveBeenCalled();
  });

  // Mutation: not swallowing the confirmation's own close after a hand-off ends the flow under the dialog (red).
  it('a hand-off to the dialog does not end the flow', async () => {
    onDelete.mockResolvedValueOnce(refusedWith(MFA_REQUIRED));
    await reachStepUp();
    expect(onEnd).not.toHaveBeenCalled();
    expect(onDeleted).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog', { name: 'Delete Role' })).toBeNull();
  });

  // Mutation: dropping `endStepUp()` from the dialog's onSuccess leaves the flow open after a verified delete (red).
  // Mutation: dropping `capture={pending?.context}` from the dialog in RoleDeleteFlow.tsx re-sends against the dialog's own capture, not the refused delete's (red).
  it('a verified delete reports deleted once, ends once, and re-sends as the refused delete', async () => {
    const refused = refusedWith(MFA_REQUIRED);
    onDelete.mockResolvedValueOnce(refused);
    await reachStepUp();
    await userEvent.type(screen.getByLabelText(CODE_LABEL), FIXTURE_OTP);
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Delete Role' }));
    await waitFor(() => expect(onEnd).toHaveBeenCalledTimes(1));
    expect(onDeleted).toHaveBeenCalledTimes(1);
    expect(onDelete).toHaveBeenLastCalledWith('role-1', {
      mfaCode: FIXTURE_OTP,
      context: expect.anything(),
    });
    // The same capture, not an equal one: the dialog never took its own.
    expect((onDelete.mock.calls[1][1] as { context: unknown }).context).toBe(refused.context);
  });

  // Mutation: dropping the apiRequestContextIsCurrent check from permissionWriteStepUp.stepUpSeedOf hands the refused delete to whoever is signed in now (red).
  it('a refusal that lands after the account changed opens no dialog and says the session ended', async () => {
    onDelete.mockImplementationOnce(async () => {
      const refused = refusedWith(MFA_REQUIRED);
      useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
      return refused;
    });
    render(<Host />);
    await userEvent.click(within(confirmDialog()).getByRole('button', { name: 'Delete Role' }));
    expect(await within(confirmDialog()).findByRole('alert')).toHaveTextContent(
      FIRST_SEND_SESSION_CHANGED
    );
    expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(onDeleted).not.toHaveBeenCalled();
  });

  // Mutation: reading the re-send's refusal as success in permissionWriteStepUp.sendResultOf reports a delete that did not happen (red).
  it('a rejected code stays in the dialog and reports nothing deleted', async () => {
    onDelete
      .mockResolvedValueOnce(refusedWith(MFA_REQUIRED))
      .mockResolvedValueOnce(refusedWith(INVALID_CODE));
    await reachStepUp();
    await userEvent.type(screen.getByLabelText(CODE_LABEL), FIXTURE_OTP);
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Delete Role' }));
    expect(await within(stepUpDialog()).findByRole('alert')).toBeInTheDocument();
    expect(onDelete).toHaveBeenCalledTimes(2);
    expect(onDeleted).not.toHaveBeenCalled();
    expect(onEnd).not.toHaveBeenCalled();
  });

  // Mutation: replacing `onClose={endStepUp}` with a no-op leaves the dialog standing after Cancel (red).
  it('cancelling the dialog ends the flow with nothing deleted', async () => {
    onDelete.mockResolvedValueOnce(refusedWith(MFA_REQUIRED));
    await reachStepUp();
    await userEvent.click(within(stepUpDialog()).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(onEnd).toHaveBeenCalledTimes(1));
    expect(onDeleted).not.toHaveBeenCalled();
    expect(onDelete).toHaveBeenCalledTimes(1);
  });

  // Mutation: opening the confirmation on `pending === null` without `&& !ending` (RoleDeleteFlow.tsx) re-mounts "Delete Role" for the commit between the dialog closing and the flow ending; it takes focus and closes, and focus never reaches the Delete button (red).
  it('cancelling the dialog never re-shows the confirmation and returns focus to the Delete button', async () => {
    onDelete.mockResolvedValueOnce(refusedWith(MFA_REQUIRED));
    await openStepUpFromTrigger(true);

    expect(await cancelStepUpWatchingConfirmation()).toEqual([]);

    expect(screen.queryByRole('dialog', { name: 'Delete Role' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Delete' })).toHaveFocus();
  });

  // Mutation: as above, or `roleListFocusTarget` aimed at nothing: with the Delete button gone, focus drops to <body> (red).
  it('with the Delete button gone, cancelling puts focus on the role list, never <body>', async () => {
    onDelete.mockResolvedValueOnce(refusedWith(MFA_REQUIRED));
    await openStepUpFromTrigger(false);

    expect(await cancelStepUpWatchingConfirmation()).toEqual([]);

    expect(screen.getByRole('button', { name: 'Moderator row' })).toHaveFocus();
    expect(document.body).not.toHaveFocus();
  });

  // Mutation: dropping `closeHost: endStepUp` from onSetUpVerification leaves the delete frozen under Settings (red).
  it('the setup link records where to return and abandons the delete', async () => {
    onDelete.mockResolvedValueOnce(refusedWith(ENROLMENT_REQUIRED));
    await reachStepUp();
    expect(screen.getByText(ENROLMENT_TEXT)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: SETUP_LINK }));
    await waitFor(() => expect(onEnd).toHaveBeenCalledTimes(1));
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual(RETURN_TO);
    expect(onDeleted).not.toHaveBeenCalled();
    expect(onDelete).toHaveBeenCalledTimes(1);
  });

  // Mutation: dropping `confirmDiscard` from the openVerificationSetup call in RoleDeleteFlow.tsx opens Settings over unsaved form edits without asking (red).
  it('a declined discard leaves the dialog up and Settings unopened', async () => {
    const confirmDiscard = vi.fn(() => false);
    onDelete.mockResolvedValueOnce(refusedWith(ENROLMENT_REQUIRED));
    await reachStepUp(confirmDiscard);
    await userEvent.click(screen.getByRole('button', { name: SETUP_LINK }));
    await waitFor(() => expect(confirmDiscard).toHaveBeenCalledTimes(1));
    expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
    expect(screen.getByText(ENROLMENT_TEXT)).toBeInTheDocument();
    expect(onEnd).not.toHaveBeenCalled();
  });

  // Mutation: awaiting `confirmDiscard` after `closeHost`, or not at all, closes the dialog before the question is answered (red).
  it('an accepted discard abandons the delete and opens Settings', async () => {
    const confirmDiscard = vi.fn(() => Promise.resolve(true));
    onDelete.mockResolvedValueOnce(refusedWith(ENROLMENT_REQUIRED));
    await reachStepUp(confirmDiscard);
    await userEvent.click(screen.getByRole('button', { name: SETUP_LINK }));
    await waitFor(() => expect(onEnd).toHaveBeenCalledTimes(1));
    expect(confirmDiscard).toHaveBeenCalledTimes(1);
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual(RETURN_TO);
    expect(onDeleted).not.toHaveBeenCalled();
  });

  // Mutation: throwing a fixed string instead of failureTextOf(outcome, ...) in handleConfirm hides the server's reason (red).
  it('a plain refusal is worded in the confirmation, which stays open', async () => {
    onDelete.mockResolvedValueOnce(refusedWith({ status: 409, body: { error: 'Role is in use' } }));
    render(<Host />);
    await userEvent.click(within(confirmDialog()).getByRole('button', { name: 'Delete Role' }));
    expect(await within(confirmDialog()).findByRole('alert')).toHaveTextContent('Role is in use');
    expect(screen.queryByRole('dialog', { name: DIALOG_TITLE })).toBeNull();
    expect(onDeleted).not.toHaveBeenCalled();
    expect(onEnd).not.toHaveBeenCalled();
  });
});
