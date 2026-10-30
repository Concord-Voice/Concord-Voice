package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/models"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// #2418: every session mint is bound to the epoch it was AUTHORIZED under, not
// merely the epoch current at mint time. A login authorized before a destructive
// reset must not mint after it, whether it arrived by password or by completing an
// MFA challenge issued pre-reset.

// newRejectPathHandler builds the minimal handler for tests that assert a REFUSAL.
// The epoch check still runs before any token MINTING, so this exercises the guard
// in isolation from token generation.
//
// #2450: it is no longer DB-only. entCache.GetTier moved ABOVE BeginTx because
// calling it under the users-row lock acquired a second pool connection while the
// handler already held one for the tx — a pool-exhaustion deadlock near
// MaxOpenConns, on a lock every destructive reset needs. The deliberate trade is
// that a login destined for refusal now pays one tier lookup: the refusal path is
// rare (it fires only during an epoch race) and the lookup is a Redis GET that
// reads through to the DB only on a miss, so the wasted work is negligible next to
// the availability risk it removes. Production always wires this cache
// (internal/api/router.go), so the nil cache here was a test-only shape.
func newRejectPathHandler(t *testing.T) (*Handler, *sql.DB, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	rdb := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)
	return &Handler{
		db:       db,
		log:      logger.NewWithWriter(io.Discard),
		entCache: entitlements.NewCache(rdb, db),
	}, db, userID
}

// newAdmitPathHandler builds a handler that can mint: Redis for the auth-failure
// clear, an entitlement cache for the tier claim, and the user_keys row
// CompleteLogin returns in its response payload (dbtest.CreateUser does not seed it).
func newAdmitPathHandler(t *testing.T) (*Handler, *sql.DB, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, _ := dbtest.SetupTestDB(t)
	rdb := setupAuthAttemptRedis(t)
	userID := dbtest.CreateUser(t, db)

	// internal/testhelpers imports internal/auth, so an in-package test cannot use
	// its E2EETestKeys fixture. CompleteLogin only base64-encodes these bytes into
	// its response, so opaque literals are sufficient.
	_, err := db.Exec(
		`INSERT INTO user_keys (user_id, wrapped_private_key, key_derivation_salt, key_version, key_derivation_alg)
		 VALUES ($1, $2, $3, 1, 'argon2id')`,
		userID, []byte("test-wrapped-private-key"), []byte("test-derivation-salt"),
	)
	require.NoError(t, err)

	h := &Handler{
		db:        db,
		redis:     rdb,
		log:       logger.NewWithWriter(io.Discard),
		jwtSecret: "test-jwt-secret-epoch-binding",
		entCache:  entitlements.NewCache(rdb, db),
	}
	return h, db, userID
}

func newLoginContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, rec
}

type tokenTheftTestDisconnector struct{}

func (tokenTheftTestDisconnector) DisconnectUser(uuid.UUID) {}
func (tokenTheftTestDisconnector) DisconnectFamilyContext(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

// epochFixtureSnapshot includes immutable membership and durable ownership as well
// as refresh rows: refusal must not silently terminalize a family or mint an owner.
func epochFixtureSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'refresh', (SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM refresh_tokens r),
 'families', (SELECT jsonb_agg(to_jsonb(f) ORDER BY family_id) FROM login_families f),
 'sids', (SELECT jsonb_agg(to_jsonb(s) ORDER BY sid) FROM login_family_sids s),
 'owners', (SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM login_family_revocations o))::text`).Scan(&snapshot))
	return snapshot
}

func epochFixtureFamilySnapshot(t *testing.T, db *sql.DB, family string) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT jsonb_build_object(
 'family', (SELECT to_jsonb(f) FROM login_families f WHERE family_id=$1),
 'sids', (SELECT jsonb_agg(to_jsonb(s) ORDER BY sid) FROM login_family_sids s WHERE family_id=$1),
 'refresh', (SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM refresh_tokens r
 JOIN login_family_sids s ON s.sid=r.id AND s.user_id=r.user_id WHERE s.family_id=$1))::text`, family).Scan(&snapshot))
	return snapshot
}

