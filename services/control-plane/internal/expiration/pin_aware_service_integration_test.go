//go:build integration

package expiration

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinScope drives one context (a channel or a DM conversation) through the
// #3459 state machine. Every test below runs against both, because the two
// scopes carry separate copies of every statement.
type pinScope struct {
	name   string
	table  string
	id     string
	scope  scope
	insert func(t *testing.T, created time.Time, pinned bool, expires, held *time.Time) string
	start  func(t *testing.T, request Request) Transition
	pin    func(t *testing.T, messageID string)
	unpin  func(t *testing.T, messageID string)
	expire func(e *ContextExpirer, transition Transition) int
	// notice is what a successful expiry announces.
	notice purgeNotice
}

func pinScopes(f expirationFixture) []pinScope {
	return []pinScope{
		{
			name: "channel", table: "messages", id: f.channel.String(), scope: channelScope,
			insert: func(t *testing.T, created time.Time, pinned bool, expires, held *time.Time) string {
				t.Helper()
				var id string
				require.NoError(t, f.db.QueryRow(`
INSERT INTO messages (channel_id, user_id, content, created_at, pinned_at, pinned_by, expires_at, held_expires_at)
VALUES ($1, $2, 'ciphertext', $3, CASE WHEN $4 THEN $3::timestamptz END, CASE WHEN $4 THEN $2::uuid END, $5, $6)
RETURNING id`, f.channel, f.owner, created, pinned, expires, held).Scan(&id))
				return id
			},
			start: func(t *testing.T, request Request) Transition {
				t.Helper()
				return startTransition(t, f.db, func(tx *sql.Tx) (Transition, error) {
					return NewService(f.db).StartChannelTransition(context.Background(), tx, f.channel.String(), request)
				})
			},
			// The four pin/unpin statements below are the shapes in
			// internal/messages/pinning.go (an unaliased messages UPDATE, an
			// aliased `dm_messages dm` UPDATE), each taking the parent lock the
			// handler takes first.
			pin: func(t *testing.T, messageID string) {
				t.Helper()
				inParentLockedTx(t, f.db, `SELECT 1 FROM channels WHERE id = $1 FOR SHARE`, f.channel, `
UPDATE messages
SET pinned_at = NOW(), pinned_by = $1, updated_at = NOW(),
    `+ChannelPinHoldAssignments+`
WHERE id = $2 AND pinned_at IS NULL`, f.owner, messageID)
			},
			unpin: func(t *testing.T, messageID string) {
				t.Helper()
				inParentLockedTx(t, f.db, `SELECT 1 FROM channels WHERE id = $1 FOR SHARE`, f.channel, `
UPDATE messages SET pinned_at = NULL, pinned_by = NULL, updated_at = NOW(),
    `+UnpinReleaseAssignments+`
WHERE id = $1 AND pinned_at IS NOT NULL`, messageID)
			},
			expire: func(e *ContextExpirer, transition Transition) int {
				return e.ExpireChannelPins(context.Background(), f.channel.String(), f.server.String(), transition)
			},
			notice: purgeNotice{kind: "server", id: f.server.String()},
		},
		{
			name: "conversation", table: "dm_messages", id: f.conversation.String(), scope: conversationScope,
			insert: func(t *testing.T, created time.Time, pinned bool, expires, held *time.Time) string {
				t.Helper()
				var id string
				require.NoError(t, f.db.QueryRow(`
INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, pinned_at, pinned_by, expires_at, held_expires_at)
VALUES ($1, $2, 'ciphertext', 'user', $3, CASE WHEN $4 THEN $3::timestamptz END, CASE WHEN $4 THEN $2::uuid END, $5, $6)
RETURNING id`, f.conversation, f.owner, created, pinned, expires, held).Scan(&id))
				return id
			},
			start: func(t *testing.T, request Request) Transition {
				t.Helper()
				return startTransition(t, f.db, func(tx *sql.Tx) (Transition, error) {
					return NewService(f.db).StartConversationTransition(context.Background(), tx, f.conversation.String(), request)
				})
			},
			pin: func(t *testing.T, messageID string) {
				t.Helper()
				inParentLockedTx(t, f.db, `SELECT 1 FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, f.conversation, `
UPDATE dm_messages dm
SET pinned_at = NOW(), pinned_by = $1, updated_at = NOW(),
    `+ConversationPinHoldAssignments+`
WHERE dm.id = $2 AND dm.conversation_id = $3 AND dm.pinned_at IS NULL`, f.owner, messageID, f.conversation)
			},
			unpin: func(t *testing.T, messageID string) {
				t.Helper()
				inParentLockedTx(t, f.db, `SELECT 1 FROM dm_conversations WHERE id = $1 FOR NO KEY UPDATE`, f.conversation, `
UPDATE dm_messages dm SET pinned_at = NULL, pinned_by = NULL, updated_at = NOW(),
    `+UnpinReleaseAssignments+`
WHERE dm.id = $1 AND dm.conversation_id = $2 AND dm.pinned_at IS NOT NULL`, messageID, f.conversation)
			},
			expire: func(e *ContextExpirer, transition Transition) int {
				return e.ExpireConversationPins(context.Background(), f.conversation.String(), false, transition)
			},
			notice: purgeNotice{kind: "dm", id: f.conversation.String()},
		},
	}
}

func startTransition(t *testing.T, db *sql.DB, start func(*sql.Tx) (Transition, error)) Transition {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	transition, err := start(tx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	requireIHeld(t, db)
	return transition
}

func inParentLockedTx(t *testing.T, db *sql.DB, lockSQL string, parent any, statement string, args ...any) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback transaction: %v", rollbackErr)
		}
	}()
	_, err = tx.Exec(lockSQL, parent)
	require.NoError(t, err)
	result, err := tx.Exec(statement, args...)
	require.NoError(t, err)
	affected, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "the pin/unpin statement must apply to exactly the one row")
	require.NoError(t, tx.Commit())
	requireIHeld(t, db)
}

// requireIHeld asserts I-HELD over the whole database: a held stamp exists only
// on a pinned row with no live stamp, in a context that keeps pins.
func requireIHeld(t *testing.T, db *sql.DB) {
	t.Helper()
	var violations int
	require.NoError(t, db.QueryRow(`
SELECT
  (SELECT count(*) FROM messages m JOIN channels c ON c.id = m.channel_id
   WHERE m.held_expires_at IS NOT NULL
     AND NOT (m.pinned_at IS NOT NULL AND m.expires_at IS NULL
              AND c.expiration_window_seconds IS NOT NULL AND NOT c.expiration_include_pinned))
+ (SELECT count(*) FROM dm_messages m JOIN dm_conversations c ON c.id = m.conversation_id
   WHERE m.held_expires_at IS NOT NULL
     AND NOT (m.pinned_at IS NOT NULL AND m.expires_at IS NULL
              AND c.expiration_window_seconds IS NOT NULL AND NOT c.expiration_include_pinned))`).Scan(&violations))
	require.Zero(t, violations, "I-HELD: held_expires_at only on a pinned, unstamped row of a context that keeps pins")
}

// stampsOf returns a row's (expires_at, held_expires_at), nil for NULL.
func stampsOf(t *testing.T, db *sql.DB, table, id string) (expires, held *time.Time) {
	t.Helper()
	var query string
	switch table {
	case "messages":
		query = `SELECT expires_at, held_expires_at FROM messages WHERE id = $1`
	case "dm_messages":
		query = `SELECT expires_at, held_expires_at FROM dm_messages WHERE id = $1`
	default:
		t.Fatalf("unsupported table %q", table)
	}
	var e, h sql.NullTime
	require.NoError(t, db.QueryRow(query, id).Scan(&e, &h))
	if e.Valid {
		value := e.Time.UTC()
		expires = &value
	}
	if h.Valid {
		value := h.Time.UTC()
		held = &value
	}
	return expires, held
}

func requireStamps(t *testing.T, db *sql.DB, table, id string, wantExpires, wantHeld *time.Time, msg string) {
	t.Helper()
	expires, held := stampsOf(t, db, table, id)
	if wantExpires == nil {
		assert.Nil(t, expires, "%s: expires_at", msg)
	} else if assert.NotNil(t, expires, "%s: expires_at", msg) {
		assert.True(t, wantExpires.Equal(*expires), "%s: expires_at = %v, want %v", msg, *expires, *wantExpires)
	}
	if wantHeld == nil {
		assert.Nil(t, held, "%s: held_expires_at", msg)
	} else if assert.NotNil(t, held, "%s: held_expires_at", msg) {
		assert.True(t, wantHeld.Equal(*held), "%s: held_expires_at = %v, want %v", msg, *held, *wantHeld)
	}
}

func messageExists(t *testing.T, db *sql.DB, table, id string) bool {
	t.Helper()
	var query string
	switch table {
	case "messages":
		query = `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1)`
	case "dm_messages":
		query = `SELECT EXISTS (SELECT 1 FROM dm_messages WHERE id = $1)`
	default:
		t.Fatalf("unsupported table %q", table)
	}
	var exists bool
	require.NoError(t, db.QueryRow(query, id).Scan(&exists))
	return exists
}

func resumeAll(t *testing.T, f expirationFixture, ps pinScope, policy Policy) {
	t.Helper()
	service := NewService(f.db)
	var got Policy
	var err error
	if ps.scope.name == "channel" {
		got, err = service.ResumeChannel(context.Background(), ps.id, policy.Revision)
	} else {
		got, err = service.ResumeConversation(context.Background(), ps.id, policy.Revision)
	}
	require.NoError(t, err)
	require.False(t, got.BackfillPending)
	requireIHeld(t, f.db)
}

func realEngine(db *sql.DB) *purge.Engine {
	engineLog := logger.New("test")
	return purge.NewEngine(db, engineLog, purge.NewReaper(db, engineLog, nil), purge.MaxExpiryCandidateIDs)
}

func sweeperAt(t *testing.T, db *sql.DB, clock func(context.Context) (time.Time, error)) *Sweeper {
	t.Helper()
	engine := realEngine(db)
	s, err := NewSweeper(SweeperDeps{
		DB: db, Clock: clock, RunExpiryBatch: engine.RunExpiryBatch,
		EmitDMPurged: func(context.Context, string, time.Time) {}, EmitServerPurged: func(context.Context, string, time.Time) {},
		Log: logger.New("test"),
	})
	require.NoError(t, err)
	return s
}

func us(at time.Time) time.Time { return at.UTC().Truncate(time.Microsecond) }

// TestPinAware_PinFragmentCorrelationColumnsResolveToTheMessage pins the
// assumption the pin fragments rest on: their parent subquery names the bare
// channel_id / conversation_id, which binds to the UPDATE's target row only
// while the parent table has no column of that name.
func TestPinAware_PinFragmentCorrelationColumnsResolveToTheMessage(t *testing.T) {
	f := newExpirationFixture(t)
	var shadowing int
	require.NoError(t, f.db.QueryRow(`
SELECT count(*) FROM information_schema.columns
WHERE table_schema = current_schema()
  AND ((table_name = 'channels' AND column_name = 'channel_id')
    OR (table_name = 'dm_conversations' AND column_name = 'conversation_id'))`).Scan(&shadowing))
	require.Zero(t, shadowing, "a parent column with this name would capture the pin fragment's correlation")
}

// TestPinAware_KeptPinsSurviveSweepAndPreflight is AC1: under a keep-pins
// policy a pinned message is never deleted by the sweeper or the preflight,
// even at a sweep cutoff past its would-be expiry, while unpinned messages in
// the same context still expire.
func TestPinAware_KeptPinsSurviveSweepAndPreflight(t *testing.T) {
	f := newExpirationFixture(t)
	created := us(time.Now().Add(-48 * time.Hour))
	window := 3600
	type rows struct{ unpinned, pinned string }
	seeded := map[string]rows{}
	for _, ps := range pinScopes(f) {
		unpinned := ps.insert(t, created, false, nil, nil)
		pinned := ps.insert(t, created, true, nil, nil)
		seeded[ps.name] = rows{unpinned, pinned}

		transition := ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply", IncludePinned: ptr(false)})
		require.True(t, transition.Current.KeepsPins())
		require.True(t, transition.Current.BackfillPending)
		resumeAll(t, f, ps, transition.Current)

		wouldBe := created.Add(time.Hour)
		requireStamps(t, f.db, ps.table, unpinned, &wouldBe, nil, ps.name+" unpinned row is stamped")
		requireStamps(t, f.db, ps.table, pinned, nil, &wouldBe, ps.name+" pinned row is held, not stamped")
	}

	farFuture := time.Now().Add(60 * 24 * time.Hour)
	s := sweeperAt(t, f.db, func(context.Context) (time.Time, error) { return farFuture, nil })
	require.NoError(t, s.RunPreflight(context.Background()))

	for _, ps := range pinScopes(f) {
		assert.False(t, messageExists(t, f.db, ps.table, seeded[ps.name].unpinned), "%s: an unpinned row still expires", ps.name)
		assert.True(t, messageExists(t, f.db, ps.table, seeded[ps.name].pinned), "%s: a kept pin survives a sweep past its would-be expiry", ps.name)
	}
	requireIHeld(t, f.db)
}

// TestPinAware_ToggleOffHoldsFutureStampsAndLeavesDueOnes is AC3: turning
// include_pinned off moves every pinned stamp that has not passed into held,
// never rescues one that has (C-NORES), and the next sweep deletes the due one.
func TestPinAware_ToggleOffHoldsFutureStampsAndLeavesDueOnes(t *testing.T) {
	f := newExpirationFixture(t)
	window := 86400
	now := us(time.Now())
	future, past := now.Add(23*time.Hour), now.Add(-24*time.Hour)
	type rows struct{ futurePin, duePin, unpinned string }
	seeded := map[string]rows{}
	for _, ps := range pinScopes(f) {
		on := ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(true)})
		require.True(t, on.Current.IncludePinned)

		r := rows{
			futurePin: ps.insert(t, now.Add(-time.Hour), true, &future, nil),
			duePin:    ps.insert(t, now.Add(-48*time.Hour), true, &past, nil),
			unpinned:  ps.insert(t, now.Add(-time.Hour), false, &future, nil),
		}
		seeded[ps.name] = r

		off := ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(false)})
		assert.True(t, off.Previous.IncludePinned, "Previous carries E read under the lock")
		assert.False(t, off.Current.IncludePinned, "Current carries the new E")
		assert.Empty(t, off.DuePinnedIDs, "turning keep ON collects nothing")

		requireStamps(t, f.db, ps.table, r.futurePin, nil, &future, ps.name+" future pin is held")
		requireStamps(t, f.db, ps.table, r.duePin, &past, nil, ps.name+" due pin is not rescued")
		requireStamps(t, f.db, ps.table, r.unpinned, &future, nil, ps.name+" unpinned row is untouched")
	}

	_, err := sweeperAt(t, f.db, nil).RunPass(context.Background())
	require.NoError(t, err)
	for _, ps := range pinScopes(f) {
		r := seeded[ps.name]
		assert.False(t, messageExists(t, f.db, ps.table, r.duePin), "%s: the next sweep deletes the due pin", ps.name)
		assert.True(t, messageExists(t, f.db, ps.table, r.futurePin), "%s: the held pin survives", ps.name)
		assert.True(t, messageExists(t, f.db, ps.table, r.unpinned), "%s: a future unpinned stamp survives", ps.name)
	}
	requireIHeld(t, f.db)
}

// TestPinAware_MovesLeaveAStampExactlyAtTheCutoff pins C-NORES at its boundary,
// where the AC3 test cannot reach: C is the database's clock at the policy
// write, so only a direct call can stamp a row at exactly C. A stamp equal to
// the cutoff has passed and must not move.
func TestPinAware_MovesLeaveAStampExactlyAtTheCutoff(t *testing.T) {
	f := newExpirationFixture(t)
	window := 3600
	cutoff := us(time.Now().Add(-time.Minute))
	for _, ps := range pinScopes(f) {
		t.Run(ps.name, func(t *testing.T) {
			ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(false)})
			atCutoff := ps.insert(t, cutoff.Add(-time.Hour), true, &cutoff, nil)
			justAfter := cutoff.Add(time.Microsecond)
			after := ps.insert(t, cutoff.Add(-time.Hour), true, &justAfter, nil)

			_, err := f.db.Exec(ps.scope.holdPinnedSQL, ps.id, cutoff)
			require.NoError(t, err)
			requireStamps(t, f.db, ps.table, atCutoff, &cutoff, nil, "hold move: a stamp equal to C stays")
			requireStamps(t, f.db, ps.table, after, nil, &justAfter, "hold move: a stamp one microsecond after C moves")

			// The apply fragment's eligibility draws the same line.
			atCutoffApply := ps.insert(t, cutoff.Add(-time.Hour), true, &cutoff, nil)
			_, err = f.db.Exec(ps.scope.reconcileApplySQL, ps.id, cutoff, window, true)
			require.NoError(t, err)
			requireStamps(t, f.db, ps.table, atCutoffApply, &cutoff, nil, "apply: a stamp equal to C is due, not eligible")
			requireIHeld(t, f.db)
		})
	}
}

// TestPinAware_IncludePinnedDefaultsAndEffectiveValue is AC4 at the service:
// an existing timer reads E=true (the column's DEFAULT TRUE), a window-only
// change keeps E, a clear reports E=false while leaving the column alone, and a
// new timer with the field omitted keeps pins.
func TestPinAware_IncludePinnedDefaultsAndEffectiveValue(t *testing.T) {
	f := newExpirationFixture(t)
	hour, day := 3600, 86400
	for _, ps := range pinScopes(f) {
		t.Run(ps.name, func(t *testing.T) {
			parent := "channels"
			if ps.scope.name == "conversation" {
				parent = "dm_conversations"
			}
			rawInclude := func() bool {
				var include bool
				require.NoError(t, f.db.QueryRow(`SELECT expiration_include_pinned FROM `+parent+` WHERE id = $1`, ps.id).Scan(&include))
				return include
			}
			// A timer that existed before 000168: window set, column at its DEFAULT.
			_, err := f.db.Exec(`UPDATE `+parent+` SET expiration_window_seconds = 3600 WHERE id = $1`, ps.id)
			require.NoError(t, err)
			existing, err := readPolicy(context.Background(), f.db, ps.scope, ps.id)
			require.NoError(t, err)
			assert.True(t, existing.policy.IncludePinned, "an existing timer keeps expiring pins")

			windowOnly := ps.start(t, Request{Mode: "set", WindowSeconds: &day, Retroactive: "new_only"})
			assert.True(t, windowOnly.Previous.IncludePinned)
			assert.True(t, windowOnly.Current.IncludePinned, "a window-only change preserves E=true")

			cleared := ps.start(t, Request{Mode: "clear", Retroactive: "leave_pending"})
			assert.False(t, cleared.Current.IncludePinned, "a cleared policy reports E=false")
			assert.True(t, rawInclude(), "clear leaves the column untouched")

			fresh := ps.start(t, Request{Mode: "set", WindowSeconds: &hour, Retroactive: "new_only"})
			assert.False(t, fresh.Current.IncludePinned, "a new timer with the field omitted keeps pins")
			assert.False(t, rawInclude())

			keptWindowOnly := ps.start(t, Request{Mode: "set", WindowSeconds: &day, Retroactive: "new_only"})
			assert.False(t, keptWindowOnly.Current.IncludePinned, "a window-only change preserves E=false")

			on := ps.start(t, Request{Mode: "set", WindowSeconds: &day, Retroactive: "new_only", IncludePinned: ptr(true)})
			assert.True(t, on.Current.IncludePinned)
			assert.Equal(t, keptWindowOnly.Current.Revision+1, on.Current.Revision, "a pins-only change bumps the revision")
			read, err := readPolicy(context.Background(), f.db, ps.scope, ps.id)
			require.NoError(t, err)
			assert.Equal(t, on.Current.IncludePinned, read.policy.IncludePinned, "readSQL reports what updatePolicySQL returned")
			assert.Equal(t, on.Current.Revision, read.policy.Revision)
			assert.Equal(t, on.Current.WindowSeconds, read.policy.WindowSeconds)
		})
	}
}

// TestPinAware_ToggleOnCollectsAndExpiresDuePins covers the K -> not-K
// transition end to end at this layer (AC2's engine half): held stamps go back
// on the timer, the due collect runs after that move, and the expirer deletes
// exactly the due pinned rows at the accepted cutoff and announces it once.
func TestPinAware_ToggleOnCollectsAndExpiresDuePins(t *testing.T) {
	f := newExpirationFixture(t)
	window := 86400
	now := us(time.Now())
	heldPast, heldFuture, stampPast := now.Add(-time.Hour), now.Add(time.Hour), now.Add(-2*time.Hour)
	for _, ps := range pinScopes(f) {
		t.Run(ps.name, func(t *testing.T) {
			ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(false)})
			dueHeld := ps.insert(t, now.Add(-25*time.Hour), true, nil, &heldPast)
			futureHeld := ps.insert(t, now.Add(-23*time.Hour), true, nil, &heldFuture)
			pinnedWhenDue := ps.insert(t, now.Add(-26*time.Hour), true, &stampPast, nil) // D-C
			dueUnpinned := ps.insert(t, now.Add(-27*time.Hour), false, &stampPast, nil)
			requireIHeld(t, f.db)

			on := ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(true)})
			assert.ElementsMatch(t, []string{dueHeld, pinnedWhenDue}, on.DuePinnedIDs,
				"due collect: pinned rows past C after the move, never an unpinned row (the sweeper owns those)")
			requireStamps(t, f.db, ps.table, dueHeld, &heldPast, nil, "held stamp restored as it was")
			requireStamps(t, f.db, ps.table, futureHeld, &heldFuture, nil, "held stamp restored as it was")

			runner := realEngine(f.db)
			notifier := &fakePurgeNotifier{}
			expirer, logs := newTestExpirer(context.Background(), t, runner, notifier)
			assert.Equal(t, 2, ps.expire(expirer, on))
			assert.Empty(t, logs.String())

			assert.False(t, messageExists(t, f.db, ps.table, dueHeld))
			assert.False(t, messageExists(t, f.db, ps.table, pinnedWhenDue))
			assert.True(t, messageExists(t, f.db, ps.table, futureHeld))
			assert.True(t, messageExists(t, f.db, ps.table, dueUnpinned), "the expirer never widens past its candidates")
			want := ps.notice
			want.cutoff = *on.Current.UpdatedAt
			require.Len(t, notifier.notices, 1)
			assert.Equal(t, want.kind, notifier.notices[0].kind)
			assert.Equal(t, want.id, notifier.notices[0].id)
			assert.True(t, want.cutoff.Equal(notifier.notices[0].cutoff))

			var audits int
			require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM message_purges WHERE context_id = $1 AND reason = 'expiry'`, ps.id).Scan(&audits))
			assert.Equal(t, 1, audits)
			requireIHeld(t, f.db)
		})
	}
}

