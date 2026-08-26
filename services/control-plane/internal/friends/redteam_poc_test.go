package friends

// Regression tests for the #1240 privacy gate, adopted from the red-team pass
// on PR #2911.
//
// These began as exploit PoCs that PASSED while the bugs existed. They are
// INVERTED: each now passes only while the fix holds and fails if it is undone.
// Each was falsified against a mutation that COMPILES — a build error would
// read exactly like a red test and prove nothing.
//
// Two proven breaks:
//
//	RT-1  BYPASS. POST /friends/codes/:code/claim writes a friendships row for
//	      the same (requester, target) pair the gate refuses, and never issues a
//	      single statement mentioning allow_friend_requests_from.
//
//	RT-2  ORACLE. GET /users/:user_id/friend-request-eligibility composes with
//	      POST /friends/request to distinguish privacy-blocked from user-blocked,
//	      which spec 2026-08-21-1239 section 2 states as a mandatory criterion
//	      that "cannot" be done.

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	rtAttacker = "11111111-1111-4111-8111-111111111111" // requester B
	rtVictim   = "22222222-2222-4222-8222-222222222222" // target A, mode = nobody
	rtCode     = "AB23CD45"
)

// rtHandler builds a real *Handler over the recording driver.
func rtHandler(respond recResponder) (*Handler, *recConn, *bytes.Buffer) {
	db, conn := newRecordingDB(respond)
	logs := &bytes.Buffer{}
	return NewHandler(db, logger.NewWithWriter(logs), nil), conn, logs
}

func rtRouter(h *Handler, actingAs string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	auth := func(c *gin.Context) { c.Set("user_id", actingAs) }
	r.POST("/api/v1/friends/request", auth, h.SendRequest)
	r.POST("/api/v1/friends/codes/:code/claim", auth, h.ClaimFriendCode)
	r.GET("/api/v1/users/:user_id/friend-request-eligibility", auth, h.GetFriendRequestEligibility)
	return r
}

