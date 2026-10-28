import React, { useId, useState } from 'react';
import { Users } from 'lucide-react';
import { useBillingStore, type SeatInvite } from '../../../stores/auth/billingStore';
import { useFriendName } from './WingSeatPanel';
import {
  SEAT_COPY,
  SEAT_FALLBACK_PAYER,
  formatExpiry,
  seatBlockedLine,
  seatInviteBody,
  seatPayerCooldown,
} from './subscriptionCopy';

// One open Wing invite, on the +1's side (#2033 PR-5, spec §13). Accept is
// not optimistic: it waits for the server, because the server may refuse it.
// Decline is optimistic: the card hides at once and comes back if the
// request fails.

interface SeatInviteCardProps {
  invite: SeatInvite;
  /** The caller's own plan end, shown when their own subscription blocks Accept. */
  ownPlanEnds?: string;
}

function acceptError(error: string, payer: string, date: string | undefined): string {
  switch (error) {
    case 'invite_expired':
      return SEAT_COPY.inviteExpired;
    case 'invite_not_found':
      return SEAT_COPY.inviteGone;
    case 'already_seat_member':
      return SEAT_COPY.alreadyMember;
    case 'own_subscription_active':
      return seatBlockedLine(formatExpiry(date));
    case 'seat_cooldown':
      return seatPayerCooldown(payer, formatExpiry(date));
    default:
      return SEAT_COPY.acceptFailed;
  }
}

const SeatInviteCard: React.FC<SeatInviteCardProps> = ({ invite, ownPlanEnds }) => {
  const acceptInvite = useBillingStore((s) => s.acceptInvite);
  const declineInvite = useBillingStore((s) => s.declineInvite);
  const payer = useFriendName(invite.payer_user_id, SEAT_FALLBACK_PAYER);
  const blockedId = useId();
  const [accepting, setAccepting] = useState(false);
  const [hidden, setHidden] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (hidden) return null;

  const blocked = invite.blocked === 'own_subscription_active';

  const accept = async (): Promise<void> => {
    if (accepting || blocked) return;
    setAccepting(true);
    setError(null);
    const result = await acceptInvite(invite.invite_id);
    setAccepting(false);
    if (!result.ok) setError(acceptError(result.error, payer, result.date));
  };

  const decline = async (): Promise<void> => {
    setHidden(true);
    setError(null);
    const result = await declineInvite(invite.invite_id);
    // An invite already gone is declined as far as the user is concerned.
    if (!result.ok && result.error !== 'invite_not_found') {
      setHidden(false);
      setError(SEAT_COPY.declineFailed);
    }
  };

  return (
    <section className="subscription-seat-invite" aria-label={SEAT_COPY.inviteRegion}>
      <p className="subscription-plan-state">
        <Users size={16} className="subscription-plan-state-icon" aria-hidden="true" />
        {SEAT_COPY.inviteRegion}
      </p>
      <p className="subscription-plan-blurb">
        {seatInviteBody(payer, formatExpiry(invite.expires_at))}
      </p>
      {blocked && (
        <p id={blockedId} className="subscription-plan-hint">
          {seatBlockedLine(formatExpiry(ownPlanEnds))}
        </p>
      )}
      {error && (
        <p className="subscription-notice subscription-notice-alert" role="alert">
          {error}
        </p>
      )}
      <div className="subscription-plan-actions">
        <button
          type="button"
          className="subscription-button"
          aria-disabled={blocked || accepting}
          aria-describedby={blocked ? blockedId : undefined}
          onClick={() => {
            void accept();
          }}
        >
          {accepting ? SEAT_COPY.accepting : SEAT_COPY.accept}
        </button>
        <button
          type="button"
          className="subscription-text-button"
          aria-disabled={accepting}
          onClick={() => {
            if (!accepting) void decline();
          }}
        >
          {SEAT_COPY.decline}
        </button>
      </div>
    </section>
  );
};

export default SeatInviteCard;