// TestPinAware_TransitionsOutOfKeepDropOrRestoreEveryHeldStamp pins I-HELD on
// every way a context stops keeping pins without the apply fragment: clear in
// both modes drops held stamps, and a clear from K still collects the pins a
// pin left stamped when already due (spec §5's predicate is "K before and not
// K after", which a clear satisfies).
func TestPinAware_TransitionsOutOfKeepDropOrRestoreEveryHeldStamp(t *testing.T) {
	window := 3600
	for _, mode := range []string{"leave_pending", "clear_pending"} {
		t.Run(mode, func(t *testing.T) {
			f := newExpirationFixture(t)
			now := us(time.Now())
			held, stampPast := now.Add(time.Hour), now.Add(-time.Hour)
			for _, ps := range pinScopes(f) {
				ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(false)})
				kept := ps.insert(t, now.Add(-time.Minute), true, nil, &held)
				pinnedWhenDue := ps.insert(t, now.Add(-2*time.Hour), true, &stampPast, nil)

				cleared := ps.start(t, Request{Mode: "clear", Retroactive: mode})
				requireStamps(t, f.db, ps.table, kept, nil, nil, ps.name+" kept pin no longer has anything to expire under")
				requireStamps(t, f.db, ps.table, pinnedWhenDue, &stampPast, nil, ps.name+" a due stamp is never cleared")
				assert.Equal(t, []string{pinnedWhenDue}, cleared.DuePinnedIDs, ps.name)
				if cleared.Current.BackfillPending {
					resumeAll(t, f, ps, cleared.Current)
					requireStamps(t, f.db, ps.table, pinnedWhenDue, &stampPast, nil, ps.name+" clear backfill does not rescue a due stamp")
				}
			}
		})
	}
}

