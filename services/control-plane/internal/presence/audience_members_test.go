package presence

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMembersExcluding_RemovesOnlyTheSender(t *testing.T) {
	sender := uuid.New()
	other := uuid.New()
	all := map[uuid.UUID]bool{sender: true, other: true}

	got := membersExcluding(all, sender)

	assert.False(t, got[sender], "the sender is never in their own audience")
	assert.True(t, got[other])
	assert.Len(t, got, 1)
}

// The whole point of the shared read: one sender's exclusion must not reach a
// sibling sender. A mutating implementation silently drops a legitimate viewer
// from the sibling's audience, which is a missing presence frame, not an error.
func TestMembersExcluding_NeverMutatesTheSharedSet(t *testing.T) {
	senderA := uuid.New()
	senderB := uuid.New()
	bystander := uuid.New()
	all := map[uuid.UUID]bool{senderA: true, senderB: true, bystander: true}

	forA := membersExcluding(all, senderA)
	forB := membersExcluding(all, senderB)

	require.Len(t, all, 3, "the shared set is never mutated")
	assert.True(t, forA[senderB], "sender A's exclusion must not remove sender B")
	assert.True(t, forB[senderA], "sender B's exclusion must not remove sender A")
	assert.True(t, forA[bystander])
	assert.True(t, forB[bystander])
}

func TestMembersExcluding_AbsentSenderIsNotAnError(t *testing.T) {
	other := uuid.New()
	all := map[uuid.UUID]bool{other: true}

	got := membersExcluding(all, uuid.New())

	assert.Len(t, got, 1, "a sender who is not a member leaves the set intact")
	assert.True(t, got[other])
}

func TestMembersExcluding_EmptyInputYieldsEmptyNonNil(t *testing.T) {
	got := membersExcluding(map[uuid.UUID]bool{}, uuid.New())

	require.NotNil(t, got, "callers range over the result; nil is a needless special case")
	assert.Empty(t, got)
}
