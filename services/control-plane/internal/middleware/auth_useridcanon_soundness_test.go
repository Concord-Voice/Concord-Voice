// This file is `package middleware`, and it is the only internal test file in
// the package — every other one is `package middleware_test`. That is
// deliberate, not drift: `canonicalUserID` is unexported, and the property
// below is a claim about THAT FUNCTION rather than about `uuid.Parse`. An
// external test could only assert `uuid.Parse(x).String()`, which would keep
// passing if the production function stopped calling it — the adjacent-claim
// failure. `internal/testhelpers` cannot be imported from here (it builds a
// Router and so imports this package), which is why only the cycle-free
// `testhelpers/testdb` subpackage is used.
package middleware

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// #3362 SOUNDNESS. The fix normalizes rather than rejects, and that choice is
// only safe if two properties hold across the WHOLE input space, not the four
// spellings the sibling bypass tests enumerate:
//
//   - DIVERGENCE-FREEDOM. Where Go and PostgreSQL both accept an input, they
//     must canonicalize it to the SAME string. The request identity crosses two
//     stores with different equality rules — SQL compares as `uuid`, Redis
//     concatenates the Go string into a key — so a disagreement re-opens the
//     desync in the other direction: the gates would key on one identity while
//     the rows resolve to another.
//
//   - COLLISION-FREEDOM. Canonicalization must never map two DISTINCT
//     identities onto one string. That failure is strictly worse than the
//     bypass this issue fixes: a bypass lets one account skip a gate, whereas a
//     merge would make one account's denylist entry and credential-epoch fence
//     apply to a different account.
//
// Both are MEASURED here against the real PostgreSQL 16 `uuid` input function
// rather than argued from the parsers' documentation, because the bug being
// fixed was itself two parsers that were each individually correct.
//
// The two accept-sets are NOT equal, in both directions, and the asymmetry is
// the interesting half:
//
//	Go-only  — `urn:uuid:<id>` and the loose 38-byte arm (`X<id>Y`, which
//	           uuid.Parse reaches via `s[1:]` without validating the
//	           delimiters). PostgreSQL rejects both. Canonicalizing them is the
//	           accepted WIDENING: they now reach the gates instead of dying at
//	           the SQL cast as a 503. What makes that safe is asserted below —
//	           their canonical form lands back inside PostgreSQL's accept-set.
//
//	PG-only  — PostgreSQL additionally accepts a hyphen after ANY group of four
//	           hex digits (`d8fe-119d-a28c-…`), a 39-byte form uuid.Parse has no
//	           arm for. Those are refused at the middleware with a 401 and never
//	           reach PostgreSQL, which is fail-closed and correct.
//
// Each bucket carries a non-emptiness assertion. Without one, a future change
// to either parser could empty a bucket and this test would keep passing while
// silently covering less — the corpus, not the code, would have moved.

const (
	// 150 identities x 13 spellings ~= 1,950 inputs, cross-checked in exactly
	// two round trips. Large enough that a systematic divergence cannot hide;
	// small enough to stay a sub-second unit test.
	soundnessIdentityCount = 150

	// Derived, never random: `crypto/rand` would make a failure
	// unreproducible and `math/rand` would need a gosec waiver for no gain.
	soundnessCorpusDomain = "#3362 user_id canonicalization corpus"
)

// soundnessIdentity derives identity i deterministically. The bytes are an
// arbitrary 128-bit value rather than a well-formed v4 UUID on purpose:
// neither uuid.Parse nor PostgreSQL validates the version or variant nibbles,
// so constraining them would only narrow the corpus.
func soundnessIdentity(i int) uuid.UUID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s %d", soundnessCorpusDomain, i)))
	u, err := uuid.FromBytes(sum[:16])
	if err != nil {
		panic(err) // 16 bytes in, cannot fail
	}
	return u
}

// hyphenEveryFour renders the PG-only spelling: eight groups of four hex
// digits. PostgreSQL accepts it; uuid.Parse has no 39-byte arm.
func hyphenEveryFour(hexDigits string) string {
	groups := make([]string, 0, 8)
	for i := 0; i < len(hexDigits); i += 4 {
		groups = append(groups, hexDigits[i:i+4])
	}
	return strings.Join(groups, "-")
}