func epochFixtureFamilyState(t *testing.T, rig familyTestRig, family string, terminal bool, reason loginfamily.AuthReason) {
	t.Helper()
	at, owner := terminalState(t, rig, family)
	require.Equal(t, terminal, at.Valid)
	require.Equal(t, terminal, owner.Valid)
	if terminal {
		var actualReason string
		require.NoError(t, rig.db.QueryRow(`SELECT reason FROM login_family_revocations WHERE operation_id=$1 AND user_id=$2`, owner.String, rig.user).Scan(&actualReason))
		assert.Equal(t, string(reason), actualReason)
	}
}

// Prepare/Bind is the public historical-mint seam: the immutable SQL expiry and
// the genuinely signed expired token come from the same prepared token.
func epochFixtureExpiredAccess(t *testing.T, rig familyTestRig) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := rig.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	user, err := lockMintUserTx(ctx, tx, rig.user)
	require.NoError(t, err)
	family, err := loginfamily.CreateFamilyTx(ctx, tx, user, "")
	require.NoError(t, err)
	sid := uuid.New()
	prepared, err := PrepareAccessToken(AccessTokenInput{UserID: uuid.MustParse(rig.user), SID: sid, EmailVerified: true}, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, loginfamily.BindSIDTx(ctx, tx, family, sid, prepared.ExpiresAt()))
	secret, err := GenerateRefreshToken()
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO refresh_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,NOW()+INTERVAL '1 day')`, sid, rig.user, HashRefreshToken(secret))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	access, err := prepared.Sign(familyTestSecret)
	require.NoError(t, err)
	_, err = ValidateAccessToken(access, familyTestSecret)
	require.ErrorIs(t, err, ErrExpiredToken)
	var exact bool
	require.NoError(t, rig.db.QueryRow(`SELECT access_expires_at=$2 FROM login_family_sids WHERE sid=$1`, sid, prepared.ExpiresAt()).Scan(&exact))
	require.True(t, exact)
	return sid.String(), prepared.ExpiresAt()
}

// TestHandleTokenTheftRevokesAfterRequestCancellation ensures a hostile client
// cannot abort the theft sweep by cancelling the request after authentication.
func TestHandleTokenTheftRevokesAfterRequestCancellation(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, family, _ := terminalMint(t, rig)
	rig.h.hub = tokenTheftTestDisconnector{}
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	c, rec := newLoginContext()
	c.Request = c.Request.WithContext(requestCtx)
	require.ErrorIs(t, c.Request.Context().Err(), context.Canceled)
	require.True(t, rig.h.handleTokenTheft(c, models.RefreshToken{UserID: rig.user}))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var activeCount int
	require.NoError(t, rig.db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL`, rig.user).Scan(&activeCount))
	assert.Zero(t, activeCount, "theft sweep must not inherit attacker request cancellation")
	epochFixtureFamilyState(t, rig, family, true, loginfamily.ReasonTheft)
}

