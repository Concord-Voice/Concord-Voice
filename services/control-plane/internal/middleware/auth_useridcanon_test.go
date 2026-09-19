package middleware_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// #3362: the request identity crosses two stores with different equality rules.
// Every SQL predicate compares `user_id` as a PostgreSQL `uuid`, which resolves
// braced, UPPERCASE and dash-less spellings to ONE value. The two identity-keyed
// Redis gates instead CONCATENATE the raw claim into a key —
// middleware.UserDisabledKey and credepoch.Key — so the same identity in two
// spellings AGREES in SQL and DISAGREES in Redis.
//
// These tests drive the full TestServer stack (real DB + Redis) through
// AuthRequired. Each one writes its Redis fixture at the CANONICAL id — which is
// what every writer in the repo produces, since they all source the value from
// c.GetString("user_id") or from a `SELECT id FROM users` uuid column — and then
// presents the same identity in each spelling.
//
// Mutation proof: remove the canonicalUserID call from AuthRequired and both
// bypass tests go red on their variant subtests while the canonical control
// stays green. That asymmetry is the point — a mutant that breaks the control
// too would mean the test is measuring something else.

// userIDSpelling is one way of writing a single identity.
type userIDSpelling struct {
	name  string
	claim string
}

// userIDSpellings returns the spellings of one identity that PostgreSQL's `uuid`
// type resolves to the SAME row: the canonical form plus the three variants.
//
// `urn:uuid:` is deliberately absent. uuid.Parse accepts it but PostgreSQL
// REJECTS the prefix, so it never reached the same row and never had this
// bypass — it has its own test below, because canonicalizing changes its
// outcome for a different reason.
func userIDSpellings(t *testing.T, canonical string) []userIDSpelling {
	t.Helper()
	// Guard the fixture, not the code: if CreateTestUser ever stopped returning
	// the canonical spelling, every "variant" below would silently become a
	// second canonical form and the tests would prove nothing.
	require.Equal(t, strings.ToLower(canonical), canonical,
		"fixture must be canonical lowercase-hyphenated for the variants to be variants")
	require.Contains(t, canonical, "-", "fixture must be hyphenated")

	return []userIDSpelling{
		{name: "canonical_control", claim: canonical},
		{name: "braced", claim: "{" + canonical + "}"},
		{name: "uppercase", claim: strings.ToUpper(canonical)},
		{name: "dashless", claim: strings.ReplaceAll(canonical, "-", "")},
	}
}

// TestAuthRequired_DisabledDenylistIsSpellingInvariant proves bypass 1.
//
// VerifyLiveTokenState checks Exists(user_disabled:<raw claim>) and has NO
// database fallback, so a spelling that misses the key is not merely slower to
// reject — it is never rejected at all. Before #3362 the three variants below
// were admitted 200 against a denylisted account.
func TestAuthRequired_DisabledDenylistIsSpellingInvariant(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "canondisabled")

	// Written exactly as the terminal-disable path writes it (age/handler.go
	// and privacy/handler.go both Set this key, both from c.GetString("user_id")).
	require.NoError(t, ts.Redis.Set(t.Context(), middleware.UserDisabledKey(user.ID), "1", 0).Err())

	for _, sp := range userIDSpellings(t, user.ID) {
		t.Run(sp.name, func(t *testing.T) {
			w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, sp.claim, ""))

			require.Equal(t, http.StatusForbidden, w.Code,
				"a disabled account must be refused for every spelling of its id; got %d: %s",
				w.Code, w.Body.String())

			var body map[string]any
			testhelpers.ParseJSON(t, w, &body)
			assert.Equal(t, "account_disabled", body["error_code"],
				"the refusal must be the denylist's, not an incidental failure elsewhere")
		})
	}
}

