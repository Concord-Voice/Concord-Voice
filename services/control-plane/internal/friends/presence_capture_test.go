package friends

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
)

// recordingCapture is a GraphPresenceCapture double that records what the
// handlers hand it. It never touches a real transaction, so it exercises the
// handler-side wiring without a database.
type recordingCapture struct {
	subjects []presencecapture.Subject
	abandons []string
}

func (c *recordingCapture) CaptureInTx(
	_ context.Context, _ *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.subjects = append(c.subjects, subject)
	return nil, nil
}

func (c *recordingCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	return tx.Commit()
}

func (c *recordingCapture) Abandon(_ presencecapture.Plan, cause presencecapture.Cause) {
	c.abandons = append(c.abandons, string(cause))
}

func TestHasGraphPresenceCaptureReportsWiring(t *testing.T) {
	h := &Handler{}
	assert.False(t, h.HasGraphPresenceCapture(), "an unwired handler must report false")

	h.SetGraphPresenceCapture(&recordingCapture{})
	assert.True(t, h.HasGraphPresenceCapture(), "a wired handler must report true")
}

// The accessor must reflect handler state, not the caller's local value — that
// distinction is what makes the router boot guard non-tautological.
func TestHasGraphPresenceCaptureIsHandlerState(t *testing.T) {
	h := &Handler{}
	capture := &recordingCapture{}
	_ = capture // constructed but never wired

	assert.False(t, h.HasGraphPresenceCapture(),
		"constructing a capture must not make the handler report wired")
}

// The handler's capture goes through the shared plumbing, so wiring the setter
// is enough to make a spec reach the bridge as a parsed subject.
func TestWiredHandlerCaptureReachesBridge(t *testing.T) {
	capture := &recordingCapture{}
	h := &Handler{}
	h.SetGraphPresenceCapture(capture)
	principal, counterpart := uuid.New(), uuid.New()

	_, err := presencehook.Capture(context.Background(), h.graphPresence, nil, presencehook.Spec{
		Family:        presencecapture.FamilyFriendshipRemove,
		Posture:       presencecapture.FailClosedBlockWrite,
		PrincipalID:   principal.String(),
		CounterpartID: counterpart.String(),
	})

	require.NoError(t, err)
	require.Len(t, capture.subjects, 1)
	assert.Equal(t, presencecapture.FamilyFriendshipRemove, capture.subjects[0].Family)
	assert.Equal(t, principal, capture.subjects[0].Principal)
	assert.Equal(t, counterpart, capture.subjects[0].Counterpart)
}

// Block is the one #2446 site that degrades rather than blocking the write:
// refusing a block because a capture read failed would let a large friend graph
// deny a safety affordance.
func TestBlockCaptureSpecDeclaresConservativeDegrade(t *testing.T) {
	blocker, blocked := uuid.New(), uuid.New()

	spec := blockCaptureSpec(blocker.String(), blocked.String())

	assert.Equal(t, presencecapture.FamilyBlock, spec.Family)
	assert.Equal(t, presencecapture.FailConservativeDegrade, spec.Posture,
		"block must declare FailConservativeDegrade: a large friend graph must not "+
			"be able to deny a safety affordance")

	subject, err := spec.Subject()
	require.NoError(t, err)
	assert.Equal(t, blocker, subject.Principal)
	assert.Equal(t, blocked, subject.Counterpart)
}

// Every other site keeps the fail-closed zero value.
func TestNonBlockSitesFailClosed(t *testing.T) {
	for _, family := range []presencecapture.Family{
		presencecapture.FamilyFriendshipAccept,
		presencecapture.FamilyFriendshipRemove,
		presencecapture.FamilyFriendsOfFriendsToggle,
	} {
		spec := presencehook.Spec{Family: family, PrincipalID: uuid.NewString()}
		subject, err := spec.Subject()
		require.NoError(t, err)
		assert.Equal(t, presencecapture.FailClosedBlockWrite, subject.FailPosture)
	}
}
