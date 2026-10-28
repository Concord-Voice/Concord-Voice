package billing

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The five seat endpoints (#2033 spec §8 and §9). Each loads what it needs
// without a lock, then runs one runSeatTx under the seat lock order, re-reads
// under the locks and decides there. Refusals are errSeatRefused and carry
// only their class (and, for a cooldown, its date); enumeration-sensitive
// misses share one byte-identical body.

const seatInviteMaxBody = 1 << 10

type seatInviteRequest struct {
	UserID  string `json:"user_id"`
	Replace bool   `json:"replace"`
}

// SeatInvite is POST /api/v1/billing/seat/invite.
func (s *Service) SeatInvite(c *gin.Context) {
	req, ok := bindStrictJSON(c, seatInviteMaxBody, func(r seatInviteRequest) bool {
		return r.UserID != ""
	})
	if !ok {
		return
	}
	caller, ok := s.seatCaller(c)
	if !ok {
		return
	}
	// A malformed id, the caller's own id, a stranger, a blocked friend and a
	// nonexistent user all get this one 404.
	invitee, err := uuid.Parse(req.UserID)
	if err != nil || invitee == caller {
		abortWebhook(c, http.StatusNotFound, seatErrNotEligible)
		return
	}
	ctx := c.Request.Context()
	var subRowID uuid.UUID
	var stripeSubID string
	err = s.DB.QueryRowContext(ctx, payerWingSQL, caller).Scan(&subRowID, &stripeSubID)
	if errors.Is(err, sql.ErrNoRows) {
		abortWebhook(c, http.StatusConflict, seatErrNotPayer)
		return
	}
	if err != nil {
		s.seatFailed(c, "invite", withStage(stageSeatRead, err))
		return
	}
	// A replace also moves the displaced holder's tier, so their row joins
	// the lock set; inviteLocked refuses if the open seat changed hands since.
	users := []uuid.UUID{caller, invitee}
	var displaced uuid.UUID
	if req.Replace {
		err = s.DB.QueryRowContext(ctx, openSeatHolderSQL, subRowID).Scan(&displaced)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.seatFailed(c, "invite", withStage(stageSeatRead, err))
			return
		}
		if err == nil {
			users = append(users, displaced)
		}
	}
	out, err := s.runSeatTx(ctx, stripeSubID, users,
		func(tx *sql.Tx, present map[uuid.UUID]bool) (seatOutcome, error) {
			return s.inviteLocked(ctx, tx, present, subRowID, caller, invitee, displaced, req.Replace)
		})
	if err != nil {
		s.seatFailed(c, "invite", err)
		return
	}
	s.afterSeatCommit(ctx, out)
	c.JSON(http.StatusCreated, gin.H{"seat": SeatDTO{
		SeatID:          out.invite.seatID.String(),
		State:           SeatStatePending,
		PayerUserID:     caller.String(),
		HolderUserID:    invitee.String(),
		InviteExpiresAt: out.invite.expiresAt.UTC().Format(time.RFC3339),
	}})
}

