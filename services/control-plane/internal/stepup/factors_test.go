package stepup

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// The literal inlineMFAMethodsSQL carried before it was assembled from the
// shared P1 fragments. It is authorization-critical (rbac's dangerous-permission
// mask and every step-up gate read it), so the refactor must not change a byte.
const inlineMFAMethodsSQLBeforeFragments = `
	SELECT EXISTS (SELECT 1 FROM user_mfa_totp WHERE user_id = $1 AND enabled AND confirmed),
	       EXISTS (SELECT 1 FROM user_mfa_webauthn WHERE user_id = $1)`

func TestInlineMFAMethodsSQL_RenderedUnchanged(t *testing.T) {
	require.Equal(t, inlineMFAMethodsSQLBeforeFragments, inlineMFAMethodsSQL)
}

func TestChooseDefault(t *testing.T) {
	early := time.Unix(1_000_000, 0)
	late := early.Add(time.Hour)
	both := []string{MethodTOTP, MethodWebAuthn}

	cases := []struct {
		name       string
		methods    []string
		totpAt     *time.Time
		webauthnAt *time.Time
		want       string
	}{
		{"no methods", []string{}, &late, &early, ""},
		{"nil methods", nil, nil, nil, ""},
		{"totp only, untimed", []string{MethodTOTP}, nil, nil, MethodTOTP},
		{"webauthn only, timed later than an absent totp", []string{MethodWebAuthn}, &late, &early, MethodWebAuthn},
		{"both timed, totp later", both, &late, &early, MethodTOTP},
		{"both timed, webauthn later", both, &early, &late, MethodWebAuthn},
		{"both timed, exact tie goes to webauthn", both, &early, &early, MethodWebAuthn},
		{"only totp timed", both, &early, nil, MethodTOTP},
		{"only webauthn timed", both, nil, &early, MethodWebAuthn},
		{"neither timed: the stronger factor", both, nil, nil, MethodWebAuthn},
		{"unknown names are ignored", []string{"email", "sms"}, nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, chooseDefault(tc.methods, tc.totpAt, tc.webauthnAt))
		})
	}
}

// matcherSkew is mfa/totp.go's totpSkew: an acceptance at t stores one of the
// steps t/30-1, t/30 and t/30+1, so the stored time lies in (t-60s, t+30s].
const matcherSkew = 1

// matchableSteps are the steps an acceptance at unix second t may store.
func matchableSteps(t int64) []int64 {
	s := t / totpStepSeconds
	return []int64{s - matcherSkew, s, s + matcherSkew}
}

// TestChooseDefault_TOTPRecencyBand pins both edges of the band (spec §3, C7):
// the order of a TOTP and a WebAuthn use holds whatever step was stored once
// TOTP is 60s or more later, or WebAuthn is 30s or more later. One second
// inside either edge, some step alignment inverts it. Every alignment of the
// key's use within a step is tried.
func TestChooseDefault_TOTPRecencyBand(t *testing.T) {
	both := []string{MethodTOTP, MethodWebAuthn}
	at := func(sec int64) *time.Time { tm := time.Unix(sec, 0); return &tm }
	const base = int64(1_000_000_020) // a step boundary

	// pick reports, over every alignment and every storable step, whether
	// the default was always and ever `want`.
	pick := func(totpAfterKey int64, want string) (always, ever bool) {
		always = true
		for off := range int64(totpStepSeconds) {
			key := base + off
			for _, step := range matchableSteps(key + totpAfterKey) {
				got := chooseDefault(both, at(step*totpStepSeconds), at(key)) == want
				always = always && got
				ever = ever || got
			}
		}
		return always, ever
	}

	always, _ := pick(60, MethodTOTP)
	require.True(t, always, "TOTP 60s after the key is the default on every stored step")
	_, inverted := pick(59, MethodWebAuthn)
	require.True(t, inverted, "59s is inside the band: some stored step reads as earlier than the key")

	always, _ = pick(-30, MethodWebAuthn)
	require.True(t, always, "a key 30s after TOTP is the default on every stored step")
	_, inverted = pick(-29, MethodTOTP)
	require.True(t, inverted, "29s is inside the band: some stored step reads as later than the key")
}

