package presencecapture

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The zero value of FailPosture must be the fail-closed posture, so a call site
// that constructs a Subject without thinking about failure gets #2445 behaviour.
func TestZeroFailPostureBlocksWrite(t *testing.T) {
	var s Subject
	assert.Equal(t, FailClosedBlockWrite, s.FailPosture,
		"zero FailPosture must be FailClosedBlockWrite")
}

// Family must be a dense enum starting at zero so a bridge switch over it is
// exhaustively checkable.
func TestFamilyEnumIsDenseAndDistinct(t *testing.T) {
	all := []Family{
		FamilyFriendshipAccept,
		FamilyFriendshipRemove,
		FamilyBlock,
		FamilyFriendsOfFriendsToggle,
	}
	seen := make(map[Family]bool, len(all))
	for i, f := range all {
		require.Equal(t, Family(i), f, "family at index %d must equal %d (enum must stay dense)", i, i)
		require.False(t, seen[f], "duplicate family value %d", f)
		seen[f] = true
	}
}

func TestSubjectCarriesNoAudience(t *testing.T) {
	// Compile-time intent check: Subject must be constructible from IDs alone.
	s := Subject{
		Family:      FamilyBlock,
		FailPosture: FailConservativeDegrade,
		Principal:   uuid.New(),
		Counterpart: uuid.New(),
	}
	assert.NotEqual(t, uuid.Nil, s.Principal, "subject must carry both principals")
	assert.NotEqual(t, uuid.Nil, s.Counterpart, "subject must carry both principals")
}

// CanRevokeVisibility gates the peripheral viewer-scoped disconnect. Getting it
// wrong in the revoking direction leaks presence to an unauthorized viewer;
// getting it wrong in the additive direction tears down every device of both
// principals for nothing (#2738 review).
func TestCanRevokeVisibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family Family
		want   bool
		why    string
	}{
		{"accept is purely additive", FamilyFriendshipAccept, false,
			"an accepted edge only widens audiences, so no viewer can hold stale state"},
		{"removal revokes", FamilyFriendshipRemove, true, "the counterpart loses FoF-reachable senders"},
		{"block revokes", FamilyBlock, true, "block is the priority regression"},
		{"FoF toggle is conservative in both directions", FamilyFriendsOfFriendsToggle, true,
			"the family alone cannot tell on->off from off->on; be wrong toward a reconnect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.family.CanRevokeVisibility(), tc.why)
		})
	}
}

// An unrecognized family must fail closed: an uncleared viewer is a disclosure,
// an unnecessary disconnect is only a reconnect.
func TestCanRevokeVisibilityUnknownFamilyFailsClosed(t *testing.T) {
	assert.True(t, Family(200).CanRevokeVisibility(),
		"an unknown family must fail closed toward disconnecting")
}

// CauseProvesNoCommit is the predicate that decides whether a failed write
// disconnects anybody. Getting it wrong in either direction is a real defect:
// too permissive and an aborted request tears down every session in the
// captured audience (the #2738 vulnerability); too strict and a genuinely
// unresolved commit leaves viewers holding revoked presence.
func TestCauseProvesNoCommit(t *testing.T) {
	proven := []Cause{CauseWriteFailed, CauseRowsAffected}
	for _, c := range proven {
		assert.True(t, CauseProvesNoCommit(c),
			"%s proves the transaction never committed, so it must disconnect nobody", c)
	}

	assert.False(t, CauseProvesNoCommit(CauseCommitUnresolved),
		"an unresolved commit is UNKNOWN state and must still fail closed")

	// A cause this package does not define must fail closed too — the caller
	// may have invented it, and an unrecognized failure is not proof of
	// anything.
	assert.False(t, CauseProvesNoCommit(Cause("something_new")),
		"an unknown cause must not be treated as proof of no commit")
	assert.False(t, CauseProvesNoCommit(""), "nor must the zero value")
}

// The two sentinels drive DIFFERENT handler responses for the same 503 status —
// pending means the write did not happen and the request is safe to retry,
// post-commit-delivery means it DID happen and a retry would duplicate it. A
// PendingError that unwrapped to the wrong sentinel, or to nothing, would route
// a committed mutation into the retry path.
func TestPendingErrorUnwrapsToTheSentinel(t *testing.T) {
	err := error(&PendingError{After: 7 * time.Second})

	assert.True(t, errors.Is(err, ErrCapturePending))
	assert.False(t, errors.Is(err, ErrPostCommitDelivery))

	var pending *PendingError
	require.True(t, errors.As(err, &pending))
	assert.Equal(t, 7*time.Second, pending.After)
}

// Handlers classify an error that has been wrapped with operation context on the
// way up, per [internal]rules/backend.md. Both the sentinel test and the Retry-After
// extraction must survive that wrapping.
func TestPendingErrorSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("accept friend request: %w", &PendingError{After: time.Second})

	assert.True(t, errors.Is(err, ErrCapturePending))

	var pending *PendingError
	require.True(t, errors.As(err, &pending))
	assert.Equal(t, time.Second, pending.After)
}

func TestPostCommitDeliverySentinelIsDistinct(t *testing.T) {
	err := fmt.Errorf("%w: hub unreachable", ErrPostCommitDelivery)

	assert.True(t, errors.Is(err, ErrPostCommitDelivery))
	assert.False(t, errors.Is(err, ErrCapturePending))
}
