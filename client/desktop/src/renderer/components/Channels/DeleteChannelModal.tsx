import React from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import DangerousActionStepUpDialog from '../Auth/DangerousActionStepUpDialog';
import { focusTargetIn } from '../ui/focusTarget';
import { useChannelStore } from '../../stores/chat/channelStore';
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
import { Channel } from '../../types/chat';

interface DeleteChannelModalProps {
  isOpen: boolean;
  channel: Channel;
  onClose: () => void;
}

/**
 * The refusal that asked for verification, the request it refused and the
 * account and server it went out as, frozen as sent, with the channel it named:
 * the dialog's words and the store update follow the request, never a prop
 * that moved on since.
 */
interface PendingDelete {
  request: FrozenRequest;
  refusal: StepUpFactorRefusal;
  context: ApiRequestContext;
  target: { id: string; name: string };
}

const DELETE_FAILED = 'Failed to delete channel';
const describeDeleteFailure = describeFailureWith(DELETE_FAILED);

// The row that opened the menu is gone once the channel is, so focus goes back
// to the list it was in.
const channelListFocusTarget = () =>
  focusTargetIn(document.querySelector<HTMLElement>('.channel-list'));

const DeleteChannelModal: React.FC<DeleteChannelModalProps> = ({ isOpen, channel, onClose }) => {
  const { pending, ending, handOff, confirmClosed, endStepUp } =
    useStepUpHandoff<PendingDelete>(onClose);

  const removeChannel = (id: string) => useChannelStore.getState().removeChannel(id);

  const handleDelete = async () => {
    const target = { id: channel.id, name: channel.name };
    const request: FrozenRequest = { path: `/api/v1/channels/${target.id}`, method: 'DELETE' };
    const first = await sendFirst(request, DELETE_FAILED);
    if (first.kind === 'ok') {
      removeChannel(target.id);
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
        title="Delete Channel"
        message={
          <>
            Are you sure you want to delete <strong>#{channel.name}</strong>? This action cannot be
            undone. All messages in this channel will be permanently deleted.
          </>
        }
        confirmLabel="Delete Channel"
        loadingLabel="Deleting..."
        onConfirm={handleDelete}
      />
      <DangerousActionStepUpDialog
        isOpen={pending !== null}
        purpose="channels.delete"
        seed={pending?.refusal}
        intro={`This server asks you to verify before you delete #${pending?.target.name ?? ''}.`}
        primaryLabel="Delete Channel"
        busyLabel="Deleting..."
        send={resender(pending?.request ?? null)}
        capture={pending?.context}
        describeFailure={describeDeleteFailure}
        onSuccess={() => {
          if (pending !== null) removeChannel(pending.target.id);
          endStepUp();
        }}
        onClose={endStepUp}
        onSetUpVerification={() => {
          void openVerificationSetup({ returnTo: { kind: 'chat' }, closeHost: endStepUp });
        }}
        focusFallback={channelListFocusTarget}
      />
    </>
  );
};

export default DeleteChannelModal;
