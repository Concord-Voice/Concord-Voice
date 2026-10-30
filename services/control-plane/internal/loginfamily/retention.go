package loginfamily

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

const gcStreams = 4

var gcMaximumID = uuid.UUID{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255}

// One bounded continuation record serves the existing single hourly caller.
// Switching pools/restarting loses progress, never authority. This deliberately
// does not grow a map for arbitrary pools, principals, or retained generations.
var gcContinuation struct {
	sync.Mutex
	db    *sql.DB
	after [gcStreams]uuid.UUID
	next  int
}

type gcPass struct {
	start   uuid.UUID
	wrapped bool
	done    bool
}

// gcAuthority is deletion-only and exists solely inside its owning transaction.
// A cohort has one principal by the matching-user operation/target constraints.
// We stream its complete sorted family set into SQL locks, not an unbounded Go
// identity map. No deletion is permitted until the full set has been checked.
type gcAuthority struct {
	tx       *sql.Tx
	id       uuid.UUID
	user     uuid.UUID
	cohort   bool
	families int64
	eligible bool
}

// SweepRetiredEvidence consumes the caller's remaining cleanup context; it adds
// no ticker, detached work, or replacement deadline. limit bounds discovery and
// lock pages, never the size of a sealed deletion unit. Callers must supply the
// existing bounded cleanup context. Counts describe confirmed commits only.
func SweepRetiredEvidence(ctx context.Context, db *sql.DB, limit int) (result GCResult, retErr error) {
	if db == nil || limit <= 0 {
		return result, NewError(ReasonArgumentInvalid, nil)
	}
	if err := ctx.Err(); err != nil {
		return GCResult{Incomplete: true}, sqlFailure(err)
	}
	if !gcContinuation.TryLock() {
		return GCResult{Incomplete: true}, nil
	}
	defer gcContinuation.Unlock()
	if gcContinuation.db != db {
		gcContinuation.db = db
		gcContinuation.after = [gcStreams]uuid.UUID{}
		gcContinuation.next = 0
	}
	limit = min(limit, 100)
	var passes [gcStreams]gcPass
	for stream := range passes {
		passes[stream].start = gcContinuation.after[stream]
	}
	remaining := gcStreams
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			result.Incomplete = true
			return result, sqlFailure(err)
		}
		stream := gcContinuation.next
		gcContinuation.next = (stream + 1) % gcStreams
		if passes[stream].done {
			continue
		}
		unit, err := gcSweepPage(ctx, db, stream, &passes[stream], limit)
		gcAccumulate(&result, unit)
		if err != nil {
			result.Incomplete = true
			return result, err
		}
		if passes[stream].done {
			remaining--
		}
	}
	return result, nil
}

