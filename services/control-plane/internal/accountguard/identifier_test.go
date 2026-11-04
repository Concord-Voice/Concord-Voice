package accountguard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// katUserID is a fixed account id, so the HMAC known answers below can be computed
// out-of-band (HKDF-SHA256 of the ring input with the identifier label's info string,
// then HMAC-SHA256 over the subject bytes) with an independent implementation.
var katUserID = uuid.MustParse("3a1f0c52-7b44-4e0a-9d21-5c6e8f1a2b03")

const (
	// HMAC-SHA256 under the identifier-label derivation of ringInputAlpha.
	katAcctHex       = "20f543fd27d1dd71a5a781307e8f20fbd99f2d93e9373a2f6a09d5f1b40ad514" // "acct" + katUserID bytes // pragma: allowlist secret
	katIdentGhostHex = "38aa526c1425534ace0ab6263dc75f9edf89215a58790f6a66e19732219c6743" // "ident" + "ghost-user" // pragma: allowlist secret
	katIdentEmptyHex = "c7330bf695409de3af13b5c1ec78707b36832945c8033e714fb90ddd066fa8f9" // "ident" + "" // pragma: allowlist secret
)

var errInjected = errors.New("injected query failure")

type queryCall struct {
	query string
	args  []any
}

// countingQuerier wraps a real pool, records every query, and can fail the Nth one
// (1-based) with errInjected before it reaches the database.
type countingQuerier struct {
	inner  RowQuerier
	mu     sync.Mutex
	calls  []queryCall
	failAt int

	// cancelBefore, when set, cancels the caller's context as the Nth query (1-based)
	// begins: the arms before it have returned, the Nth has not yet reached the database.
	cancelBefore int
	cancel       context.CancelFunc
}

func (c *countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.mu.Lock()
	c.calls = append(c.calls, queryCall{query: query, args: args})
	n := len(c.calls)
	c.mu.Unlock()
	if c.cancelBefore == n && c.cancel != nil {
		c.cancel()
	}
	if c.failAt == n {
		return nil, errInjected
	}
	return c.inner.QueryContext(ctx, query, args...)
}

func (c *countingQuerier) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

