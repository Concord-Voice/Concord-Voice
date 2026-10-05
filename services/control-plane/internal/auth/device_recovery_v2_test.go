package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/devicerecovery"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func deviceFixture(t *testing.T) (devicerecovery.Context, devicerecovery.Offer, string) {
	t.Helper()
	b, err := os.ReadFile("../../../../docs/design/trusted-recovery-v2-vectors.json")
	require.NoError(t, err)
	var v struct {
		Context devicerecovery.Context `json:"context"`
		Offer   devicerecovery.Offer   `json:"offer"`
		Payload string                 `json:"encrypted_payload"`
	}
	require.NoError(t, json.Unmarshal(b, &v))
	return v.Context, v.Offer, v.Payload
}
func deviceCreateV2(t *testing.T, userID, token string) devicerecovery.CreateBody {
	t.Helper()
	c, _, _ := deviceFixture(t)
	return devicerecovery.CreateBody{ProtocolVersion: 2, RecoveryToken: token, ServerOrigin: c.ServerOrigin, AccountBinding: devicerecovery.AccountBinding(userID), RequesterNonce: c.RequesterNonce, RequesterPublicKey: c.RequesterPublicKey}
}
func deviceOfferV2(t *testing.T, ts *testhelpers.TestServer, userID, requestID string) (string, string) {
	t.Helper()
	_, o, p := deviceFixture(t)
	var c devicerecovery.Context
	var expiry int64
	require.NoError(t, ts.DB.QueryRow(`SELECT server_origin,account_binding,requester_nonce,ephemeral_public_key,recovery_token_jti_hash,(extract(epoch from expires_at)*1000)::bigint FROM recovery_requests WHERE id=$1`, requestID).Scan(&c.ServerOrigin, newBinaryString(&c.AccountBinding), newBinaryString(&c.RequesterNonce), newBinaryString(&c.RequesterPublicKey), newBinaryString(&c.RecoveryTokenJTIHash), &expiry))
	c.ProtocolVersion, c.RequestID, c.ExpiresAt = 2, requestID, expiry
	_, h, err := devicerecovery.Transcript(c, o)
	require.NoError(t, err)
	hash := devicerecovery.Encode(h)
	_, err = devicerecovery.Respond(context.Background(), ts.DB, userID, "", requestID, devicerecovery.RespondBody{Action: "offer", ProtocolVersion: 2, ResponderPublicKey: o.ResponderPublicKey, ResponderNonce: o.ResponderNonce, TranscriptHash: hash})
	require.NoError(t, err)
	return hash, p
}

type binaryString struct{ target *string }

func newBinaryString(s *string) binaryString { return binaryString{s} }
func (s binaryString) Scan(src any) error {
	b, ok := src.([]byte)
	if !ok {
		return devicerecovery.ErrInvalid
	}
	*s.target = devicerecovery.Encode(b)
	return nil
}

func TestDeviceRecoveryV2ActualRoutesCompleteAndResetTokenSurvives(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "devv2routes")
	token := getRecoveryToken2(t, ts, user)
	_, err := ts.DB.Exec(`INSERT INTO trusted_recovery_devices(user_id,device_name,machine_id) VALUES($1,'device',$2)`, user.ID, uuid.NewString())
	require.NoError(t, err)
	create := ts.DoRequest("POST", pathDeviceReqCreate, deviceCreateV2(t, user.ID, token), nil)
	require.Equal(t, 200, create.Code, create.Body.String())
	var pending struct {
		devicerecovery.Context
		Status string `json:"status"`
	}
	testhelpers.ParseJSON(t, create, &pending)
	require.Equal(t, "pending", pending.Status)
	_, o, payload := deviceFixture(t)
	_, h, err := devicerecovery.Transcript(pending.Context, o)
	require.NoError(t, err)
	digest := devicerecovery.Encode(h)
	route := "/api/v1/mfa/recovery-requests/" + pending.RequestID + "/respond"
	access := testhelpers.AuthHeaders(user.AccessToken)
	offered := ts.DoRequest("POST", route, map[string]any{"action": "offer", "protocol_version": 2, "responder_public_key": o.ResponderPublicKey, "responder_nonce": o.ResponderNonce, "transcript_hash": digest}, access)
	require.Equal(t, 200, offered.Code, offered.Body.String())
	approved := ts.DoRequest("POST", route, map[string]any{"action": "approve", "protocol_version": 2, "transcript_hash": digest, "encrypted_payload": payload}, access)
	require.Equal(t, 200, approved.Code, approved.Body.String())
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	poll := ts.DoRequest("GET", pathDeviceReqPoll+pending.RequestID, nil, headers)
	require.Equal(t, 200, poll.Code, poll.Body.String())
	require.Contains(t, poll.Body.String(), "encrypted_payload")
	completeRoute := pathDeviceReqPoll + pending.RequestID + "/complete"
	// Access authentication cannot complete a requester ceremony.
	denied := ts.DoRequest("POST", completeRoute, devicerecovery.CompleteBody{ProtocolVersion: 2, TranscriptHash: digest}, access)
	require.Equal(t, 401, denied.Code, denied.Body.String())
	complete := ts.DoRequest("POST", completeRoute, devicerecovery.CompleteBody{ProtocolVersion: 2, TranscriptHash: digest}, headers)
	require.Equal(t, 200, complete.Code, complete.Body.String())
	poll = ts.DoRequest("GET", pathDeviceReqPoll+pending.RequestID, nil, headers)
	require.Equal(t, 200, poll.Code)
	require.NotContains(t, poll.Body.String(), "encrypted_payload")
	require.NotContains(t, poll.Body.String(), "requester_nonce")
	// Completion does not mark the recovery-purpose JTI spent for password reset.
	var used int64
	claims, err := mfa.ValidateChallengeToken(token, testhelpers.TestJWTSecret, mfa.PurposeRecovery)
	require.NoError(t, err)
	used, err = ts.Redis.Exists(context.Background(), "recovery_token_used:"+claims.ID).Result()
	require.NoError(t, err)
	require.Zero(t, used)
}
