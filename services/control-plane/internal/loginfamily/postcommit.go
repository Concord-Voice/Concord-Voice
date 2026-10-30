package loginfamily

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// PostCommitCloseTimeout bounds the synchronous transport fence a revoking
// request performs after its commit. It is deliberately short: the durable
// owner, not this fast path, is the delivery obligation.
const PostCommitCloseTimeout = 3 * time.Second

// FamilyCloser fences and closes every live transport bound to one exact
// family, returning only after the fence is in place (websocket.Hub).
type FamilyCloser interface {
	DisconnectFamilyContext(ctx context.Context, userID, familyID uuid.UUID) error
}

const (
	authOwnerFamilyPage = `SELECT family_id FROM login_families
 WHERE auth_operation_id=$1 AND user_id=$2 AND terminal_at IS NOT NULL AND family_id>$3
 ORDER BY family_id LIMIT $4`
	epochOwnerFamilyPage = `SELECT family_id FROM login_families
 WHERE epoch_generation=$1 AND user_id=$2 AND terminal_at IS NOT NULL AND family_id>$3
 ORDER BY family_id LIMIT $4`
)

// CloseCommittedFamilies synchronously fences the live transports of every
// family the committed owners made terminal, so a revoked socket stops sending
// and receiving before the revoking request answers. Call it only after a
// definite commit. It is a fast path, never the obligation: an error leaves
// the sealed owner for the reconciler to retry and must not rescind or fail
// the committed revocation. The caller's cancellation is detached so a client
// disconnect cannot abandon the fence; PostCommitCloseTimeout bounds it.
func CloseCommittedFamilies(ctx context.Context, db *sql.DB, closer FamilyCloser, owners []DeliveryOwner) error {
	if db == nil || closer == nil {
		return errors.New("post-commit family close unavailable")
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PostCommitCloseTimeout)
	defer cancel()
	var outcome error
	for _, owner := range owners {
		outcome = errors.Join(outcome, closeOwnerFamilies(closeCtx, db, closer, owner))
	}
	return outcome
}

func closeOwnerFamilies(ctx context.Context, db *sql.DB, closer FamilyCloser, owner DeliveryOwner) error {
	query, generation := authOwnerFamilyPage, owner.AuthOperationID
	if owner.Kind == OwnerEpoch {
		query, generation = epochOwnerFamilyPage, owner.EpochGeneration
	} else if owner.Kind != OwnerAuth {
		return NewError(ReasonOwnershipInvalid, nil)
	}
	if owner.UserID == uuid.Nil || generation == uuid.Nil {
		return NewError(ReasonOwnershipInvalid, nil)
	}
	var outcome error
	after := uuid.Nil
	for {
		page, err := ownerFamilyPage(ctx, db, query, generation, owner.UserID, after)
		if err != nil {
			return errors.Join(outcome, err)
		}
		for _, family := range page {
			outcome = errors.Join(outcome, closer.DisconnectFamilyContext(ctx, owner.UserID, family))
		}
		if len(page) < targetPageSize {
			return outcome
		}
		after = page[len(page)-1]
	}
}

func ownerFamilyPage(ctx context.Context, db *sql.DB, query string, generation, user, after uuid.UUID) (page []uuid.UUID, retErr error) {
	rows, err := db.QueryContext(ctx, query, generation, user, after, targetPageSize)
	if err != nil {
		return nil, sqlFailure(err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			page, retErr = nil, sqlFailure(errors.Join(retErr, closeErr))
		}
	}()
	for rows.Next() {
		var family uuid.UUID
		if err := rows.Scan(&family); err != nil {
			return nil, sqlFailure(err)
		}
		page = append(page, family)
	}
	if err := rows.Err(); err != nil {
		return nil, sqlFailure(err)
	}
	return page, nil
}
