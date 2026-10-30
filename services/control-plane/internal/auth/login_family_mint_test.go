package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginFamilyPreparedAccessFreezesExactClaims(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 30, 0, 987654321, time.UTC)
	input := AccessTokenInput{UserID: uuid.New(), SID: uuid.New(), CredentialEpoch: "00112233445566778899aabbccddeeff", EmailVerified: true, Tier: "premium"}
	prepared, err := PrepareAccessToken(input, now)
	require.NoError(t, err)
	expected := jwt.NewNumericDate(now.Add(15 * time.Minute)).Time
	require.True(t, prepared.ExpiresAt().Equal(expected))
	first, err := prepared.Sign(familyTestSecret)
	require.NoError(t, err)
	second, err := prepared.Sign(familyTestSecret)
	require.NoError(t, err)
	require.Equal(t, first, second, "signing again cannot choose another JTI or consult the clock")
	claims := &Claims{}
	_, _, err = jwt.NewParser().ParseUnverified(first, claims)
	require.NoError(t, err)
	assert.Equal(t, input.UserID.String(), claims.UserID)
	assert.Equal(t, input.SID.String(), claims.SessionID)
	assert.Equal(t, input.CredentialEpoch, claims.CredentialEpoch)
	require.NotNil(t, claims.EmailVerified)
	assert.True(t, *claims.EmailVerified)
	assert.Equal(t, input.Tier, claims.Tier)
	assert.NotEmpty(t, claims.ID)
	assert.True(t, claims.IssuedAt.Equal(jwt.NewNumericDate(now).Time))
	assert.True(t, claims.NotBefore.Equal(claims.IssuedAt.Time))
	assert.True(t, claims.ExpiresAt.Equal(prepared.ExpiresAt()))
	assert.Equal(t, 15*time.Minute, claims.ExpiresAt.Sub(claims.IssuedAt.Time))
	// The historical instant deliberately predates execution: late signing must
	// retain that instant even if the resulting token has already expired.
	assert.True(t, claims.ExpiresAt.Equal(expected))
}

func TestLoginFamilyPreparedAccessRejectsInvalidIdentityAndZeroValue(t *testing.T) {
	for _, input := range []AccessTokenInput{{}, {UserID: uuid.New()}, {SID: uuid.New()}} {
		_, err := PrepareAccessToken(input, time.Now())
		require.Error(t, err)
	}
	var zero PreparedAccessToken
	token, err := zero.Sign(familyTestSecret)
	require.Error(t, err)
	assert.Empty(t, token)
	assert.True(t, zero.ExpiresAt().IsZero())
}

// Four production INSERT owners: registration completion's generator,
// password/MFA's common final mint, OAuth, and intentional continuation.
// Normal and grace refresh exercise the other two owners in the sibling file.
func TestLoginFamilyCanonicalMintOwnersPersistSignedExpiry(t *testing.T) {
	for _, owner := range []string{"registration", "password-and-mfa-final-mint", "oauth", "continuation"} {
		t.Run(owner, func(t *testing.T) {
			rig := newFamilyTestRig(t)
			c, w := familyTestContext()
			var access, refresh, sid string
			var err error
			switch owner {
			case "registration":
				access, refresh, sid, err = rig.h.generateTokenPair(c, rig.user, true)
			case "password-and-mfa-final-mint":
				// Both authenticated password and successful MFA callers enter the same
				// credential-bound final mint; this covers that canonical SQL owner.
				require.True(t, rig.h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword))
				pair := familyResponse(t, w)
				access, refresh = pair.Access, pair.Refresh
			case "oauth":
				access, refresh, sid, err = rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
			case "continuation":
				access, refresh, sid, err = rig.h.IssueTokenPair(c, rig.user, "")
			}
			require.NoError(t, err)
			require.NotEmpty(t, refresh)
			family, signedSID := assertFamilyAccess(t, rig, access)
			if sid != "" {
				assert.Equal(t, sid, signedSID)
			}
			var count int
			require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_families WHERE user_id = $1 AND family_id = $2`, rig.user, family).Scan(&count))
			assert.Equal(t, 1, count)
			// Another deliberate credential mint always creates a distinct login.
			another, _, _, err := rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
			require.NoError(t, err)
			otherFamily, _ := assertFamilyAccess(t, rig, another)
			assert.NotEqual(t, family, otherFamily)
		})
	}
}

func TestLoginFamilyFailedMintRollsBackBothRefreshAndMembership(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, err := rig.db.Exec(`ALTER TABLE login_family_sids ADD CONSTRAINT family_fixture_refuse_bind CHECK (false) NOT VALID`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := rig.db.Exec(`ALTER TABLE login_family_sids DROP CONSTRAINT family_fixture_refuse_bind`)
		require.NoError(t, err)
	})
	access, refresh, sid, err := rig.h.IssueAccessAndRefresh(context.Background(), rig.user)
	require.Error(t, err)
	assert.Empty(t, access)
	assert.Empty(t, refresh)
	assert.Empty(t, sid)
	for _, query := range []string{`SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, `SELECT count(*) FROM login_families WHERE user_id = $1`, `SELECT count(*) FROM login_family_sids WHERE user_id = $1`} {
		var n int
		require.NoError(t, rig.db.QueryRow(query, rig.user).Scan(&n))
		assert.Zero(t, n)
	}
}

// Deferred failure occurs at COMMIT after successful INSERTs, so an early
// response/cookie write cannot hide behind an earlier statement failure.
func TestLoginFamilyCommitFailurePublishesNoCredentials(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, err := rig.db.Exec(`CREATE FUNCTION family_fixture_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture rejects commit'; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := rig.db.Exec(`DROP FUNCTION family_fixture_fail_commit() CASCADE`)
		require.NoError(t, err)
	})
	_, err = rig.db.Exec(`CREATE CONSTRAINT TRIGGER family_fixture_commit_guard AFTER INSERT ON login_family_sids DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION family_fixture_fail_commit()`)
	require.NoError(t, err)
	c, w := familyTestContext()
	require.False(t, rig.h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotContains(t, w.Body.String(), "access_token")
	assert.NotContains(t, w.Body.String(), "refresh_token")
	assert.Empty(t, w.Header().Values("Set-Cookie"))
	for _, query := range []string{`SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, `SELECT count(*) FROM login_families WHERE user_id = $1`, `SELECT count(*) FROM login_family_sids WHERE user_id = $1`} {
		var n int
		require.NoError(t, rig.db.QueryRow(query, rig.user).Scan(&n))
		assert.Zero(t, n, "failed commit must roll back all identity evidence")
	}
}

func TestLoginFamilyPasswordLoginRouteCreatesFreshFamily(t *testing.T) {
	rig := newFamilyTestRig(t)
	const password = "FamilyFixturePassphrase123!"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	var email string
	require.NoError(t, rig.db.QueryRow(`UPDATE users SET password_hash = $2 WHERE id = $1 RETURNING email`, rig.user, hash).Scan(&email))
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	require.NoError(t, err)
	var prior string
	for i := 0; i < 2; i++ {
		c, w := familyTestContext()
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(string(body)))
		c.Request.RemoteAddr = "192.0.2.10:1234"
		c.Request.Header.Set("Content-Type", "application/json")
		rig.h.Login(c)
		pair := familyResponse(t, w)
		family, _ := assertFamilyAccess(t, rig, pair.Access)
		if prior != "" {
			assert.NotEqual(t, prior, family)
		}
		prior = family
	}
}

