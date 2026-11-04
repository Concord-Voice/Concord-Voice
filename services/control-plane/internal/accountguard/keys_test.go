package accountguard

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic, obviously fake derivation inputs. They never leave the test DB.
const (
	ringInputAlpha = "synthetic-ring-input-alpha-0001"
	ringInputBeta  = "synthetic-ring-input-beta-0002"
)

// allLabels pairs every label with the info string the design pins. Both columns
// are literals on purpose: deriving them from the code under test would make the
// pin vacuous.
var allLabels = []struct {
	name  string
	label Label
	info  string
}{
	{"identifier", LabelIdentifier, "concord/accountguard/identifier/v1"},
	{"fingerprint", LabelFingerprint, "concord/accountguard/fingerprint/v1"},
	{"guard-token", LabelGuardToken, "concord/accountguard/guard-token/v1"},
	{"challenge", LabelChallenge, "concord/accountguard/challenge/v1"},
	{"address", LabelAddress, "concord/accountguard/address/v1"},
	{"job", LabelJob, "concord/accountguard/job/v1"},
	{"key-check", LabelKeyCheck, "concord/accountguard/key-check/v1"},
}

// alphaKnownAnswers holds the expected HKDF-SHA256 outputs (salt = nil, 32 bytes, hex)
// for ringInputAlpha, computed out-of-band with an independent HMAC implementation.
// The order is allLabels' order: identifier, fingerprint, guard-token, challenge,
// address, job, key-check.
var alphaKnownAnswers = []string{
	"eb36fb51d5c0d4fb5eb2e576303d14453253c3ef2fa1abc7911f02a440ec476a", // pragma: allowlist secret
	"2b628249b5bdaae7da65df45f84da49e6fb02e77c86d488ceb25e8c913bbe5a6", // pragma: allowlist secret
	"f8c950c0fb0ef99b15a14eb3427f7fe16e3bd562534f77298c0059d460f74e8e", // pragma: allowlist secret
	"119e27efcf0a4f7f921be905a1093368b6753900977866bb64c9d0a17b4e4a0f", // pragma: allowlist secret
	"262410a2338b3fb6f95cda6c7cef921a4b0e66787f2d723d0dae461dab82b196", // pragma: allowlist secret
	"90ce633646f48af0899908eccddfea0f9b17387da43fcbe9a3e0bc9c52862b0f", // pragma: allowlist secret
	"91b3faecad7e61ac435aa72755a0e7b73d36d6632dfb23bd5c1a1980ff5af482", // pragma: allowlist secret
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

func TestLabel_InfoStringsPinned(t *testing.T) {
	for _, tc := range allLabels {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.info, tc.label.info())
		})
	}
}

func TestLabel_ConstantsAreDistinct(t *testing.T) {
	seen := map[Label]string{}
	for _, tc := range allLabels {
		if prior, dup := seen[tc.label]; dup {
			t.Errorf("labels %q and %q share the value %d", prior, tc.name, tc.label)
		}
		seen[tc.label] = tc.name
	}
	assert.Len(t, seen, 7)
}

// The label set is closed: of every possible Label value, exactly the seven named
// constants carry an info string. A new label must be added deliberately here.
func TestLabel_SetIsClosed(t *testing.T) {
	named := map[string]bool{}
	for _, tc := range allLabels {
		named[tc.info] = true
	}
	var withInfo []string
	for v := 0; v <= 255; v++ {
		if info := Label(v).info(); info != "" {
			withInfo = append(withInfo, info)
			assert.True(t, named[info], "Label(%d) carries the unpinned info string %q", v, info)
		}
	}
	assert.Len(t, withInfo, 7, "exactly the seven pinned labels may carry an info string")
}

// ---------------------------------------------------------------------------
// DeriveKey
// ---------------------------------------------------------------------------

