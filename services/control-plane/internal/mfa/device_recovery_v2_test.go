package mfa_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/devicerecovery"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func recoveryV2Fixture(t *testing.T, ts *testhelpers.TestServer, userID string) (devicerecovery.Row, devicerecovery.RespondBody, devicerecovery.RespondBody) {
	t.Helper()
	b, err := os.ReadFile("../../../../docs/design/trusted-recovery-v2-vectors.json")
	require.NoError(t, err)
	var v struct {
		Context devicerecovery.Context `json:"context"`
		Offer   devicerecovery.Offer   `json:"offer"`
		Payload string                 `json:"encrypted_payload"`
	}
	require.NoError(t, json.Unmarshal(b, &v))
	_, err = ts.DB.Exec(`INSERT INTO trusted_recovery_devices(user_id,device_name,machine_id) VALUES($1,'device',$2)`, userID, uuid.NewString())
	require.NoError(t, err)
	r, err := devicerecovery.Create(context.Background(), ts.DB, userID, "public-jti", time.Now().Add(time.Hour), devicerecovery.CreateBody{ProtocolVersion: 2, ServerOrigin: v.Context.ServerOrigin, AccountBinding: devicerecovery.AccountBinding(userID), RequesterNonce: v.Context.RequesterNonce, RequesterPublicKey: v.Context.RequesterPublicKey})
	require.NoError(t, err)
	_, hash, err := devicerecovery.Transcript(r.Context, v.Offer)
	require.NoError(t, err)
	offer := devicerecovery.RespondBody{Action: "offer", ProtocolVersion: 2, ResponderPublicKey: v.Offer.ResponderPublicKey, ResponderNonce: v.Offer.ResponderNonce, TranscriptHash: devicerecovery.Encode(hash)}
	approve := devicerecovery.RespondBody{Action: "approve", ProtocolVersion: 2, TranscriptHash: offer.TranscriptHash, EncryptedPayload: v.Payload}
	return r, offer, approve
}

// actionV2Map emits exactly the fields allowed for an action, so null/empty
// fields of the internal Go transport struct never sneak into wire fixtures.
func actionV2Map(b devicerecovery.RespondBody) map[string]any {
	r := map[string]any{"action": b.Action, "protocol_version": 2}
	if b.Action == "offer" {
		r["responder_public_key"] = b.ResponderPublicKey
		r["responder_nonce"] = b.ResponderNonce
		r["transcript_hash"] = b.TranscriptHash
	}
	if b.Action == "approve" {
		r["transcript_hash"] = b.TranscriptHash
		r["encrypted_payload"] = b.EncryptedPayload
	}
	return r
}
