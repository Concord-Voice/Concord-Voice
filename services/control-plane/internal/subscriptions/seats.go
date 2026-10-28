package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// Wing seats on GET /subscriptions/me (#2033 spec §8, PR-5). Read-only: the
// seat rows are written by internal/billing. Liveness is derived here exactly
// as tier enforcement derives it, through entitlements.LiveSeatParentPredicate,
// so a pending invite past its expiry, or a seat whose parent lapsed, reads as
// what it is now even before anything materializes it. Ids only: the renderer
// takes names from its friend list.

// sourceSeat is the legacy top-level source when the enforced access is a
// Wing seat. It is never a subscriptions.source value.
const sourceSeat = "seat"

// Seat roles.
const (
	seatRolePayer  = "payer"
	seatRoleMember = "member"
)

// blockedOwnSubscription marks an invite the caller cannot accept while their
// own Stripe subscription is live (Accept's 409 own_subscription_active).
const blockedOwnSubscription = "own_subscription_active"

// SeatDTO is the caller's side of a Wing seat. For a payer it is present
// whenever they pay for a live Wing; SeatID and State are absent when no
// invite is open, and NextSwapAt is set while the 30-day cooldown runs. For
// a member it describes their active seat; Suspended is set while its
// parent does not entitle (paused or lapsed), and the seat grants nothing.
type SeatDTO struct {
	Role              string  `json:"role"`
	SeatID            string  `json:"seat_id,omitempty"`
	State             string  `json:"state,omitempty"`
	CounterpartUserID string  `json:"counterpart_user_id,omitempty"`
	InviteExpiresAt   *string `json:"invite_expires_at,omitempty"`
	NextSwapAt        *string `json:"next_swap_at,omitempty"`
	Suspended         bool    `json:"suspended,omitempty"`
}

// InviteDTO is one open invite the caller may answer.
type InviteDTO struct {
	InviteID    string `json:"invite_id"`
	PayerUserID string `json:"payer_user_id"`
	ExpiresAt   string `json:"expires_at"`
	Blocked     string `json:"blocked,omitempty"`
}

const (
	// payerWingQuery: the caller's live premium Wing, most recent first, and
	// its open seat, if any. A pending invite past its expiry reads expired.
	payerWingQuery = `SELECT s.id, s.holder_user_id,
	       CASE WHEN s.state = 'pending' AND s.invite_expires_at <= NOW() THEN 'expired' ELSE s.state END,
	       s.invite_expires_at
	  FROM subscriptions
	  LEFT JOIN subscription_seats s
	    ON s.subscription_id = subscriptions.id AND s.state IN ('pending', 'active')
	 WHERE subscriptions.user_id = $1 AND subscriptions.source = 'stripe'
	   AND subscriptions.plan = 'wing' AND subscriptions.tier = 'premium'
	   AND ` + entitlements.LiveRowPredicate + `
	 ORDER BY subscriptions.current_period_end DESC, subscriptions.created_at DESC, subscriptions.id
	 LIMIT 1`

	// nextSwapQuery mirrors billing's cooldown for "anyone but the current
	// holder": the latest counted activation plus 30 days, while that is
	// still ahead.
	nextSwapQuery = `SELECT MAX(activated_at) + INTERVAL '30 days' FROM subscription_seats
	 WHERE payer_user_id = $1 AND activated_at > NOW() - INTERVAL '30 days'
	   AND end_reason IS DISTINCT FROM 'holder_subscribed'`

	// memberSeatQuery: the caller's active seat, entitling or not, with the
	// parent's tier, status and period end for the legacy top-level fields.
	// A seat whose parent is paused or lapsed stays active (Reconcile leaves
	// it), so it is still shown, marked suspended, for Leave Wing.
	memberSeatQuery = `SELECT s.id, s.payer_user_id, subscriptions.tier, subscriptions.status,
	       subscriptions.current_period_end, (` + entitlements.LiveSeatParentPredicate + `)
	  FROM subscription_seats s
	  JOIN subscriptions ON subscriptions.id = s.subscription_id
	 WHERE s.holder_user_id = $1 AND s.state = 'active'`

	invitesQuery = `SELECT s.id, s.payer_user_id, s.invite_expires_at
	  FROM subscription_seats s
	  JOIN subscriptions ON subscriptions.id = s.subscription_id
	 WHERE s.holder_user_id = $1 AND s.state = 'pending' AND s.invite_expires_at > NOW()
	   AND subscriptions.source = 'stripe' AND subscriptions.plan = 'wing'
	   AND subscriptions.tier = 'premium' AND ` + entitlements.LiveRowPredicate + `
	 ORDER BY s.invited_at DESC, s.id`
)