func TestFactors_DefaultWithin(t *testing.T) {
	early := time.Unix(1_000_000, 0)
	late := early.Add(time.Hour)
	f := Factors{Methods: []string{MethodTOTP, MethodWebAuthn}, totpLastUse: &late, webauthnLastUse: &early}

	require.Equal(t, MethodTOTP, f.Default())
	require.Equal(t, MethodTOTP, f.DefaultWithin([]string{"totp", "webauthn", "email"}))
	require.Equal(t, MethodWebAuthn, f.DefaultWithin([]string{"webauthn"}),
		"a surface that does not offer the recent factor gets the other one")
	require.Equal(t, "", f.DefaultWithin([]string{"email", "sms"}),
		"a challenge offering only recovery methods gets no default")
	require.Equal(t, "", f.DefaultWithin(nil))
}

func factorsSetTOTP(t *testing.T, db *sql.DB, userID string, lastStep any, hashes []string, used []bool) {
	t.Helper()
	_, err := db.Exec(`UPDATE user_mfa_totp SET last_used_step = $2, backup_codes_hash = $3, backup_codes_used = $4 WHERE user_id = $1`,
		userID, lastStep, pq.Array(hashes), pq.Array(used))
	require.NoError(t, err)
}

func factorsAddKey(t *testing.T, db *sql.DB, userID string, createdAt time.Time, lastUsed any) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_mfa_webauthn (id, user_id, credential_id, credential_name, credential_type, public_key, sign_count, created_at, last_used_at)
		VALUES ($1, $2, $3, 'Key', 'hardware', '\x00', 0, $4, $5)`,
		uuid.New().String(), userID, []byte("cred-"+uuid.New().String()), createdAt, lastUsed)
	require.NoError(t, err)
}

func TestInlineMFAFactors(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	stepAt := func(tm time.Time) int64 { return tm.Unix() / totpStepSeconds }

	t.Run("no factors: empty, never nil, no default, no backup", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		f, e := InlineMFAFactors(ctx, db, userID)
		require.Nil(t, e)
		require.NotNil(t, f.Methods)
		require.Empty(t, f.Methods)
		require.Equal(t, "", f.Default())
		require.False(t, f.BackupCodeAvailable)
	})

	t.Run("pending TOTP is ignored, its backup codes too", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestTOTP(t, db, userID, true, false)
		factorsSetTOTP(t, db, userID, stepAt(base), []string{"h1"}, []bool{false})
		f, e := InlineMFAFactors(ctx, db, userID)
		require.Nil(t, e)
		require.Empty(t, f.Methods)
		require.False(t, f.BackupCodeAvailable)
	})

	t.Run("TOTP used after the key: TOTP is the default", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestTOTP(t, db, userID, true, true)
		factorsSetTOTP(t, db, userID, stepAt(base.Add(10*time.Minute)), []string{}, []bool{})
		factorsAddKey(t, db, userID, base.Add(-time.Hour), base)
		f, e := InlineMFAFactors(ctx, db, userID)
		require.Nil(t, e)
		require.Equal(t, []string{MethodTOTP, MethodWebAuthn}, f.Methods)
		require.Equal(t, MethodTOTP, f.Default())
	})

	// A stored step reads as time step*30 (spec §3): a key used at that exact
	// second ties, which goes to WebAuthn, and one step later TOTP wins. A wrong
	// multiplier or a missing conversion fails one of the two.
	t.Run("step to time: a tie at step*30, TOTP one step on", func(t *testing.T) {
		keyStep := base.Unix() / totpStepSeconds
		keyAt := time.Unix(keyStep*totpStepSeconds, 0).UTC()
		for _, tc := range []struct {
			step int64
			want string
		}{
			{keyStep, MethodWebAuthn},
			{keyStep + 1, MethodTOTP},
		} {
			userID := subjectTestUser(t, db)
			subjectTestTOTP(t, db, userID, true, true)
			factorsSetTOTP(t, db, userID, tc.step, []string{}, []bool{})
			factorsAddKey(t, db, userID, keyAt.Add(-time.Hour), keyAt)
			f, e := InlineMFAFactors(ctx, db, userID)
			require.Nil(t, e)
			require.Equal(t, tc.want, f.Default(), "stored step %d", tc.step)
		}
	})

	t.Run("the most recent of several keys counts", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestTOTP(t, db, userID, true, true)
		factorsSetTOTP(t, db, userID, stepAt(base), []string{}, []bool{})
		factorsAddKey(t, db, userID, base.Add(-48*time.Hour), base.Add(-24*time.Hour))
		factorsAddKey(t, db, userID, base.Add(-48*time.Hour), base.Add(5*time.Minute))
		f, e := InlineMFAFactors(ctx, db, userID)
		require.Nil(t, e)
		require.Equal(t, MethodWebAuthn, f.Default())
	})

	t.Run("a registered, never-used key counts from its creation", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		subjectTestTOTP(t, db, userID, true, true)
		factorsSetTOTP(t, db, userID, stepAt(base), []string{}, []bool{})
		factorsAddKey(t, db, userID, base.Add(10*time.Minute), nil)
		f, e := InlineMFAFactors(ctx, db, userID)
		require.Nil(t, e)
		require.Equal(t, MethodWebAuthn, f.Default())
	})

	// Backup availability against the real verifier lives in
	// factors_backup_verifier_test.go: mfa imports stepup.

	t.Run("a missing users row is subjectGone, never an empty set", func(t *testing.T) {
		f, e := InlineMFAFactors(ctx, db, uuid.New().String())
		require.NotNil(t, e)
		require.Equal(t, http.StatusUnauthorized, e.Status)
		require.Equal(t, ErrMsgSessionNoLongerValid, e.Body["error"])
		require.Nil(t, e.Cause)
		require.Nil(t, f.Methods)
	})

	t.Run("a read failure is a 500 with a cause", func(t *testing.T) {
		userID := subjectTestUser(t, db)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, e := InlineMFAFactors(canceled, db, userID)
		require.NotNil(t, e)
		require.Equal(t, http.StatusInternalServerError, e.Status)
		require.Error(t, e.Cause)
	})
}

// TestP1Equivalence holds the four P1 readers to one answer. loadSubjectSQL
// keeps its own copy of the predicate (keyed on u.id), so this is what stops
// it drifting from the shared fragments.
func TestP1Equivalence(t *testing.T) {
	db := subjectTestDB(t)
	ctx := context.Background()

	fixtures := []struct {
		name  string
		setup func(t *testing.T, userID string)
	}{
		{"no factors", func(*testing.T, string) {}},
		{"pending TOTP", func(t *testing.T, id string) { subjectTestTOTP(t, db, id, true, false) }},
		{"disabled confirmed TOTP", func(t *testing.T, id string) { subjectTestTOTP(t, db, id, false, true) }},
		{"confirmed TOTP", func(t *testing.T, id string) { subjectTestTOTP(t, db, id, true, true) }},
		{"one key", func(t *testing.T, id string) { subjectTestWebAuthn(t, db, id) }},
		{"several keys", func(t *testing.T, id string) {
			factorsAddKey(t, db, id, time.Now(), nil)
			factorsAddKey(t, db, id, time.Now(), nil)
		}},
		{"both factors", func(t *testing.T, id string) {
			subjectTestTOTP(t, db, id, true, true)
			subjectTestWebAuthn(t, db, id)
		}},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			userID := subjectTestUser(t, db)
			fx.setup(t, userID)

			f, fe := InlineMFAFactors(ctx, db, userID)
			require.Nil(t, fe)
			m, err := InlineMFAMethods(ctx, db, userID)
			require.NoError(t, err)
			s, se := LoadSubject(ctx, db, userID)
			require.Nil(t, se)
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			ls, le := LockSubjectTx(ctx, tx, userID, LockForShare, "")
			require.NoError(t, tx.Rollback())
			require.Nil(t, le)

			require.Equal(t, m, f.Methods, "InlineMFAFactors")
			require.Equal(t, m, s.MFAMethods, "LoadSubject")
			require.Equal(t, m, ls.MFAMethods, "LockSubjectTx")
		})
	}

	t.Run("no users row", func(t *testing.T) {
		gone := uuid.New().String()
		_, fe := InlineMFAFactors(ctx, db, gone)
		require.NotNil(t, fe)
		require.Equal(t, http.StatusUnauthorized, fe.Status)
		_, se := LoadSubject(ctx, db, gone)
		require.NotNil(t, se)
		require.Equal(t, http.StatusUnauthorized, se.Status)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, le := LockSubjectTx(ctx, tx, gone, LockForShare, "")
		require.NoError(t, tx.Rollback())
		require.NotNil(t, le)
		require.Equal(t, http.StatusUnauthorized, le.Status)

		// InlineMFAMethods answers any uuid (it has no FROM clause). That is
		// today's authorization-critical behaviour and this change keeps it.
		m, err := InlineMFAMethods(ctx, db, gone)
		require.NoError(t, err)
		require.NotNil(t, m)
		require.Empty(t, m)
	})
}
