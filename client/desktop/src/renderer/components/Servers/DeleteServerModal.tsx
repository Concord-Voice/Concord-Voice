import React from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import DangerousActionStepUpDialog from '../Auth/DangerousActionStepUpDialog';
import { focusTargetIn } from '../ui/focusTarget';
import { useServerStore } from '../../stores/chat/serverStore';
import { useStepUpHandoff } from '../../hooks/auth/useStepUpHandoff';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import {
  describeFailureWith,
  resender,
  sendFirst,
  type FrozenRequest,
} from '../../services/system/dangerousActionRequest';
import type { ApiRequestContext } from '../../services/system/requestContext';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';
import { ServerWithRole } from '../../types/server';
import './DeleteServerModal.css';

interface DeleteServerModalProps {
  isOpen: boolean;
  server: ServerWithRole;
  onClose: () => void;
}

/**
 * The refusal that asked for verification, the request it refused and the
 * account and server it went out as, frozen as sent, with the server it named:
 * the dialog's words and the store update follow the request, never a prop
 * that moved on since.
 */
interface PendingDelete {
  request: FrozenRequest;
  refusal: StepUpFactorRefusal;
  context: ApiRequestContext;
  target: { id: string; name: string };
}

const DELETE_FAILED = 'Failed to delete server';
const describeDeleteFailure = describeFailureWith(DELETE_FAILED);

// The server's own icon is gone once it is, so focus goes back to the rail it
// sat in.
const serverRailFocusTarget = () =>
  focusTargetIn(document.querySelector<HTMLElement>('.server-bar'));

const DeleteServerModal: React.FC<DeleteServerModalProps> = ({ isOpen, server, onClose }) => {
  const { pending, ending, handOff, confirmClosed, endStepUp } =
    useStepUpHandoff<PendingDelete>(onClose);

  const removeServer = (id: string) => useServerStore.getState().removeServer(id);

  const handleDelete = async () => {
    const target = { id: server.id, name: server.name };
    const request: FrozenRequest = { path: `/api/v1/servers/${target.id}`, method: 'DELETE' };
    const first = await sendFirst(request, DELETE_FAILED);
    if (first.kind === 'ok') {
      removeServer(target.id);
      return;
    }
    // A server that enforces MFA wants a verified factor before it deletes.
    handOff({ request, refusal: first.refusal, context: first.context, target });
  };

  return (
    <>
      <ConfirmActionModal
        isOpen={isOpen && pending === null && !ending}
        onClose={confirmClosed}
        title="Delete Server"
        message={
          <>
            Are you sure you want to delete <strong>{server.name}</strong>? This action cannot be
            undone. All channels and messages in this server will be permanently deleted.
          </>
        }
        confirmLabel="Delete Server"
        loadingLabel="Deleting..."
        onConfirm={handleDelete}
        confirmationInput={{
          label: (
            <>
              Type <strong>{server.name}</strong> to confirm
            </>
          ),
          expectedValue: server.name,
        }}
      />
      <DangerousActionStepUpDialog
        isOpen={pending !== null}
        purpose="servers.delete"
        seed={pending?.refusal}
        intro={`This server asks you to verify before you delete ${pending?.target.name ?? ''}.`}
        primaryLabel="Delete Server"
        busyLabel="Deleting..."
        send={resender(pending?.request ?? null)}
        capture={pending?.context}
        describeFailure={describeDeleteFailure}
        onSuccess={() => {
          if (pending !== null) removeServer(pending.target.id);
          endStepUp();
        }}
        onClose={endStepUp}
        onSetUpVerification={() => {
          void openVerificationSetup({ returnTo: { kind: 'chat' }, closeHost: endStepUp });
        }}
        focusFallback={serverRailFocusTarget}
      />
    </>
  );
};

export default DeleteServerModal;
