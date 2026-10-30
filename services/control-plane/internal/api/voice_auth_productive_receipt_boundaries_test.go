package api

import (
	"database/sql"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Only the handler transaction pool is wrapped. Authentication/epoch middleware
// keeps the ordinary pool, so unrelated transactions cannot consume the seam.
func productiveReceiptRouter(t *testing.T, r *voiceAuthRig, db *sql.DB, boot string) *gin.Engine {
	t.Helper()
	rc := redistest.Client(t)
	h := newVoiceEnforcementSessionHandler(db, r.secret, &bootstrapRequester{secret: r.secret, boot: boot})
	router := gin.New()
	registerVoiceAuthAliases(router.Group("/api/v1"))
	group := router.Group("", middleware.AuthRequired(r.secret, rc, r.db, credepoch.New(r.db, rc, nil)), middleware.MediaPlaneServiceHop(r.secret, nil), middleware.RequireVerifiedEmail())
	group.POST("/api/v1/internal/voice/auth-connections", h.RegisterConnection)
	group.POST("/api/v1/voice/enforcement-sessions", h.Register)
	router.POST("/api/v1/internal/voice/enforcement/health", h.HealthBootstrap)
	return router
}

func TestVoiceAuthProductiveReceiptCommitBoundaries(t *testing.T) {
	for _, writer := range []string{"connection_register", "channel_attach", "dm_attach", "channel_renew", "dm_renew", "prepared_bootstrap"} {
		for _, acknowledgement := range []string{"definite", "unknown"} {
			t.Run(writer+"/"+acknowledgement, func(t *testing.T) {
				var r *voiceAuthRig
				if writer == "prepared_bootstrap" {
					r = newVoiceAuthRigPrepared(t)
				} else {
					r = newVoiceAuthRig(t)
				}
				endpoint := protocol.EndpointConnectionRegister
				var payload any = r.registration()
				binding := protocol.Binding{UserID: r.userID.String(), FamilyID: r.familyID.String(), Target: r.target}
				var room *protocol.RoomRegister
				expected := protocol.ExpectedResponse{UserID: r.userID.String()}
				boot := r.target.NodeBootID
				if strings.Contains(writer, "attach") || strings.Contains(writer, "renew") {
					binding = r.register(t)
					kind := "channel"
					if strings.HasPrefix(writer, "dm_") {
						kind = "dm"
					}
					p := roomBindingFixture(t, r, kind)
					if strings.HasSuffix(writer, "renew") {
						roomBindingAttach(t, r, binding, p)
						p.Mode = "renew"
					}
					room = &p
					payload = p
					endpoint = protocol.EndpointRoomRegister
					expected = protocol.ExpectedResponse{Binding: &binding}
				}
				var wire protocol.HTTPRequest
				var request protocol.Request
				if writer == "prepared_bootstrap" {
					boot = uuid.NewString()
					r.target.NodeBootID = boot
					endpoint = protocol.EndpointHealthBootstrap
					expected = protocol.ExpectedResponse{}
					wire, request = bootstrapWire(t, r, boot)
					bootstrapInstall(r, newVoiceEnforcementSessionHandler(r.db, r.secret, &bootstrapRequester{secret: r.secret, boot: boot}))
				} else {
					wire, request = r.signed(t, endpoint, payload, r.token)
				}
				connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
				require.NoError(t, err)
				var wrapped *sql.DB
				state := &closurePausedCommit{ready: make(chan struct{}), resume: make(chan struct{})}
				var resume sync.Once
				if acknowledgement == "definite" {
					wrapped = sql.OpenDB(closurePausedConnector{Connector: connector, state: state})

				} else {
					wrapped = sql.OpenDB(closureUnknownConnector{connector})
				}
				t.Cleanup(func() { require.NoError(t, wrapped.Close()) })
				if acknowledgement == "definite" {
					t.Cleanup(func() { resume.Do(func() { close(state.resume) }) })
				}
				result := retentionReceiptStart(productiveReceiptRouter(t, r, wrapped, boot), wire)
				if acknowledgement == "definite" {
					retentionReceiptWait(t, state.ready)
					select {
					case <-result:
						t.Fatal("HTTP response escaped before COMMIT acknowledgement")
					default:
					}
				} else {
					closureFailure(t, retentionReceiptResult(t, result), http.StatusServiceUnavailable)
				}
				// The read goes to the unwrapped pool and observes the physically committed
				// exact row, including renew's already-existing generation.
				var durable int
				if writer == "prepared_bootstrap" {
					require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_boot_fences WHERE node_boot_id=$1 AND retired_at IS NULL`, boot).Scan(&durable))
				} else {
					var got protocol.Binding
					require.NoError(t, r.db.QueryRow(`SELECT user_id,family_id,connection_generation,node_boot_id,socket_id,credential_epoch FROM voice_auth_connections WHERE connection_generation=$1`, binding.Target.ConnectionGeneration).Scan(&got.UserID, &got.FamilyID, &got.Target.ConnectionGeneration, &got.Target.NodeBootID, &got.Target.SocketID, &got.Target.CredentialEpoch))
					require.Equal(t, binding, got)
					durable = 1
					if room != nil {
						require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_bindings", room.SessionGeneration))
						var parent string
						require.NoError(t, r.db.QueryRow(`SELECT connection_generation FROM voice_auth_room_bindings WHERE session_generation=$1`, room.SessionGeneration).Scan(&parent))
						require.Equal(t, binding.Target.ConnectionGeneration, parent)
					}
				}
				require.Equal(t, 1, durable)
				if acknowledgement == "definite" {
					if writer != "prepared_bootstrap" {
						runtimeRelease(t, r, binding, room)
					}
					retentionReceiptRetire(t, r)
					gc := retentionReceiptGC(t, r)
					if writer != "prepared_bootstrap" {
						require.EqualValues(t, 1, gc.DeletedConnectionClosures)
						if room != nil {
							require.EqualValues(t, 1, gc.DeletedRoomClosures)
						}
					}
					require.NoError(t, r.db.QueryRow(`SELECT count(*) FROM voice_auth_connections WHERE node_boot_id=$1`, boot).Scan(&durable))
					require.Zero(t, durable)
					if room != nil {
						require.Zero(t, roomBindingCount(t, r, "voice_auth_room_bindings", room.SessionGeneration))
						require.Zero(t, roomBindingCount(t, r, "voice_auth_room_closures", room.SessionGeneration))
					}
					resume.Do(func() { close(state.resume) })
					response := r.verify(t, endpoint, retentionReceiptResult(t, result), request, expected)
					productiveReceiptValue(t, writer, response, binding, room, boot)
					require.Zero(t, state.subsequentSQL.Load(), "signing must use request-owned copied receipt")
				} else {
					// Retry carries a fresh proof and goes through the ordinary productive
					// routes. Unknown commit is durable; it is never modeled as rollback.
					if writer == "prepared_bootstrap" {
						wire, request = bootstrapWire(t, r, boot)
					} else {
						wire, request = r.signed(t, endpoint, payload, r.token)
					}
					response := r.verify(t, endpoint, r.do(wire), request, expected)
					productiveReceiptValue(t, writer, response, binding, room, boot)
				}
			})
		}
	}
}
func productiveReceiptValue(t *testing.T, writer string, response protocol.Response, binding protocol.Binding, room *protocol.RoomRegister, boot string) {
	t.Helper()
	if writer == "prepared_bootstrap" {
		value, ok := response.HealthBootstrap()
		require.True(t, ok)
		require.Equal(t, protocol.HealthBootstrapResult{Version: 1, VoiceEnforcementProtocol: 4, NodeBootID: boot, Activation: "prepared", TransportOK: true, AdmissionGranted: false}, value)
		return
	}
	if room != nil {
		value, ok := response.RoomRegistration()
		require.True(t, ok)
		require.Equal(t, binding, value.Binding)
		require.Equal(t, roomBindingTuple(*room), value.Room)
		return
	}
	value, ok := response.Registration()
	require.True(t, ok)
	require.Equal(t, binding, protocol.Binding{UserID: value.UserID, FamilyID: value.FamilyID, Target: value.Target})
}