func gcSweepPage(ctx context.Context, db *sql.DB, stream int, pass *gcPass, limit int) (result GCResult, err error) {
	page, err := gcCandidates(ctx, db, stream, *pass, limit)
	if err != nil {
		return result, err
	}
	if len(page) == 0 {
		if pass.start != uuid.Nil && !pass.wrapped {
			pass.wrapped = true
		} else {
			pass.done = true
		}
		gcContinuation.after[stream] = uuid.Nil
		return result, nil
	}
	for _, id := range page {
		// Advance even on a skipped/failed unit so a busy prefix cannot starve
		// later work. The circular next pass revisits every retained identity.
		gcContinuation.after[stream] = id
		unit, err := gcUnit(ctx, db, stream, id, limit)
		gcAccumulate(&result, unit)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func gcCandidates(ctx context.Context, db *sql.DB, stream int, pass gcPass, limit int) (ids []uuid.UUID, retErr error) {
	through := gcMaximumID
	if pass.wrapped {
		through = pass.start
	}
	rows, err := db.QueryContext(ctx, gcCandidateQueries[stream], gcContinuation.after[stream], through, limit)
	if err != nil {
		return nil, sqlFailure(err)
	}
	defer func() { retErr = gcRowsClose(rows, retErr) }()
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return nil, sqlFailure(err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, sqlFailure(err)
	}
	return ids, nil
}

func gcAccumulate(result *GCResult, unit GCResult) {
	result.DeletedSIDs += unit.DeletedSIDs
	result.DeletedFamilies += unit.DeletedFamilies
	result.DeletedOperations += unit.DeletedOperations
	result.DeletedConnectionClosures += unit.DeletedConnectionClosures
	result.DeletedRoomClosures += unit.DeletedRoomClosures
	result.Incomplete = result.Incomplete || unit.Incomplete
}

func gcRowsClose(rows *sql.Rows, cause error) error {
	if err := rows.Close(); err != nil {
		return sqlFailure(errors.Join(cause, err))
	}
	return cause
}

func gcContended(err error) bool {
	var failure *Error
	if errors.As(err, &failure) && failure.Reason() == ReasonCommitUnknown {
		return false
	}
	var pg *pq.Error
	return errors.As(err, &pg) && (pg.Code == "55P03" || pg.Code == "40P01")
}

func gcUnit(ctx context.Context, db *sql.DB, stream int, id uuid.UUID, limit int) (result GCResult, retErr error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return result, sqlFailure(err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = sqlFailure(errors.Join(retErr, rollbackErr))
			result = GCResult{Incomplete: true}
			return
		}
		if gcContended(retErr) {
			retErr = nil
			result = GCResult{Incomplete: true}
		}
	}()
	// NOWAIT protects explicit row locks; this also bounds implicit FK/trigger
	// lock upgrades during deletion. A timeout rolls back the complete unit.
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout = '1ms'; SET CONSTRAINTS ALL DEFERRED`); err != nil {
		return result, sqlFailure(err)
	}
	if stream < 2 {
		result, err = gcFamilyUnit(ctx, tx, stream == 0, id, limit)
	} else {
		result, err = gcClosureUnit(ctx, tx, stream == 2, id)
	}
	if err != nil {
		return GCResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return GCResult{Incomplete: true}, NewError(ReasonCommitUnknown, err)
	}
	return result, nil
}

func gcFamilyUnit(ctx context.Context, tx *sql.Tx, cohort bool, id uuid.UUID, limit int) (GCResult, error) {
	authority, err := gcLockFamilies(ctx, tx, cohort, id, limit)
	if err != nil {
		return GCResult{}, err
	}
	if authority.families == 0 {
		return GCResult{}, nil
	}
	if !authority.eligible {
		return GCResult{Incomplete: true}, nil
	}
	return authority.delete(ctx)
}

func gcLockFamilies(ctx context.Context, tx *sql.Tx, cohort bool, id uuid.UUID, limit int) (a gcAuthority, retErr error) {
	a = gcAuthority{tx: tx, cohort: cohort, id: id}
	query := `SELECT user_id FROM login_families WHERE family_id = $1 AND auth_operation_id IS NULL`
	if cohort {
		query = `SELECT user_id FROM login_family_revocations WHERE operation_id = $1`
	}
	if err := tx.QueryRowContext(ctx, query, id).Scan(&a.user); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return a, nil
		}
		return a, sqlFailure(err)
	}
	present, err := gcLockPrincipal(ctx, tx, a.user)
	if err != nil {
		return a, err
	}
	a.families, err = a.lockAllFamilies(ctx, limit)
	if err != nil {
		return a, err
	}
	if a.families == 0 {
		return a, nil
	}
	// A fresh statement after ALL family locks is essential for erased users.
	// A principal that appeared during acquisition refuses this unit; never take
	// a new earlier user lock here. Live orphan families cannot use erased mode.
	var principalUnchanged bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1) = $2`, a.user, present).Scan(&principalUnchanged); err != nil {
		return a, sqlFailure(err)
	}
	if !principalUnchanged {
		return a, nil
	}
	consistent, err := a.checkOwner(ctx, present)
	if err != nil || !consistent {
		return a, err
	}
	if err = tx.QueryRowContext(ctx, gcFamilyEligible, cohort, id).Scan(&a.eligible); err != nil {
		return a, sqlFailure(err)
	}
	return a, nil
}

func (a gcAuthority) lockAllFamilies(ctx context.Context, limit int) (total int64, err error) {
	var after uuid.UUID
	for {
		last, count, err := a.lockPage(ctx, after, limit)
		if err != nil {
			return total, err
		}
		total += count
		if count == 0 {
			return total, nil
		}
		after = last
	}
}