func (c *countingQuerier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

type resolveEnv struct {
	db   *sql.DB // fixtures
	pool *sql.DB // what the resolver queries through, so leaked rows show in its stats
	q    *countingQuerier
	ring *KeyRing
	now  time.Time
}

func newResolveEnv(t *testing.T) *resolveEnv {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	pool := extraPool(t)
	return &resolveEnv{
		db:   db,
		pool: pool,
		q:    &countingQuerier{inner: pool},
		ring: NewKeyRing(nil, ringInputAlpha), // Key never touches the database
		// Microsecond-aligned, as timestamptz is, so boundary cases compare exactly.
		now: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func (e *resolveEnv) resolve(raw string) (Resolution, error) {
	return ResolveIdentifier(context.Background(), e.q, e.ring, raw, e.now)
}

// mustResolve calls ResolveIdentifier, requires success, and requires that no rows
// handle outlived the call.
func (e *resolveEnv) mustResolve(t *testing.T, raw string) Resolution {
	t.Helper()
	res, err := e.resolve(raw)
	require.NoError(t, err)
	assertNoOpenRows(t, e.pool)
	return res
}

// assertNoOpenRows fails when the pool still has a connection checked out, which is
// what an unclosed *sql.Rows leaves behind.
func assertNoOpenRows(t *testing.T, pool *sql.DB) {
	t.Helper()
	assert.Zero(t, pool.Stats().InUse, "every rows result must be closed before the call returns")
}

func insertUser(t *testing.T, db *sql.DB, id uuid.UUID, username, email string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO users (id, email, username, password_hash, age_verified, email_verified)
		 VALUES ($1, $2, $3, 'x', true, true)`,
		id, email, username,
	)
	require.NoError(t, err)
}

// newUser inserts an account with a fresh id and returns the id.
func newUser(t *testing.T, db *sql.DB, username, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	insertUser(t, db, id, username, email)
	return id
}

// addHistory inserts an address history row. address must already be lower(btrim()).
func addHistory(t *testing.T, db *sql.DB, userID uuid.UUID, address string, retiredAt time.Time, wasPrimary bool) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO user_credential_history (user_id, kind, address, retired_at, was_mature, was_primary, was_verified)
		 VALUES ($1, 'address', $2, $3, false, $4, true)`,
		userID, address, retiredAt, wasPrimary,
	)
	require.NoError(t, err)
}

// identifierSubject is the expected HMAC for message = label ‖ body, computed from the
// ring input through the already-tested derivation.
func identifierSubject(t *testing.T, label string, body []byte) [32]byte {
	t.Helper()
	k, err := DeriveKey(ringInputAlpha, LabelIdentifier)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, k)
	mac.Write([]byte(label))
	mac.Write(body)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

func acctSubject(t *testing.T, id uuid.UUID) [32]byte {
	t.Helper()
	return identifierSubject(t, "acct", id[:])
}

func identSubject(t *testing.T, norm string) [32]byte {
	t.Helper()
	return identifierSubject(t, "ident", []byte(norm))
}

func hexTo32(t *testing.T, s string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	var out [32]byte
	copy(out[:], raw)
	return out
}

// assertThreeArms requires exactly the three lookups, in the fixed order.
func assertThreeArms(t *testing.T, q *countingQuerier, label string) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	require.Len(t, q.calls, 3, "%s: all three lookups must run", label)
	arm1, arm2, arm3 := strings.ToLower(q.calls[0].query), strings.ToLower(q.calls[1].query), strings.ToLower(q.calls[2].query)
	assert.Contains(t, arm1, "username", label)
	assert.NotContains(t, arm1, "user_credential_history", label)
	assert.Contains(t, arm2, "email", label)
	assert.NotContains(t, arm2, "user_credential_history", label)
	assert.Contains(t, arm3, "user_credential_history", label)
	for i, arm := range []string{arm1, arm2, arm3} {
		assert.Contains(t, arm, "limit 2", "%s: arm %d must read at most two rows", label, i+1)
	}
}

// ---------------------------------------------------------------------------
// Arm behaviour and precedence
// ---------------------------------------------------------------------------

func TestResolve_UsernameWinsOverEmailArm(t *testing.T) {
	env := newResolveEnv(t)
	byName := newUser(t, env.db, "dual-name", "owner-a@example.test")
	byEmail := newUser(t, env.db, "someone-else", "dual-name") // this email equals A's username
	other := newUser(t, env.db, "history-owner", "owner-c@example.test")
	addHistory(t, env.db, other, "dual-name", env.now.Add(-time.Hour), true) // arm 3 matches a third account too

	res := env.mustResolve(t, "dual-name")

	assert.True(t, res.Resolved)
	assert.Equal(t, byName, res.UserID, "the username arm wins")
	assert.NotEqual(t, byEmail, res.UserID)
	assert.Equal(t, acctSubject(t, byName), res.SubjectHMAC)
	assertThreeArms(t, env.q, "all three arms match different accounts")
}

func TestResolve_EmailArmWinsOverHistoryArm(t *testing.T) {
	env := newResolveEnv(t)
	current := newUser(t, env.db, "current-holder", "shared@example.test")
	former := newUser(t, env.db, "former-holder", "former@example.test")
	addHistory(t, env.db, former, "shared@example.test", env.now.Add(-time.Hour), true)

	res := env.mustResolve(t, "shared@example.test")

	assert.True(t, res.Resolved)
	assert.Equal(t, current, res.UserID, "a current primary beats a retired one")
	assertThreeArms(t, env.q, "email beats history")
}

func TestResolve_CurrentPrimary(t *testing.T) {
	env := newResolveEnv(t)
	id := newUser(t, env.db, "alice", "alice@example.test")
	newUser(t, env.db, "bob", "bob@example.test")

	res := env.mustResolve(t, "alice@example.test")

	assert.True(t, res.Resolved)
	assert.Equal(t, id, res.UserID)
	assert.Equal(t, acctSubject(t, id), res.SubjectHMAC)
	assertThreeArms(t, env.q, "current primary")
}

func TestResolve_MixedCaseAndPaddedInputResolves(t *testing.T) {
	env := newResolveEnv(t)
	byName := newUser(t, env.db, "MixedName", "Mixed.Mail@Example.Test")
	byHistory := newUser(t, env.db, "history-user", "history-user@example.test")
	addHistory(t, env.db, byHistory, "old.address@example.test", env.now.Add(-24*time.Hour), true)

	cases := []struct {
		name string
		raw  string
		want uuid.UUID
	}{
		{"username, lower input over mixed-case storage", "mixedname", byName},
		{"username, upper input with padding", "  MIXEDNAME \t", byName},
		{"email, mixed case and padded", "\n mixed.MAIL@example.TEST  ", byName},
		{"retired address, upper case and padded", "  OLD.Address@EXAMPLE.test ", byHistory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := env.mustResolve(t, tc.raw)
			assert.True(t, res.Resolved)
			assert.Equal(t, tc.want, res.UserID)
		})
	}
}