func TestDeriveKey_LabelsSeparate(t *testing.T) {
	keys := map[string]string{}
	for _, tc := range allLabels {
		k, err := DeriveKey(ringInputAlpha, tc.label)
		require.NoError(t, err, tc.name)
		require.Len(t, k, 32, tc.name)
		if prior, dup := keys[string(k)]; dup {
			t.Errorf("labels %q and %q derived the same key", prior, tc.name)
		}
		keys[string(k)] = tc.name
	}
	assert.Len(t, keys, 7)
}

func TestDeriveKey_MatchesHKDFSHA256WithNilSalt(t *testing.T) {
	require.Len(t, alphaKnownAnswers, len(allLabels))
	for i, tc := range allLabels {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveKey(ringInputAlpha, tc.label)
			require.NoError(t, err)

			want, err := hkdf.Key(sha256.New, []byte(ringInputAlpha), nil, tc.info, 32)
			require.NoError(t, err)
			assert.Equal(t, want, got)

			assert.Equal(t, alphaKnownAnswers[i], hex.EncodeToString(got))
		})
	}
}

func TestDeriveKey_DeterministicAndInputSensitive(t *testing.T) {
	a1, err := DeriveKey(ringInputAlpha, LabelIdentifier)
	require.NoError(t, err)
	a2, err := DeriveKey(ringInputAlpha, LabelIdentifier)
	require.NoError(t, err)
	b, err := DeriveKey(ringInputBeta, LabelIdentifier)
	require.NoError(t, err)

	assert.Equal(t, a1, a2, "same input and label must derive the same key")
	assert.NotEqual(t, a1, b, "a different input must derive a different key")
}

func TestDeriveKey_ReturnsAnIndependentSlice(t *testing.T) {
	first, err := DeriveKey(ringInputAlpha, LabelAddress)
	require.NoError(t, err)
	want := bytes.Clone(first)
	for i := range first {
		first[i] = 0
	}

	second, err := DeriveKey(ringInputAlpha, LabelAddress)
	require.NoError(t, err)
	assert.Equal(t, want, second, "mutating a returned key must not affect later derivations")
}

func TestDeriveKey_EmptySecretIsUnavailable(t *testing.T) {
	for _, tc := range allLabels {
		t.Run(tc.name, func(t *testing.T) {
			k, err := DeriveKey("", tc.label)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKeyUnavailable)
			assert.Empty(t, k)
		})
	}
}

func TestDeriveKey_UnknownLabelIsRefused(t *testing.T) {
	for _, l := range []Label{0, 8, 200} {
		k, err := DeriveKey(ringInputAlpha, l)
		require.Error(t, err, "label %d is outside the closed set and must not derive a key", l)
		assert.ErrorIs(t, err, ErrUnknownLabel)
		assert.Empty(t, k)
	}
}

// ---------------------------------------------------------------------------
// KeyRing helpers
// ---------------------------------------------------------------------------

// ringDB returns a migrated test database with an empty key registry. The
// registry is not in testdb's reset list, so this helper empties it (and restarts
// the identity) before and after the test.
func ringDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	resetRegistry(t, db)
	// Runs before SetupTestDB's own cleanup (LIFO), while the pool is still open.
	t.Cleanup(func() { resetRegistry(t, db) })
	return db
}

func resetRegistry(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`TRUNCATE accountguard_hmac_keys RESTART IDENTITY`)
	require.NoError(t, err)
}

// extraPool opens another pool on the same test database (a separate set of
// connections) and closes it when the test ends.
func extraPool(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func registryRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM accountguard_hmac_keys`).Scan(&n))
	return n
}

// sequenceLast reports the identity sequence's last_value; 0 means nextval has
// never been called. An INSERT ... ON CONFLICT DO NOTHING advances it even when
// the insert conflicts, so a change here exposes an insert-first implementation.
func sequenceLast(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var last int64
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(last_value, 0) FROM pg_sequences WHERE sequencename = 'accountguard_hmac_keys_id_seq'`,
	).Scan(&last))
	return last
}

