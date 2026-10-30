package loginfamily_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func displacementFamily(t *testing.T, tx *sql.Tx, u lf.UserAuthority, user uuid.UUID, machine, ip, agent any, expired bool, siblings int) lf.AccessIdentity {
	t.Helper()
	f, err := lf.CreateFamilyTx(ctx, tx, u, "")
	require.NoError(t, err)
	var first lf.AccessIdentity
	for i := 0; i < siblings; i++ {
		identity := lf.AccessIdentity{UserID: user, SID: uuid.New(), AccessExpiresAt: expires()}
		require.NoError(t, lf.BindSIDTx(ctx, tx, f, identity.SID, identity.AccessExpiresAt))
		deadline := expires()
		if expired {
			deadline = time.Now().Add(-time.Hour)
		}
		_, err = tx.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id,ip_address,user_agent) VALUES($1,$2,$3,$4,$5,$6,$7)`, identity.SID, user, identity.SID.String(), deadline, machine, ip, agent)
		require.NoError(t, err)
		if i == 0 {
			first = identity
		}
	}
	return first
}

func displacementRevoke(t *testing.T, tx *sql.Tx, targets lf.Targets) lf.RevocationResult {
	t.Helper()
	result, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonDisplacement})
	require.NoError(t, err)
	return result
}

func TestDisplacementLargeFrontierIsAtomicDeduplicatedAndReplayExcludesFreshFamily(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	seed := transaction(t, db)
	u := authority(t, seed, user, lf.UserNoKeyUpdate)
	var first lf.AccessIdentity
	for i := 0; i < 207; i++ {
		identity := displacementFamily(t, seed, u, user, "machine-large", "198.51.100.1", "fixture-agent", i%2 == 0, 3)
		if i == 0 {
			first = identity
		}
	}
	unmatched := displacementFamily(t, seed, u, user, "other-machine", "198.51.100.1", "fixture-agent", false, 1)
	require.NoError(t, seed.Commit())
	tx := transaction(t, db)
	actor := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "machine-large", "198.51.100.1", "fixture-agent")
	require.NoError(t, err)
	// Capture itself must not terminalize any family or refresh row.
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
	result := displacementRevoke(t, tx, targets)
	require.EqualValues(t, 207, result.NewlyTerminalFamilies)
	require.Len(t, result.Owners, 1)
	fresh := displacementFamily(t, tx, actor, user, "machine-large", "198.51.100.1", "fixture-agent", false, 1)
	// Other connections cannot observe a partially terminalized frontier.
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`))
	require.NoError(t, tx.Commit())
	require.EqualValues(t, 207, count(t, db, `SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`))
	require.EqualValues(t, 621, count(t, db, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`))
	require.EqualValues(t, 207, count(t, db, `SELECT count(*) FROM login_family_revocation_targets`))
	for _, identity := range []lf.AccessIdentity{fresh, unmatched} {
		_, err = lf.ResolveVerifiedAccess(ctx, db, identity)
		require.NoError(t, err)
	}
	replayTx := transaction(t, db)
	replayUser := authority(t, replayTx, user, lf.UserNoKeyUpdate)
	old, err := lf.ResolveTargetSIDTx(ctx, replayTx, replayUser, first.SID)
	require.NoError(t, err)
	replay := displacementRevoke(t, replayTx, old)
	require.Zero(t, replay.NewlyTerminalFamilies)
	require.Equal(t, result.Owners, replay.Owners)
	require.NoError(t, replayTx.Commit())
	_, err = lf.ResolveVerifiedAccess(ctx, db, fresh)
	require.NoError(t, err)
}

func TestDisplacementExactMachineAndFallbackPredicates(t *testing.T) {
	cases := []struct {
		name, machine, ip, agent string
		matches                  []int
	}{
		{"machine takes precedence", "chosen", "198.51.100.2", "agent", []int{0, 1}},
		{"machine ignores empty fallback IP", "chosen", "", "agent", []int{0, 1}},
		{"machine ignores invalid fallback IP", "chosen", "not-an-address", "agent", []int{0, 1}},
		{"fallback includes nonempty machine", "", "198.51.100.2", "agent", []int{0, 2, 3}},
		{"empty user agent is literal", "", "198.51.100.4", "", []int{4}},
		{"no matches yields no operation", "absent", "198.51.100.2", "agent", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := database(t)
			user := testdb.CreateUser(t, db)
			foreign := testdb.CreateUser(t, db)
			seed := transaction(t, db)
			u := authority(t, seed, user, lf.UserNoKeyUpdate)
			metadata := [][3]any{
				{"chosen", "198.51.100.2", "agent"},
				{"chosen", "198.51.100.3", "different-agent"},
				{"different", "198.51.100.2", "agent"},
				{nil, "198.51.100.2", "agent"},
				{"", "198.51.100.4", ""},
				{nil, nil, nil},
				{"", nil, ""},
				{"", "198.51.100.4", nil},
				{"chosen", "198.51.100.2", "agent"},
			}
			identities := make([]lf.AccessIdentity, 0, len(metadata))
			for i, m := range metadata {
				identities = append(identities, displacementFamily(t, seed, u, user, m[0], m[1], m[2], i == 1, 1))
			}
			_, err := seed.Exec(`UPDATE refresh_tokens SET revoked_at=clock_timestamp() WHERE id=$1`, identities[8].SID)
			require.NoError(t, err)
			require.NoError(t, seed.Commit())
			foreignIdentity := newFamily(t, db, foreign, expires())
			refresh(t, db, foreignIdentity, false)
			_, err = db.Exec(`UPDATE refresh_tokens SET machine_id='chosen',ip_address='198.51.100.2',user_agent='agent' WHERE id=$1`, foreignIdentity.SID)
			require.NoError(t, err)
			tx := transaction(t, db)
			actor := authority(t, tx, user, lf.UserNoKeyUpdate)
			targets, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, tc.machine, tc.ip, tc.agent)
			require.NoError(t, err)
			result := displacementRevoke(t, tx, targets)
			require.EqualValues(t, len(tc.matches), result.NewlyTerminalFamilies)
			require.NoError(t, tx.Commit())
			for i, identity := range identities {
				matched := false
				for _, n := range tc.matches {
					if n == i {
						matched = true
					}
				}
				_, err = lf.ResolveVerifiedAccess(ctx, db, identity)
				if matched {
					reason(t, err, lf.ErrDenied, lf.ReasonFamilyTerminal)
				} else {
					require.NoError(t, err)
				}
			}
			_, err = lf.ResolveVerifiedAccess(ctx, db, foreignIdentity)
			require.NoError(t, err)
			if len(tc.matches) == 0 {
				require.Empty(t, result.Owners)
				require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
			}
		})
	}
}

func TestDisplacementMixedInvalidMembershipRefusesBeforeFamilyLocks(t *testing.T) {
	for _, kind := range []string{"unknown", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			db := database(t)
			user := testdb.CreateUser(t, db)
			seed := transaction(t, db)
			u := authority(t, seed, user, lf.UserNoKeyUpdate)
			valid := displacementFamily(t, seed, u, user, "mixed", "198.51.100.1", "", false, 1)
			require.NoError(t, seed.Commit())
			invalidSID := uuid.New()
			if kind == "foreign" {
				other := testdb.CreateUser(t, db)
				invalidSID = newFamily(t, db, other, expires()).SID
			}
			_, err := db.Exec(`INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at,machine_id) VALUES($1,$2,$3,clock_timestamp()-interval '1 hour','mixed')`, invalidSID, user, invalidSID.String())
			require.NoError(t, err)
			// The default rollout is prepared. That state must not exempt unknown rows.
			require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM login_family_rollout WHERE activated_at IS NULL`))
			tx := transaction(t, db)
			actor := authority(t, tx, user, lf.UserNoKeyUpdate)
			targets, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "mixed", "198.51.100.1", "")
			reason(t, err, lf.ErrTargetNotFound, lf.ReasonTargetUnknown)
			_, err = lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonDisplacement})
			require.ErrorIs(t, err, lf.ErrContract)
			// A conflicting family lock succeeds NOWAIT while the refused owner stays open.
			probe := transaction(t, db)
			var id uuid.UUID
			require.NoError(t, probe.QueryRow(`SELECT f.family_id FROM login_families f JOIN login_family_sids s USING(family_id) WHERE s.sid=$1 FOR UPDATE OF f NOWAIT`, valid.SID).Scan(&id))
			require.NoError(t, probe.Commit())
			// Simulate the active floor only inside this rolled-back transaction;
			// this is a capture strictness test, not cutover receipt validation.
			_, err = tx.Exec(`UPDATE login_family_rollout SET activated_at=clock_timestamp(),receipt='{}'::jsonb WHERE id`)
			require.NoError(t, err)
			_, err = lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "mixed", "198.51.100.1", "")
			reason(t, err, lf.ErrTargetNotFound, lf.ReasonTargetUnknown)
			require.NoError(t, tx.Rollback())
			require.Zero(t, count(t, db, `SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`))
			require.Zero(t, count(t, db, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`))
			require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
		})
	}
}

func TestDisplacementCaptureRequiresExactTransactionAndMutationAuthority(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserShare)
	_, err := lf.CaptureDisplacementTargetsTx(ctx, tx, u, "", "", "")
	require.ErrorIs(t, err, lf.ErrContract)
	require.NoError(t, tx.Rollback())
	owner := transaction(t, db)
	actor := authority(t, owner, user, lf.UserNoKeyUpdate)
	other := transaction(t, db)
	_, err = lf.CaptureDisplacementTargetsTx(ctx, other, actor, "", "", "")
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.CaptureDisplacementTargetsTx(ctx, owner, lf.UserAuthority{}, "", "", "")
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.CaptureDisplacementTargetsTx(ctx, nil, actor, "", "", "")
	require.ErrorIs(t, err, lf.ErrContract)
	require.NoError(t, owner.Commit())
	_, err = lf.CaptureDisplacementTargetsTx(ctx, owner, actor, "", "", "")
	require.ErrorIs(t, err, lf.ErrContract)
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
}

func TestDisplacementFrozenFrontierSurvivesConcurrentRefreshCleanup(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	seed := transaction(t, db)
	u := authority(t, seed, user, lf.UserNoKeyUpdate)
	for i := 0; i < 207; i++ {
		displacementFamily(t, seed, u, user, "cleanup", "198.51.100.1", "", true, 2)
	}
	require.NoError(t, seed.Commit())
	blocker := transaction(t, db)
	var family uuid.UUID
	var xid int64
	require.NoError(t, blocker.QueryRow(`SELECT family_id FROM login_families WHERE user_id=$1 ORDER BY family_id LIMIT 1 FOR NO KEY UPDATE`, user).Scan(&family))
	require.NoError(t, blocker.QueryRow(`SELECT txid_current()`).Scan(&xid))
	tx := transaction(t, db)
	actor := authority(t, tx, user, lf.UserNoKeyUpdate)
	type outcome struct {
		targets lf.Targets
		err     error
	}
	done := make(chan outcome, 1)
	captureCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	go func() {
		targets, err := lf.CaptureDisplacementTargetsTx(captureCtx, tx, actor, "cleanup", "198.51.100.1", "")
		done <- outcome{targets, err}
	}()
	testdb.WaitForRowLockWaiter(t, db, xid)
	// A waiter on the first family proves selection occurred and F-N is pending.
	// Cleanup must complete before that family is released: no refresh row locks.
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 3*time.Second)
	defer cleanupCancel()
	deleted, err := db.ExecContext(cleanupCtx, `DELETE FROM refresh_tokens WHERE user_id=$1 AND expires_at<clock_timestamp()`, user)
	require.NoError(t, err)
	n, err := deleted.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 414, n)
	require.NoError(t, blocker.Commit())
	captured := <-done
	require.NoError(t, captured.err)
	result := displacementRevoke(t, tx, captured.targets)
	require.EqualValues(t, 207, result.NewlyTerminalFamilies)
	require.NoError(t, tx.Commit())
	require.EqualValues(t, 207, count(t, db, `SELECT count(*) FROM login_family_revocation_targets`))
}

func TestDisplacementLaterPageFailureRollsBackWholeCapture(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	seed := transaction(t, db)
	u := authority(t, seed, user, lf.UserNoKeyUpdate)
	for i := 0; i < 207; i++ {
		displacementFamily(t, seed, u, user, "page-failure", "198.51.100.1", "", false, 1)
	}
	require.NoError(t, seed.Commit())
	blocker := transaction(t, db)
	var family uuid.UUID
	var xid int64
	require.NoError(t, blocker.QueryRow(`SELECT family_id FROM login_families WHERE user_id=$1 ORDER BY family_id OFFSET 100 LIMIT 1 FOR NO KEY UPDATE`, user).Scan(&family))
	require.NoError(t, blocker.QueryRow(`SELECT txid_current()`).Scan(&xid))
	tx := transaction(t, db)
	actor := authority(t, tx, user, lf.UserNoKeyUpdate)
	_, err := tx.Exec(`SET LOCAL lock_timeout='2s'`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "page-failure", "198.51.100.1", "")
		done <- err
	}()
	testdb.WaitForRowLockWaiter(t, db, xid)
	// Observe the second-page waiter before the database lock timeout fails it.
	reason(t, <-done, lf.ErrUnavailable, lf.ReasonSQLUnavailable)
	require.NoError(t, tx.Rollback())
	require.NoError(t, blocker.Commit())
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
	require.EqualValues(t, 207, count(t, db, `SELECT count(*) FROM refresh_tokens`))
}

func TestDisplacementTerminalTargetRetainsExistingOwner(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	seed := transaction(t, db)
	u := authority(t, seed, user, lf.UserNoKeyUpdate)
	old := displacementFamily(t, seed, u, user, "terminal", "198.51.100.1", "", false, 1)
	require.NoError(t, seed.Commit())
	terminalTx := transaction(t, db)
	terminalUser := authority(t, terminalTx, user, lf.UserNoKeyUpdate)
	oldTargets, err := lf.ResolveTargetSIDTx(ctx, terminalTx, terminalUser, old.SID)
	require.NoError(t, err)
	original := displacementRevoke(t, terminalTx, oldTargets)
	require.EqualValues(t, 1, original.NewlyTerminalFamilies)
	require.NoError(t, terminalTx.Commit())
	// Refresh metadata is not family authority: an unrevoked retained row whose
	// family is already terminal must resolve to that family's existing owner.
	_, err = db.Exec(`UPDATE refresh_tokens SET revoked_at=NULL WHERE id=$1`, old.SID)
	require.NoError(t, err)
	tx := transaction(t, db)
	actor := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "terminal", "198.51.100.1", "")
	require.NoError(t, err)
	replay := displacementRevoke(t, tx, targets)
	require.Zero(t, replay.NewlyTerminalFamilies)
	require.Equal(t, original.Owners, replay.Owners)
	require.NoError(t, tx.Commit())
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM login_family_revocations`))
	require.EqualValues(t, 1, count(t, db, `SELECT count(*) FROM login_family_revocation_targets`))
	_, err = lf.ResolveVerifiedAccess(ctx, db, old)
	reason(t, err, lf.ErrDenied, lf.ReasonFamilyTerminal)
}

