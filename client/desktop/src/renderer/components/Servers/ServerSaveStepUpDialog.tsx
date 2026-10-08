import React, { useRef } from 'react';
import DangerousActionStepUpDialog, {
  type DangerousActionSendResult,
} from '../Auth/DangerousActionStepUpDialog';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import type { ApiRequestContext } from '../../services/system/requestContext';
import { describeFailureWith } from '../../services/system/dangerousActionRequest';
import {
  resendServerUpdate,
  UPDATE_FAILED,
  type ServerUpdateBody,
  type UpdatedServer,
} from '../../services/system/serverUpdateApi';

/** A refused save, frozen as the first send made it, with the account and server it went out as. */
export interface PendingServerSave {
  serverId: string;
  body: ServerUpdateBody;
  refusal: StepUpFactorRefusal;
  context: ApiRequestContext;
}

interface ServerSaveStepUpDialogProps {
  /** The save the server asked to verify, or null while no dialog is up. */
  pending: PendingServerSave | null;
  /** The re-sent save went through: `server` is what the route returned for `serverId`, the frozen one. */
  onSaved: (server: UpdatedServer, serverId: string) => void;
  /** Cancel, Escape, the close button, and "Set up verification" all end the dialog here. */
  onClose: () => void;
  /** Opens verification setup, given the function that closes this dialog. */
  onSetUpVerification: (closeHost: () => void) => void;
  focusFallback: () => HTMLElement | null;
}

const describeSaveFailure = describeFailureWith(UPDATE_FAILED);

/**
 * The verification step for Server Settings' save (#3456 §3.4), over the page. The page's form
 * cannot be edited while it is up, and the re-send uses the body frozen at the first send, not
 * the form.
 */
const ServerSaveStepUpDialog: React.FC<ServerSaveStepUpDialogProps> = ({
  pending,
  onSaved,
  onClose,
  onSetUpVerification,
  focusFallback,
}) => {
  const savedRef = useRef<UpdatedServer | null>(null);

  const send = async (
    mfaCode: string | undefined,
    context: ApiRequestContext
  ): Promise<DangerousActionSendResult> => {
    if (pending === null) return { kind: 'aborted' };
    const result = await resendServerUpdate(pending.serverId, pending.body, mfaCode, context);
    if (result.kind === 'ok') savedRef.current = result.server;
    return result;
  };

  const handleSuccess = () => {
    const saved = savedRef.current;
    savedRef.current = null;
    if (saved !== null && pending !== null) onSaved(saved, pending.serverId);
  };

  return (
    <DangerousActionStepUpDialog
      isOpen={pending !== null}
      purpose="servers.update"
      seed={pending?.refusal}
      intro="This server asks you to verify before you save changes to its settings."
      primaryLabel="Save Changes"
      busyLabel="Saving..."
      send={send}
      capture={pending?.context}
      describeFailure={describeSaveFailure}
      onSuccess={handleSuccess}
      onClose={onClose}
      onSetUpVerification={() => onSetUpVerification(onClose)}
      focusFallback={focusFallback}
    />
  );
};

export default ServerSaveStepUpDialog;