func TestRevokeAllRefreshTokens_ExcludesCurrentToken(t *testing.T) {
	rig := newFamilyTestRig(t)
	current, currentFamily, currentSID := terminalMint(t, rig)
	_, otherFamily, otherSID := terminalMint(t, rig)
	_, revokedFamily, revokedSID := terminalMint(t, rig)
	_, err := RevokeRefreshSession(actorContext(t, current.Access), rig.db, rig.user, revokedSID, "")
	require.NoError(t, err)
	var revokedBefore time.Time
	require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, revokedSID).Scan(&revokedBefore))
	revokedAt, revokedOwner := terminalState(t, rig, revokedFamily)
	foreign := rig
	foreign.user = dbtest.CreateUser(t, rig.db).String()
	_, foreignFamily, foreignSID := terminalMint(t, foreign)

	result, err := RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, RevokeAllRefreshTokensOptions{ExcludeTokenHash: HashRefreshToken(current.Refresh)})
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.RevokedRefreshRows)
	assert.EqualValues(t, 1, result.NewlyTerminalFamilies)
	for sid, want := range map[string]bool{currentSID: false, otherSID: true, foreignSID: false} {
		var at sql.NullTime
		require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, sid).Scan(&at))
		assert.Equal(t, want, at.Valid)
	}
	epochFixtureFamilyState(t, rig, currentFamily, false, "")
	epochFixtureFamilyState(t, rig, otherFamily, true, loginfamily.ReasonExplicitRevoke)
	epochFixtureFamilyState(t, foreign, foreignFamily, false, "")
	var revokedAfter time.Time
	require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, revokedSID).Scan(&revokedAfter))
	assert.Equal(t, revokedBefore, revokedAfter)
	at, owner := terminalState(t, rig, revokedFamily)
	assert.Equal(t, revokedAt, at)
	assert.Equal(t, revokedOwner, owner)
}