func gcLockPrincipal(ctx context.Context, tx *sql.Tx, user uuid.UUID) (bool, error) {
	var lockedUser uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, user).Scan(&lockedUser)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, sqlFailure(err)
	}
	return true, nil
}

func (a gcAuthority) lockPage(ctx context.Context, after uuid.UUID, limit int) (last uuid.UUID, count int64, retErr error) {
	rows, err := a.tx.QueryContext(ctx, gcFamilyPage, a.cohort, a.id, after, limit)
	if err != nil {
		return last, count, sqlFailure(err)
	}
	defer func() { retErr = gcRowsClose(rows, retErr) }()
	for rows.Next() {
		var user uuid.UUID
		if err = rows.Scan(&last, &user); err != nil {
			return last, count, sqlFailure(err)
		}
		if user != a.user {
			return last, count, NewError(ReasonOwnershipInvalid, nil)
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return last, count, sqlFailure(err)
	}
	return last, count, nil
}

func (a gcAuthority) checkOwner(ctx context.Context, present bool) (bool, error) {
	var valid bool
	if !a.cohort {
		err := a.tx.QueryRowContext(ctx, `SELECT count(*) = 1 AND bool_and(user_id = $2 AND
 ($3 OR terminal_at IS NOT NULL)) FROM login_families WHERE family_id = $1 AND auth_operation_id IS NULL`, a.id, a.user, present).Scan(&valid)
		if err != nil {
			return false, sqlFailure(err)
		}
		return valid, nil
	}
	err := a.tx.QueryRowContext(ctx, `SELECT sealed_at IS NOT NULL AND completed_at IS NOT NULL
 FROM login_family_revocations WHERE operation_id = $1 AND user_id = $2 FOR UPDATE NOWAIT`, a.id, a.user).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, sqlFailure(err)
	}
	if !valid {
		return false, nil
	}
	if err = a.tx.QueryRowContext(ctx, gcCohortConsistent, a.id, a.user, a.families).Scan(&valid); err != nil {
		return false, sqlFailure(err)
	}
	return valid, nil
}

func (a gcAuthority) delete(ctx context.Context) (result GCResult, err error) {
	if a.tx == nil || a.id == uuid.Nil || a.user == uuid.Nil || a.families == 0 || !a.eligible {
		return result, NewError(ReasonCapabilityInvalid, nil)
	}
	result.DeletedSIDs, err = gcExec(ctx, a.tx, gcDeleteSIDs, a.cohort, a.id)
	if err != nil {
		return result, err
	}
	if a.cohort {
		changed, err := gcExec(ctx, a.tx, `DELETE FROM login_family_revocation_targets WHERE operation_id = $1`, a.id)
		if err != nil {
			return result, err
		}
		if changed != a.families {
			return result, NewError(ReasonOwnershipInvalid, nil)
		}
	}
	result.DeletedFamilies, err = gcExec(ctx, a.tx, gcDeleteFamilies, a.cohort, a.id)
	if err != nil {
		return result, err
	}
	if result.DeletedFamilies != a.families {
		return result, NewError(ReasonOwnershipInvalid, nil)
	}
	if a.cohort {
		result.DeletedOperations, err = gcExec(ctx, a.tx, `DELETE FROM login_family_revocations WHERE operation_id = $1`, a.id)
		if err == nil && result.DeletedOperations != 1 {
			err = NewError(ReasonOwnershipInvalid, nil)
		}
	}
	return result, err
}

func gcExec(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, sqlFailure(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, sqlFailure(err)
	}
	return changed, nil
}

type gcClosure struct {
	connection uuid.UUID
	boot       uuid.UUID
	user       uuid.NullUUID
	family     uuid.NullUUID
	epoch      string
}

