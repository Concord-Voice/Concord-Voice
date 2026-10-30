package loginfamily

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

const markerSetting = "concord.loginfamily_authority"

// AcquireWriterBarrierTx acquires the rollout barrier once in a READ COMMITTED transaction.
func AcquireWriterBarrierTx(ctx context.Context, tx *sql.Tx) (WriterBarrier, error) {
	if tx == nil {
		return WriterBarrier{}, NewError(ReasonCapabilityInvalid, nil)
	}
	// Captures must see commits that preceded the completed user-lock wait.
	// A repeatable-read snapshot can miss those families without conflicting on
	// the unchanged users tuple, so it cannot supply this authority contract.
	var isolation string
	if err := tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		return WriterBarrier{}, sqlFailure(err)
	}
	if isolation != "read committed" {
		return WriterBarrier{}, NewError(ReasonArgumentInvalid, nil)
	}
	token, err := newToken()
	if err != nil {
		return WriterBarrier{}, err
	}
	// Claim once even if two constructor calls on the same transaction interleave.
	var claimed string
	if err = tx.QueryRowContext(ctx, `SELECT CASE WHEN COALESCE(current_setting($1, true), '') = '' THEN set_config($1, $2, true) ELSE '' END`, markerSetting, token.String()).Scan(&claimed); err != nil {
		return WriterBarrier{}, sqlFailure(err)
	}
	if claimed != token.String() {
		return WriterBarrier{}, NewError(ReasonCapabilityInvalid, nil)
	}
	var singleton bool
	if err = tx.QueryRowContext(ctx, `SELECT id FROM login_family_rollout WHERE id = TRUE FOR SHARE`).Scan(&singleton); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WriterBarrier{}, NewError(ReasonOwnershipInvalid, err)
		}
		return WriterBarrier{}, sqlFailure(err)
	}
	return WriterBarrier{state: &authorityState{tx: tx, marker: token}}, nil
}

func newToken() (uuid.UUID, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, NewError(ReasonSQLUnavailable, err)
	}
	return id, nil
}

func checkState(ctx context.Context, tx *sql.Tx, state *authorityState) error {
	if state == nil || tx == nil || state.tx != tx || state.marker == uuid.Nil {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	var marker string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(current_setting($1, true), '')`, markerSetting).Scan(&marker); err != nil {
		return sqlFailure(err)
	}
	if marker != state.marker.String() {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	return nil
}
func checkToken(token uuid.UUID) error {
	if token == uuid.Nil {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	return nil
}
func sortedIDs(ids []uuid.UUID) ([]uuid.UUID, error) {
	result := slices.Clone(ids)
	for _, id := range result {
		if id == uuid.Nil {
			return nil, NewError(ReasonArgumentInvalid, nil)
		}
	}
	slices.SortFunc(result, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(result), nil
}

// LockUsersTx acquires the complete sorted user set without later additions or mode upgrades.
func LockUsersTx(ctx context.Context, tx *sql.Tx, barrier WriterBarrier, users []uuid.UUID, mode UserLockMode) (UserAuthoritySet, error) {
	if mode < UserShare || mode > UserUpdate {
		return UserAuthoritySet{}, NewError(ReasonArgumentInvalid, nil)
	}
	ids, err := sortedIDs(users)
	if err != nil {
		return UserAuthoritySet{}, err
	}
	if len(ids) == 0 {
		return UserAuthoritySet{}, NewError(ReasonArgumentInvalid, nil)
	}
	state := barrier.state
	if err = checkState(ctx, tx, state); err != nil {
		return UserAuthoritySet{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.usersToken != uuid.Nil {
		if state.epochs == nil || mode > state.mode || !slices.Equal(ids, state.users) {
			return UserAuthoritySet{}, NewError(ReasonCapabilityInvalid, nil)
		}
		if err = checkToken(state.usersToken); err != nil {
			return UserAuthoritySet{}, err
		}
		return UserAuthoritySet{state: state}, nil
	}
	query := `SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1 FOR SHARE`
	if mode == UserNoKeyUpdate {
		query = `SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1 FOR NO KEY UPDATE`
	}
	if mode == UserUpdate {
		query = `SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1 FOR UPDATE`
	}
	epochs := make(map[uuid.UUID]string, len(ids))
	// Mark the complete requested set before SQL; a failed attempt cannot be used
	// to acquire a different or weaker prefix in this same transaction.
	state.users = ids
	state.mode = mode
	for _, id := range ids {
		var epoch string
		if err = tx.QueryRowContext(ctx, query, id).Scan(&epoch); err != nil {
			state.usersToken = state.marker // A nil epochs map keeps this attempt invalid.
			if errors.Is(err, sql.ErrNoRows) {
				return UserAuthoritySet{}, NewError(ReasonAccessIdentity, err)
			}
			return UserAuthoritySet{}, sqlFailure(err)
		}
		epochs[id] = epoch
	}
	state.usersToken, err = newToken()
	if err != nil {
		state.usersToken = state.marker
		return UserAuthoritySet{}, err
	}
	state.epochs = epochs
	return UserAuthoritySet{state: state}, nil
}

// ForUser returns a local membership view; subsequent Tx helpers validate transaction liveness.
func (set UserAuthoritySet) ForUser(id uuid.UUID) (UserAuthority, error) {
	if set.state == nil || id == uuid.Nil {
		return UserAuthority{}, NewError(ReasonCapabilityInvalid, nil)
	}
	set.state.mu.Lock()
	defer set.state.mu.Unlock()
	if _, ok := set.state.epochs[id]; !ok || set.state.usersToken == uuid.Nil {
		return UserAuthority{}, NewError(ReasonCapabilityInvalid, nil)
	}
	return UserAuthority{state: set.state, userID: id}, nil
}
func checkUser(ctx context.Context, tx *sql.Tx, user UserAuthority, mutate bool) error {
	if user.state == nil || user.userID == uuid.Nil {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	user.state.mu.Lock()
	_, member := user.state.epochs[user.userID]
	mode, token := user.state.mode, user.state.usersToken
	user.state.mu.Unlock()
	if !member || (mutate && mode < UserNoKeyUpdate) {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	if err := checkState(ctx, tx, user.state); err != nil {
		return err
	}
	return checkToken(token)
}
func validEpoch(epoch string) bool {
	if epoch == "" {
		return true
	}
	if len(epoch) != 32 {
		return false
	}
	for _, c := range epoch {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func currentEpoch(ctx context.Context, tx *sql.Tx, user uuid.UUID) (string, error) {
	var epoch string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`, user).Scan(&epoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NewError(ReasonAccessIdentity, err)
		}
		return "", sqlFailure(err)
	}
	return epoch, nil
}

