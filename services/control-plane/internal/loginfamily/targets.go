package loginfamily

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

const targetPageSize = 100

type captureFilter struct {
	kind     string
	sids     []string
	preserve uuid.UUID
	epoch    string
	staged   uuid.UUID
}

// ResolveTargetSIDTx captures the owned family for a SID, including an already-terminal family.
func ResolveTargetSIDTx(ctx context.Context, tx *sql.Tx, user UserAuthority, sid uuid.UUID) (Targets, error) {
	return ResolveMatchedSIDsTx(ctx, tx, user, []uuid.UUID{sid})
}

// ResolveMatchedSIDsTx validates every SID before capturing the deduplicated family frontier.
func ResolveMatchedSIDsTx(ctx context.Context, tx *sql.Tx, user UserAuthority, sids []uuid.UUID) (Targets, error) {
	if err := checkUser(ctx, tx, user, true); err != nil {
		return Targets{}, err
	}
	// Complete validation precedes any capture or family lock, including for
	// mixed own/foreign input. Missing and foreign membership are identical.
	ids, err := sortedIDs(sids)
	if err != nil {
		return Targets{}, NewError(ReasonTargetUnknown, nil)
	}
	values := make([]string, 0, len(ids))
	for _, sid := range ids {
		if _, _, err = sidBinding(ctx, tx, user.userID, sid); err != nil {
			if errors.Is(err, ErrDenied) {
				return Targets{}, NewError(ReasonTargetUnknown, nil)
			}
			return Targets{}, err
		}
		values = append(values, sid.String())
	}
	return captureTargets(ctx, tx, user, captureFilter{kind: "sids", sids: values})
}

// CaptureDisplacementTargetsTx captures the exact password-login refresh match,
// including expired but unrevoked rows, before the caller creates a fresh family.
func CaptureDisplacementTargetsTx(ctx context.Context, tx *sql.Tx, user UserAuthority, machineID, ipAddress, userAgent string) (Targets, error) {
	if err := checkUser(ctx, tx, user, true); err != nil {
		return Targets{}, err
	}
	if err := prepareTargetStorage(ctx, tx, user.state); err != nil {
		return Targets{}, err
	}
	staged, err := newToken()
	if err != nil {
		return Targets{}, err
	}
	// Keep unused fallback metadata out of the machine query entirely: binding
	// an IP parameter would parse it as inet even in an unselected CASE arm.
	predicate := ` AND r.machine_id=$3`
	args := []any{user.userID, staged, machineID}
	if machineID == "" {
		predicate = ` AND r.ip_address=$3 AND r.user_agent=$4`
		args = []any{user.userID, staged, ipAddress, userAgent}
	}
	// Validate all matched SIDs and retain their deduplicated families in one
	// statement snapshot. Refresh cleanup may remove expired rows without the
	// user lock, so later family pages must read only this frozen SQL frontier.
	// LEFT JOIN preserves missing/foreign membership as a uniform refusal.
	var valid bool
	err = tx.QueryRowContext(ctx, `WITH matched AS MATERIALIZED (
 SELECT f.family_id FROM refresh_tokens r
 LEFT JOIN login_family_sids s ON s.sid=r.id AND s.user_id=r.user_id
 LEFT JOIN login_families f ON f.family_id=s.family_id AND f.user_id=r.user_id
 WHERE r.user_id=$1 AND r.revoked_at IS NULL`+predicate+`
), validity AS MATERIALIZED (
 SELECT NOT EXISTS (SELECT 1 FROM matched WHERE family_id IS NULL) AS valid
), staged AS (
 INSERT INTO pg_temp.loginfamily_targets(capture,family_id)
 SELECT DISTINCT $2::uuid,family_id FROM matched WHERE (SELECT valid FROM validity)
 RETURNING family_id
)
SELECT valid FROM validity`, args...).Scan(&valid)
	if err != nil {
		return Targets{}, sqlFailure(err)
	}
	if !valid {
		return Targets{}, NewError(ReasonTargetUnknown, nil)
	}
	return captureTargets(ctx, tx, user, captureFilter{kind: "displacement", staged: staged})
}

// CaptureUserTargetsTx captures all owned families except the complete verified preserved family.
func CaptureUserTargetsTx(ctx context.Context, tx *sql.Tx, user UserAuthority, preserve *AccessIdentity) (Targets, error) {
	if err := checkUser(ctx, tx, user, true); err != nil {
		return Targets{}, err
	}
	filter := captureFilter{kind: "user"}
	if preserve != nil {
		if preserve.UserID != user.userID {
			return Targets{}, NewError(ReasonPreserveUnknown, nil)
		}
		// The user mutation lock stabilizes the family. Do not lock the preserved
		// family first: its UUID may sort after a target family.
		f, err := resolveAccess(ctx, tx, *preserve, 0)
		if err != nil {
			if errors.Is(err, ErrDenied) {
				return Targets{}, NewError(ReasonPreserveUnknown, nil)
			}
			return Targets{}, err
		}
		filter.preserve = f.binding.FamilyID
	}
	return captureTargets(ctx, tx, user, filter)
}