// TestAuthRequired_CredentialEpochFenceIsSpellingInvariant proves bypass 2.
//
// The fence (#2201) fails closed while a destructive credential operation is
// in flight, via a `blocked:<opID>` marker in Redis. A spelling that misses the
// marker falls through to dbEpoch — whose SQL DOES canonicalize — so the token
// is compared against the still-current epoch, matches, and is admitted. The
// DB epoch below is what makes that fall-through admit rather than merely
// degrade, which is the reported mechanism.
func TestAuthRequired_CredentialEpochFenceIsSpellingInvariant(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "canonepoch")

	const dbEpoch = "epochDB1"
	_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, dbEpoch, user.ID)
	require.NoError(t, err)

	// A destructive credential operation is blocked-in-flight at the canonical id.
	require.NoError(t, ts.Redis.Set(t.Context(),
		credepoch.Key(user.ID), "blocked:op1", 5*time.Minute).Err())

	for _, sp := range userIDSpellings(t, user.ID) {
		t.Run(sp.name, func(t *testing.T) {
			// The claim carries the epoch the DB read-through would return, so a
			// spelling that misses the blocked marker is ADMITTED rather than
			// rejected on a mismatch — the bypass, not a near-miss.
			w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, sp.claim, dbEpoch))

			assert.Equal(t, http.StatusUnauthorized, w.Code,
				"a blocked credential-epoch marker must fail closed for every spelling of its id; got %d: %s",
				w.Code, w.Body.String())
		})
	}

	// ADMISSIBILITY CONTROL. Above this line every assertion is a bare 401, and
	// 401 is abortUnauthorized — the catch-all for a bad signature, a bad issuer,
	// an expired token, a blacklisted jti and a missing claim, as well as the
	// epoch mismatch under test. Without a control, a future change to epochToken
	// (a new required claim, an issuer change) makes all four subtests pass for
	// the wrong reason while the file reports full coverage of a gate it never
	// reached. Dropping the marker must make the SAME tokens admissible.
	require.NoError(t, ts.Redis.Del(t.Context(), credepoch.Key(user.ID)).Err())
	for _, sp := range userIDSpellings(t, user.ID) {
		t.Run("admissible_without_marker/"+sp.name, func(t *testing.T) {
			w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, sp.claim, dbEpoch))
			assert.Equal(t, http.StatusOK, w.Code,
				"with the blocked marker gone the same token must be admitted — otherwise the 401s above prove nothing; got %d: %s",
				w.Code, w.Body.String())
		})
	}
}

// TestAuthRequired_URNSpellingIsCanonicalizedAndGated pins the one ACCEPTED
// WIDENING in #3362.
//
// `urn:uuid:<id>` is the fourth spelling uuid.Parse accepts and the only one
// PostgreSQL rejects. Before canonicalization it passed the string assertion,
// missed both Redis gates, reached the `WHERE id = $1` cast and surfaced as a
// 503 — failing closed by accident, via a parser error, rather than by design.
// It is now normalized like any other spelling and meets the same gates, so a
// denylisted account is refused rather than 503'd.
func TestAuthRequired_URNSpellingIsCanonicalizedAndGated(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "canonurn")
	require.NoError(t, ts.Redis.Set(t.Context(), middleware.UserDisabledKey(user.ID), "1", 0).Err())

	w := doAuthRequest(t, ts, bearerPrefix+epochToken(t, "urn:uuid:"+user.ID, ""))

	require.Equal(t, http.StatusForbidden, w.Code,
		"the urn spelling must reach the denylist rather than die at the SQL cast; got %d: %s",
		w.Code, w.Body.String())

	// Assert the CARRIER, not just the code. 403 is also `RequireVerifiedEmail`'s
	// refusal (auth.go), and this route only avoids it by sitting in the
	// pending-OK tier — a routing fact stated nowhere near this test. Pinning
	// error_code means a later tier change cannot silently give the assertion a
	// second producer.
	var body map[string]any
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "account_disabled", body["error_code"],
		"the refusal must be the denylist's, not an email-verification 403")
}

// TestAuthRequired_RejectsUnparseableUserIDClaim pins the refusal arm.
//
// The first three cases are the tightening: before #3362 they passed the
// `.(string)` assertion, missed both Redis gates and died at the PostgreSQL
// `uuid` cast, surfacing a malformed-input request as a 503 "Authentication
// temporarily unavailable" — a dependency fault the dependency never had. They
// are now refused at the middleware.
//
// The last three already returned 401 via the type assertion; they are pinned
// so a future canonicalUserID cannot regress the KEY-vs-VALUE distinction that
// IsAccessToken documents for the `purpose` claim (#2899) — an attacker picks
// the encoding.
func TestAuthRequired_RejectsUnparseableUserIDClaim(t *testing.T) {
	ts := setupTS(t)
	const canonical = "11111111-2222-3333-4444-555555555555"

	for _, tc := range []struct {
		name  string
		claim any
	}{
		{name: "garbage", claim: "not-a-uuid"},
		{name: "leading_space", claim: " " + canonical},
		{name: "trailing_newline", claim: canonical + "\n"},
		{name: "empty_string", claim: ""},
		{name: "numeric", claim: float64(12345)},
		{name: "absent", claim: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{
				"exp": time.Now().Add(15 * time.Minute).Unix(),
				"iat": time.Now().Unix(),
			}
			if tc.claim != nil {
				claims["user_id"] = tc.claim
			}

			w := doAuthRequest(t, ts, bearerPrefix+makeToken(t, claims, testSecret))

			assert.Equal(t, http.StatusUnauthorized, w.Code,
				"an unusable user_id claim must be refused at the middleware, not carried to PostgreSQL; got %d: %s",
				w.Code, w.Body.String())
		})
	}
}
