package middleware_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3362: the two tests below pin the property the whole change exists to
// establish, and the design decision it rests on. Neither was covered by
// auth_useridcanon_test.go, which asserts only REFUSALS — every one of its 15
// subtests expects a 403 or a 401.
//
// That omission was not cosmetic. A mutant that canonicalizes for the gates and
// then publishes the RAW claim downstream —
//
//	c.Set("user_id", claims["user_id"])   // instead of the canonical userID
//
// passes every subtest in that file AND every pre-existing test in this
// package. The gates are spelling-invariant either way; what breaks is the
// contract every downstream Redis key builder depends on. This file is what
// kills it.

// TestAuthRequired_PublishesCanonicalUserIDOnContext pins the invariant
// canonicalUserID's docblock claims: `c.Get("user_id")` is a canonical UUID BY
// CONSTRUCTION.
//
// It is load-bearing for code far outside this package. `UserDisabledKey` and
// `credepoch.Key` CONCATENATE this value verbatim, and their authenticated
// writers — privacy/handler.go and age/handler.go — read it straight from the
// context. If AuthRequired ever published the raw claim again, those writers
// would key a denylist entry under a spelling the reader here can no longer
// look up, which is this bug inverted: the fix would be writing the miss rather
// than closing it.
//
// It doubles as the ADMISSION control the refusal-only file lacks. Asserting
// 200 is what pins "NORMALIZE RATHER THAN REJECT" — the alternative design the
// docblock argues against at length. Without it, a later change to reject
// non-canonical input outright would surface only indirectly, as a disabled
// account's 403 flipping to 401, which names the wrong cause.
func TestAuthRequired_PublishesCanonicalUserIDOnContext(t *testing.T) {
	r := authRequiredUnderTest(t)
	const canonical = "0f1e2d3c-4b5a-4968-8776-655443322110"

	for _, sp := range []struct{ name, claim string }{
		{"canonical_control", canonical},
		{"braced", "{" + canonical + "}"},
		{"uppercase", strings.ToUpper(canonical)},
		{"dashless", strings.ReplaceAll(canonical, "-", "")},
		{"urn", "urn:uuid:" + canonical},
	} {
		t.Run(sp.name, func(t *testing.T) {
			// Guard the fixture's DISTINCTNESS, not a property of the canonical
			// form: if a "variant" ever equals the canonical spelling it becomes a
			// second control, and the row proves nothing while still passing.
			if sp.name != "canonical_control" {
				require.NotEqual(t, canonical, sp.claim,
					"a variant row must actually differ from the canonical form")
			}

			tok := makeToken(t, jwt.MapClaims{
				"user_id": sp.claim,
				"exp":     time.Now().Add(15 * time.Minute).Unix(),
				"iat":     time.Now().Unix(),
			}, tokenClassSecret)

			rec := getProtected(t, r, tok)

			require.Equal(t, http.StatusOK, rec.Code,
				"a legitimate non-canonical spelling must still be ADMITTED (normalize, not reject); body: %s",
				rec.Body.String())

			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, canonical, body["user_id"],
				"AuthRequired must publish the CANONICAL id on the context — every downstream Redis key builder concatenates this value verbatim")
		})
	}
}

// TestAuthRequired_RejectsPostgresOnlySpellings pins the bypass variants
// #3362's original matrix MISSED — the ones PostgreSQL resolves to the same
// user row but `uuid.Parse` refuses outright.
//
// The original matrix listed three bypassing spellings (braced, UPPERCASE,
// dash-less) because it was derived from what `uuid.Parse` accepts. That was
// the wrong generator: the bug is "PostgreSQL resolves it to the victim's row
// while the Redis key misses", so the class is defined by what POSTGRES
// accepts. PG additionally takes a hyphen after ANY group of four hex digits,
// and braces around any of those forms. Each was a live instance of the same
// bypass pre-fix, and none was tested.
//
// They are closed by REFUSAL, not by normalization — `uuid.Parse` knows only
// lengths 32/36/38/45, so none of these reaches a gate at all. That asymmetry
// is worth keeping in view: the two parsers' accepted sets are NOT nested.
// `uuid.Parse` admits shapes PostgreSQL refuses (`urn:uuid:`, and any 38-byte
// string whose middle 36 bytes are canonical — its brace arm never validates
// the delimiters) and refuses shapes PostgreSQL admits (every case below).
func TestAuthRequired_RejectsPostgresOnlySpellings(t *testing.T) {
	r := authRequiredUnderTest(t)
	const canonical = "0f1e2d3c-4b5a-4968-8776-655443322110"

	dashless := strings.ReplaceAll(canonical, "-", "")
	groups := make([]string, 0, len(dashless)/4)
	for i := 0; i+4 <= len(dashless); i += 4 {
		groups = append(groups, dashless[i:i+4])
	}
	everyFour := strings.Join(groups, "-") // 8 groups of 4 — PG-legal, 39 chars

	for _, tc := range []struct{ name, claim string }{
		{"pg_extra_hyphens", everyFour},
		{"pg_braced_dashless", "{" + dashless + "}"},
		{"pg_braced_extra_hyphens", "{" + everyFour + "}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := makeToken(t, jwt.MapClaims{
				"user_id": tc.claim,
				"exp":     time.Now().Add(15 * time.Minute).Unix(),
				"iat":     time.Now().Unix(),
			}, tokenClassSecret)

			rec := getProtected(t, r, tok)

			assert.Equal(t, http.StatusUnauthorized, rec.Code,
				"PostgreSQL resolves %q to a real row, so it must not reach the gates unnormalized; body: %s",
				tc.claim, rec.Body.String())
		})
	}
}