// CaptureEpochTargetsTx captures the fixed frontier belonging to the specified superseded epoch.
func CaptureEpochTargetsTx(ctx context.Context, tx *sql.Tx, user UserAuthority, epoch string) (Targets, error) {
	if err := checkUser(ctx, tx, user, true); err != nil {
		return Targets{}, err
	}
	if !validEpoch(epoch) {
		return Targets{}, NewError(ReasonArgumentInvalid, nil)
	}
	return captureTargets(ctx, tx, user, captureFilter{kind: "epoch", epoch: epoch})
}
func captureTargets(ctx context.Context, tx *sql.Tx, user UserAuthority, filter captureFilter) (Targets, error) {
	if err := prepareTargetStorage(ctx, tx, user.state); err != nil {
		return Targets{}, err
	}
	token, err := newToken()
	if err != nil {
		return Targets{}, err
	}
	after := uuid.Nil
	for {
		page, err := capturePage(ctx, tx, user.userID, filter, after)
		if err != nil {
			return Targets{}, err
		}
		for _, id := range page {
			if _, err = tx.ExecContext(ctx, `INSERT INTO pg_temp.loginfamily_targets(capture,family_id) VALUES ($1,$2)`, token, id); err != nil {
				return Targets{}, sqlFailure(err)
			}
		}
		if len(page) < targetPageSize {
			break
		}
		after = page[len(page)-1]
	}
	targets := Targets{user: user, token: token}
	if filter.kind == "epoch" {
		epoch := filter.epoch
		targets.epoch = &epoch
	}
	return targets, nil
}
func capturePage(ctx context.Context, tx *sql.Tx, user uuid.UUID, filter captureFilter, after uuid.UUID) (page []uuid.UUID, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.family_id FROM login_families f
 WHERE f.user_id=$1 AND f.family_id>$2
 AND (($3='user' AND f.family_id<>$4)
   OR ($3='epoch' AND f.credential_epoch=$5)
   OR ($3='sids' AND EXISTS (SELECT 1 FROM login_family_sids s
       WHERE s.family_id=f.family_id AND s.user_id=$1 AND s.sid=ANY($6::uuid[])))
   OR ($3='displacement' AND EXISTS (SELECT 1 FROM pg_temp.loginfamily_targets t
       WHERE t.capture=$7 AND t.family_id=f.family_id)))
 ORDER BY f.family_id LIMIT 100 FOR NO KEY UPDATE OF f`, user, after, filter.kind, filter.preserve, filter.epoch, pq.Array(filter.sids), filter.staged)
	if err != nil {
		return nil, sqlFailure(err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			page = nil
			retErr = sqlFailure(errors.Join(retErr, closeErr))
		}
	}()
	page = make([]uuid.UUID, 0, targetPageSize)
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return nil, sqlFailure(err)
		}
		page = append(page, id)
	}
	if err = rows.Err(); err != nil {
		return nil, sqlFailure(err)
	}
	return page, nil
}
func checkTargets(ctx context.Context, tx *sql.Tx, targets Targets) error {
	if err := checkUser(ctx, tx, targets.user, true); err != nil {
		return err
	}
	return checkToken(targets.token)
}
func capturedPage(ctx context.Context, tx *sql.Tx, targets Targets, after uuid.UUID) (page []familyRecord, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT f.user_id,f.family_id,f.credential_epoch,f.terminal_at,f.auth_operation_id,f.epoch_generation
 FROM pg_temp.loginfamily_targets t JOIN login_families f ON f.family_id=t.family_id
 WHERE t.capture=$1 AND f.user_id=$2 AND f.family_id>$3 ORDER BY f.family_id LIMIT 100`, targets.token, targets.user.userID, after)
	if err != nil {
		return nil, sqlFailure(err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			page = nil
			retErr = sqlFailure(errors.Join(retErr, closeErr))
		}
	}()
	page = make([]familyRecord, 0, targetPageSize)
	for rows.Next() {
		var f familyRecord
		if err = rows.Scan(&f.binding.UserID, &f.binding.FamilyID, &f.binding.CredentialEpoch, &f.terminal, &f.auth, &f.epoch); err != nil {
			return nil, sqlFailure(err)
		}
		page = append(page, f)
	}
	if err = rows.Err(); err != nil {
		return nil, sqlFailure(err)
	}
	return page, nil
}

// Only captures require temporary SQL storage. Productive guards and mint paths
// incur no temporary-table DDL. Rows live until the caller commits or rolls back.
func prepareTargetStorage(ctx context.Context, tx *sql.Tx, state *authorityState) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.targetsReady {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE loginfamily_targets
 (capture UUID NOT NULL, family_id UUID NOT NULL, PRIMARY KEY (capture, family_id)) ON COMMIT DROP`); err != nil {
		return sqlFailure(err)
	}
	state.targetsReady = true
	return nil
}