// ---------------------------------------------------------------------------
// History arm
// ---------------------------------------------------------------------------

func TestResolve_RetiredPrimaryWithin30d(t *testing.T) {
	env := newResolveEnv(t)
	id := newUser(t, env.db, "mover", "new-home@example.test")
	addHistory(t, env.db, id, "old-home@example.test", env.now.Add(-10*24*time.Hour), true)

	res := env.mustResolve(t, "old-home@example.test")

	assert.True(t, res.Resolved)
	assert.Equal(t, id, res.UserID)
	assert.Equal(t, acctSubject(t, id), res.SubjectHMAC)
	assertThreeArms(t, env.q, "retired primary")
}

func TestResolve_TwoRetiredRowsOfOneAccountResolve(t *testing.T) {
	env := newResolveEnv(t)
	id := newUser(t, env.db, "returner", "now@example.test")
	addHistory(t, env.db, id, "twice@example.test", env.now.Add(-5*24*time.Hour), true)
	addHistory(t, env.db, id, "twice@example.test", env.now.Add(-2*24*time.Hour), true)

	res := env.mustResolve(t, "twice@example.test")

	assert.True(t, res.Resolved, "one account is one account, however many rows name it")
	assert.Equal(t, id, res.UserID)
}

func TestResolve_RetiredFromTwoAccountsDoesNotResolve(t *testing.T) {
	env := newResolveEnv(t)
	first := newUser(t, env.db, "first-owner", "first@example.test")
	second := newUser(t, env.db, "second-owner", "second@example.test")
	addHistory(t, env.db, first, "handed-down@example.test", env.now.Add(-20*24*time.Hour), true)
	addHistory(t, env.db, second, "handed-down@example.test", env.now.Add(-3*24*time.Hour), true)

	res := env.mustResolve(t, "handed-down@example.test")

	assert.False(t, res.Resolved, "an address retired from two accounts names neither")
	assert.Equal(t, uuid.Nil, res.UserID)
	assert.Equal(t, identSubject(t, "handed-down@example.test"), res.SubjectHMAC)
	assertThreeArms(t, env.q, "ambiguous history")
}

func TestResolve_Retired31dAgoDoesNotResolve(t *testing.T) {
	env := newResolveEnv(t)
	id := newUser(t, env.db, "stale-owner", "current-stale@example.test")
	addHistory(t, env.db, id, "too-old@example.test", env.now.Add(-31*24*time.Hour), true)

	res := env.mustResolve(t, "too-old@example.test")

	assert.False(t, res.Resolved)
	assert.Equal(t, uuid.Nil, res.UserID)
	assert.Equal(t, identSubject(t, "too-old@example.test"), res.SubjectHMAC)
}