func TestRevokeAllRefreshTokens_ResolvesActiveExcludedSession(t *testing.T) {
	for _, scenario := range []string{"selected bearer", "rotated bearer", "expired refresh bearer", "unknown bearer", "terminal bearer", "foreign bearer", "mismatched expiry bearer", "signed expired bearer", "cookie only", "revoked cookie only", "expired cookie only"} {
		t.Run(scenario, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			bearer, bearerFamily, bearerSID := terminalMint(t, rig)
			cookie, cookieFamily, cookieSID := terminalMint(t, rig)
			other, otherFamily, _ := terminalMint(t, rig)
			foreign := rig
			foreign.user = dbtest.CreateUser(t, rig.db).String()
			_, foreignFamily, foreignSID := terminalMint(t, foreign)
			claims, err := ValidateAccessToken(bearer.Access, familyTestSecret)
			require.NoError(t, err)
			options := RevokeAllRefreshTokensOptions{ExcludeSessionID: bearerSID, ExcludeAccessExpiresAt: claims.ExpiresAt.Time, ExcludeTokenHash: HashRefreshToken(cookie.Refresh)}
			preserved := bearerFamily
			switch scenario {
			case "rotated bearer":
				c, w := familyTestContext()
				c.Request.Header.Set("X-Refresh-Token", bearer.Refresh)
				rig.h.Refresh(c)
				rotated := familyResponse(t, w)
				family, _ := assertFamilyAccess(t, rig, rotated.Access)
				require.Equal(t, bearerFamily, family)
				_, err = rig.db.Exec(`UPDATE refresh_tokens SET expires_at=NOW()-INTERVAL '1 hour' WHERE id=$1`, bearerSID)
				require.NoError(t, err)
			case "expired refresh bearer", "expired cookie only":
				sid := bearerSID
				if scenario == "expired cookie only" {
					sid = cookieSID
				}
				_, err = rig.db.Exec(`UPDATE refresh_tokens SET expires_at=NOW()-INTERVAL '1 hour' WHERE id=$1`, sid)
				require.NoError(t, err)
			case "unknown bearer":
				options.ExcludeSessionID = uuid.NewString()
			case "terminal bearer", "revoked cookie only":
				sid := bearerSID
				if scenario == "revoked cookie only" {
					sid = cookieSID
				}
				_, err = RevokeRefreshSession(actorContext(t, other.Access), rig.db, rig.user, sid, "")
				require.NoError(t, err)
			case "foreign bearer":
				options.ExcludeSessionID = foreignSID
			case "mismatched expiry bearer":
				options.ExcludeAccessExpiresAt = claims.ExpiresAt.Add(time.Second)
			case "signed expired bearer":
				options.ExcludeSessionID, options.ExcludeAccessExpiresAt = epochFixtureExpiredAccess(t, rig)
			}
			switch scenario {
			case "cookie only", "revoked cookie only", "expired cookie only":
				options.ExcludeSessionID = ""
				options.ExcludeAccessExpiresAt = time.Time{}
				preserved = cookieFamily
			}
			refusal := scenario == "unknown bearer" || scenario == "terminal bearer" || scenario == "foreign bearer" || scenario == "mismatched expiry bearer" || scenario == "signed expired bearer" || scenario == "revoked cookie only" || scenario == "expired cookie only"
			before := epochFixtureSnapshot(t, rig.db)
			preservedBefore := epochFixtureFamilySnapshot(t, rig.db, preserved)
			foreignBefore := epochFixtureFamilySnapshot(t, rig.db, foreignFamily)
			result, err := RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, options)
			if refusal {
				require.ErrorIs(t, err, ErrNoActiveRefreshSession)
				assert.Equal(t, loginfamily.RevocationResult{}, result)
				assert.Equal(t, before, epochFixtureSnapshot(t, rig.db), "refusal must preserve all rows and terminal owners")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, preservedBefore, epochFixtureFamilySnapshot(t, rig.db, preserved))
			assert.Equal(t, foreignBefore, epochFixtureFamilySnapshot(t, rig.db, foreignFamily))
			assert.EqualValues(t, 2, result.RevokedRefreshRows)
			assert.EqualValues(t, 2, result.NewlyTerminalFamilies)
			for _, family := range []string{bearerFamily, cookieFamily, otherFamily} {
				epochFixtureFamilyState(t, rig, family, family != preserved, loginfamily.ReasonExplicitRevoke)
			}
			epochFixtureFamilyState(t, foreign, foreignFamily, false, "")
			var live int
			require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens r JOIN login_family_sids s ON s.sid=r.id WHERE s.family_id=$1 AND r.revoked_at IS NULL`, preserved).Scan(&live))
			assert.Equal(t, 1, live, "the entire preserved family retains its active successor")
			var foreignRevoked sql.NullTime
			require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, foreignSID).Scan(&foreignRevoked))
			assert.False(t, foreignRevoked.Valid)
		})
	}
}

func TestRevokeAllRefreshTokens_WithoutExclusionPreservesOtherUsersAndRevokedRows(t *testing.T) {
	rig := newFamilyTestRig(t)
	active, activeFamily, activeSID := terminalMint(t, rig)
	_, revokedFamily, revokedSID := terminalMint(t, rig)
	_, err := RevokeRefreshSession(actorContext(t, active.Access), rig.db, rig.user, revokedSID, "")
	require.NoError(t, err)
	var revokedBefore time.Time
	require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, revokedSID).Scan(&revokedBefore))
	beforeAt, beforeOwner := terminalState(t, rig, revokedFamily)
	foreign := rig
	foreign.user = dbtest.CreateUser(t, rig.db).String()
	_, foreignFamily, foreignSID := terminalMint(t, foreign)
	result, err := RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, RevokeAllRefreshTokensOptions{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.RevokedRefreshRows)
	assert.EqualValues(t, 1, result.NewlyTerminalFamilies)
	for sid, expected := range map[string]bool{activeSID: true, foreignSID: false} {
		var at sql.NullTime
		require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, sid).Scan(&at))
		assert.Equal(t, expected, at.Valid)
	}
	epochFixtureFamilyState(t, rig, activeFamily, true, loginfamily.ReasonExplicitRevoke)
	epochFixtureFamilyState(t, foreign, foreignFamily, false, "")
	var revokedAfter time.Time
	require.NoError(t, rig.db.QueryRow(`SELECT revoked_at FROM refresh_tokens WHERE id=$1`, revokedSID).Scan(&revokedAfter))
	assert.Equal(t, revokedBefore, revokedAfter)
	at, owner := terminalState(t, rig, revokedFamily)
	assert.Equal(t, beforeAt, at)
	assert.Equal(t, beforeOwner, owner)
}

func TestRevokeAllRefreshTokens_RejectsSupersededCredentialEpoch(t *testing.T) {
	rig := newFamilyTestRig(t)
	const currentEpoch = "44444444444444444444444444444444"
	_, err := rig.db.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, rig.user, currentEpoch)
	require.NoError(t, err)
	terminalMint(t, rig)
	before := epochFixtureSnapshot(t, rig.db)
	result, err := RevokeAllRefreshTokens(context.Background(), rig.db, rig.user, RevokeAllRefreshTokensOptions{
		ExpectedCredentialEpoch: "55555555555555555555555555555555",
		EnforceCredentialEpoch:  true,
	})
	require.ErrorIs(t, err, credepoch.ErrEpochMismatch)
	assert.Equal(t, loginfamily.RevocationResult{}, result)
	assert.Equal(t, before, epochFixtureSnapshot(t, rig.db), "a stale step-up authorization must not revoke a newer session")
}

func TestRevokeAllRefreshTokens_UnknownUser_ReturnsError(t *testing.T) {
	rig := newFamilyTestRig(t)
	terminalMint(t, rig)
	before := epochFixtureSnapshot(t, rig.db)
	result, err := RevokeAllRefreshTokens(context.Background(), rig.db, uuid.NewString(), RevokeAllRefreshTokensOptions{})
	require.ErrorIs(t, err, loginfamily.ErrDenied)
	var classified *loginfamily.Error
	require.ErrorAs(t, err, &classified)
	assert.Equal(t, loginfamily.ReasonAccessIdentity, classified.Reason())
	assert.Equal(t, loginfamily.RevocationResult{}, result)
	assert.Equal(t, before, epochFixtureSnapshot(t, rig.db))
}

func TestRevokeAllRefreshTokens_CanceledContext_ReturnsError(t *testing.T) {
	rig := newFamilyTestRig(t)
	terminalMint(t, rig)
	before := epochFixtureSnapshot(t, rig.db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := RevokeAllRefreshTokens(ctx, rig.db, rig.user, RevokeAllRefreshTokensOptions{})
	require.ErrorIs(t, err, loginfamily.ErrUnavailable)
	var classified *loginfamily.Error
	require.ErrorAs(t, err, &classified)
	assert.Equal(t, loginfamily.ReasonSQLUnavailable, classified.Reason())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, loginfamily.RevocationResult{}, result)
	assert.Equal(t, before, epochFixtureSnapshot(t, rig.db))
}

func TestCompleteLoginRejectsSupersededEpoch(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)

	// The login was authorized under E1; a destructive reset then advanced the
	// durable epoch to E2 before the mint could run.
	_, err := db.Exec(`UPDATE users SET credential_epoch = '22222222222222222222222222222222' WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "11111111111111111111111111111111", securityevent.AuthPassword)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"a login authorized under a superseded epoch must be refused")
	require.Equal(t, []securityevent.Event{{
		EventType:  securityevent.EventCredentialEpoch,
		Outcome:    securityevent.OutcomeDenied,
		Severity:   securityevent.SeverityHigh,
		ReasonCode: securityevent.ReasonCredentialEpochMismatch,
	}}, recorder.events)
	require.True(t, middleware.NightwatchHandled(c))
	serialized, err := json.Marshal(recorder.events)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), userID.String())
	require.NotContains(t, string(serialized), "11111111111111111111111111111111")
	require.NotContains(t, string(serialized), "22222222222222222222222222222222")

	var live int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
	).Scan(&live))
	assert.Zero(t, live, "a refused login must leave no live refresh row")
}