func TestDisplacementSQLFailureReturnsUnavailableWithoutOperation(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	tx := transaction(t, db)
	actor := authority(t, tx, user, lf.UserNoKeyUpdate)
	_, err := tx.Exec(`SELECT 1/0`)
	require.Error(t, err)
	_, err = lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "", "", "")
	reason(t, err, lf.ErrUnavailable, lf.ReasonSQLUnavailable)
	require.NoError(t, tx.Rollback())
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
}

func TestDisplacementEmptyFallbackIPFailsWithoutWildcardOrMutation(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	seed := transaction(t, db)
	u := authority(t, seed, user, lf.UserNoKeyUpdate)
	displacementFamily(t, seed, u, user, nil, nil, "", false, 1)
	displacementFamily(t, seed, u, user, "", "198.51.100.5", "", false, 1)
	require.NoError(t, seed.Commit())
	tx := transaction(t, db)
	actor := authority(t, tx, user, lf.UserNoKeyUpdate)
	_, err := lf.CaptureDisplacementTargetsTx(ctx, tx, actor, "", "", "")
	// ip_address is INET: SQL equality with an empty address is invalid input,
	// never a wildcard matching NULL or every address.
	reason(t, err, lf.ErrUnavailable, lf.ReasonSQLUnavailable)
	require.NoError(t, tx.Rollback())
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_families WHERE terminal_at IS NOT NULL`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`))
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))
}
