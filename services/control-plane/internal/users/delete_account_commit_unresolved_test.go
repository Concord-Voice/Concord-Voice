package users

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	natsclient "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/nats"
)

// commitUnresolvedCapture models a commit whose acknowledgement was lost. The
// rollback arm returns the same unknown result without committing, which proves
// the privacy clears are fail-closed while media needs durable absence evidence.
type commitUnresolvedCapture struct{ commit bool }

func (*commitUnresolvedCapture) WithGatedTx(
	context.Context, presencecapture.Subject, func(*sql.Tx) error,
) error {
	return errors.New("commitUnresolvedCapture: DeleteAccount must not use WithGatedTx")
}

func (*commitUnresolvedCapture) CaptureInTx(
	context.Context, *sql.Tx, presencecapture.Subject,
) (presencecapture.Plan, error) {
	return nil, nil
}

func (c *commitUnresolvedCapture) Complete(
	_ context.Context, tx *sql.Tx, _ presencecapture.Plan,
) error {
	if c.commit {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return presencecapture.ErrCommitUnresolved
}

func (*commitUnresolvedCapture) Abandon(presencecapture.Plan, presencecapture.Cause) {}

var _ presencecapture.GraphPresenceCapture = (*commitUnresolvedCapture)(nil)

// observeErasureClear uses the real control-plane NATS client so this
// regression observes the cross-replica privacy clear rather than merely
// assuming the AccountService's publisher was wired.
func observeErasureClear(t *testing.T, expectedUserID uuid.UUID) (*natsclient.Client, <-chan map[string]interface{}) {
	t.Helper()
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	publisher, err := natsclient.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live erasure-clear assertion (runs in CI)", err)
	}
	if !publisher.IsConnected() {
		_ = publisher.Close()
		t.Skip("NATS unavailable; skipping live erasure-clear assertion (runs in CI)")
	}
	t.Cleanup(func() { _ = publisher.Close() })

	observer, err := natsclient.Connect(natsURL)
	if err != nil {
		t.Skipf("NATS unavailable (%v); skipping live erasure-clear assertion (runs in CI)", err)
	}
	if !observer.IsConnected() {
		_ = observer.Close()
		t.Skip("NATS unavailable; skipping live erasure-clear assertion (runs in CI)")
	}
	t.Cleanup(func() { _ = observer.Close() })
	clears := make(chan map[string]interface{}, 1)
	subscription, err := observer.Subscribe(NATSSubjectPresenceErasureCleared, func(data []byte) {
		var payload map[string]interface{}
		if json.Unmarshal(data, &payload) != nil || payload["user_id"] != expectedUserID.String() {
			return
		}
		select {
		case clears <- payload:
		default:
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := observer.FlushTimeout(2 * time.Second); err != nil {
		t.Skipf("NATS unavailable (%v); skipping live erasure-clear assertion (runs in CI)", err)
	}

	return publisher, clears
}

func requireErasureClear(t *testing.T, clears <-chan map[string]interface{}, userID uuid.UUID) {
	t.Helper()
	select {
	case payload := <-clears:
		assert.Equal(t, userID.String(), payload["user_id"])
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cross-replica erasure-clear event")
	}
}

func TestDeleteAccountCommitUnresolvedPreservesPrivacyOutputButGatesMedia(t *testing.T) {
	for _, tc := range []struct {
		name       string
		commit     bool
		wantAbsent bool
		wantMedia  bool
	}{
		{name: "commit acknowledgement lost", commit: true, wantAbsent: true, wantMedia: true},
		{name: "rollback remains unknown", commit: false, wantAbsent: false, wantMedia: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := testdb.SetupTestDB(t)
			t.Cleanup(cleanup)
			owner, erased := testdb.CreateUser(t, db), testdb.CreateUser(t, db)
			serverID := uuid.New()
			_, err := db.Exec(`INSERT INTO servers (id, name, owner_id)
				VALUES ($1, 'unresolved-erasure', $2)`, serverID, owner)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO server_members (server_id, user_id) VALUES ($1, $2)`, serverID, erased)
			require.NoError(t, err)
			channelID := uuid.New()
			_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type)
				VALUES ($1, $2, 'incomplete-erasure', 'text')`, channelID, serverID)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO channel_initial_key_distributions (channel_id, creator_id)
				VALUES ($1, $2)`, channelID, erased)
			require.NoError(t, err)
			attachmentKey := "attachments/unresolved-" + erased.String()
			_, err = db.Exec(`INSERT INTO media_files
				(uploader_id, file_type, media_tier, key_version, channel_id, mime_type, file_size, storage_key)
				VALUES ($1, 'file', 2, 1, $2, 'application/octet-stream', 1, $3)`,
				erased, channelID, attachmentKey)
			require.NoError(t, err)

			drain := &stubActivePlanDrain{drainedCategories: []presence.Category{presence.CategoryPrivateCall}}
			service := newAccountServiceWithDrain(t, db, drain)
			service.SetGraphPresenceCapture(&commitUnresolvedCapture{commit: tc.commit})
			publisher, erasureClears := observeErasureClear(t, erased)
			service.SetNATS(publisher)
			var broadcasts [][2]string
			service.SetChannelDeletedBroadcaster(func(server, channel string) {
				broadcasts = append(broadcasts, [2]string{server, channel})
			})
			var reclaimed []media.BlobRef
			service.SetErasedMediaReclaimer(func(_ context.Context, _ []media.BlobRef, tier2 []media.BlobRef) {
				reclaimed = append(reclaimed, tier2...)
			})

			err = service.DeleteAccount(context.Background(), erased.String())
			require.ErrorIs(t, err, presencecapture.ErrCommitUnresolved)
			assert.Equal(t, tc.wantAbsent, countUsers(t, db, erased) == 0)
			assert.Equal(t, 1, drain.clearCount(),
				"an unknown commit must still clear captured presence state")
			assert.Equal(t, [][2]string{{serverID.String(), channelID.String()}}, broadcasts,
				"the captured incomplete-channel fanout is a fail-closed privacy clear")
			requireErasureClear(t, erasureClears, erased)
			if tc.wantMedia {
				require.Len(t, reclaimed, 1)
				assert.Equal(t, attachmentKey, reclaimed[0].Key)
			} else {
				assert.Empty(t, reclaimed,
					"without durable absence proof media must remain for existing orphan recovery")
			}
		})
	}
}

func TestReclaimErasedMediaUnresolvedWithoutProofSkipsDestructiveHandoff(t *testing.T) {
	called := false
	service := NewAccountService(nil, nil)
	service.SetErasedMediaReclaimer(func(context.Context, []media.BlobRef, []media.BlobRef) {
		called = true
	})

	service.reclaimErasedMedia(context.Background(), uuid.NewString(), true, erasedMedia{
		tier2: []media.BlobRef{media.NewBlobRef("attachments/unresolved", sql.NullString{})},
	})

	assert.False(t, called, "unavailable durable absence proof must leave media to orphan recovery")
}