func TestResolve_RetentionWindowBoundary(t *testing.T) {
	env := newResolveEnv(t)
	edge := newUser(t, env.db, "edge-owner", "edge-current@example.test")
	inside := newUser(t, env.db, "inside-owner", "inside-current@example.test")
	outside := newUser(t, env.db, "outside-owner", "outside-current@example.test")
	boundary := env.now.Add(-decide.HistoryRetention)
	addHistory(t, env.db, edge, "at-edge@example.test", boundary, true)
	addHistory(t, env.db, inside, "one-ms-inside@example.test", boundary.Add(time.Millisecond), true)
	addHistory(t, env.db, outside, "one-ms-outside@example.test", boundary.Add(-time.Millisecond), true)

	t.Run("retired exactly at the boundary does not resolve", func(t *testing.T) {
		res := env.mustResolve(t, "at-edge@example.test")
		assert.False(t, res.Resolved, "the window is retired_at > now - retention")
	})
	t.Run("retired one millisecond inside resolves", func(t *testing.T) {
		res := env.mustResolve(t, "one-ms-inside@example.test")
		assert.True(t, res.Resolved)
		assert.Equal(t, inside, res.UserID)
	})
	t.Run("retired one millisecond outside does not resolve", func(t *testing.T) {
		res := env.mustResolve(t, "one-ms-outside@example.test")
		assert.False(t, res.Resolved)
	})
}

func TestResolve_NonPrimaryHistoryIgnored(t *testing.T) {
	env := newResolveEnv(t)
	id := newUser(t, env.db, "backup-keeper", "main@example.test")
	addHistory(t, env.db, id, "was-backup@example.test", env.now.Add(-time.Hour), false)

	res := env.mustResolve(t, "was-backup@example.test")

	assert.False(t, res.Resolved, "a retired backup address never names the account")
	assert.Equal(t, identSubject(t, "was-backup@example.test"), res.SubjectHMAC)
}

func TestResolve_CurrentBackupAddressNeverResolves(t *testing.T) {
	env := newResolveEnv(t)
	id := newUser(t, env.db, "has-backup", "primary@example.test")
	_, err := env.db.Exec(`UPDATE users SET backup_email = $1 WHERE id = $2`, "backup@example.test", id)
	require.NoError(t, err)

	res := env.mustResolve(t, "backup@example.test")

	assert.False(t, res.Resolved, "no arm reads the backup address")
	assertThreeArms(t, env.q, "backup address")
}

// ---------------------------------------------------------------------------
// Ambiguity fails closed
// ---------------------------------------------------------------------------

// users.email is UNIQUE on the raw value, so differently-cased spellings coexist and
// LOWER(email) can match several accounts. More than one match resolves nothing.
func TestResolve_EmailsDifferingOnlyInCaseDoNotResolve(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "upper-owner", "Foo@example.test")
	newUser(t, env.db, "lower-owner", "foo@example.test")

	for _, raw := range []string{"foo@example.test", "Foo@Example.Test", "  FOO@EXAMPLE.TEST"} {
		t.Run(raw, func(t *testing.T) {
			env.q.mu.Lock()
			env.q.calls = nil
			env.q.mu.Unlock()

			res := env.mustResolve(t, raw)

			assert.False(t, res.Resolved)
			assert.Equal(t, uuid.Nil, res.UserID)
			assert.Equal(t, identSubject(t, strings.ToLower(strings.TrimSpace(raw))), res.SubjectHMAC)
			assertThreeArms(t, env.q, raw)
		})
	}
}

func TestResolve_ThreeEmailSpellingsDoNotResolve(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "owner-one", "Trio@example.test")
	newUser(t, env.db, "owner-two", "trio@example.test")
	newUser(t, env.db, "owner-three", "TRIO@example.test")

	res := env.mustResolve(t, "trio@example.test")

	assert.False(t, res.Resolved, "three matches resolve nothing, as two do")
}