func TestLoginFamilyRegistrationConfirmationRouteBindsFirstFamily(t *testing.T) {
	rig := newFamilyTestRig(t)
	ctx := context.Background()
	const password = "RegistrationFixturePassphrase123!"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	pending, _, _, err := rig.h.pending.InsertOrTakeover(ctx, InsertParams{Email: "family-registration@example.test", Username: "familyregistration", PasswordHash: hash, WrappedPrivateKey: []byte{1}, KeyDerivationSalt: []byte{2}, KeyDerivationAlg: "argon2id", PublicKey: []byte{3}}, password)
	require.NoError(t, err)
	rec, err := json.Marshal(verificationRecord{CodeHash: hashCode("123456"), Email: "family-registration@example.test"})
	require.NoError(t, err)
	require.NoError(t, rig.h.redis.Set(ctx, redisKey(pending), rec, time.Minute).Err())
	c, w := familyTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/register/confirm", strings.NewReader(`{"pending_id":"`+pending+`","code":"123456"}`))
	c.Request.RemoteAddr = "192.0.2.10:1234"
	c.Request.Header.Set("Content-Type", "application/json")
	rig.h.ConfirmRegistration(c)
	pair := familyResponse(t, w)
	claims, err := ValidateAccessToken(pair.Access, familyTestSecret)
	require.NoError(t, err)
	rig.user = claims.UserID
	assertFamilyAccess(t, rig, pair.Access)
	require.NotEmpty(t, w.Header().Values("Set-Cookie"))
}

// Authority errors intentionally retain route-specific credential/internal
// envelopes. Wrapped SQL failures must remain retryable 503s, never look like
// an invalid credential; denials must never expose the underlying cause.
func TestLoginFamilyAuthorityErrorHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"wrapped SQL unavailable", fmt.Errorf("lookup: %w", loginfamily.NewError(loginfamily.ReasonSQLUnavailable, errors.New("private storage detail"))), http.StatusServiceUnavailable, "Authentication temporarily unavailable"},
		{"ambiguous commit", loginfamily.NewError(loginfamily.ReasonCommitUnknown, nil), http.StatusServiceUnavailable, "Authentication temporarily unavailable"},
		{"terminal family", loginfamily.NewError(loginfamily.ReasonFamilyTerminal, nil), http.StatusUnauthorized, "route credential denied"},
		{"expired authority", loginfamily.NewError(loginfamily.ReasonAccessExpired, nil), http.StatusUnauthorized, "route credential denied"},
		{"invalid capability", loginfamily.NewError(loginfamily.ReasonCapabilityInvalid, nil), http.StatusInternalServerError, "route internal failure"},
		{"unexpected failure", errors.New("private storage detail"), http.StatusInternalServerError, "route internal failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := familyTestContext()
			writeLoginFamilyError(c, tc.err, "route credential denied", "route internal failure")
			require.Equal(t, tc.status, w.Code)
			var body map[string]string
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, map[string]string{"error": tc.message}, body)
			assert.Empty(t, w.Header().Values("Set-Cookie"))
		})
	}
}

