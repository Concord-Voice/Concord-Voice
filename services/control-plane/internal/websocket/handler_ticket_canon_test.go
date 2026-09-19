package websocket

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

// TestAuthenticateViaTicket_DenylistIsSpellingInvariant pins the ticket rail's
// own canonicalization (#3362).
//
// WHY THIS TEST HAD TO BE WRITTEN, stated plainly because the reasoning it
// corrects was published in the PR that added it. That PR asserted the ticket-
// rail change was "defense in depth whose mutant survives BY CONSTRUCTION",
// reasoning that AuthRequired canonicalizes before these gates run so nothing
// driving the public surface could discriminate. That is false, and it is false
// for a specific reason worth keeping:
//
//	THE TICKET RAIL NEVER READS AuthRequired's CONTEXT VALUE.
//
// `auth.ValidateTicket` returns a string Redis has been holding since an
// EARLIER request, written by `IssueTicket` on a different call — possibly on a
// different replica, possibly one deployed before this change. AuthRequired ran
// before the ticket was WRITTEN, not before these lines execute. The original
// ordering argument was about the wrong pair of events.
//
// The claim also rested on the wrong evidence: the mutant was judged by running
// the EXISTING suites and observing them stay green. That establishes "no test
// that already exists discriminates" — not "no test can". This file is the
// difference between those two statements.
//
// Not a live hole on a uniform fleet: post-#3362 `IssueTicket` stores a
// canonical `c.Get("user_id")`. The reachable windows are a rolling deploy —
// `ValidateTicket`'s own docblock already reasons about a 30 s window of legacy
// stored values — and any future mint path, which is the case canonicalUserID
// normalizes for in the first place. Defense in depth, now actually pinned.
func TestAuthenticateViaTicket_DenylistIsSpellingInvariant(t *testing.T) {
	redisClient := setupHubTestRedis(t)
	h := NewHandler(nil, nil, redisClient, testJWTSecret, nil, nil, nil)

	userID := uuid.New()
	ctx := context.Background()

	// The denylist entry is written at the CANONICAL id, which is what every
	// writer in the repo produces (they all source it from c.Get("user_id") or
	// from a `SELECT id FROM users` uuid column).
	require.NoError(t, redisClient.Set(ctx, middleware.UserDisabledKey(userID.String()), "1", 0).Err())

	for _, sp := range []struct{ name, stored string }{
		{"canonical_control", userID.String()},
		{"braced", "{" + userID.String() + "}"},
		{"uppercase", strings.ToUpper(userID.String())},
		{"dashless", strings.ReplaceAll(userID.String(), "-", "")},
	} {
		t.Run(sp.name, func(t *testing.T) {
			if sp.name != "canonical_control" {
				require.NotEqual(t, userID.String(), sp.stored,
					"a variant row must actually differ from the canonical form")
			}

			// Seed the ticket exactly as IssueTicket would, but with the spelling
			// under test as the STORED value — the thing a pre-#3362 replica or a
			// future mint path could legitimately have put there.
			ticket := "canon-" + sp.name + "-" + uuid.NewString()
			require.NoError(t, redisClient.Set(ctx, wsTicketKeyPfx+ticket, sp.stored, 30*time.Second).Err())

			r, _ := http.NewRequest("GET", wsTicketPath+ticket, nil)
			c := newGinContext(r)

			_, _, err := h.authenticateWebSocket(c)

			require.Error(t, err,
				"a disabled account must be refused for every spelling STORED IN THE TICKET")
			assert.Contains(t, err.Error(), "account disabled",
				"the refusal must be the denylist's, not an incidental failure elsewhere")
		})
	}
}