// spellingsOf returns every way this corpus writes one identity. The labels are
// for failure messages only — no assertion branches on them, because a label is
// a claim about the string and the string is what the parsers see.
func spellingsOf(u uuid.UUID) []struct{ label, raw string } {
	c := u.String()
	hexDigits := strings.ReplaceAll(c, "-", "")
	return []struct{ label, raw string }{
		{"canonical", c},
		{"uppercase", strings.ToUpper(c)},
		{"braced", "{" + c + "}"},
		{"braced_uppercase", "{" + strings.ToUpper(c) + "}"},
		{"dashless", hexDigits},
		{"dashless_uppercase", strings.ToUpper(hexDigits)},
		{"urn", "urn:uuid:" + c},
		{"urn_uppercase_prefix", "URN:UUID:" + c},
		{"pg_hyphen_every_four", hyphenEveryFour(hexDigits)},
		{"loose_38_byte_arm", "X" + c + "Y"},
		{"leading_space", " " + c},
		{"trailing_newline", c + "\n"},
		{"truncated", c[:len(c)-1]},
	}
}

// postgresVerdicts asks the real `uuid` input function about every raw string,
// in two passes. A single pass with a CASE around the cast would be shorter,
// but PostgreSQL does not promise that a CASE arm is left unevaluated in every
// plan, and a thrown cast would abort the query rather than answer it. Pass one
// classifies with the exception-free `pg_input_is_valid`; pass two casts only
// what pass one already admitted, so no cast can raise.
func postgresVerdicts(t *testing.T, db *sql.DB, raws []string) (accepted map[string]bool, canonical map[string]string) {
	t.Helper()

	accepted = make(map[string]bool, len(raws))
	rows, err := db.QueryContext(t.Context(),
		`SELECT raw, pg_input_is_valid(raw, 'uuid') FROM unnest($1::text[]) AS t(raw)`,
		pq.Array(raws))
	require.NoError(t, err, "pg_input_is_valid requires PostgreSQL 16+")
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		var ok bool
		require.NoError(t, rows.Scan(&raw, &ok))
		accepted[raw] = ok
	}
	require.NoError(t, rows.Err())
	require.Len(t, accepted, len(dedupe(raws)), "every input must come back classified")

	valid := make([]string, 0, len(raws))
	for raw, ok := range accepted {
		if ok {
			valid = append(valid, raw)
		}
	}

	canonical = make(map[string]string, len(valid))
	castRows, err := db.QueryContext(t.Context(),
		`SELECT raw, raw::uuid::text FROM unnest($1::text[]) AS t(raw)`,
		pq.Array(valid))
	require.NoError(t, err)
	defer func() { _ = castRows.Close() }()
	for castRows.Next() {
		var raw, canon string
		require.NoError(t, castRows.Scan(&raw, &canon))
		canonical[raw] = canon
	}
	require.NoError(t, castRows.Err())

	return accepted, canonical
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func TestCanonicalUserID_AgreesWithPostgresAndNeverMergesIdentities(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	defer cleanup()

	type sample struct {
		identity int
		label    string
		raw      string
	}

	samples := make([]sample, 0, soundnessIdentityCount*13)
	for i := 0; i < soundnessIdentityCount; i++ {
		for _, sp := range spellingsOf(soundnessIdentity(i)) {
			samples = append(samples, sample{identity: i, label: sp.label, raw: sp.raw})
		}
	}

	// PASS 1 — run the production function over the corpus and keep what it
	// produced. Nothing is asserted yet, because PostgreSQL has to be asked
	// about the OUTPUTS as well as the inputs.
	type verdict struct {
		canonical string
		reason    securityevent.ReasonCode
		ok        bool
	}
	got := make([]verdict, len(samples))
	toClassify := make([]string, 0, len(samples)*2)
	for i, s := range samples {
		canon, reason, ok := canonicalUserID(jwt.MapClaims{"user_id": s.raw})
		got[i] = verdict{canonical: canon, reason: reason, ok: ok}
		toClassify = append(toClassify, s.raw)
		if ok {
			// The widening assertion below asks PostgreSQL about a value this
			// function INVENTED, which is never guaranteed to appear in the
			// corpus. Classifying it explicitly is what stops a `map[string]bool`
			// miss — zero value `false` — from reading as a measured rejection.
			toClassify = append(toClassify, canon)
		}
	}

	pgAccepts, pgCanonical := postgresVerdicts(t, db, dedupe(toClassify))

	// goCanonical -> the set of distinct identities that reached it.
	merged := make(map[string]map[int]struct{})

	var bothAccept, goOnly, pgOnly, neither int

	for i, s := range samples {
		goCanon, reason, goOK := got[i].canonical, got[i].reason, got[i].ok

		pgOK, measured := pgAccepts[s.raw]
		require.True(t, measured, "input %q was never classified by PostgreSQL", s.raw)

		switch {
		case goOK && pgOK:
			bothAccept++
			// DIVERGENCE-FREEDOM.
			require.Equal(t, pgCanonical[s.raw], goCanon,
				"identity %d spelled %s (%q): Go canonicalized to %q but PostgreSQL to %q — "+
					"the Redis-keyed gates and the SQL predicates would resolve different identities",
				s.identity, s.label, s.raw, goCanon, pgCanonical[s.raw])

		case goOK && !pgOK:
			goOnly++
			// THE WIDENING IS ONLY SAFE IF IT LANDS INSIDE PostgreSQL'S
			// ACCEPT-SET. Before #3362 these spellings passed the string
			// assertion, missed both Redis gates and then died at the `uuid`
			// cast as a 503 — a dependency fault the dependency never had.
			// Canonicalizing is what makes them reach the gates instead, and
			// this is the assertion that they now survive the cast too.
			canonOK, canonMeasured := pgAccepts[goCanon]
			require.True(t, canonMeasured,
				"canonical form %q was never classified — the widening cannot be judged", goCanon)
			require.True(t, canonOK,
				"identity %d spelled %s (%q): Go admits it but its canonical form %q "+
					"is still not valid `uuid` input — canonicalizing only moved the 503",
				s.identity, s.label, s.raw, goCanon)

		case !goOK && pgOK:
			pgOnly++
			// Fail-closed and correct: refused at the middleware, so the value
			// never reaches a query that would have matched the row.
			require.Empty(t, goCanon, "a refusal must publish no identity")

		default:
			neither++
		}

		if goOK {
			// COLLISION-FREEDOM bookkeeping.
			if merged[goCanon] == nil {
				merged[goCanon] = make(map[int]struct{})
			}
			merged[goCanon][s.identity] = struct{}{}

			// The observability half: a spelling that IS its own canonical form
			// is unremarkable, and any other admitted spelling means some mint
			// path emitted a non-canonical id. Derived by comparing the strings
			// rather than by the label, so an identity whose hex happens to be
			// case-insensitive cannot make this assert the wrong arm.
			if s.raw == goCanon {
				require.Equal(t, securityevent.ReasonCode(""), reason,
					"an already-canonical claim is not an event: %q", s.raw)
			} else {
				require.Equal(t, securityevent.ReasonIdentityClaimNonCanonical, reason,
					"an admitted non-canonical claim must be visible: %q", s.raw)
			}

			// Canonicalization is idempotent, or the gates could key on a
			// different string on a second pass through the same claim.
			again, againReason, againOK := canonicalUserID(jwt.MapClaims{"user_id": goCanon})
			require.True(t, againOK)
			require.Equal(t, goCanon, again, "canonicalization must be a fixed point")
			require.Equal(t, securityevent.ReasonCode(""), againReason)
		}
	}

	// COLLISION-FREEDOM.
	for canon, identities := range merged {
		require.Len(t, identities, 1,
			"canonical form %q was reached by %d DISTINCT identities — canonicalization "+
				"merged them, so one account's denylist entry and credential-epoch fence "+
				"would apply to another", canon, len(identities))
	}
	require.Len(t, merged, soundnessIdentityCount,
		"every identity must own exactly one canonical form")

	// BUCKET NON-EMPTINESS. Each of these guards a claim this test would
	// otherwise assert vacuously if the corpus (or a parser) stopped producing
	// that shape.
	require.Positive(t, bothAccept, "corpus covers no input both parsers accept")
	require.Positive(t, goOnly, "corpus covers no Go-only widening (urn / loose 38-byte arm)")
	require.Positive(t, pgOnly, "corpus covers no PostgreSQL-only spelling (hyphen after any four digits)")
	require.Positive(t, neither, "corpus covers no input both parsers refuse")

	t.Logf("#3362 soundness: %d inputs — both=%d go_only=%d pg_only=%d neither=%d; "+
		"0 divergences, 0 collisions", len(samples), bothAccept, goOnly, pgOnly, neither)
}