type familyRecord struct {
	binding  Binding
	terminal sql.NullTime
	auth     uuid.NullUUID
	epoch    uuid.NullUUID
}

func readFamily(ctx context.Context, tx *sql.Tx, user, family uuid.UUID, mode FamilyLockMode) (familyRecord, error) {
	query := `SELECT user_id, family_id, credential_epoch, terminal_at, auth_operation_id, epoch_generation FROM login_families WHERE user_id=$1 AND family_id=$2`
	switch mode {
	case FamilyShare:
		query = `SELECT user_id, family_id, credential_epoch, terminal_at, auth_operation_id, epoch_generation FROM login_families WHERE user_id=$1 AND family_id=$2 FOR SHARE`
	case FamilyNoKeyUpdate:
		query = `SELECT user_id, family_id, credential_epoch, terminal_at, auth_operation_id, epoch_generation FROM login_families WHERE user_id=$1 AND family_id=$2 FOR NO KEY UPDATE`
	}
	var f familyRecord
	err := tx.QueryRowContext(ctx, query, user, family).Scan(&f.binding.UserID, &f.binding.FamilyID, &f.binding.CredentialEpoch, &f.terminal, &f.auth, &f.epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return f, NewError(ReasonAccessIdentity, err)
	}
	if err != nil {
		return f, sqlFailure(err)
	}
	return f, nil
}
func sidBinding(ctx context.Context, tx *sql.Tx, user, sid uuid.UUID) (uuid.UUID, time.Time, error) {
	var family uuid.UUID
	var expiry time.Time
	err := tx.QueryRowContext(ctx, `SELECT family_id, access_expires_at FROM login_family_sids WHERE user_id=$1 AND sid=$2`, user, sid).Scan(&family, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, time.Time{}, NewError(ReasonAccessIdentity, err)
	}
	if err != nil {
		return uuid.Nil, time.Time{}, sqlFailure(err)
	}
	return family, expiry, nil
}
func checkAccess(ctx context.Context, tx *sql.Tx, identity AccessIdentity, persisted time.Time, f familyRecord) error {
	// Identity has already resolved; lifetime precedes epoch and terminal state.
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return sqlFailure(err)
	}
	if identity.AccessExpiresAt.IsZero() || !identity.AccessExpiresAt.Equal(persisted) || !persisted.After(now) {
		return NewError(ReasonAccessExpired, nil)
	}
	epoch, err := currentEpoch(ctx, tx, identity.UserID)
	if err != nil {
		return err
	}
	if !validEpoch(identity.CredentialEpoch) || epoch != identity.CredentialEpoch || f.binding.CredentialEpoch != identity.CredentialEpoch {
		return NewError(ReasonEpochMismatch, nil)
	}
	if f.terminal.Valid {
		return NewError(ReasonFamilyTerminal, nil)
	}
	return nil
}
func resolveAccess(ctx context.Context, tx *sql.Tx, identity AccessIdentity, mode FamilyLockMode) (familyRecord, error) {
	if identity.UserID == uuid.Nil || identity.SID == uuid.Nil {
		return familyRecord{}, NewError(ReasonAccessIdentity, nil)
	}
	id, expiry, err := sidBinding(ctx, tx, identity.UserID, identity.SID)
	if err != nil {
		return familyRecord{}, err
	}
	f, err := readFamily(ctx, tx, identity.UserID, id, mode)
	if err != nil {
		return familyRecord{}, err
	}
	if err = checkAccess(ctx, tx, identity, expiry, f); err != nil {
		return familyRecord{}, err
	}
	return f, nil
}