// ---------------------------------------------------------------------------
// KeyRing
// ---------------------------------------------------------------------------

func TestKeyRing_ResolvesAndCaches(t *testing.T) {
	t.Run("first call registers the key and stores its check digest", func(t *testing.T) {
		db := ringDB(t)
		ring := NewKeyRing(db, ringInputAlpha)

		id, err := ring.CurrentKeyID(context.Background())
		require.NoError(t, err)
		assert.Positive(t, id)
		assert.Equal(t, 1, registryRows(t, db))

		var stored []byte
		var storedID int16
		require.NoError(t, db.QueryRow(`SELECT id, key_check FROM accountguard_hmac_keys`).Scan(&storedID, &stored))
		assert.Equal(t, storedID, id)

		wantCheck, err := DeriveKey(ringInputAlpha, LabelKeyCheck)
		require.NoError(t, err)
		assert.Equal(t, wantCheck, stored, "key_check is DeriveKey(secret, LabelKeyCheck)")
		assert.NotEqual(t, []byte(ringInputAlpha), stored, "the raw input must never be stored")
		for _, tc := range allLabels {
			if tc.label == LabelKeyCheck {
				continue
			}
			other, err := DeriveKey(ringInputAlpha, tc.label)
			require.NoError(t, err)
			assert.NotEqual(t, other, stored, "the stored digest must not equal the %s key", tc.name)
		}
	})

	t.Run("the same ring serves later calls from its cache", func(t *testing.T) {
		ringDB(t) // migrate and empty the registry; the ring below uses its own pool
		own := extraPool(t)
		ring := NewKeyRing(own, ringInputAlpha)

		first, err := ring.CurrentKeyID(context.Background())
		require.NoError(t, err)

		// With the pool closed any query fails, so a second answer can only be the cache.
		require.NoError(t, own.Close())
		second, err := ring.CurrentKeyID(context.Background())
		require.NoError(t, err, "a resolved id must be served without touching the database")
		assert.Equal(t, first, second)
	})

	t.Run("a new ring finds the existing row without burning an identity value", func(t *testing.T) {
		db := ringDB(t)
		first, err := NewKeyRing(db, ringInputAlpha).CurrentKeyID(context.Background())
		require.NoError(t, err)
		seqBefore := sequenceLast(t, db)
		require.Equal(t, int64(first), seqBefore, "the first registration consumes exactly one identity value")

		second, err := NewKeyRing(db, ringInputAlpha).CurrentKeyID(context.Background())
		require.NoError(t, err)

		assert.Equal(t, first, second)
		assert.Equal(t, 1, registryRows(t, db))
		assert.Equal(t, seqBefore, sequenceLast(t, db),
			"resolving an already-registered key must SELECT first; an insert-first path burns an identity value")
	})
}

func TestKeyRing_RotationGetsNewIDAndReturnRestoresOld(t *testing.T) {
	db := ringDB(t)
	bg := context.Background()

	a, err := NewKeyRing(db, ringInputAlpha).CurrentKeyID(bg)
	require.NoError(t, err)
	b, err := NewKeyRing(db, ringInputBeta).CurrentKeyID(bg)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "a rotated key must get its own id")

	seq := sequenceLast(t, db)
	again, err := NewKeyRing(db, ringInputAlpha).CurrentKeyID(bg)
	require.NoError(t, err)
	assert.Equal(t, a, again, "returning to an earlier key must restore its original id")
	assert.Equal(t, 2, registryRows(t, db), "rotation back must not add a row")
	assert.Equal(t, seq, sequenceLast(t, db), "rotation back must not burn an identity value")
}