// TestCompleteLoginRejectsClaimlessChallengeForRotatedUser locks the fail-closed
// legacy decision (spec §5): a pre-#2418 challenge carries no epoch claim, and for a
// user who has rotated that is refused rather than admitted. Deliberate deviation
// from #2418's stated acceptance criteria; exposure is one 5-minute challenge TTL.
func TestCompleteLoginRejectsClaimlessChallengeForRotatedUser(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)

	_, err := db.Exec(`UPDATE users SET credential_epoch = '11111111111111111111111111111111' WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "", securityevent.AuthPassword)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"a claimless legacy challenge must be refused for a rotated user")
	var live int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL`, userID).Scan(&live))
	assert.Zero(t, live, "a refused challenge must leave no live refresh row")
}

func TestCompleteLoginAdmitsCurrentEpoch(t *testing.T) {
	rig := newFamilyTestRig(t)
	const epoch = "11111111111111111111111111111111"
	_, err := rig.db.Exec(`UPDATE users SET credential_epoch=$2, email_verified=true WHERE id=$1`, rig.user, epoch)
	require.NoError(t, err)
	c, rec := newLoginContext()
	require.True(t, rig.h.CompleteLogin(c, rig.user, true, epoch, securityevent.AuthPassword))
	require.Equal(t, http.StatusOK, rec.Code, "an uncontended login must succeed")
	pair := familyResponse(t, rec)
	assertFamilyAccess(t, rig, pair.Access)
	claims, err := ValidateAccessToken(pair.Access, familyTestSecret)
	require.NoError(t, err)
	assert.Equal(t, epoch, claims.CredentialEpoch)
	var live int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL`, rig.user).Scan(&live))
	assert.Equal(t, 1, live, "a successful login mints exactly one live refresh row")
}

// TestCompleteLoginAdmitsNeverRotatedUser locks the MatchEpoch NULL-admits rule: a
// user who has never rotated accepts any expectedEpoch, including "". This is what
// keeps never-rotated accounts and the pre-rollout deploy window working.
func TestCompleteLoginAdmitsNeverRotatedUser(t *testing.T) {
	h, db, userID := newAdmitPathHandler(t)

	_, err := db.Exec(`UPDATE users SET email_verified = true WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "", securityevent.AuthPassword)

	assert.Equal(t, http.StatusOK, rec.Code,
		"a never-rotated user (NULL epoch) admits any expected epoch")
}