// Ambiguity anywhere voids the call, even when an earlier arm found exactly one
// account: the username arm names A, the email arm matches two others.
func TestResolve_AmbiguousLaterArmVoidsEarlierResolution(t *testing.T) {
	env := newResolveEnv(t)
	byName := newUser(t, env.db, "dual", "owner-of-dual@example.test")
	newUser(t, env.db, "email-holder-upper", "Dual")
	newUser(t, env.db, "email-holder-lower", "dual")

	res := env.mustResolve(t, "dual")

	assert.False(t, res.Resolved, "an ambiguous later arm voids an earlier arm's single match")
	assert.NotEqual(t, byName, res.UserID)
	assert.Equal(t, uuid.Nil, res.UserID)
	assert.Equal(t, identSubject(t, "dual"), res.SubjectHMAC)
	assertThreeArms(t, env.q, "username match, ambiguous email")
}

// An address held by two current accounts is in use: handing the identifier to a
// former holder through the history arm would name the wrong owner. The history arm
// still runs, so the work stays equal, but it cannot resolve.
func TestResolve_AmbiguousEmailDoesNotFallThroughToHistory(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "upper-holder", "Foo@example.test")
	newUser(t, env.db, "lower-holder", "foo@example.test")
	former := newUser(t, env.db, "former-holder", "former-now@example.test")
	addHistory(t, env.db, former, "foo@example.test", env.now.Add(-time.Hour), true)

	res := env.mustResolve(t, "foo@example.test")

	assert.False(t, res.Resolved, "an ambiguous arm makes the whole call unresolved")
	assert.NotEqual(t, former, res.UserID)
	assert.Equal(t, uuid.Nil, res.UserID)
	assert.Equal(t, identSubject(t, "foo@example.test"), res.SubjectHMAC)
	assertThreeArms(t, env.q, "ambiguous email then history")
}

// ---------------------------------------------------------------------------
// Equal work
// ---------------------------------------------------------------------------

func TestResolve_AllArmsRunForUnknown(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "present", "present@example.test")

	cases := []struct {
		name string
		raw  string
	}{
		{"resolving by username", "present"},
		{"resolving by email", "present@example.test"},
		{"not resolving", "nobody-by-that-name"},
		{"empty", ""},
		{"whitespace only", " \t\r\n "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.q.mu.Lock()
			env.q.calls = nil
			env.q.mu.Unlock()

			_, err := env.resolve(tc.raw)

			require.NoError(t, err)
			assertThreeArms(t, env.q, tc.name)
			assertNoOpenRows(t, env.pool)
		})
	}
}

func TestResolve_EmptyAndWhitespaceInputResolveNothing(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "present", "present@example.test")
	want := identSubject(t, "")

	for _, raw := range []string{"", "   ", "\t\n \r"} {
		t.Run("input of length "+strconv.Itoa(len(raw)), func(t *testing.T) {
			env.q.mu.Lock()
			env.q.calls = nil
			env.q.mu.Unlock()

			res := env.mustResolve(t, raw)

			assert.False(t, res.Resolved)
			assert.Equal(t, uuid.Nil, res.UserID)
			assert.Equal(t, want, res.SubjectHMAC, "the unresolved subject of nothing is the keyed digest of the empty string")
			assertThreeArms(t, env.q, "empty input")
		})
	}
}

// ---------------------------------------------------------------------------
// Subjects
// ---------------------------------------------------------------------------

func TestResolve_SubjectHMACStableAndDomainSeparated(t *testing.T) {
	env := newResolveEnv(t)
	id := newUser(t, env.db, "subject-user", "subject@example.test")

	byName := env.mustResolve(t, "subject-user")
	byEmail := env.mustResolve(t, "SUBJECT@example.test")
	again := env.mustResolve(t, "subject-user")

	require.True(t, byName.Resolved)
	require.True(t, byEmail.Resolved)
	assert.Equal(t, byName.SubjectHMAC, byEmail.SubjectHMAC, "one account, one subject, whichever identifier named it")
	assert.Equal(t, byName.SubjectHMAC, again.SubjectHMAC, "stable across calls")
	assert.Equal(t, acctSubject(t, id), byName.SubjectHMAC)

	// An unknown identifier spelled exactly like the account's id must not collide
	// with the account's own subject.
	spelled := env.mustResolve(t, id.String())
	assert.False(t, spelled.Resolved)
	assert.NotEqual(t, byName.SubjectHMAC, spelled.SubjectHMAC)
	assert.Equal(t, identSubject(t, id.String()), spelled.SubjectHMAC)

	// The two forms are separated by their labels, not merely by their bodies.
	assert.NotEqual(t, identSubject(t, string(id[:])), acctSubject(t, id))
}