func TestKeyRing_ConcurrentResolveOneRow(t *testing.T) {
	ringDB(t) // migrate and empty the registry
	const workers = 8

	rings := make([]*KeyRing, workers)
	for i := range rings {
		rings[i] = NewKeyRing(extraPool(t), ringInputAlpha) // one pool, hence its own connections, per ring
	}

	start := make(chan struct{})
	ids := make([]int16, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range rings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = rings[i].CurrentKeyID(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	for i := range rings {
		require.NoError(t, errs[i], "ring %d", i)
		assert.Equal(t, ids[0], ids[i], "every ring must agree on the id")
	}
	assert.Equal(t, 1, registryRows(t, extraPool(t)), "racing registrations must leave exactly one row")
}

func TestKeyRing_FailureNotCached(t *testing.T) {
	t.Run("a cancelled call fails, writes nothing, and the next call resolves", func(t *testing.T) {
		db := ringDB(t)
		ring := NewKeyRing(db, ringInputAlpha)

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		id, err := ring.CurrentKeyID(cancelled)
		require.Error(t, err)
		assert.Zero(t, id)
		assert.Equal(t, 0, registryRows(t, db))

		id, err = ring.CurrentKeyID(context.Background())
		require.NoError(t, err, "a failed resolution must not poison the ring")
		assert.Positive(t, id)
		assert.Equal(t, 1, registryRows(t, db), "the retry must reach the database, not a cached failure")
	})

	t.Run("an unusable database reports an error and registers nothing", func(t *testing.T) {
		db := ringDB(t)
		dead := extraPool(t)
		require.NoError(t, dead.Close())

		id, err := NewKeyRing(dead, ringInputAlpha).CurrentKeyID(context.Background())
		require.Error(t, err)
		assert.Zero(t, id)
		assert.Equal(t, 0, registryRows(t, db))
	})
}

func TestKeyRing_EmptySecretInsertsNothing(t *testing.T) {
	db := ringDB(t)
	seq := sequenceLast(t, db)

	id, err := NewKeyRing(db, "").CurrentKeyID(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyUnavailable)
	assert.Zero(t, id)
	assert.Equal(t, 0, registryRows(t, db), "an empty input must never register a key")
	assert.Equal(t, seq, sequenceLast(t, db))
}

func TestKeyRing_KeyDerivesFromTheRingSecretWithoutTheDatabase(t *testing.T) {
	t.Run("equals DeriveKey for every label", func(t *testing.T) {
		ring := NewKeyRing(nil, ringInputAlpha) // a nil pool proves no query is attempted
		for _, tc := range allLabels {
			got, err := ring.Key(tc.label)
			require.NoError(t, err, tc.name)
			want, err := DeriveKey(ringInputAlpha, tc.label)
			require.NoError(t, err, tc.name)
			assert.Equal(t, want, got, tc.name)
		}
	})

	t.Run("empty secret is unavailable", func(t *testing.T) {
		_, err := NewKeyRing(nil, "").Key(LabelIdentifier)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrKeyUnavailable)
	})

	t.Run("unknown label is refused", func(t *testing.T) {
		_, err := NewKeyRing(nil, ringInputAlpha).Key(Label(200))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnknownLabel)
	})
}

// A registry read or write that fails is "the key id is unavailable" to a caller, and
// the driver error stays reachable beneath it for diagnosis.
func TestKeyRing_RegistryFailureWrapsErrKeyUnavailable(t *testing.T) {
	t.Run("read failure on a cancelled context", func(t *testing.T) {
		db := ringDB(t)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := NewKeyRing(db, ringInputAlpha).CurrentKeyID(cancelled)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrKeyUnavailable)
		assert.ErrorIs(t, err, context.Canceled, "the cause must stay reachable")
	})

	t.Run("read failure on a closed pool", func(t *testing.T) {
		ringDB(t)
		dead := extraPool(t)
		require.NoError(t, dead.Close())

		_, err := NewKeyRing(dead, ringInputAlpha).CurrentKeyID(context.Background())
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrKeyUnavailable)
		// database/sql exports no sentinel for a closed pool, so reachability is read through the
		// chain, and Error() must not carry the text (RT-1: the package rule is no driver text).
		requireDriverTextWithheld(t, err, closedPoolText)
	})

	t.Run("write failure on a read-only session", func(t *testing.T) {
		db := ringDB(t)
		readOnly := readOnlyPool(t)

		_, err := NewKeyRing(readOnly, ringInputAlpha).CurrentKeyID(context.Background())
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrKeyUnavailable)
		var pgErr *pq.Error
		require.ErrorAs(t, err, &pgErr, "the registration INSERT must be what failed, and its cause must stay reachable")
		assert.Equal(t, "25006", string(pgErr.Code), "read_only_sql_transaction")
		assert.NotContains(t, err.Error(), pgErr.Message, "RT-1: the driver message never reaches Error()")
		assert.Equal(t, 0, registryRows(t, db))
	})
}