// TestPinAware_PreflightDeletesDuePinnedRowsBehindOlderCandidates is AC9: the
// boot preflight terminates under a keep-pins policy and deletes a pinned row
// that was already due (D-C) even when 5,001 older candidates sit ahead of it in
// discovery, while a held pin, whose would-be expiry is older still, survives.
// No keep predicate exists in discovery (C-SWEEP), so nothing can live-lock.
func TestPinAware_PreflightDeletesDuePinnedRowsBehindOlderCandidates(t *testing.T) {
	f := newExpirationFixture(t)
	cutoff := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	window := 3600
	for _, ps := range pinScopes(f) {
		ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(false)})
	}
	_, err := f.db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at, expires_at)
SELECT $1, $2, 'older', $3, $4 FROM generate_series(1, 5001)`, f.channel, f.owner, cutoff.Add(-3*time.Hour), cutoff.Add(-2*time.Hour))
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at, expires_at)
SELECT $1, $2, 'older', 'user', $3, $4 FROM generate_series(1, 5001)`, f.conversation, f.owner, cutoff.Add(-3*time.Hour), cutoff.Add(-2*time.Hour))
	require.NoError(t, err)

	duePinned := map[string]string{}
	keptPinned := map[string]string{}
	dueAt, heldAt := cutoff.Add(-time.Hour), cutoff.Add(-4*time.Hour)
	for _, ps := range pinScopes(f) {
		duePinned[ps.name] = ps.insert(t, cutoff.Add(-2*time.Hour), true, &dueAt, nil)
		keptPinned[ps.name] = ps.insert(t, cutoff.Add(-5*time.Hour), true, nil, &heldAt)
	}
	requireIHeld(t, f.db)

	s := sweeperAt(t, f.db, func(context.Context) (time.Time, error) { return cutoff, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, s.RunPreflight(ctx), "the preflight terminates")

	var remaining int
	require.NoError(t, f.db.QueryRow(`SELECT (SELECT count(*) FROM messages WHERE expires_at < $1) + (SELECT count(*) FROM dm_messages WHERE expires_at < $1)`, cutoff).Scan(&remaining))
	assert.Zero(t, remaining)
	for _, ps := range pinScopes(f) {
		assert.False(t, messageExists(t, f.db, ps.table, duePinned[ps.name]), "%s: a pinned row already due when pinned is deleted", ps.name)
		assert.True(t, messageExists(t, f.db, ps.table, keptPinned[ps.name]), "%s: a held pin is never a candidate", ps.name)
	}
	requireIHeld(t, f.db)
}