// inviteLocked is the invite under the advisory key and the users' locks;
// displaced is the open seat's holder as read before locking (uuid.Nil when
// there was none). The cooldown is checked before replace revokes anything,
// so a refused swap leaves the old seat intact.
func (s *Service) inviteLocked(ctx context.Context, tx *sql.Tx, present map[uuid.UUID]bool,
	subRowID, payer, invitee, displaced uuid.UUID, replace bool,
) (seatOutcome, error) {
	if !present[payer] {
		return seatOutcome{}, refuse(http.StatusConflict, seatErrNotPayer)
	}
	if !present[invitee] {
		return seatOutcome{}, refuse(http.StatusNotFound, seatErrNotEligible)
	}
	live, err := parentLive(ctx, tx, subRowID, payer)
	if err != nil {
		return seatOutcome{}, err
	}
	if !live {
		return seatOutcome{}, refuse(http.StatusConflict, seatErrNotPayer)
	}
	var out seatOutcome
	if out.changes, err = expireStaleInvites(ctx, tx, subRowID); err != nil {
		return seatOutcome{}, err
	}
	var eligible bool
	if err := tx.QueryRowContext(ctx, eligibleInviteeSQL, payer, invitee).Scan(&eligible); err != nil {
		return seatOutcome{}, withStage(stageSeatRead, err)
	}
	if !eligible {
		return seatOutcome{}, refuse(http.StatusNotFound, seatErrNotEligible)
	}
	var openID, openHolder uuid.UUID
	var openState string
	err = tx.QueryRowContext(ctx, openSeatForUpdateSQL, subRowID).Scan(&openID, &openHolder, &openState)
	hasOpen := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return seatOutcome{}, withStage(stageSeatRead, err)
	}
	// A holder that is not the locked one means the seat changed hands after
	// the pre-read; the client re-reads and tries again.
	if hasOpen && (!replace || openHolder == invitee || openHolder != displaced) {
		return seatOutcome{}, refuse(http.StatusConflict, seatErrOccupied)
	}
	until, err := cooldownUntil(ctx, tx, payer, invitee)
	if err != nil {
		return seatOutcome{}, err
	}
	if !until.IsZero() {
		return seatOutcome{}, cooldownRefusal(until)
	}
	if hasOpen {
		if err := s.replaceOpenSeat(ctx, tx, &out, openID, payer, openHolder, openState); err != nil {
			return seatOutcome{}, err
		}
	}
	inv := &seatInvite{holder: invitee}
	if err := tx.QueryRowContext(ctx, insertSeatSQL, subRowID, payer, invitee).Scan(&inv.seatID, &inv.expiresAt); err != nil {
		return seatOutcome{}, withStage(stageSeatWrite, err)
	}
	out.invite = inv
	return out, nil
}

// replaceOpenSeat ends the seat a replace=true invite displaces: an active
// seat is swapped (its holder loses premium now), a pending one withdrawn.
func (s *Service) replaceOpenSeat(ctx context.Context, tx *sql.Tx, out *seatOutcome,
	seatID, payer, holder uuid.UUID, state string,
) error {
	reason := seatEndPayerWithdrew
	if state == SeatStateActive {
		reason = seatEndPayerSwapped
	}
	return s.endWithTier(ctx, tx, out, seatID, payer, holder, state, SeatStateRevoked, reason)
}

// endWithTier ends one open seat and records the change, plus the holder's
// tier on both sides when the seat was active.
func (s *Service) endWithTier(ctx context.Context, tx *sql.Tx, out *seatOutcome,
	seatID, payer, holder uuid.UUID, fromState, toState, reason string,
) error {
	var before string
	var err error
	if fromState == SeatStateActive {
		if before, err = seatTier(ctx, tx, holder); err != nil {
			return err
		}
	}
	if err := endSeat(ctx, tx, seatID, toState, reason); err != nil {
		return err
	}
	out.changes = append(out.changes, seatChange{seatID: seatID, payer: payer, holder: holder, state: toState})
	if fromState != SeatStateActive {
		return nil
	}
	after, err := seatTier(ctx, tx, holder)
	if err != nil {
		return err
	}
	out.tiers = append(out.tiers, tierMove{user: holder, before: before, after: after})
	return nil
}