// readOnlyPool opens a pool whose sessions default to read-only transactions: SELECT
// succeeds and an INSERT fails, which drives the write half of a resolution. It
// assumes testdb.DatabaseURL() is URL-form (postgres://...), because the session
// option is added as a query parameter; it fails the test, without printing the DSN
// (which carries a credential), when the configured DSN is anything else.
func readOnlyPool(t *testing.T) *sql.DB {
	t.Helper()
	u, err := url.Parse(testdb.DatabaseURL())
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("readOnlyPool: the test database DSN must be a postgres:// URL")
	}
	q := u.Query()
	q.Set("options", "-c default_transaction_read_only=on")
	u.RawQuery = q.Encode()

	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A nil ring and a zero-value ring hold no secret and no pool: every method refuses
// with ErrKeyUnavailable instead of panicking, and printing them is safe.
func TestKeyRing_NilAndZeroRingsRefuse(t *testing.T) {
	rings := map[string]*KeyRing{
		"nil":  nil,
		"zero": {},
	}
	for name, ring := range rings {
		t.Run(name, func(t *testing.T) {
			key, err := ring.Key(LabelIdentifier)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKeyUnavailable)
			assert.ErrorContains(t, err, "no ring", "a missing ring is not the same fault as an empty secret")
			assert.Empty(t, key)

			id, err := ring.CurrentKeyID(context.Background())
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKeyUnavailable)
			assert.ErrorContains(t, err, "no ring", "a missing ring is not the same fault as an empty secret")
			assert.Zero(t, id)

			assert.NotPanics(t, func() { _ = fmt.Sprintf("%v", ring) })
		})
	}
}

// Printing a ring (by pointer or by value, with any verb or flags) must never reveal its input.
func TestKeyRing_FormattingNeverPrintsTheSecret(t *testing.T) {
	const probe = "synthetic-format-probe-7f3a91"
	ring := NewKeyRing(nil, probe)
	byValue := reflect.ValueOf(ring).Elem().Interface() // a copy of the struct, taken without vet's copylocks objecting

	// Every verb fmt defines, plus width and precision forms. A ring that redacts only
	// through String and GoString is clean for %v and %s and still prints its whole
	// state for the rest, which fmt reports as a bad verb after dereferencing the
	// inner pointer.
	verbs := []string{
		"%v", "%+v", "%#v", "%T", "%s", "%q", "%x", "%X", "%d", "%b", "%o", "%O",
		"%c", "%U", "%t", "%e", "%E", "%f", "%F", "%g", "%G", "%p",
		"%8.3v", "%-12s", "%+.2f", "%#x", "% x", "%10.4q", "%08d",
	}
	for _, verb := range verbs {
		t.Run(verb, func(t *testing.T) {
			assert.NotContains(t, fmt.Sprintf(verb, ring), probe, "*KeyRing")
			assert.NotContains(t, fmt.Sprintf(verb, byValue), probe, "KeyRing value")
		})
	}
}