// TestPinAware_ApplyBackfillUnderKeepDrainsWithPinChurn is AC10: a retroactive
// apply under a keep-pins policy drains to backfill_pending=false on both
// scopes while pins and unpins land between its batches. Each pin or unpin
// moves a row toward its target shape or out of eligibility, so the batches
// stay monotone (R2).
func TestPinAware_ApplyBackfillUnderKeepDrainsWithPinChurn(t *testing.T) {
	f := newExpirationFixture(t)
	window := 2592000
	created := us(time.Now().Add(-48 * time.Hour))
	target := created.Add(time.Duration(window) * time.Second)
	_, err := f.db.Exec(`INSERT INTO messages (channel_id, user_id, content, created_at)
SELECT $1, $2, 'ciphertext', $3 FROM generate_series(1, 12001)`, f.channel, f.owner, created)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO dm_messages (conversation_id, user_id, content, type, created_at)
SELECT $1, $2, 'ciphertext', 'user', $3 FROM generate_series(1, 12001)`, f.conversation, f.owner, created)
	require.NoError(t, err)

	for _, ps := range pinScopes(f) {
		t.Run(ps.name, func(t *testing.T) {
			for i := 0; i < 3; i++ {
				ps.insert(t, created, true, nil, nil)
			}
			transition := ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "apply", IncludePinned: ptr(false)})
			require.True(t, transition.Current.BackfillPending)
			var pinnedHeld int
			require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+ps.table+` WHERE `+scopeColumn(ps)+` = $1 AND pinned_at IS NOT NULL AND held_expires_at = $2`, ps.id, target).Scan(&pinnedHeld))
			assert.Equal(t, 3, pinnedHeld, "start reconciles pinned rows synchronously, before any batch")

			service := NewService(f.db)
			policy := transition.Current
			for batches := 0; policy.BackfillPending; batches++ {
				require.Less(t, batches, 10, "the backfill must converge")
				policy, _, err = service.runBatch(context.Background(), ps.scope, ps.id, transition.Current.Revision, policy)
				require.NoError(t, err)
				requireIHeld(t, f.db)
				if !policy.BackfillPending {
					break
				}
				churnBetweenBatches(t, f, ps)
			}

			var mode sql.NullString
			require.NoError(t, f.db.QueryRow(`SELECT expiration_backfill_mode FROM `+parentTable(ps)+` WHERE id = $1`, ps.id).Scan(&mode))
			assert.False(t, mode.Valid, "the marker cleared")
			var offTarget int
			require.NoError(t, f.db.QueryRow(`
SELECT count(*) FROM `+ps.table+`
WHERE `+scopeColumn(ps)+` = $1
  AND (CASE WHEN pinned_at IS NOT NULL THEN (expires_at, held_expires_at) IS DISTINCT FROM (NULL::timestamptz, $2::timestamptz)
            ELSE (expires_at, held_expires_at) IS DISTINCT FROM ($2::timestamptz, NULL::timestamptz) END)`, ps.id, target).Scan(&offTarget))
			assert.Zero(t, offTarget, "every pinned row is held at created_at + W and every unpinned row is stamped at it")
		})
	}
}

// churnBetweenBatches pins one row the backfill has not reached, pins one it
// has, and unpins one held pin, each only while such a row still exists.
func churnBetweenBatches(t *testing.T, f expirationFixture, ps pinScope) {
	t.Helper()
	if unreached := optionalMessageID(t, f.db, `SELECT id FROM `+ps.table+` WHERE `+scopeColumn(ps)+` = $1 AND pinned_at IS NULL AND expires_at IS NULL LIMIT 1`, ps.id); unreached != "" {
		ps.pin(t, unreached)
		expires, held := stampsOf(t, f.db, ps.table, unreached)
		assert.Nil(t, expires)
		assert.Nil(t, held, "pinning an unstamped row moves nothing; the next batch holds it")
	}
	if reached := optionalMessageID(t, f.db, `SELECT id FROM `+ps.table+` WHERE `+scopeColumn(ps)+` = $1 AND pinned_at IS NULL AND expires_at IS NOT NULL LIMIT 1`, ps.id); reached != "" {
		ps.pin(t, reached)
		expires, held := stampsOf(t, f.db, ps.table, reached)
		assert.Nil(t, expires, "pinning a future stamp in a keep-pins context holds it")
		assert.NotNil(t, held)
	}
	if heldPin := optionalMessageID(t, f.db, `SELECT id FROM `+ps.table+` WHERE `+scopeColumn(ps)+` = $1 AND pinned_at IS NOT NULL AND held_expires_at IS NOT NULL LIMIT 1`, ps.id); heldPin != "" {
		ps.unpin(t, heldPin)
		expires, held := stampsOf(t, f.db, ps.table, heldPin)
		assert.NotNil(t, expires, "unpinning puts the held stamp back on the timer")
		assert.Nil(t, held)
	}
}

func optionalMessageID(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var id string
	err := db.QueryRow(query, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return id
}

func scopeColumn(ps pinScope) string {
	if ps.scope.name == "channel" {
		return "channel_id"
	}
	return "conversation_id"
}

func parentTable(ps pinScope) string {
	if ps.scope.name == "channel" {
		return "channels"
	}
	return "dm_conversations"
}

// TestPinAware_ApplyOnRecomputesHeldStampsInBothDirections is AC11 (R4):
// turning pinned expiry on with a retroactive apply stamps each pin at
// created_at + W1, recomputed from the new window, whether that lengthens or
// shortens its old held value, and never restores the held value itself.
func TestPinAware_ApplyOnRecomputesHeldStampsInBothDirections(t *testing.T) {
	f := newExpirationFixture(t)
	now := us(time.Now())
	keepWindow, day := 3600, 86400
	for _, ps := range pinScopes(f) {
		t.Run(ps.name, func(t *testing.T) {
			ps.start(t, Request{Mode: "set", WindowSeconds: &keepWindow, Retroactive: "new_only", IncludePinned: ptr(false)})
			oldCreated, newCreated := now.Add(-48*time.Hour), now.Add(-2*time.Hour)
			lengthenedStillDue := ps.insert(t, oldCreated, true, nil, ptr(oldCreated.Add(time.Hour)))
			shortenedNowDue := ps.insert(t, oldCreated, true, nil, ptr(oldCreated.Add(30*24*time.Hour)))
			lengthenedFuture := ps.insert(t, newCreated, true, nil, ptr(newCreated.Add(time.Hour)))
			requireIHeld(t, f.db)

			on := ps.start(t, Request{Mode: "set", WindowSeconds: &day, Retroactive: "apply", IncludePinned: ptr(true)})
			requireStamps(t, f.db, ps.table, lengthenedStillDue, ptr(oldCreated.Add(24*time.Hour)), nil, "1h held -> 24h stamp")
			requireStamps(t, f.db, ps.table, shortenedNowDue, ptr(oldCreated.Add(24*time.Hour)), nil, "30d held -> 24h stamp")
			requireStamps(t, f.db, ps.table, lengthenedFuture, ptr(newCreated.Add(24*time.Hour)), nil, "a past 1h held value is not re-armed")
			assert.ElementsMatch(t, []string{lengthenedStillDue, shortenedNowDue}, on.DuePinnedIDs)
			resumeAll(t, f, ps, on.Current)
		})
	}
}

// TestPinAware_PinAndUnpinFragments covers every arm of the pin fragments in
// the statement shapes pinning.go uses: a pin in a keep-pins context holds a
// stamp that has not passed and leaves a due one (D-C); a pin anywhere else
// moves nothing; an unpin puts a held stamp back on the timer.
func TestPinAware_PinAndUnpinFragments(t *testing.T) {
	f := newExpirationFixture(t)
	window := 86400
	now := us(time.Now())
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	for _, ps := range pinScopes(f) {
		t.Run(ps.name, func(t *testing.T) {
			noWindow := ps.insert(t, now, false, &future, nil)
			ps.pin(t, noWindow)
			requireStamps(t, f.db, ps.table, noWindow, &future, nil, "no window: a pin moves nothing")

			ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(true)})
			included := ps.insert(t, now, false, &future, nil)
			ps.pin(t, included)
			requireStamps(t, f.db, ps.table, included, &future, nil, "pins expire: a pin moves nothing")

			ps.start(t, Request{Mode: "set", WindowSeconds: &window, Retroactive: "new_only", IncludePinned: ptr(false)})
			kept := ps.insert(t, now, false, &future, nil)
			ps.pin(t, kept)
			requireStamps(t, f.db, ps.table, kept, nil, &future, "keep pins: a future stamp is held")
			due := ps.insert(t, now.Add(-2*time.Hour), false, &past, nil)
			ps.pin(t, due)
			requireStamps(t, f.db, ps.table, due, &past, nil, "keep pins: a due stamp stays (D-C)")
			unstamped := ps.insert(t, now, false, nil, nil)
			ps.pin(t, unstamped)
			requireStamps(t, f.db, ps.table, unstamped, nil, nil, "keep pins: nothing to hold")

			ps.unpin(t, kept)
			requireStamps(t, f.db, ps.table, kept, &future, nil, "unpin: the held stamp is back on the timer")
			ps.unpin(t, due)
			requireStamps(t, f.db, ps.table, due, &past, nil, "unpin: a live stamp is unchanged")
			ps.unpin(t, unstamped)
			requireStamps(t, f.db, ps.table, unstamped, nil, nil, "unpin: nothing to release")
		})
	}
}
