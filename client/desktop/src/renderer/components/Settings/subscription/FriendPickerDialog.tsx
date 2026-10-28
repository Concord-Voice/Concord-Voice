import React, { useId, useState } from 'react';
import Modal from '../../ui/Modal';
import { useFriendStore } from '../../../stores/chat/friendStore';
import { useBillingStore } from '../../../stores/auth/billingStore';
import { SEAT_COPY, formatExpiry, seatInviteCooldown } from './subscriptionCopy';

// Picks the friend a Wing payer invites (#2033 PR-5, spec §13): a radio list
// over the local accepted friends that commits only on Send invite. Every
// refusal reads the same ("check that you're still friends"), matching the
// server's one 404 for every ineligible invitee; only the cooldown, which the
// payer can act on, names its date.

interface FriendPickerDialogProps {
  isOpen: boolean;
  /** Change +1: the invite replaces the active seat. */
  replace: boolean;
  /** The current +1, left out of a Change +1 list. */
  excludeUserId?: string;
  /** Shown above the list for Change +1. */
  changeWarning?: string;
  onClose: () => void;
}

const FriendPickerDialog: React.FC<FriendPickerDialogProps> = ({
  isOpen,
  replace,
  excludeUserId,
  changeWarning,
  onClose,
}) => {
  const friends = useFriendStore((s) => s.friends);
  const inviteSeat = useBillingStore((s) => s.inviteSeat);
  const groupNameId = useId();
  const [selected, setSelected] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const close = (): void => {
    if (sending) return;
    setSelected(null);
    setError(null);
    onClose();
  };

  const send = async (): Promise<void> => {
    if (!selected || sending) return;
    setSending(true);
    setError(null);
    const result = await inviteSeat(selected, replace);
    setSending(false);
    if (result.ok) {
      setSelected(null);
      onClose();
      return;
    }
    setError(
      result.error === 'seat_cooldown'
        ? seatInviteCooldown(formatExpiry(result.date))
        : SEAT_COPY.pickerFailed
    );
  };

  const choices = friends.filter((f) => f.userId !== excludeUserId);

  return (
    <Modal isOpen={isOpen} onClose={close} title={SEAT_COPY.pickerTitle} width="small">
      <div className="subscription-seat-picker">
        {changeWarning && <p className="subscription-plan-blurb">{changeWarning}</p>}
        <fieldset className="subscription-seat-picker-list" disabled={sending}>
          <legend className="subscription-sr-only">{SEAT_COPY.pickerTitle}</legend>
          {choices.length === 0 && <p className="subscription-plan-blurb">{SEAT_COPY.noFriends}</p>}
          {choices.map((f) => (
            <label key={f.userId} className="subscription-seat-picker-option">
              <input
                type="radio"
                name={groupNameId}
                value={f.userId}
                checked={selected === f.userId}
                onChange={() => {
                  setSelected(f.userId);
                  setError(null);
                }}
              />
              {f.displayName || f.username}
            </label>
          ))}
        </fieldset>
        {error && (
          <p className="subscription-notice subscription-notice-alert" role="alert">
            {error}
          </p>
        )}
        <div className="subscription-plan-actions">
          <button
            type="button"
            className="subscription-button"
            aria-disabled={!selected || sending}
            onClick={() => {
              void send();
            }}
          >
            {sending ? SEAT_COPY.pickerSending : SEAT_COPY.pickerSend}
          </button>
          <button type="button" className="subscription-text-button" onClick={close}>
            Cancel
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default FriendPickerDialog;