// SeatRevoke is DELETE /api/v1/billing/seat: the payer revokes the active
// seat or withdraws the pending invite.
func (s *Service) SeatRevoke(c *gin.Context) {
	caller, ok := s.seatCaller(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	var seatID, holder uuid.UUID
	var stripeSubID string
	err := s.DB.QueryRowContext(ctx, payerOpenSeatSQL, caller).Scan(&seatID, &holder, &stripeSubID)
	if errors.Is(err, sql.ErrNoRows) {
		abortWebhook(c, http.StatusNotFound, seatErrNoSeat)
		return
	}
	if err != nil {
		s.seatFailed(c, "revoke", withStage(stageSeatRead, err))
		return
	}
	out, err := s.runSeatTx(ctx, stripeSubID, []uuid.UUID{caller, holder},
		func(tx *sql.Tx, _ map[uuid.UUID]bool) (seatOutcome, error) {
			return s.endOpenSeatLocked(ctx, tx, seatID, caller, holder, func(state string) (string, string, bool) {
				switch state {
				case SeatStateActive:
					return SeatStateRevoked, seatEndPayerRevoked, true
				case SeatStatePending:
					return SeatStateRevoked, seatEndPayerWithdrew, true
				}
				return "", "", false
			})
		})
	if err != nil {
		s.seatFailed(c, "revoke", err)
		return
	}
	s.afterSeatCommit(ctx, out)
	c.AbortWithStatus(http.StatusNoContent)
}

// SeatLeave is POST /api/v1/billing/seat/leave: the holder leaves an active seat.
func (s *Service) SeatLeave(c *gin.Context) {
	caller, ok := s.seatCaller(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	var seatID, payer uuid.UUID
	var stripeSubID string
	err := s.DB.QueryRowContext(ctx, holderActiveSeatSQL, caller).Scan(&seatID, &payer, &stripeSubID)
	if errors.Is(err, sql.ErrNoRows) {
		abortWebhook(c, http.StatusNotFound, seatErrNoSeat)
		return
	}
	if err != nil {
		s.seatFailed(c, "leave", withStage(stageSeatRead, err))
		return
	}
	out, err := s.runSeatTx(ctx, stripeSubID, []uuid.UUID{payer, caller},
		func(tx *sql.Tx, _ map[uuid.UUID]bool) (seatOutcome, error) {
			return s.endOpenSeatLocked(ctx, tx, seatID, payer, caller, func(state string) (string, string, bool) {
				return SeatStateLeft, seatEndHolderLeft, state == SeatStateActive
			})
		})
	if err != nil {
		s.seatFailed(c, "leave", err)
		return
	}
	s.afterSeatCommit(ctx, out)
	c.AbortWithStatus(http.StatusNoContent)
}

// endOpenSeatLocked re-reads the seat under its lock and ends it when next
// says the current state allows; otherwise the seat moved first and the op is
// a 404 no_seat.
func (s *Service) endOpenSeatLocked(ctx context.Context, tx *sql.Tx, seatID, payer, holder uuid.UUID,
	next func(state string) (toState, reason string, ok bool),
) (seatOutcome, error) {
	var state string
	err := tx.QueryRowContext(ctx, relockSeatSQL, seatID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return seatOutcome{}, refuse(http.StatusNotFound, seatErrNoSeat)
	}
	if err != nil {
		return seatOutcome{}, withStage(stageSeatRead, err)
	}
	toState, reason, ok := next(state)
	if !ok {
		return seatOutcome{}, refuse(http.StatusNotFound, seatErrNoSeat)
	}
	var out seatOutcome
	if err := s.endWithTier(ctx, tx, &out, seatID, payer, holder, state, toState, reason); err != nil {
		return seatOutcome{}, err
	}
	return out, nil
}

// SeatAccept is POST /api/v1/billing/seat-invites/:id/accept.
func (s *Service) SeatAccept(c *gin.Context) {
	caller, ok := s.seatCaller(c)
	if !ok {
		return
	}
	seatID, payer, stripeSubID, ok := s.loadOwnInvite(c, caller)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	// Best effort, before any lock (R33): close the caller's own open Checkout
	// so a sheet left open on another device cannot complete into a double
	// pay. Reconcile's holder_subscribed is the backstop when this misses.
	s.preExpireOwnCheckout(ctx, caller)
	out, err := s.runSeatTx(ctx, stripeSubID, []uuid.UUID{payer, caller},
		func(tx *sql.Tx, present map[uuid.UUID]bool) (seatOutcome, error) {
			return s.acceptLocked(ctx, tx, present, seatID, payer, caller)
		})
	if err != nil {
		s.seatFailed(c, "accept", err)
		return
	}
	s.afterSeatCommit(ctx, out)
	c.JSON(http.StatusOK, gin.H{"seat": SeatDTO{
		SeatID:          seatID.String(),
		State:           SeatStateActive,
		PayerUserID:     payer.String(),
		HolderUserID:    caller.String(),
		InviteExpiresAt: out.expiresAt.UTC().Format(time.RFC3339),
	}})
}

// acceptLocked is accept under its locks, in the spec's order: the
// caller-scoped re-read, expiry and the parent (410), the caller's own live
// Stripe row (409), the cooldown, then the active-holder index.
func (s *Service) acceptLocked(ctx context.Context, tx *sql.Tx, present map[uuid.UUID]bool,
	seatID, payer, holder uuid.UUID,
) (seatOutcome, error) {
	if !present[payer] || !present[holder] {
		return seatOutcome{}, refuse(http.StatusNotFound, seatErrInviteNotFound)
	}
	var subRowID uuid.UUID
	var expired bool
	err := tx.QueryRowContext(ctx, relockPendingSQL, seatID, holder).Scan(&subRowID, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return seatOutcome{}, refuse(http.StatusNotFound, seatErrInviteNotFound)
	}
	if err != nil {
		return seatOutcome{}, withStage(stageSeatRead, err)
	}
	live, err := parentLive(ctx, tx, subRowID, payer)
	if err != nil {
		return seatOutcome{}, err
	}
	if expired || !live {
		return seatOutcome{}, refuse(http.StatusGone, seatErrInviteExpired)
	}
	var ownEnd time.Time
	err = tx.QueryRowContext(ctx, ownLiveStripeEndSQL, holder).Scan(&ownEnd)
	if err == nil {
		return seatOutcome{}, &errSeatRefused{status: http.StatusConflict, class: seatErrOwnSub,
			body: map[string]any{"current_period_end": ownEnd.UTC().Format(time.RFC3339)}}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return seatOutcome{}, withStage(stageSeatRead, err)
	}
	until, err := cooldownUntil(ctx, tx, payer, holder)
	if err != nil {
		return seatOutcome{}, err
	}
	if !until.IsZero() {
		return seatOutcome{}, cooldownRefusal(until)
	}
	before, err := seatTier(ctx, tx, holder)
	if err != nil {
		return seatOutcome{}, err
	}
	var expiresAt time.Time
	if err := tx.QueryRowContext(ctx, activateSeatSQL, seatID).Scan(&expiresAt); err != nil {
		if isUniqueViolation(err) {
			return seatOutcome{}, refuse(http.StatusConflict, seatErrAlreadyMember)
		}
		return seatOutcome{}, withStage(stageSeatWrite, err)
	}
	after, err := seatTier(ctx, tx, holder)
	if err != nil {
		return seatOutcome{}, err
	}
	return seatOutcome{
		changes: []seatChange{{seatID: seatID, payer: payer, holder: holder, state: SeatStateActive}},
		// Pushed even when the tier did not move (a code holder accepting), so
		// the holder's page shows where premium now comes from.
		tiers:     []tierMove{{user: holder, before: before, after: after}},
		expiresAt: expiresAt,
	}, nil
}

// SeatDecline is POST /api/v1/billing/seat-invites/:id/decline.
func (s *Service) SeatDecline(c *gin.Context) {
	caller, ok := s.seatCaller(c)
	if !ok {
		return
	}
	seatID, payer, stripeSubID, ok := s.loadOwnInvite(c, caller)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	out, err := s.runSeatTx(ctx, stripeSubID, []uuid.UUID{payer, caller},
		func(tx *sql.Tx, _ map[uuid.UUID]bool) (seatOutcome, error) {
			var subRowID uuid.UUID
			var expired bool
			err := tx.QueryRowContext(ctx, relockPendingSQL, seatID, caller).Scan(&subRowID, &expired)
			if errors.Is(err, sql.ErrNoRows) {
				return seatOutcome{}, refuse(http.StatusNotFound, seatErrInviteNotFound)
			}
			if err != nil {
				return seatOutcome{}, withStage(stageSeatRead, err)
			}
			if err := endSeat(ctx, tx, seatID, SeatStateDeclined, seatEndDeclined); err != nil {
				return seatOutcome{}, err
			}
			return seatOutcome{changes: []seatChange{
				{seatID: seatID, payer: payer, holder: caller, state: SeatStateDeclined},
			}}, nil
		})
	if err != nil {
		s.seatFailed(c, "decline", err)
		return
	}
	s.afterSeatCommit(ctx, out)
	c.AbortWithStatus(http.StatusNoContent)
}

// loadOwnInvite is the caller-scoped load (F1). A malformed id, someone
// else's invite, a nonexistent one and one no longer pending all write the
// same 404 invite_not_found.
func (s *Service) loadOwnInvite(c *gin.Context, caller uuid.UUID) (seatID, payer uuid.UUID, stripeSubID string, ok bool) {
	seatID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		abortWebhook(c, http.StatusNotFound, seatErrInviteNotFound)
		return uuid.Nil, uuid.Nil, "", false
	}
	err = s.DB.QueryRowContext(c.Request.Context(), seatByHolderSQL, seatID, caller).Scan(&payer, &stripeSubID)
	if errors.Is(err, sql.ErrNoRows) {
		abortWebhook(c, http.StatusNotFound, seatErrInviteNotFound)
		return uuid.Nil, uuid.Nil, "", false
	}
	if err != nil {
		s.seatFailed(c, "load_invite", withStage(stageSeatRead, err))
		return uuid.Nil, uuid.Nil, "", false
	}
	return seatID, payer, stripeSubID, true
}

// preExpireOwnCheckout expires the caller's open Checkout sessions, if they
// have a Customer. Failures are logged at warn and never block the accept.
func (s *Service) preExpireOwnCheckout(ctx context.Context, userID uuid.UUID) {
	var cus string
	err := s.DB.QueryRowContext(ctx, customerByUserSQL, userID).Scan(&cus)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, seatPreCloseTimeout)
		defer cancel()
		err = s.expireOpenSessions(ctx, cus)
	}
	if err != nil {
		s.Log.Warn("billing: could not close the invitee's open checkout before accepting a seat",
			logKeyErrorClass, errorClass(err), logKeyStage, stageSeatPreClose)
	}
}

// seatCaller reads the authenticated user id, writing a 401 when it is unusable.
func (s *Service) seatCaller(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.GetString("user_id"))
	if err != nil {
		s.Log.Error("billing: seat request has no usable authenticated user id")
		abortWebhook(c, http.StatusUnauthorized, portalErrUnauth)
		return uuid.Nil, false
	}
	return id, true
}

// seatFailed writes a refusal as its own status and class, and anything else
// as 503 billing_unavailable with an error log naming the op and stage.
func (s *Service) seatFailed(c *gin.Context, op string, err error) {
	var r *errSeatRefused
	if errors.As(err, &r) {
		body := gin.H{"error": r.class}
		for k, v := range r.body {
			body[k] = v
		}
		c.AbortWithStatusJSON(r.status, body)
		return
	}
	s.Log.Error("billing: seat operation failed",
		"op", op, logKeyErrorClass, errorClass(err), logKeyStage, errorStage(err))
	abortWebhook(c, http.StatusServiceUnavailable, portalErrUnavailable)
}

// isUniqueViolation reports a PostgreSQL 23505.
func isUniqueViolation(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505"
}
