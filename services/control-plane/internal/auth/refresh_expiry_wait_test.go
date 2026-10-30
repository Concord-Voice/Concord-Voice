package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A secret admitted before expiry must be checked again at final consumption.
// In particular, transaction-start NOW() remains stale across an authority wait.
// Refresh-row waits also exercise consumption after grace's successor lookup.
func TestRefreshExpiryAfterLockWait(t *testing.T) {
	for _, mode := range []string{"normal", "grace"} {
		for _, barrier := range []string{"user", "family", "refresh"} {
			for _, crossExpiry := range []bool{false, true} {
				name := mode + "/" + barrier + "/live control"
				if crossExpiry {
					name = mode + "/" + barrier + "/expires while waiting"
				}
				t.Run(name, func(t *testing.T) {
					rig := newFamilyTestRig(t)
					mint, _ := familyTestContext()
					access, presented, consumedSID, err := rig.h.IssueTokenPair(mint, rig.user, "")
					require.NoError(t, err)
					family, originalSID := assertFamilyAccess(t, rig, access)
					wantBefore := 1
					if mode == "grace" {
						first, firstResponse := familyTestContext()
						first.Request.Header.Set("X-Refresh-Token", presented)
						rig.h.Refresh(first)
						pair := familyResponse(t, firstResponse)
						nextFamily, nextSID := assertFamilyAccess(t, rig, pair.Access)
						require.Equal(t, family, nextFamily)
						consumedSID = nextSID
						wantBefore = 2
					}

					// Only refresh lifetime changes. The family and exact signed SID
					// membership remain canonical; no access identity is fabricated.
					var deadline time.Time
					require.NoError(t, rig.db.QueryRow(`UPDATE refresh_tokens
SET expires_at=clock_timestamp()+INTERVAL '3 seconds'
WHERE id=$1 AND revoked_at IS NULL AND remember_me=true RETURNING expires_at`, consumedSID).Scan(&deadline))
					before := refreshExpiryWaitSnapshot(t, rig)
					refreshExpiryWaitCounts(t, rig, wantBefore, wantBefore, 1)

					blocker, err := rig.db.BeginTx(context.Background(), nil)
					require.NoError(t, err)
					defer func() {
						err := blocker.Rollback()
						if err != nil && !errors.Is(err, sql.ErrTxDone) {
							t.Error(err)
						}
					}()
					var locked string
					queryPart := "FROM users"
					switch barrier {
					case "user":
						err = blocker.QueryRow(`SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, rig.user).Scan(&locked)
					case "family":
						queryPart = "FROM login_families"
						err = blocker.QueryRow(`SELECT family_id FROM login_families WHERE family_id=$1 FOR NO KEY UPDATE`, family).Scan(&locked)
					case "refresh":
						queryPart = "refresh_tokens"
						err = blocker.QueryRow(`SELECT id FROM refresh_tokens WHERE id=$1 FOR UPDATE`, consumedSID).Scan(&locked)
					}
					require.NoError(t, err)
					var blockerPID int
					require.NoError(t, blocker.QueryRow(`SELECT pg_backend_pid()`).Scan(&blockerPID))
					c, w := familyTestContext()
					requestCtx, cancel := context.WithCancel(c.Request.Context())
					defer cancel()
					c.Request = c.Request.WithContext(requestCtx)
					c.Request.Header.Set("X-Refresh-Token", presented)
					c.Request.Header.Set("X-Forwarded-Proto", "https")
					done := make(chan struct{})
					go func() { defer close(done); rig.h.Refresh(c) }()
					defer func() {
						cancel()
						if err := blocker.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
							t.Error(err)
						}
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Error("refresh did not stop after barrier release/cancellation")
						}
					}()

					// This request's SQL waiter proves that Refresh already admitted
					// the real secret and entered the mint transaction before expiry.
					var requestPID int
					var observedAt, transactionStart time.Time
					require.Eventually(t, func() bool {
						err := rig.db.QueryRow(`SELECT pid,xact_start,clock_timestamp()
FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))
AND query LIKE '%' || $2 || '%' AND state='active' AND wait_event_type='Lock'`, blockerPID, queryPart).
							Scan(&requestPID, &transactionStart, &observedAt)
						return err == nil
					}, 5*time.Second, 10*time.Millisecond, "real Refresh must wait at the selected SQL barrier")
					require.True(t, observedAt.Before(deadline), "secret must reach the barrier before expiry")
					require.True(t, transactionStart.Before(deadline), "premise: transaction-start NOW() predates expiry")
					select {
					case <-done:
						t.Fatal("refresh returned while its SQL barrier was held")
					default:
					}
					if crossExpiry {
						// Bounded waiting for the real persisted deadline, not a sleep
						// used to infer that a request probably reached a lock.
						require.Eventually(t, func() bool {
							var crossedWhileBlocked bool
							err := rig.db.QueryRow(`SELECT clock_timestamp()>$1 AND $2=ANY(pg_blocking_pids(pid))
FROM pg_stat_activity WHERE pid=$3 AND state='active' AND wait_event_type='Lock'`, deadline, blockerPID, requestPID).Scan(&crossedWhileBlocked)
							return err == nil && crossedWhileBlocked
						}, 5*time.Second, 10*time.Millisecond, "the observed request must remain blocked across actual expiry")
					}
					require.NoError(t, blocker.Commit())
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Fatal("refresh did not finish after barrier release")
					}

					if crossExpiry {
						assert.Equal(t, http.StatusUnauthorized, w.Code, "expired admitted refresh must be refused")
						assert.False(t, strings.Contains(w.Body.String(), "access_token"), "expired refresh must not publish access credentials")
						assert.False(t, strings.Contains(w.Body.String(), "refresh_token"), "expired refresh must not publish a successor secret")
						assert.True(t, len(w.Header().Values("Set-Cookie")) == 0, "expired refresh must not publish cookies")
						assert.Equal(t, before, refreshExpiryWaitSnapshot(t, rig), "failed consumption must leave complete identity rows unchanged")
						refreshExpiryWaitCounts(t, rig, wantBefore, wantBefore, 1)
						return
					}

					// The identical barrier and secret succeed when released live.
					var finishedLive bool
					require.NoError(t, rig.db.QueryRow(`SELECT clock_timestamp()<$1`, deadline).Scan(&finishedLive))
					require.True(t, finishedLive, "control must finish before the same short deadline")
					pair := familyResponse(t, w)
					nextFamily, nextSID := assertFamilyAccess(t, rig, pair.Access)
					assert.Equal(t, family, nextFamily)
					assert.NotEqual(t, originalSID, nextSID)
					assert.NotEqual(t, consumedSID, nextSID)
					var correctSuccessor, consumed bool
					require.NoError(t, rig.db.QueryRow(`SELECT predecessor_id=$2 AND remember_me AND expires_at>clock_timestamp()+INTERVAL '29 days'
FROM refresh_tokens WHERE id=$1 AND token_hash=$3 AND revoked_at IS NULL`, nextSID, consumedSID, HashRefreshToken(pair.Refresh)).Scan(&correctSuccessor))
					assert.True(t, correctSuccessor, "control must mint the exact remember-me successor")
					require.NoError(t, rig.db.QueryRow(`SELECT revoked_at IS NOT NULL AND revoked_at<$2 FROM refresh_tokens WHERE id=$1`, consumedSID, deadline).Scan(&consumed))
					assert.True(t, consumed, "control must actually consume before expiry")
					cookies := w.Result().Cookies()
					require.Len(t, cookies, 1)
					assert.Equal(t, "refresh_token", cookies[0].Name)
					// Gin SetCookie query-escapes the opaque base64 secret; match
					// the decoded cookie exactly without printing either credential.
					cookieSecret, err := url.QueryUnescape(cookies[0].Value)
					require.NoError(t, err)
					assert.True(t, cookieSecret == pair.Refresh, "cookie must carry delivered successor")
					assert.Equal(t, 30*24*60*60, cookies[0].MaxAge)
					assert.True(t, cookies[0].Secure)
					assert.True(t, cookies[0].HttpOnly)
					refreshExpiryWaitCounts(t, rig, wantBefore+1, wantBefore+1, 1)
				})
			}
		}
	}
}

// Digest full rows to avoid exposing token hashes or other identity data on failure.
func refreshExpiryWaitSnapshot(t *testing.T, rig familyTestRig) [32]byte {
	t.Helper()
	var snapshot string
	require.NoError(t, rig.db.QueryRow(`SELECT jsonb_build_object(
'user',(SELECT to_jsonb(u) FROM users u WHERE u.id=$1),
'families',(SELECT jsonb_agg(to_jsonb(f) ORDER BY f.family_id) FROM login_families f WHERE f.user_id=$1),
'sids',(SELECT jsonb_agg(to_jsonb(s) ORDER BY s.sid) FROM login_family_sids s WHERE s.user_id=$1),
'refresh',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM refresh_tokens r WHERE r.user_id=$1))::text`, rig.user).Scan(&snapshot))
	return sha256.Sum256([]byte(snapshot))
}

func refreshExpiryWaitCounts(t *testing.T, rig familyTestRig, wantRefresh, wantSIDs, wantActive int) {
	t.Helper()
	var refresh, sids, active, families, terminal, operations int
	require.NoError(t, rig.db.QueryRow(`SELECT
(SELECT count(*) FROM refresh_tokens WHERE user_id=$1),
(SELECT count(*) FROM login_family_sids WHERE user_id=$1),
(SELECT count(*) FROM refresh_tokens WHERE user_id=$1 AND revoked_at IS NULL),
(SELECT count(*) FROM login_families WHERE user_id=$1),
(SELECT count(*) FROM login_families WHERE user_id=$1 AND terminal_at IS NOT NULL),
(SELECT count(*) FROM login_family_revocations WHERE user_id=$1)`, rig.user).
		Scan(&refresh, &sids, &active, &families, &terminal, &operations))
	assert.Equal(t, wantRefresh, refresh, "refresh row count")
	assert.Equal(t, wantSIDs, sids, "SID membership row count")
	assert.Equal(t, wantActive, active, "unconsumed refresh row count (including expired rows)")
	assert.Equal(t, 1, families, "family count")
	assert.Zero(t, terminal, "expiry must not manufacture a terminalization")
	assert.Zero(t, operations, "expiry must not manufacture a revocation operation")
}