func rtDo(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// Shared responder fixture.
//
// One world: victim A exists, has allow_friend_requests_from = 'nobody', shares
// no server with attacker B, and holds one live, unrevoked, unlimited-use
// friend code. blockedStatus, when non-empty, is the friendships.status row
// that exists between the two.
// ---------------------------------------------------------------------------

type rtWorld struct {
	// eligible is what the gate CASE statement evaluates to for (A, B).
	eligible bool
	// existingStatus is the friendships.status row between A and B, "" for none.
	existingStatus string
	// autoAccept is friend_codes.auto_accept for rtCode.
	autoAccept bool
}

func (w rtWorld) responder() recResponder {
	return func(q string, _ []driver.NamedValue) (*recRows, error) {
		switch {
		// --- the #1240 privacy gate (internal/friends/privacy.go) ---
		case strings.Contains(q, "allow_friend_requests_from"):
			return row([]string{"eligible"}, w.eligible), nil

		// --- friendship conflict probe ---
		case strings.Contains(q, "SELECT status FROM friendships"):
			if w.existingStatus == "" {
				return nil, nil // zero rows -> sql.ErrNoRows
			}
			return row([]string{"status"}, w.existingStatus), nil

		// --- friend-code preview (pre-transaction, unlocked) ---
		case strings.Contains(q, "SELECT user_id, auto_accept FROM friend_codes"):
			return row([]string{"user_id", "auto_accept"}, rtVictim, w.autoAccept), nil

		// --- friend-code locked read ---
		case strings.Contains(q, "FROM friend_codes WHERE code = $1 FOR UPDATE"):
			return row(
				[]string{"id", "user_id", "max_uses", "use_count", "expires_at", "is_revoked", "auto_accept"},
				"33333333-3333-4333-8333-333333333333", // code id
				rtVictim,                               // owner
				nil,                                    // max_uses NULL == unlimited
				int64(0),                               // use_count
				time.Now().UTC().Add(12*time.Hour),     // expires_at
				false,                                  // is_revoked
				w.autoAccept,
			), nil

		case strings.Contains(q, "INSERT INTO friendships"):
			return row([]string{"id", "created_at"},
				"44444444-4444-4444-8444-444444444444", "2026-08-22T00:00:00Z"), nil

		case strings.Contains(q, "UPDATE friend_codes SET use_count"):
			return nil, nil

		case strings.Contains(q, "SELECT username, display_name, avatar_url FROM users"):
			return row([]string{"username", "display_name", "avatar_url"}, "someone", nil, nil), nil

		case strings.Contains(q, "SELECT id FROM users WHERE LOWER(username)"):
			return row([]string{"id"}, rtVictim), nil
		}
		return nil, nil
	}
}

// ===========================================================================
// RT-1 -- BYPASS: the friend-code claim path never consults the gate.
// ===========================================================================

func TestRT1_FriendCodeClaimBypassesTheFriendRequestPrivacyGate(t *testing.T) {
	world := rtWorld{eligible: false} // A's mode == 'nobody' => gate says NO

	// ---- Arm 1 (control): the gated route refuses. ----
	hSend, connSend, _ := rtHandler(world.responder())
	wSend := rtDo(rtRouter(hSend, rtAttacker), http.MethodPost,
		"/api/v1/friends/request", `{"user_id":"`+rtVictim+`"}`)

	if wSend.Code != http.StatusForbidden {
		t.Fatalf("control arm: expected the gate to refuse with 403, got %d: %s",
			wSend.Code, wSend.Body.String())
	}
	if !mentions(connSend.statements(), "allow_friend_requests_from") {
		t.Fatalf("control arm: SendRequest never consulted the gate; fixture is wrong")
	}
	if mentions(connSend.statements(), "INSERT INTO friendships") {
		t.Fatalf("control arm: the gate let a friendships row through")
	}

	// ---- Arm 2 (exploit): the ungated route writes the row. ----
	hClaim, connClaim, logs := rtHandler(world.responder())
	wClaim := rtDo(rtRouter(hClaim, rtAttacker), http.MethodPost,
		"/api/v1/friends/codes/"+rtCode+"/claim", "")

	stmts := connClaim.statements()
	t.Logf("claim-path statements:\n  %s", strings.Join(stmts, "\n  "))

	if mentions(stmts, "allow_friend_requests_from") || mentions(stmts, "privacy_settings") {
		t.Log("SECURE: the claim path consulted the privacy gate and refused (#1240 RT-1)")
		return
	}
	if !mentions(stmts, "INSERT INTO friendships") {
		t.Fatalf("claim path issued no friendships INSERT; got %v", stmts)
	}
	if wClaim.Code != http.StatusOK {
		t.Fatalf("claim expected 200, got %d: %s", wClaim.Code, wClaim.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(wClaim.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode claim body: %v (%s)", err, wClaim.Body.String())
	}
	if body["friendship_id"] == nil || body["status"] != "pending" {
		t.Fatalf("unexpected claim body: %s", wClaim.Body.String())
	}

	t.Fatalf("REGRESSION — EXPLOITED: gate refused POST /friends/request with 403 %q for the SAME pair, "+
		"while POST /friends/codes/%s/claim created friendship %v status=%v. Logs: %s",
		wSend.Body.String(), rtCode, body["friendship_id"], body["status"],
		strings.TrimSpace(logs.String()))
}

// TestRT1b shows the worst form: an auto_accept code turns the same bypass into
// an immediately-ACCEPTED friendship -- not a pending request the victim could
// decline -- while the victim's setting reads 'nobody'.
func TestRT1b_AutoAcceptCodeManufacturesAnAcceptedFriendshipUnderNobody(t *testing.T) {
	world := rtWorld{eligible: false, autoAccept: true}

	h, conn, _ := rtHandler(world.responder())
	w := rtDo(rtRouter(h, rtAttacker), http.MethodPost,
		"/api/v1/friends/codes/"+rtCode+"/claim", "")

	// The gate check comes FIRST: once the fix is in, the claim is refused, so
	// asserting the 200 before this would fail on the secure path.
	if mentions(conn.statements(), "allow_friend_requests_from") {
		if w.Code == http.StatusOK {
			t.Fatalf("REGRESSION: the gate was consulted but the claim still succeeded: %s",
				w.Body.String())
		}
		t.Logf("SECURE: the claim path consulted the privacy gate and refused -> %d %s (#1240 RT-1)",
			w.Code, strings.TrimSpace(w.Body.String()))
		return
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "accepted" {
		t.Fatalf("expected an accepted friendship, got %s", w.Body.String())
	}
	t.Fatalf("REGRESSION — EXPLOITED: allow_friend_requests_from='nobody' and the attacker is now an "+
		"ACCEPTED friend: %s", w.Body.String())
}

// ===========================================================================
// RT-2 -- ORACLE: eligibility + send distinguishes privacy-blocked from
// user-blocked, the exact pair spec section 2 says cannot be distinguished.
// ===========================================================================

func TestRT2_EligibilityEndpointDeanonymisesTheSharedRejection(t *testing.T) {
	type probe struct {
		name     string
		world    rtWorld
		sendCode int
		sendBody string
		eligible bool
	}

	// Scenario P: A's mode is 'nobody'. No block row.
	pWorld := rtWorld{eligible: false, existingStatus: ""}
	// Scenario U: A's mode is 'everyone' (gate says yes) but A BLOCKED B.
	uWorld := rtWorld{eligible: true, existingStatus: "blocked"}

	results := make([]probe, 0, 2)
	for _, tc := range []struct {
		name  string
		world rtWorld
	}{{"privacy-blocked", pWorld}, {"user-blocked", uWorld}} {
		hs, _, sendLogs := rtHandler(tc.world.responder())
		ws := rtDo(rtRouter(hs, rtAttacker), http.MethodPost,
			"/api/v1/friends/request", `{"user_id":"`+rtVictim+`"}`)

		he, _, _ := rtHandler(tc.world.responder())
		we := rtDo(rtRouter(he, rtAttacker), http.MethodGet,
			"/api/v1/users/"+rtVictim+"/friend-request-eligibility", "")

		var eb map[string]any
		if err := json.Unmarshal(we.Body.Bytes(), &eb); err != nil {
			t.Fatalf("%s: decode eligibility: %v (%s)", tc.name, err, we.Body.String())
		}
		flag, ok := eb["eligible"].(bool)
		if !ok {
			t.Fatalf("%s: eligibility body has no boolean 'eligible': %s", tc.name, we.Body.String())
		}
		if s := strings.TrimSpace(sendLogs.String()); s != "" {
			t.Logf("%s: SendRequest log output: %s", tc.name, s)
		}
		results = append(results, probe{tc.name, tc.world, ws.Code, ws.Body.String(), flag})
	}

	p, u := results[0], results[1]
	t.Logf("privacy-blocked: POST -> %d %s | GET eligibility -> %v", p.sendCode, p.sendBody, p.eligible)
	t.Logf("user-blocked   : POST -> %d %s | GET eligibility -> %v", u.sendCode, u.sendBody, u.eligible)

	// Precondition: the shared rejection really is byte-identical (spec's claim).
	if p.sendCode != u.sendCode || p.sendBody != u.sendBody {
		t.Fatalf("precondition failed: the two rejections already differ (%d %q vs %d %q)",
			p.sendCode, p.sendBody, u.sendCode, u.sendBody)
	}
	if p.sendCode != http.StatusForbidden {
		t.Fatalf("precondition failed: expected 403 from both, got %d", p.sendCode)
	}

	// The break: one extra unprivileged GET separates them.
	if p.eligible == u.eligible {
		t.Logf("SECURE: the eligibility bit is %v for BOTH privacy-blocked and user-blocked, "+
			"so the shared 403 stays indistinguishable (#1240 RT-2)", p.eligible)
		return
	}
	t.Fatalf("REGRESSION — EXPLOITED: the 403 is identical, but eligible=%v vs eligible=%v "+
		"tells the requester whether the target BLOCKED them or merely set a privacy mode. "+
		"Spec section 2 states this distinction cannot be made.", p.eligible, u.eligible)
}
