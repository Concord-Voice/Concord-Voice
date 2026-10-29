import React, { useEffect, useId, useState } from 'react';
import { useFriendStore } from '../../../stores/chat/friendStore';
import { useBillingStore, type SeatInfo, type SeatResult } from '../../../stores/auth/billingStore';
import ConfirmActionModal from '../../ui/ConfirmActionModal';
import FriendPickerDialog from './FriendPickerDialog';
import {
  SEAT_COPY,
  SEAT_FALLBACK_HOLDER,
  formatExpiry,
  seatActiveLine,
  seatChangeConfirm,
  seatCooldownLine,
  seatExpiredLine,
  seatPendingLine,
  seatRemoveConfirm,
} from './subscriptionCopy';

// The Wing payer's well inside PlanCard (#2033 PR-5, spec §13). States come
// straight from `/me.seat` (role payer): no open seat (Empty / No friends),
// pending, active, or expired. The cooldown line shows whenever the server
// sends a future `next_swap_at`; it blocks Change +1, the one action that
// always picks someone new. No server event marks the deadline, so a timer
// re-reads /me then and the clock alone lifts the block if that read fails. An empty well stays usable, because re-inviting the last +1 is
// exempt and the server answers anyone else with the date.

/** A friend's display name, or the fallback when they are not on the list. */
export function useFriendName(userId: string | undefined, fallback: string): string {
  return useFriendStore((s) => {
    const friend = userId ? s.friends.find((f) => f.userId === userId) : undefined;
    return friend?.displayName || friend?.username || fallback;
  });
}

interface WingSeatPanelProps {
  seat: SeatInfo;
}

type Confirm = 'remove' | 'withdraw' | null;

/** Copy for the remove / withdraw confirmation dialog. */
function confirmCopy(confirm: Confirm, name: string, nextSwap: string | undefined) {
  if (confirm === 'withdraw') {
    return {
      title: SEAT_COPY.withdraw,
      message: `Withdraw your invite to ${name}?`,
      confirmLabel: SEAT_COPY.withdraw,
      cancelLabel: undefined,
    };
  }
  return {
    title: SEAT_COPY.remove,
    message: seatRemoveConfirm(name, nextSwap),
    confirmLabel: SEAT_COPY.remove,
    cancelLabel: confirm === 'remove' ? `Keep ${name}` : undefined,
  };
}

const WingSeatPanel: React.FC<WingSeatPanelProps> = ({ seat }) => {
  const removeSeat = useBillingStore((s) => s.removeSeat);
  const fetchMe = useBillingStore((s) => s.fetchMe);
  const hasFriends = useFriendStore((s) => s.friends.length > 0);
  const name = useFriendName(seat.counterpart_user_id, SEAT_FALLBACK_HOLDER);
  const cooldownId = useId();
  const [picker, setPicker] = useState<'invite' | 'change' | null>(null);
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [now, setNow] = useState(() => Date.now());

  const swapAt = seat.next_swap_at ? Date.parse(seat.next_swap_at) : Number.NaN;
  useEffect(() => {
    const wait = swapAt - Date.now();
    if (Number.isNaN(wait) || wait <= 0) return undefined;
    // ponytail: setTimeout caps at ~24.8 days; a longer cooldown fires early,
    // `now` moves and the effect re-arms for the rest.
    const timer = globalThis.setTimeout(
      () => {
        setNow(Date.now());
        void fetchMe();
      },
      Math.min(wait, 2 ** 31 - 1)
    );
    return () => globalThis.clearTimeout(timer);
  }, [swapAt, now, fetchMe]);

  const nextSwap = swapAt > now ? formatExpiry(seat.next_swap_at) : undefined;
  const describedBy = nextSwap ? cooldownId : undefined;
  const cooldown = nextSwap ? (
    <p id={cooldownId} className="subscription-plan-hint">
      {seatCooldownLine(nextSwap)}
    </p>
  ) : null;

  const runRemove = async (): Promise<void> => {
    const result: SeatResult = await removeSeat();
    // A seat that is already gone is the outcome the user asked for.
    if (!result.ok && result.error !== 'no_seat') throw new Error(SEAT_COPY.actionFailed);
  };

  let body: React.ReactNode;
  if (seat.state === 'active') {
    body = (
      <>
        <p className="subscription-plan-blurb">{seatActiveLine(name)}</p>
        {cooldown}
        <div className="subscription-plan-actions">
          <button
            type="button"
            className="subscription-button"
            aria-disabled={nextSwap !== undefined}
            aria-describedby={describedBy}
            onClick={() => {
              if (!nextSwap) setPicker('change');
            }}
          >
            {SEAT_COPY.change}
          </button>
          <button
            type="button"
            className="subscription-text-button"
            onClick={() => setConfirm('remove')}
          >
            {SEAT_COPY.remove}
          </button>
        </div>
      </>
    );
  } else if (seat.state === 'pending') {
    body = (
      <>
        <p className="subscription-plan-blurb">
          {seatPendingLine(name, formatExpiry(seat.invite_expires_at))}
        </p>
        <div className="subscription-plan-actions">
          <button
            type="button"
            className="subscription-text-button"
            onClick={() => setConfirm('withdraw')}
          >
            {SEAT_COPY.withdraw}
          </button>
        </div>
      </>
    );
  } else if (hasFriends) {
    body = (
      <>
        {seat.state === 'expired' && (
          <p className="subscription-plan-blurb">{seatExpiredLine(name)}</p>
        )}
        {cooldown}
        <div className="subscription-plan-actions">
          <button
            type="button"
            className="subscription-button"
            aria-describedby={describedBy}
            onClick={() => setPicker('invite')}
          >
            {seat.state === 'expired' ? SEAT_COPY.inviteSomeoneElse : SEAT_COPY.inviteFriend}
          </button>
        </div>
      </>
    );
  } else {
    body = <p className="subscription-plan-blurb">{SEAT_COPY.noFriends}</p>;
  }

  return (
    <section className="subscription-seat-well" aria-label={SEAT_COPY.wellTitle}>
      <h4 className="subscription-seat-well-title">{SEAT_COPY.wellTitle}</h4>
      {body}
      <FriendPickerDialog
        isOpen={picker !== null}
        replace={picker === 'change'}
        excludeUserId={picker === 'change' ? seat.counterpart_user_id : undefined}
        changeWarning={picker === 'change' ? seatChangeConfirm(name) : undefined}
        onClose={() => setPicker(null)}
      />
      <ConfirmActionModal
        isOpen={confirm !== null}
        onClose={() => setConfirm(null)}
        {...confirmCopy(confirm, name, nextSwap)}
        loadingLabel="Updating…"
        onConfirm={runRemove}
      />
    </section>
  );
};

export default WingSeatPanel;