// TestCompleteLoginRefusesDisabledAccount covers the terminal disabled gate inside
// the locked snapshot — the check now reads `disabled` under FOR NO KEY UPDATE, so a
// disable committing concurrently cannot slip a session past it.
func TestCompleteLoginRefusesDisabledAccount(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)

	_, err := db.Exec(`UPDATE users SET disabled = TRUE WHERE id = $1`, userID)
	require.NoError(t, err)

	c, rec := newLoginContext()
	h.CompleteLogin(c, userID.String(), true, "", securityevent.AuthPassword)

	assert.Equal(t, http.StatusForbidden, rec.Code, "a disabled account must not mint a session")

	var live int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, userID,
	).Scan(&live))
	assert.Zero(t, live)
}

// TestReadCredentialEpoch covers the helper that backs challenge stamping, including
// its fail-closed behaviour for an unknown user — the SSO and suspicious-refresh
// paths both abort rather than stamping an empty epoch.
func TestReadCredentialEpoch(t *testing.T) {
	h, db, userID := newRejectPathHandler(t)

	epoch, err := h.readCredentialEpoch(context.Background(), userID.String())
	require.NoError(t, err)
	assert.Empty(t, epoch, "a never-rotated user reads an empty epoch")

	_, err = db.Exec(`UPDATE users SET credential_epoch = '11111111111111111111111111111111' WHERE id = $1`, userID)
	require.NoError(t, err)
	epoch, err = h.readCredentialEpoch(context.Background(), userID.String())
	require.NoError(t, err)
	assert.Equal(t, "11111111111111111111111111111111", epoch)

	_, err = h.readCredentialEpoch(context.Background(), uuid.New().String())
	require.Error(t, err, "an unknown user must error, never yield an empty epoch")
}
