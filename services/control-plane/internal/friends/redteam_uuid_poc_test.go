package friends

// Regression tests for #1240 RT-3, adopted from the red-team pass on PR #2911.
// Inverted from exploit PoCs: green only while the fix holds.
//
// RT-3: uuid.Parse accepts a SUPERSET of what Postgres's uuid input accepts,
// and both #1240 entry points forward the RAW, un-normalised string to the
// query. Three consequences, one root cause:
//
//	(a) "urn:uuid:<u>" clears validation, Postgres cannot cast it -> 500 + an
//	    ERROR log on both endpoints. Pre-PR SendRequest coerced this same error
//	    into 404 "User not found", so the PR regresses it to a 500.
//	(b) uppercase hex of your OWN id slips past SendRequest's
//	    `targetUserID == userID` self-check (Postgres folds uuid case, Go does
//	    not) and reaches the INSERT, which the friendships_check CHECK
//	    constraint rejects -> another 500 + ERROR log instead of the 400.
//	(c) the same trick makes the eligibility endpoint answer its documented
//	    "self -> false" contract with the gate's verdict instead.

import (
	"database/sql/driver"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	rtURNForm = "urn:uuid:22222222-2222-4222-8222-222222222222"
	// Also needs hex letters, and for the same reason as rtUpperShout: an
	// all-digit id makes the "shout" a no-op and the test vacuous.
	rtUpperSelf = "2222beef-2222-4222-8222-2222222222ab"
	// Must contain hex LETTERS. The attack IS an alternate-case encoding, so a
	// digits-only UUID makes strings.ToUpper a no-op — which is exactly why the
	// original fixture tripped its own premise guard and proved nothing.
	rtUpperShout = "1111abcd-1111-4111-8111-1111111111ef"
)

// Verified against the live container:
//
//	psql -c "select 'urn:uuid:2222...'::uuid"
//	ERROR:  invalid input syntax for type uuid: "urn:uuid:2222..."
//	psql -c "select 'AAAAAAAA-...'::uuid = 'aaaaaaaa-...'::uuid"  -> t
var errPgInvalidUUIDText = errors.New(
	`pq: invalid input syntax for type uuid: "` + rtURNForm + `"`)

var errPgFriendshipsSelfCheck = errors.New(
	`pq: new row for relation "friendships" violates check constraint "friendships_check"`)

// pgLikeResponder models Postgres: it rejects a target string Postgres could
// not cast, and otherwise answers as if the row existed.
func pgLikeResponder(t *testing.T) recResponder {
	t.Helper()
	return func(q string, args []driver.NamedValue) (*recRows, error) {
		castable := func(v driver.Value) bool {
			s, ok := v.(string)
			if !ok {
				return true
			}
			_, err := uuid.Parse(s)
			// Postgres accepts hyphenated / un-hyphenated / braced, NOT urn:uuid:.
			return err == nil && !strings.HasPrefix(strings.ToLower(s), "urn:uuid:")
		}
		for _, a := range args {
			if !castable(a.Value) {
				return nil, errPgInvalidUUIDText
			}
		}
		switch {
		case strings.Contains(q, "allow_friend_requests_from"):
			return row([]string{"eligible"}, true), nil
		case strings.Contains(q, "INSERT INTO friendships"):
			// Postgres enforces requester_id <> addressee_id (case-folded).
			if len(args) >= 2 {
				a, _ := args[0].Value.(string)
				b, _ := args[1].Value.(string)
				ua, errA := uuid.Parse(a)
				ub, errB := uuid.Parse(b)
				if errA == nil && errB == nil && ua == ub {
					return nil, errPgFriendshipsSelfCheck
				}
			}
			return row([]string{"id", "created_at"},
				"44444444-4444-4444-8444-444444444444", "2026-08-22T00:00:00Z"), nil
		}
		return nil, nil
	}
}

func TestRT3a_URNFormClearsValidationAndDiesAtTheCast(t *testing.T) {
	if _, err := uuid.Parse(rtURNForm); err != nil {
		t.Fatalf("premise failed: uuid.Parse rejected %q", rtURNForm)
	}

	hGet, _, getLogs := rtHandler(pgLikeResponder(t))
	wGet := rtDo(rtRouter(hGet, rtAttacker), http.MethodGet,
		"/api/v1/users/"+rtURNForm+"/friend-request-eligibility", "")

	hPost, _, postLogs := rtHandler(pgLikeResponder(t))
	wPost := rtDo(rtRouter(hPost, rtAttacker), http.MethodPost,
		"/api/v1/friends/request", `{"user_id":"`+rtURNForm+`"}`)

	t.Logf("GET  -> %d %s | log: %s", wGet.Code, strings.TrimSpace(wGet.Body.String()),
		strings.TrimSpace(getLogs.String()))
	t.Logf("POST -> %d %s | log: %s", wPost.Code, strings.TrimSpace(wPost.Body.String()),
		strings.TrimSpace(postLogs.String()))

	if wGet.Code != http.StatusInternalServerError || wPost.Code != http.StatusInternalServerError {
		t.Logf("SECURE: the URN form is canonicalised, not 500ed: GET=%d POST=%d (#1240 RT-3)", wGet.Code, wPost.Code)
		return
	}
	if !strings.Contains(getLogs.String(), "level=ERROR") ||
		!strings.Contains(postLogs.String(), "level=ERROR") {
		t.Fatalf("expected a server-side ERROR log on both paths")
	}
	t.Fatalf("REGRESSION — EXPLOITED: a uuid.Parse-valid target Postgres cannot cast 500s both #1240 " +
		"entry points and writes an ERROR log, at 10/min each.")
}