// ResolveVerifiedAccess returns an expiry-checked snapshot, never transaction authority.
func ResolveVerifiedAccess(ctx context.Context, db *sql.DB, identity AccessIdentity) (grant AccessGrant, retErr error) {
	if db == nil {
		return grant, NewError(ReasonArgumentInvalid, nil)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true})
	if err != nil {
		return grant, sqlFailure(err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = NewError(ReasonSQLUnavailable, errors.Join(retErr, rollbackErr))
			grant = AccessGrant{}
		}
	}()
	f, err := resolveAccess(ctx, tx, identity, 0)
	if err != nil {
		return grant, err
	}
	if err = tx.Commit(); err != nil {
		return grant, NewError(ReasonCommitUnknown, err)
	}
	return AccessGrant{UserID: identity.UserID, FamilyID: f.binding.FamilyID, SID: identity.SID, CredentialEpoch: identity.CredentialEpoch, AccessExpiresAt: identity.AccessExpiresAt}, nil
}

// GuardAfterUserLockTx locks and validates the exact signed access identity after user locking.
func GuardAfterUserLockTx(ctx context.Context, tx *sql.Tx, user UserAuthority, identity AccessIdentity, mode FamilyLockMode) (LiveFamily, error) {
	if mode != FamilyShare && mode != FamilyNoKeyUpdate {
		return LiveFamily{}, NewError(ReasonArgumentInvalid, nil)
	}
	if err := checkUser(ctx, tx, user, mode == FamilyNoKeyUpdate); err != nil {
		return LiveFamily{}, err
	}
	if identity.UserID != user.userID {
		return LiveFamily{}, NewError(ReasonAccessIdentity, nil)
	}
	f, err := resolveAccess(ctx, tx, identity, mode)
	if err != nil {
		return LiveFamily{}, err
	}
	token, err := newToken()
	if err != nil {
		return LiveFamily{}, err
	}
	return LiveFamily{user: user, binding: f.binding, mode: mode, token: token}, nil
}

// GuardRefreshSIDTx locks a live current-epoch family without requiring live access-token expiry.
func GuardRefreshSIDTx(ctx context.Context, tx *sql.Tx, user UserAuthority, sid uuid.UUID) (LiveFamily, error) {
	if err := checkUser(ctx, tx, user, true); err != nil {
		return LiveFamily{}, err
	}
	if sid == uuid.Nil {
		return LiveFamily{}, NewError(ReasonAccessIdentity, nil)
	}
	id, _, err := sidBinding(ctx, tx, user.userID, sid)
	if err != nil {
		return LiveFamily{}, err
	}
	f, err := readFamily(ctx, tx, user.userID, id, FamilyNoKeyUpdate)
	if err != nil {
		return LiveFamily{}, err
	}
	epoch, err := currentEpoch(ctx, tx, user.userID)
	if err != nil {
		return LiveFamily{}, err
	}
	if epoch != f.binding.CredentialEpoch {
		return LiveFamily{}, NewError(ReasonEpochMismatch, nil)
	}
	if f.terminal.Valid {
		return LiveFamily{}, NewError(ReasonFamilyTerminal, nil)
	}
	token, err := newToken()
	if err != nil {
		return LiveFamily{}, err
	}
	return LiveFamily{user: user, binding: f.binding, mode: FamilyNoKeyUpdate, token: token}, nil
}

