package keyrotation_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
)

func TestIssuedChannelEpoch_IsTheHigherOfWrapsAndLedger(t *testing.T) {
	db := krSetupDB(t)
	owner, _, channelID := krSeedServerChannel(t, db)

	epoch, err := keyrotation.IssuedChannelEpoch(context.Background(), db, uuid.NewString())
	require.NoError(t, err)
	assert.Equal(t, 1, epoch, "a channel with no keys yet has issued epoch 1")

	krSeedEpoch(t, db, channelID, owner, 3)
	epoch, err = keyrotation.IssuedChannelEpoch(context.Background(), db, channelID)
	require.NoError(t, err)
	assert.Equal(t, 3, epoch, "the revocation ledger's successor counts before any epoch-3 wrap exists")
}

func TestIssuedDMEpoch_IsTheHigherOfWrapsAndLedger(t *testing.T) {
	db := krSetupDB(t)
	owner, _, _ := krSeedServerChannel(t, db)
	convID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, convID, owner)
	require.NoError(t, err)

	epoch, err := keyrotation.IssuedDMEpoch(context.Background(), db, convID)
	require.NoError(t, err)
	assert.Equal(t, 1, epoch)

	_, err = db.Exec(`INSERT INTO dm_key_revocations (conversation_id, revoked_epoch, successor_epoch, reason)
		VALUES ($1, 1, 2, 'test')`, convID)
	require.NoError(t, err)
	epoch, err = keyrotation.IssuedDMEpoch(context.Background(), db, convID)
	require.NoError(t, err)
	assert.Equal(t, 2, epoch)
}

func TestIssuedEpoch_ReportsAQueryFailure(t *testing.T) {
	db := krSetupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := keyrotation.IssuedChannelEpoch(ctx, db, uuid.NewString())
	assert.Error(t, err)
	_, err = keyrotation.IssuedDMEpoch(ctx, db, uuid.NewString())
	assert.Error(t, err)
}