func TestRT3b_UppercaseSelfTargetSlipsPastSendRequestSelfCheck(t *testing.T) {
	// Authenticate AS the letter-bearing id and target its own shouted form, so
	// this is genuinely a self-target in an alternate encoding.
	self := rtUpperShout
	shout := strings.ToUpper(self)
	if shout == self {
		t.Fatalf("premise failed: the shouted form is not distinct from %q", self)
	}

	h, conn, logs := rtHandler(pgLikeResponder(t))
	w := rtDo(rtRouter(h, self), http.MethodPost,
		"/api/v1/friends/request", `{"user_id":"`+shout+`"}`)

	t.Logf("POST self-as-uppercase -> %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	t.Logf("statements: %v", conn.statements())

	if w.Code == http.StatusBadRequest {
		t.Logf("SECURE: the self-check caught the shouted form -> %s (#1240 RT-3)", w.Body.String())
		return
	}
	if !mentions(conn.statements(), "INSERT INTO friendships") {
		t.Fatalf("expected the self-check bypass to reach the INSERT; got %v", conn.statements())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected the CHECK constraint to surface as 500, got %d", w.Code)
	}
	t.Fatalf("REGRESSION — EXPLOITED: `Cannot send a friend request to yourself` (400) was bypassed; the "+
		"request reached INSERT INTO friendships and returned 500. Log: %s",
		strings.TrimSpace(logs.String()))
}

func TestRT3c_UppercaseSelfProbeBreaksTheEligibilitySelfContract(t *testing.T) {
	shout := strings.ToUpper(rtUpperSelf)
	if shout == rtUpperSelf {
		t.Fatalf("premise failed: the shouted form is not distinct from %q", rtUpperSelf)
	}

	h, _, _ := rtHandler(pgLikeResponder(t))
	w := rtDo(rtRouter(h, rtUpperSelf), http.MethodGet,
		"/api/v1/users/"+shout+"/friend-request-eligibility", "")

	t.Logf("GET self-as-uppercase -> %d %s", w.Code, strings.TrimSpace(w.Body.String()))

	if !strings.Contains(w.Body.String(), `"eligible":true`) {
		t.Logf("SECURE: the self probe honoured the contract -> %s (#1240 RT-3)", w.Body.String())
		return
	}
	t.Fatalf("REGRESSION — EXPLOITED: the documented `self -> {\"eligible\": false}` contract is bypassed " +
		"by shouting your own UUID; the endpoint answers with the gate verdict instead.")
}

// TestSelfCheckSurvivesANonCanonicalJWTClaim covers the OTHER side of RT-3,
// raised by Gitar: the path param is canonicalised but the JWT claim is not.
//
// GenerateAccessToken takes a plain string and no auth-middleware step parses
// it, so the claim is canonical only by the convention that every caller hands
// it a DB-rendered uuid. Comparing canonical against raw makes the self-check
// depend on that. Not exploitable today — the token is signed — so this is a
// lock on the invariant, not a proof of a live break.
func TestSelfCheckSurvivesANonCanonicalJWTClaim(t *testing.T) {
	self := rtUpperSelf
	shoutedClaim := strings.ToUpper(self)
	if shoutedClaim == self {
		t.Fatalf("premise failed: the shouted claim is not distinct from %q", self)
	}

	// Authenticate with the SHOUTED form as the claim, probe the canonical form.
	h, _, _ := rtHandler(pgLikeResponder(t))
	w := rtDo(rtRouter(h, shoutedClaim), http.MethodGet,
		"/api/v1/users/"+self+"/friend-request-eligibility", "")

	t.Logf("claim=%s target=%s -> %d %s", shoutedClaim, self, w.Code,
		strings.TrimSpace(w.Body.String()))
	if !strings.Contains(w.Body.String(), `"eligible":false`) {
		t.Fatalf("REGRESSION: a self-probe with a non-canonical claim escaped the "+
			"self-check and was answered by the gate instead: %d %s",
			w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// TestSendRequestSelfCheckSurvivesANonCanonicalJWTClaim mirrors the test above
// onto the OTHER sameUser call site.
//
// Raised by Gitar: the fix touched two call sites and only one was covered.
// TestRT3b exercises a non-canonical TARGET; this exercises a non-canonical
// CLAIM, which is the half `resolveTargetUserID` cannot normalise because the
// claim never passes through it. Covering one call site and assuming the other
// is the failure mode this whole file exists to catch.
func TestSendRequestSelfCheckSurvivesANonCanonicalJWTClaim(t *testing.T) {
	self := rtUpperSelf
	shoutedClaim := strings.ToUpper(self)
	if shoutedClaim == self {
		t.Fatalf("premise failed: the shouted claim is not distinct from %q", self)
	}

	// Authenticated as the SHOUTED form, targeting the canonical form of the
	// same user: a self-request wearing two different spellings.
	h, conn, _ := rtHandler(pgLikeResponder(t))
	w := rtDo(rtRouter(h, shoutedClaim), http.MethodPost,
		"/api/v1/friends/request", `{"user_id":"`+self+`"}`)

	t.Logf("claim=%s target=%s -> %d %s", shoutedClaim, self, w.Code,
		strings.TrimSpace(w.Body.String()))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("REGRESSION: a self-request with a non-canonical claim escaped the "+
			"self-check -> %d %s; statements=%v",
			w.Code, strings.TrimSpace(w.Body.String()), conn.statements())
	}
	if !strings.Contains(w.Body.String(), "yourself") {
		t.Fatalf("expected the self-check 400, got a different 400: %s", w.Body.String())
	}
	// The self-check must short-circuit BEFORE any database work.
	if mentions(conn.statements(), "allow_friend_requests_from") {
		t.Fatalf("the self-check should return before the eligibility read; statements=%v",
			conn.statements())
	}
}