// CreateFamilyTx creates a fresh family only when expectedEpoch matches current user authority.
func CreateFamilyTx(ctx context.Context, tx *sql.Tx, user UserAuthority, expectedEpoch string) (LiveFamily, error) {
	if err := checkUser(ctx, tx, user, true); err != nil {
		return LiveFamily{}, err
	}
	if !validEpoch(expectedEpoch) {
		return LiveFamily{}, NewError(ReasonArgumentInvalid, nil)
	}
	epoch, err := currentEpoch(ctx, tx, user.userID)
	if err != nil {
		return LiveFamily{}, err
	}
	if epoch != expectedEpoch {
		return LiveFamily{}, NewError(ReasonEpochMismatch, nil)
	}
	id, err := newToken()
	if err != nil {
		return LiveFamily{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO login_families(family_id,user_id,credential_epoch) VALUES ($1,$2,$3)`, id, user.userID, epoch); err != nil {
		return LiveFamily{}, sqlFailure(err)
	}
	token, err := newToken()
	if err != nil {
		return LiveFamily{}, err
	}
	return LiveFamily{user: user, binding: Binding{UserID: user.userID, FamilyID: id, CredentialEpoch: epoch}, mode: FamilyNoKeyUpdate, token: token}, nil
}

// BindSIDTx binds a SID immutably to the family and the actual normalized signed expiry.
func BindSIDTx(ctx context.Context, tx *sql.Tx, family LiveFamily, sid uuid.UUID, expiry time.Time) error {
	if err := checkUser(ctx, tx, family.user, true); err != nil {
		return err
	}
	if family.mode != FamilyNoKeyUpdate || family.binding.FamilyID == uuid.Nil {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	if err := checkToken(family.token); err != nil {
		return err
	}
	// NumericDate is second precision; silently rounding here would persist a
	// different authority lifetime from the token the caller actually signed.
	if sid == uuid.Nil || expiry.IsZero() || expiry.Nanosecond() != 0 {
		return NewError(ReasonArgumentInvalid, nil)
	}
	f, err := readFamily(ctx, tx, family.user.userID, family.binding.FamilyID, 0)
	if err != nil {
		return err
	}
	if f.terminal.Valid {
		return NewError(ReasonFamilyTerminal, nil)
	}
	epoch, err := currentEpoch(ctx, tx, family.user.userID)
	if err != nil {
		return err
	}
	if epoch != family.binding.CredentialEpoch || f.binding != family.binding {
		return NewError(ReasonEpochMismatch, nil)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO login_family_sids(sid,family_id,user_id,access_expires_at) VALUES ($1,$2,$3,$4) ON CONFLICT (sid) DO NOTHING`, sid, family.binding.FamilyID, family.user.userID, expiry); err != nil {
		return sqlFailure(err)
	}
	var actualFamily, actualUser uuid.UUID
	var actualExpiry time.Time
	if err = tx.QueryRowContext(ctx, `SELECT family_id,user_id,access_expires_at FROM login_family_sids WHERE sid=$1`, sid).Scan(&actualFamily, &actualUser, &actualExpiry); err != nil {
		return sqlFailure(err)
	}
	if actualFamily != family.binding.FamilyID || actualUser != family.user.userID || !actualExpiry.Equal(expiry) {
		return NewError(ReasonIdentityConflict, nil)
	}
	return nil
}

// EnsureWriterBarrierTx claims the shared writer barrier unless this
// transaction already holds it (claimed by any authority helper).
func EnsureWriterBarrierTx(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	var claimed string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(current_setting($1, true), '')`, markerSetting).Scan(&claimed); err != nil {
		return sqlFailure(err)
	}
	if claimed != "" {
		return nil
	}
	_, err := AcquireWriterBarrierTx(ctx, tx)
	return err
}

// GuardLockedActorFamilyTx share-locks and validates the actor's exact signed
// family (binding, lifetime, epoch, terminal state). Precondition: this
// transaction holds the writer barrier and already holds the actor's users row
// in any mode, so no terminalizer can hold that family and F-S never waits.
// See lock-contract amendment 7.
func GuardLockedActorFamilyTx(ctx context.Context, tx *sql.Tx, identity AccessIdentity) error {
	if tx == nil {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	var claimed string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(current_setting($1, true), '')`, markerSetting).Scan(&claimed); err != nil {
		return sqlFailure(err)
	}
	if claimed == "" {
		return NewError(ReasonCapabilityInvalid, nil)
	}
	_, err := resolveAccess(ctx, tx, identity, FamilyShare)
	return err
}