func TestResolve_UnresolvedSubjectGroupsByNormalizedIdentifier(t *testing.T) {
	env := newResolveEnv(t)

	a := env.mustResolve(t, "Ghost-User")
	b := env.mustResolve(t, "  ghost-user\t")
	c := env.mustResolve(t, "someone-else")

	assert.False(t, a.Resolved)
	assert.Equal(t, a.SubjectHMAC, b.SubjectHMAC, "case and padding do not split an unknown identifier's budget")
	assert.NotEqual(t, a.SubjectHMAC, c.SubjectHMAC, "different identifiers get different subjects")
	assert.NotEqual(t, [32]byte{}, a.SubjectHMAC, "a zero digest never escapes a successful call")
	assert.NotEqual(t, [32]byte{}, c.SubjectHMAC)
}

func TestResolve_SubjectHMACKnownAnswers(t *testing.T) {
	env := newResolveEnv(t)
	insertUser(t, env.db, katUserID, "kat-user", "kat@example.test")

	t.Run("resolved", func(t *testing.T) {
		res := env.mustResolve(t, "kat-user")
		require.True(t, res.Resolved)
		assert.Equal(t, hexTo32(t, katAcctHex), res.SubjectHMAC)
	})
	t.Run("unresolved", func(t *testing.T) {
		res := env.mustResolve(t, "  GHOST-user ")
		assert.Equal(t, hexTo32(t, katIdentGhostHex), res.SubjectHMAC)
	})
	t.Run("empty", func(t *testing.T) {
		res := env.mustResolve(t, "")
		assert.Equal(t, hexTo32(t, katIdentEmptyHex), res.SubjectHMAC)
	})
}

func TestResolve_SubjectDependsOnTheRing(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "ring-user", "ring@example.test")

	first := env.mustResolve(t, "ring-user")
	env.ring = NewKeyRing(nil, ringInputBeta)
	second := env.mustResolve(t, "ring-user")

	require.True(t, first.Resolved)
	require.True(t, second.Resolved)
	assert.NotEqual(t, first.SubjectHMAC, second.SubjectHMAC, "the digest is keyed, not a bare hash")
}

// ---------------------------------------------------------------------------
// Failure paths
// ---------------------------------------------------------------------------

func TestResolve_DownRingRunsNoQuery(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "present", "present@example.test")

	rings := map[string]*KeyRing{
		"nil ring":         nil,
		"zero ring":        {},
		"empty input ring": NewKeyRing(nil, ""),
		"ring with a pool": NewKeyRing(env.db, ""),
	}
	for name, ring := range rings {
		t.Run(name, func(t *testing.T) {
			env.q.mu.Lock()
			env.q.calls = nil
			env.q.mu.Unlock()

			res, err := ResolveIdentifier(context.Background(), env.q, ring, "present", env.now)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKeyUnavailable)
			assert.Equal(t, Resolution{}, res, "no partial result on failure")
			assert.Zero(t, env.q.count(), "the ring is resolved before any query")
		})
	}
}

func TestResolve_QueryErrorReturnsErrorAndNoResolution(t *testing.T) {
	for arm := 1; arm <= 3; arm++ {
		t.Run("arm "+strconv.Itoa(arm), func(t *testing.T) {
			env := newResolveEnv(t)
			newUser(t, env.db, "present", "present@example.test")
			env.q.failAt = arm

			res, err := env.resolve("present")

			require.Error(t, err)
			assert.ErrorIs(t, err, errInjected, "the cause stays reachable through the chain")
			assert.NotContains(t, err.Error(), errInjected.Error(), "RT-1: the driver text never reaches Error()")
			assert.Equal(t, Resolution{}, res, "an error never carries a resolution, even one an earlier arm found")
			assertNoOpenRows(t, env.pool)
		})
	}
}

