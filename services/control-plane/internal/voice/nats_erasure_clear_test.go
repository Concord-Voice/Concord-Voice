package voice

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// erasureClearObserver records BOTH sinks the handler could reach: the targeted
// clear it is supposed to use, and the fleet-wide disconnect it must never use.
//
// Observing both is the point. An earlier version of these tests watched only
// the disconnect hook, which the handler stopped calling when the mechanism
// changed — so every assertion held even with the validation, the no-database
// refusal and the live-account rejection all removed. Watching only the sink a
// handler no longer uses is indistinguishable from having no test at all
// (CodeRabbit, PR #2840).
type erasureClearObserver struct {
	cleared     []uuid.UUID
	disconnects int
}

func newErasureClearSubscriber(o *erasureClearObserver) *NATSSubscriber {
	return &NATSSubscriber{
		log:                                  logger.New("test"),
		clearErasedSenderHook:                func(id uuid.UUID) { o.cleared = append(o.cleared, id) },
		disconnectAllRichPresenceClientsHook: func() { o.disconnects++ },
	}
}

func withTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

// The accept path must actually CLEAR, and clear the sender it was told about.
func TestPresenceErasureClearClearsTheNamedSender(t *testing.T) {
	erased := uuid.New()
	var o erasureClearObserver
	s := newErasureClearSubscriber(&o)
	s.db = withTestDB(t) // the row was never inserted, exactly as after an erasure

	s.handlePresenceErasureCleared([]byte(`{"user_id":"` + erased.String() + `"}`))

	require.Equal(t, []uuid.UUID{erased}, o.cleared,
		"an erased account's clear must reach the targeted sink, naming that sender")
	require.Zero(t, o.disconnects,
		"and must never reach the fleet-wide disconnect")
}

// A clear naming an account that STILL EXISTS must be rejected outright — the
// one case the existence check genuinely proves wrong.
func TestPresenceErasureClearRejectsALiveAccount(t *testing.T) {
	db := withTestDB(t)
	live := uuid.New()
	_, err := db.Exec(`
		INSERT INTO users (id, username, email, password_hash, created_at, updated_at)
		VALUES ($1, $2, $3, 'x', NOW(), NOW())`,
		live, "erasureclearlive_"+live.String()[:8], live.String()+"@example.test")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, live) })

	var o erasureClearObserver
	s := newErasureClearSubscriber(&o)
	s.db = db

	s.handlePresenceErasureCleared([]byte(`{"user_id":"` + live.String() + `"}`))

	require.Empty(t, o.cleared, "a live account's status must not be cleared")
	require.Zero(t, o.disconnects)
}

// Malformed payloads arrive over the wire and must reach neither sink.
func TestPresenceErasureClearRejectsBadPayloads(t *testing.T) {
	db := withTestDB(t)
	for name, payload := range map[string]string{
		"not json":     `{`,
		"missing id":   `{}`,
		"empty id":     `{"user_id":""}`,
		"not a uuid":   `{"user_id":"definitely-not-a-uuid"}`,
		"wrong type":   `{"user_id":42}`,
		"null payload": `null`,
	} {
		t.Run(name, func(t *testing.T) {
			var o erasureClearObserver
			s := newErasureClearSubscriber(&o)
			s.db = db

			s.handlePresenceErasureCleared([]byte(payload))

			require.Empty(t, o.cleared, "a malformed clear must clear nobody")
			require.Zero(t, o.disconnects, "and must not escalate to a disconnect")
		})
	}
}

// Without a database the claim cannot be authorized at all, so it is refused
// rather than acted on.
func TestPresenceErasureClearRefusesWhenUnverifiable(t *testing.T) {
	var o erasureClearObserver
	s := newErasureClearSubscriber(&o) // no db wired

	s.handlePresenceErasureCleared([]byte(`{"user_id":"` + uuid.New().String() + `"}`))

	require.Empty(t, o.cleared, "an unverifiable claim must not be acted on")
	require.Zero(t, o.disconnects)
}

// No input may reach the fleet-wide disconnect. That sink was a denial-of-service
// primitive — red-team PoCs proved a forged random UUID, an unbounded replay and
// a lookup error all reached it — and the fix is that the action is now
// proportional to the claim rather than merely harder to forge.
func TestErasureClearNeverFleetDisconnects(t *testing.T) {
	db := withTestDB(t)
	forged := uuid.New().String()

	for name, payload := range map[string]string{
		"forged random uuid":  `{"user_id":"` + forged + `"}`,
		"replay of the same":  `{"user_id":"` + forged + `"}`,
		"another random uuid": `{"user_id":"` + uuid.New().String() + `"}`,
		"malformed":           `{"user_id":"nope"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var o erasureClearObserver
			s := newErasureClearSubscriber(&o)
			s.db = db

			s.handlePresenceErasureCleared([]byte(payload))

			require.Zero(t, o.disconnects,
				"no erasure-clear input may reach the fleet-wide disconnect")
		})
	}
}

// Publisher and subscriber must name the same subject. They live in different
// packages, and the subscriber deliberately registers the publisher's exported
// constant rather than a local literal — this pins that value so a rename cannot
// silently orphan the subscription.
func TestErasureClearSubjectMatchesThePublisher(t *testing.T) {
	require.Equal(t, "presence.erasure.cleared", users.NATSSubjectPresenceErasureCleared)
}

// A lookup error must fail OPEN — proceed with the clear rather than suppress
// it. The clear is inert when the claim is false, so letting it through costs
// nothing, whereas failing closed would let an unreachable database block a
// legitimate erasure clear.
func TestPresenceErasureClearProceedsWhenTheLookupFails(t *testing.T) {
	db := withTestDB(t)
	require.NoError(t, db.Close()) // every query on it now errors

	erased := uuid.New()
	var o erasureClearObserver
	s := newErasureClearSubscriber(&o)
	s.db = db

	s.handlePresenceErasureCleared([]byte(`{"user_id":"` + erased.String() + `"}`))

	require.Equal(t, []uuid.UUID{erased}, o.cleared,
		"a lookup error must not suppress a legitimate clear")
	require.Zero(t, o.disconnects)
}

type recordingUnsubscriber struct{ calls int }

func (r *recordingUnsubscriber) Unsubscribe() error { r.calls++; return nil }

// Subscribe must leave nothing running when a later step in the same call fails.
// Before this, a failed erasure subscribe returned with the dispatcher goroutine
// and the wildcard subscription still live, and a retry then failed with
// "voice lifecycle subscriber already started" — a leak that also made the
// failure unrecoverable (CodeRabbit, PR #2840).
func TestUnwindLifecycleSubscriptionReleasesEverything(t *testing.T) {
	sub := &recordingUnsubscriber{}
	dispatcher := newVoiceLifecycleDispatcher(func(string, []byte) {}, func(voiceLifecycleDropCounts) {})

	s := &NATSSubscriber{log: logger.New("test")}
	s.lifecycleDispatcher = dispatcher

	s.unwindLifecycleSubscription(sub, dispatcher)

	require.Equal(t, 1, sub.calls, "the wildcard subscription must be released")
	s.lifecycleDispatchMu.Lock()
	defer s.lifecycleDispatchMu.Unlock()
	require.Nil(t, s.lifecycleDispatcher,
		"the dispatcher must be cleared so a retry is not refused as already started")
}

// It must also tolerate being called before either was established.
func TestUnwindLifecycleSubscriptionToleratesNils(t *testing.T) {
	s := &NATSSubscriber{log: logger.New("test")}

	require.NotPanics(t, func() { s.unwindLifecycleSubscription(nil, nil) })
}
