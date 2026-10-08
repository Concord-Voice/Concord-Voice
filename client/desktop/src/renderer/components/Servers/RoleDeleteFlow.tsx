import React, { useEffect, useRef } from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import DangerousActionStepUpDialog from '../Auth/DangerousActionStepUpDialog';
import { focusTargetIn } from '../ui/focusTarget';
import { useStepUpHandoff } from '../../hooks/auth/useStepUpHandoff';
import { describeFailureWith } from '../../services/system/dangerousActionRequest';
import {
  failureTextOf,
  sendResultOf,
  stepUpSeedOf,
  type PermissionWriteStepUpSeed,
} from '../../services/system/permissionWriteStepUp';
import {
  WRITE_UNKNOWN,
  type PermissionWriteOutcome,
  type WriteConfirmation,
} from '../../stores/chat/permissionStore';
import type { VerificationReturn } from '../../stores/ui/settingsOverlayStore';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';

/** The role the user asked to delete. Its name is kept: the role leaves the list as it goes. */
export interface RoleDeleteTarget {
  id: string;
  name: string;
}

interface RoleDeleteFlowProps {
  /** Open while set. */
  target: RoleDeleteTarget | null;
  onDelete: (roleId: string, confirmation?: WriteConfirmation) => Promise<PermissionWriteOutcome>;
  /** The role is gone. The caller clears its selection; nothing else ends the flow. */
  onDeleted: () => void;
  /** The flow is over: deleted, cancelled or abandoned. The caller drops `target`. */
  onEnd: () => void;
  /** Where "Back to …" goes once verification is set up. */
  returnTo: VerificationReturn;
  /**
   * Asked before "Set up verification" leaves Server Settings, when the open
   * form holds edits that would go with it: resolves false to stay (D-4).
   */
  confirmDiscard?: () => boolean | Promise<boolean>;
}

/**
 * The role whose delete the server refused for verification, that refusal, and
 * the account and server the delete went out as: the dialog's capture.
 */
interface PendingDelete extends PermissionWriteStepUpSeed {
  roleId: string;
}

const DELETE_FAILED = 'Failed to delete role';
const describeDeleteFailure = describeFailureWith(DELETE_FAILED);

// The row that opened the confirmation is gone once the role is, so focus goes
// back to the list it was in.
export const roleListFocusTarget = () =>
  focusTargetIn(document.querySelector<HTMLElement>('.role-hierarchy'));

/**
 * Takes focus to `fallback` when the flow closes with focus on `<body>`: the
 * Delete button left with its role, so `ui/Modal` had nothing to return to. A
 * cancel finds focus back on that button and leaves it alone.
 */
function useFocusFallbackOnEnd(isOpen: boolean, fallback: () => HTMLElement | null): void {
  const wasOpenRef = useRef(isOpen);
  useEffect(() => {
    const ended = wasOpenRef.current && !isOpen;
    wasOpenRef.current = isOpen;
    if (!ended) return;
    const active = document.activeElement;
    if (active === null || active === document.body) fallback()?.focus();
  }, [isOpen, fallback]);
}

/**
 * Delete Role (#3456 §3.4, R11): a confirmation, and on a server that enforces
 * MFA, the swap to `DangerousActionStepUpDialog`.
 *
 * The first send is the delete as it always was. A refusal that asks for
 * verification hands over to the dialog in place of the confirmation, which
 * closes itself once `onConfirm` resolves; the dialog re-sends the same delete
 * with `mfa_code`. Anything else the server says is worded in the confirmation.
 */
const RoleDeleteFlow: React.FC<RoleDeleteFlowProps> = ({
  target,
  onDelete,
  onDeleted,
  onEnd,
  returnTo,
  confirmDiscard,
}) => {
  const { pending, ending, handOff, confirmClosed, endStepUp } =
    useStepUpHandoff<PendingDelete>(onEnd);
  useFocusFallbackOnEnd(target !== null, roleListFocusTarget);

  const handleConfirm = async () => {
    if (target === null) return;
    const outcome = await onDelete(target.id).catch(() => WRITE_UNKNOWN);
    if (outcome.ok) {
      onDeleted();
      return;
    }
    // A server that enforces MFA wants a verified factor before it deletes.
    const seed = stepUpSeedOf(outcome);
    if (seed === null) throw new Error(failureTextOf(outcome, DELETE_FAILED));
    handOff({ roleId: target.id, ...seed });
  };

  const resend = async (mfaCode: string | undefined, context: WriteConfirmation['context']) => {
    if (pending === null) return { kind: 'aborted' } as const;
    // A throw is the dialog's to read (an abort sent nothing; anything else may have).
    return sendResultOf(await onDelete(pending.roleId, { mfaCode, context }));
  };

  return (
    <>
      <ConfirmActionModal
        isOpen={target !== null && pending === null && !ending}
        onClose={confirmClosed}
        title="Delete Role"
        message={
          <>
            Are you sure you want to delete the role <strong>{target?.name}</strong>? Members who
            hold it lose the permissions it gives. This action cannot be undone.
          </>
        }
        confirmLabel="Delete Role"
        loadingLabel="Deleting..."
        onConfirm={handleConfirm}
      />
      <DangerousActionStepUpDialog
        isOpen={pending !== null}
        purpose="roles.delete"
        seed={pending?.refusal}
        capture={pending?.context}
        intro={`This server asks you to verify before you delete the role ${target?.name ?? ''}.`}
        primaryLabel="Delete Role"
        busyLabel="Deleting..."
        send={resend}
        describeFailure={describeDeleteFailure}
        onSuccess={() => {
          onDeleted();
          endStepUp();
        }}
        onClose={endStepUp}
        onSetUpVerification={() => {
          void openVerificationSetup({ returnTo, confirmDiscard, closeHost: endStepUp });
        }}
        focusFallback={roleListFocusTarget}
      />
    </>
  );
};

export default RoleDeleteFlow;