func TestResolve_CancelledContextReturnsError(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "present", "present@example.test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := ResolveIdentifier(ctx, env.q, env.ring, "present", env.now)

	require.Error(t, err)
	assert.Equal(t, Resolution{}, res)
	assertNoOpenRows(t, env.pool)
}

func TestResolve_ClosesEveryRowsResult(t *testing.T) {
	env := newResolveEnv(t)
	byName := newUser(t, env.db, "closer", "closer@example.test")
	addHistory(t, env.db, byName, "closer-old@example.test", env.now.Add(-time.Hour), true)
	newUser(t, env.db, "Dup", "Dup@example.test")
	newUser(t, env.db, "dup2", "dup@example.test")

	for _, raw := range []string{
		"closer",                  // arm 1 resolves
		"closer@example.test",     // arm 2 resolves
		"closer-old@example.test", // arm 3 resolves
		"dup@example.test",        // arm 2 sees two rows and stops reading at the limit
		"nobody",                  // nothing matches
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := env.resolve(raw)
			require.NoError(t, err)
			assertNoOpenRows(t, env.pool)
		})
	}
}

// ---------------------------------------------------------------------------
// Arguments, malformed input, empty values, cancellation, non-ASCII
// ---------------------------------------------------------------------------

// maxIdentifierBytes is the longest trimmed identifier that reaches the driver
// unchanged (an email is at most 254 bytes; a username is shorter).
const maxIdentifierBytes = 320

// assertArmArgs requires that the lookups received the trimmed input as $1 and, in the
// history arm, the start of the retention window as $2.
func assertArmArgs(t *testing.T, q *countingQuerier, wantFirst string, now time.Time, label string) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	require.Len(t, q.calls, 3, label)
	for i := range 2 {
		require.Len(t, q.calls[i].args, 1, "%s: arm %d takes only the identifier", label, i+1)
		assert.Equal(t, wantFirst, q.calls[i].args[0], "%s: arm %d $1", label, i+1)
	}
	require.Len(t, q.calls[2].args, 2, "%s: the history arm takes the identifier and the window start", label)
	assert.Equal(t, wantFirst, q.calls[2].args[0], "%s: history arm $1", label)
	got, ok := q.calls[2].args[1].(time.Time)
	require.True(t, ok, "%s: $2 must be a time.Time", label)
	assert.True(t, now.Add(-decide.HistoryRetention).Equal(got), "%s: $2 is now minus the retention", label)
}

func TestResolve_ArmsReceiveTrimmedInputAndWindowStart(t *testing.T) {
	env := newResolveEnv(t)
	cases := []struct {
		name, raw, want string
	}{
		{"padded mixed case is trimmed, not lowercased", "  Mixed.Case@Example.Test\t", "Mixed.Case@Example.Test"},
		{"unpadded passes through", "plain-name", "plain-name"},
		{"whitespace only trims to empty", " \t\r\n ", ""},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.q.reset()
			env.mustResolve(t, tc.raw)
			assertThreeArms(t, env.q, tc.name)
			assertArmArgs(t, env.q, tc.want, env.now, tc.name)
		})
	}
}