// Required integration contract for the new release/retirement writers:
//
//   - RoomManager.pendingNativeClose/nativeTransportCreations/nativeRecvClosures,
//     socket voiceEnforcementSession snapshots, lease teardown, registration and
//     renewal promises, and VoiceEnforcementSessionReleaseQueue's pending/held/
//     retrying/overflow owners belong to the media boot. Only the accepted
//     independently verified physical/native/inflight retirement gate settles
//     these owners; SQL absence or an elapsed lease does not.
//   - CP release/provisional-close requests must hold the same generation keys
//     until all receipt material is copied into immutable request-owned state
//     before commit. Postcommit signing uses only that copy, never a SQL reread;
//     rollback/unknown commit cannot produce a success receipt. After retirement,
//     replay copies surviving evidence under these keys or returns 410/the
//     retired_boot proof, without recreating closure evidence.
//   - Auth/epoch delivery parents, DM exact ejections, and the existing command
//     outbox retain SQL obligations. Even published outbox rows are retained here
//     until ordinary verified broker retirement removes them. JetStream command
//     payloads carry their own immutable identities, not closure-row references.
//
// The new writers are not wired by this package. Their native/receipt integration
// tests gate enabling this sweep; the SQL checks below cannot prove that runtime
// contract. A future owner that needs to reread closure SQL must participate in
// these keys or keep the corresponding durable pending obligation alive.
func gcClosureUnit(ctx context.Context, tx *sql.Tx, room bool, id uuid.UUID) (result GCResult, err error) {
	query := `SELECT connection_generation, node_boot_id, user_id, family_id, credential_epoch
 FROM voice_auth_connection_closures WHERE connection_generation = $1`
	if room {
		query = `SELECT connection_generation, node_boot_id, user_id, family_id, credential_epoch
 FROM voice_auth_room_closures WHERE session_generation = $1`
	}
	var closure gcClosure
	err = tx.QueryRowContext(ctx, query, id).Scan(&closure.connection, &closure.boot, &closure.user, &closure.family, &closure.epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, sqlFailure(err)
	}
	var retired bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM voice_auth_boot_fences
 WHERE node_boot_id = $1 AND retired_at IS NOT NULL)`, closure.boot).Scan(&retired); err != nil {
		return result, sqlFailure(err)
	}
	if !retired {
		return GCResult{Incomplete: true}, nil
	}
	locked, err := gcTryGeneration(ctx, tx, "connection", closure.connection)
	if err != nil || !locked {
		return GCResult{Incomplete: true}, err
	}
	var session uuid.UUID
	if room {
		session = id
		locked, err = gcTryGeneration(ctx, tx, "room", session)
		if err != nil || !locked {
			return GCResult{Incomplete: true}, err
		}
	}
	var eligible bool
	if err = tx.QueryRowContext(ctx, gcClosureEligible, closure.boot, closure.connection,
		session, closure.user, closure.family, closure.epoch).Scan(&eligible); err != nil {
		return result, sqlFailure(err)
	}
	if !eligible {
		return GCResult{Incomplete: true}, nil
	}
	return gcDeleteClosure(ctx, tx, room, id)
}

func gcTryGeneration(ctx context.Context, tx *sql.Tx, domain string, id uuid.UUID) (bool, error) {
	if (domain != "connection" && domain != "room") || id == uuid.Nil {
		return false, NewError(ReasonArgumentInvalid, nil)
	}
	var locked bool
	// Exact SQL recipe from login_family_generation_lock; hash equality never
	// establishes identity. The deletion triggers reenter this held lock.
	err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(
 'concord.login-family.v1/' || $1 || '/' || $2::uuid::text, 0))`, domain, id).Scan(&locked)
	if err != nil {
		return false, sqlFailure(err)
	}
	return locked, nil
}

func gcDeleteClosure(ctx context.Context, tx *sql.Tx, room bool, id uuid.UUID) (result GCResult, err error) {
	if room {
		result.DeletedRoomClosures, err = gcExec(ctx, tx, `DELETE FROM voice_auth_room_closures WHERE session_generation = $1`, id)
		return result, err
	}
	var children bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM voice_auth_room_closures WHERE connection_generation = $1)`, id).Scan(&children); err != nil {
		return result, sqlFailure(err)
	}
	if children {
		return GCResult{Incomplete: true}, nil
	}
	result.DeletedConnectionClosures, err = gcExec(ctx, tx, `DELETE FROM voice_auth_connection_closures WHERE connection_generation = $1`, id)
	return result, err
}