// seatRead is the caller's seat side of /me.
type seatRead struct {
	seat    *SeatDTO
	member  *liveRow // the member seat as a legacy row, or nil
	invites []InviteDTO
}

// readSeats reads the caller's seat and invites. ownStripe says whether the
// caller has a live own Stripe row (an invite they cannot accept yet).
func (h *Handler) readSeats(ctx context.Context, userID string, ownStripe bool) (seatRead, error) {
	var out seatRead
	payer, err := h.readPayerSeat(ctx, userID)
	if err != nil {
		return seatRead{}, err
	}
	out.seat = payer
	if payer == nil {
		if out.seat, out.member, err = h.readMemberSeat(ctx, userID); err != nil {
			return seatRead{}, err
		}
	}
	if out.invites, err = h.readInvites(ctx, userID, ownStripe); err != nil {
		return seatRead{}, err
	}
	return out, nil
}

func (h *Handler) readPayerSeat(ctx context.Context, userID string) (*SeatDTO, error) {
	var seatID, holder, state sql.NullString
	var expires sql.NullTime
	err := h.db.QueryRowContext(ctx, payerWingQuery, userID).Scan(&seatID, &holder, &state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read payer seat: %w", err)
	}
	dto := &SeatDTO{Role: seatRolePayer, SeatID: seatID.String, State: state.String, CounterpartUserID: holder.String}
	if state.String == "pending" {
		dto.InviteExpiresAt = nullTimeString(expires)
	}
	var next sql.NullTime
	if err := h.db.QueryRowContext(ctx, nextSwapQuery, userID).Scan(&next); err != nil {
		return nil, fmt.Errorf("read next swap: %w", err)
	}
	dto.NextSwapAt = nullTimeString(next)
	return dto, nil
}

func (h *Handler) readMemberSeat(ctx context.Context, userID string) (*SeatDTO, *liveRow, error) {
	var seatID, payer string
	var entitling bool
	row := liveRow{source: sourceSeat}
	err := h.db.QueryRowContext(ctx, memberSeatQuery, userID).Scan(&seatID, &payer, &row.tier, &row.status, &row.periodEnd, &entitling)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read member seat: %w", err)
	}
	dto := &SeatDTO{Role: seatRoleMember, SeatID: seatID, State: "active", CounterpartUserID: payer, Suspended: !entitling}
	if !entitling {
		return dto, nil, nil
	}
	return dto, &row, nil
}

func (h *Handler) readInvites(ctx context.Context, userID string, ownStripe bool) (invites []InviteDTO, err error) {
	rows, err := h.db.QueryContext(ctx, invitesQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("read invites: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && err == nil {
			invites, err = nil, fmt.Errorf("close invites: %w", cerr)
		}
	}()
	for rows.Next() {
		var inv InviteDTO
		var expires time.Time
		if err := rows.Scan(&inv.InviteID, &inv.PayerUserID, &expires); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		inv.ExpiresAt = rfc3339UTC(expires)
		if ownStripe {
			inv.Blocked = blockedOwnSubscription
		}
		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read invites: %w", err)
	}
	return invites, nil
}

// withSeatRow inserts the member seat into the ordered live rows where
// getMeQuery's order puts it: after every premium Stripe row, before any
// premium code row and before any lower tier (spec §5, "PR-5 inserts the seat
// between the last two").
func withSeatRow(live []liveRow, seat *liveRow) []liveRow {
	if seat == nil {
		return live
	}
	at := len(live)
	for i := range live {
		if live[i].tier != entitlements.TierPremium || live[i].source != sourceStripe {
			at = i
			break
		}
	}
	out := make([]liveRow, 0, len(live)+1)
	out = append(out, live[:at]...)
	out = append(out, *seat)
	return append(out, live[at:]...)
}