// Malformed or oversized input never reaches the driver (the driver's rejection message
// echoes the bytes), but the three lookups still run, with an empty $1.
func TestResolve_MalformedOrOversizedInputIsSubstitutedNotSent(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "present", "present@example.test")

	cases := []struct {
		name string
		raw  string
	}{
		{"invalid UTF-8", "\xff"},
		{"invalid UTF-8 padded", "  \xfe\xff "},
		{"NUL byte", "a\x00b"},
		{"321 bytes", strings.Repeat("a", maxIdentifierBytes+1)},
		{"321 bytes padded", "  " + strings.Repeat("a", maxIdentifierBytes+1) + "\t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.q.reset()

			res, err := env.resolve(tc.raw)

			require.NoError(t, err, "no error, so no input bytes can ride in one")
			assertNoOpenRows(t, env.pool)
			assert.False(t, res.Resolved)
			assert.Equal(t, uuid.Nil, res.UserID)
			assert.Equal(t, identSubject(t, strings.ToLower(strings.TrimSpace(tc.raw))), res.SubjectHMAC)
			assertThreeArms(t, env.q, tc.name)
			assertArmArgs(t, env.q, "", env.now, tc.name)
		})
	}

	t.Run("exactly 320 bytes is not substituted", func(t *testing.T) {
		env.q.reset()
		long := strings.Repeat("a", maxIdentifierBytes)

		res := env.mustResolve(t, " "+long+" ")

		assert.False(t, res.Resolved)
		assert.Equal(t, identSubject(t, long), res.SubjectHMAC)
		assertThreeArms(t, env.q, "320 bytes")
		assertArmArgs(t, env.q, long, env.now, "320 bytes")
	})
}

// A substituted value is empty, and an empty identifier never resolves, even when an
// account holds an empty username, email or history address (the schema permits them).
func TestResolve_EmptyValuesInTheDatabaseNeverResolve(t *testing.T) {
	env := newResolveEnv(t)
	newUser(t, env.db, "", "empty-username@example.test")
	newUser(t, env.db, "empty-email", "")
	historyOwner := newUser(t, env.db, "empty-history", "empty-history@example.test")
	addHistory(t, env.db, historyOwner, "", env.now.Add(-time.Hour), true)

	for _, raw := range []string{"", "   ", "\t\r\n", "\xff", "a\x00b", strings.Repeat("a", maxIdentifierBytes+1)} {
		t.Run("input of "+strconv.Itoa(len(raw))+" bytes", func(t *testing.T) {
			env.q.reset()

			res := env.mustResolve(t, raw)

			assert.False(t, res.Resolved, "an empty value in the database is not an account's name")
			assert.Equal(t, uuid.Nil, res.UserID)
			assert.Equal(t, identSubject(t, strings.ToLower(strings.TrimSpace(raw))), res.SubjectHMAC)
			assertThreeArms(t, env.q, "empty values present")
		})
	}
}

// A context cancelled after an arm returned stops the call with an error, not with
// whatever the earlier arms found.
func TestResolve_MidArmCancellationReturnsErrorAndNoResolution(t *testing.T) {
	for _, arm := range []int{2, 3} {
		t.Run("cancelled as arm "+strconv.Itoa(arm)+" begins", func(t *testing.T) {
			env := newResolveEnv(t)
			newUser(t, env.db, "present", "present@example.test")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env.q.cancelBefore, env.q.cancel = arm, cancel

			res, err := ResolveIdentifier(ctx, env.q, env.ring, "present", env.now)

			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, Resolution{}, res, "arm 1 found the account, yet an error carries no resolution")
			assertNoOpenRows(t, env.pool)
		})
	}
}

// The comparison lowercases in SQL on both sides, so a stored title-case letter that Go
// and the database fold differently still resolves by the same spelling. Lowercasing in
// Go before the query would compare a Go-folded input with a database-folded column.
func TestResolve_NonASCIIStoredValueResolvesBySameSpelling(t *testing.T) {
	env := newResolveEnv(t)
	const titleCase = "\u01c5" // U+01C5, a letter whose lower case differs from itself
	byName := newUser(t, env.db, titleCase+"name", "plain-owner@example.test")
	byEmail := newUser(t, env.db, "plain-name", titleCase+"mail@example.test")

	t.Run("username", func(t *testing.T) {
		res := env.mustResolve(t, titleCase+"name")
		assert.True(t, res.Resolved)
		assert.Equal(t, byName, res.UserID)
	})
	t.Run("email", func(t *testing.T) {
		res := env.mustResolve(t, "  "+titleCase+"mail@example.test ")
		assert.True(t, res.Resolved)
		assert.Equal(t, byEmail, res.UserID)
	})
}