// The insert is idempotent in the database itself: a second insert of a registered
// digest is a no-op, which is what lets two racing rings converge on one row.
func TestInsertKeySQL_OnConflictDoesNothing(t *testing.T) {
	db := ringDB(t)
	check, err := DeriveKey(ringInputAlpha, LabelKeyCheck)
	require.NoError(t, err)

	_, err = db.Exec(insertKeySQL, check)
	require.NoError(t, err)
	var first int16
	require.NoError(t, db.QueryRow(selectKeyIDSQL, check).Scan(&first))

	res, err := db.Exec(insertKeySQL, check)
	require.NoError(t, err, "a conflicting insert must be a no-op, not an error")
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	assert.Zero(t, affected)

	var second int16
	require.NoError(t, db.QueryRow(selectKeyIDSQL, check).Scan(&second))
	assert.Equal(t, first, second)
	assert.Equal(t, 1, registryRows(t, db))
}

// One ring shared by many goroutines: every caller gets the same id and one row exists.
func TestKeyRing_SharedRingConcurrentResolve(t *testing.T) {
	db := ringDB(t)
	ring := NewKeyRing(db, ringInputAlpha)
	const workers = 16

	start := make(chan struct{})
	ids := make([]int16, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = ring.CurrentKeyID(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	for i := range workers {
		require.NoError(t, errs[i], "caller %d", i)
		assert.Equal(t, ids[0], ids[i], "caller %d", i)
	}
	assert.Positive(t, ids[0])
	assert.Equal(t, 1, registryRows(t, db))
}

// closedPoolText is database/sql's message for a statement on a closed pool. The error is
// unexported, so the tests below identify the driver cause by its text.
const closedPoolText = "database is closed"

// causeReachable reports whether any error strictly beneath err (through Unwrap() error and
// Unwrap() []error alike) has a message containing text. It is how a test proves a cause is
// still reachable once Error() stops carrying it.
func causeReachable(err error, text string) bool {
	var children []error
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		if c := u.Unwrap(); c != nil {
			children = []error{c}
		}
	case interface{ Unwrap() []error }:
		children = u.Unwrap()
	}
	for _, c := range children {
		if strings.Contains(c.Error(), text) || causeReachable(c, text) {
			return true
		}
	}
	return false
}

// requireDriverTextWithheld is RT-1's pair: the driver cause is reachable beneath err, and
// err.Error() does not carry its text.
func requireDriverTextWithheld(t *testing.T, err error, text string) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, causeReachable(err, text), "the driver cause must stay reachable through the chain")
	assert.NotContains(t, err.Error(), text, "RT-1: the driver text must not reach Error()")
}

// RT-1: every exported helper that reaches the database withholds the driver text on a driver
// failure, keeps its sentinel matchable, and keeps the cause reachable.
func TestExportedHelpers_DriverFailureCarriesNoDriverText(t *testing.T) {
	ringDB(t)
	dead := extraPool(t)
	require.NoError(t, dead.Close())
	ring := NewKeyRing(dead, ringInputAlpha)
	ctx := context.Background()
	now := time.Now()

	calls := []struct {
		name string
		call func() error
	}{
		{"CurrentKeyID", func() error { _, err := ring.CurrentKeyID(ctx); return err }},
		{"SealGuardToken", func() error {
			_, err := SealGuardToken(ctx, ring, GuardClaims{UserID: uuid.New(), Purpose: PurposeCancel, IssuedAt: now})
			return err
		}},
		{"OpenGuardToken", func() error { _, err := OpenGuardToken(ctx, ring, "AAAA", PurposeCancel, now); return err }},
		{"MintChallenge", func() error { _, err := MintChallenge(ctx, ring, ChallengeCancel, now); return err }},
		{"VerifyChallenge", func() error { _, err := VerifyChallenge(ctx, ring, "AAAA", ChallengeCancel, now); return err }},
		{"NewCandidate", func() error {
			_, err := NewCandidate(ctx, ring, decide.ReqBreakGlass, nil, false)
			return err
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrKeyUnavailable, "the sentinel stays matchable")
			requireDriverTextWithheld(t, err, closedPoolText)
		})
	}
}