// Each canonical caller must return no credentials when its own transaction
// cannot begin. A closed real SQL pool gives a deterministic storage outage.
func TestLoginFamilyCanonicalMintStorageUnavailable(t *testing.T) {
	rig := newFamilyTestRig(t)
	broken, err := sql.Open("postgres", "postgres://127.0.0.1:1/unused")
	require.NoError(t, err)
	require.NoError(t, broken.Close())
	h := NewHandler(broken, rig.h.redis, rig.h.log, familyTestSecret, nil)
	h.entCache = rig.h.entCache
	for _, owner := range []string{"registration", "continuation", "oauth", "password-mfa"} {
		t.Run(owner, func(t *testing.T) {
			c, w := familyTestContext()
			var access, refresh, sid string
			var err error
			switch owner {
			case "registration":
				access, refresh, sid, err = h.generateTokenPair(c, rig.user, true)
			case "continuation":
				access, refresh, sid, err = h.IssueTokenPair(c, rig.user, "")
			case "oauth":
				access, refresh, sid, err = h.IssueAccessAndRefresh(context.Background(), rig.user)
			case "password-mfa":
				require.False(t, h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword))
				require.Equal(t, http.StatusInternalServerError, w.Code)
			}
			if owner != "password-mfa" {
				require.Error(t, err)
			}
			assert.Empty(t, access)
			assert.Empty(t, refresh)
			assert.Empty(t, sid)
			assert.Empty(t, w.Header().Values("Set-Cookie"))
		})
	}
	var count int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_family_sids WHERE user_id=$1`, rig.user).Scan(&count))
	assert.Zero(t, count)
}

func TestLoginFamilyCanonicalMintRejectsUnknownUser(t *testing.T) {
	rig := newFamilyTestRig(t)
	missing := uuid.NewString()
	for _, owner := range []string{"registration", "continuation", "oauth", "password-mfa"} {
		t.Run(owner, func(t *testing.T) {
			c, w := familyTestContext()
			var access, refresh, sid string
			var err error
			switch owner {
			case "registration":
				access, refresh, sid, err = rig.h.generateTokenPair(c, missing, true)
			case "continuation":
				access, refresh, sid, err = rig.h.IssueTokenPair(c, missing, "")
			case "oauth":
				access, refresh, sid, err = rig.h.IssueAccessAndRefresh(context.Background(), missing)
			case "password-mfa":
				require.False(t, rig.h.CompleteLogin(c, missing, true, "", securityevent.AuthPassword))
				require.Equal(t, http.StatusUnauthorized, w.Code)
			}
			if owner != "password-mfa" {
				require.Error(t, err)
			}
			assert.Empty(t, access)
			assert.Empty(t, refresh)
			assert.Empty(t, sid)
			assert.Empty(t, w.Header().Values("Set-Cookie"))
		})
	}
}

func TestLoginFamilyContinuationRejectsStaleCredentialAuthority(t *testing.T) {
	rig := newFamilyTestRig(t)
	_, err := rig.db.Exec(`UPDATE users SET credential_epoch=$2 WHERE id=$1`, rig.user, "00112233445566778899aabbccddeeff")
	require.NoError(t, err)
	c, w := familyTestContext()
	access, refresh, sid, err := rig.h.IssueTokenPair(c, rig.user, "")
	require.ErrorIs(t, err, ErrContinuationEpochAdvanced)
	assert.Empty(t, access)
	assert.Empty(t, refresh)
	assert.Empty(t, sid)
	assert.Empty(t, w.Header().Values("Set-Cookie"))
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT count(*) FROM login_families WHERE user_id=$1`, rig.user).Scan(&n))
	assert.Zero(t, n)
}

func TestLoginFamilyAuthorityWaitCancellationIsUnavailable(t *testing.T) {
	rig := newFamilyTestRig(t)
	ctx := context.Background()
	blocker, err := rig.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		err := blocker.Rollback()
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	_, err = lockMintUserTx(ctx, blocker, rig.user)
	require.NoError(t, err)
	var pid int
	require.NoError(t, blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	c, w := familyTestContext()
	requestCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(requestCtx)
	done := make(chan bool, 1)
	go func() { done <- rig.h.CompleteLogin(c, rig.user, true, "", securityevent.AuthPassword) }()
	require.Eventually(t, func() bool {
		var waiting int
		err := rig.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiting)
		return err == nil && waiting > 0
	}, 5*time.Second, 10*time.Millisecond, "request must reach the real SQL authority wait before cancellation")
	cancel()
	select {
	case minted := <-done:
		require.False(t, minted)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled authority wait did not return")
	}
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.JSONEq(t, `{"error":"Authentication temporarily unavailable"}`, w.Body.String())
	assert.Empty(t, w.Header().Values("Set-Cookie"))
}
