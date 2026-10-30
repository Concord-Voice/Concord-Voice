package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	protocol "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// This driver seam stops the actual handler before its first closure SELECT.
// It neither grants authority nor substitutes any SQL result. Wrapping the
// existing commit seam also observes the physical commit before signing.
type retentionReadConnector struct {
	driver.Connector
	query         string
	ready, resume chan struct{}
	once          *sync.Once
}

func (c retentionReadConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return retentionReadConn{Conn: conn, seam: c}, nil
}

type retentionReadConn struct {
	driver.Conn
	seam retentionReadConnector
}

func (c retentionReadConn) Prepare(query string) (driver.Stmt, error) {
	if strings.Contains(query, c.seam.query) {
		c.seam.once.Do(func() { close(c.seam.ready); <-c.seam.resume })
	}
	return c.Conn.Prepare(query)
}
func (c retentionReadConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}
func retentionReceiptRouter(db *sql.DB, secret string) *gin.Engine {
	h := newVoiceEnforcementSessionHandler(db, secret)
	router := gin.New()
	router.POST("/api/v1/internal/voice/auth-connections/release", h.ReleaseConnection)
	router.POST("/api/v1/internal/voice/auth-connections/provisional-close", h.ProvisionalClose)
	router.POST("/api/v1/internal/voice/enforcement-sessions/release", h.Release)
	return router
}
func retentionReceiptStart(router *gin.Engine, wire protocol.HTTPRequest) <-chan *httptest.ResponseRecorder {
	request := httptest.NewRequest(wire.Method, wire.Path, strings.NewReader(string(wire.Body)))
	for _, header := range wire.Headers {
		request.Header.Add(header.Name, header.Value)
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); router.ServeHTTP(w, request); result <- w }()
	return result
}
func retentionReceiptWait(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not reach observed SQL barrier")
	}
}
func retentionReceiptResult(t *testing.T, result <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-result:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
		return nil
	}
}
func retentionReceiptRetire(t *testing.T, r *voiceAuthRig) {
	t.Helper()
	// The existing SQL trigger freshly rejects retirement while a live parent or
	// room remains. This is disposable fixture retirement, not a native witness.
	_, err := r.db.Exec(`UPDATE voice_auth_boot_fences SET retired_at=clock_timestamp(),updated_at=clock_timestamp() WHERE node_boot_id=$1`, r.target.NodeBootID)
	require.NoError(t, err)
}
func retentionReceiptGC(t *testing.T, r *voiceAuthRig) loginfamily.GCResult {
	t.Helper()
	result, err := loginfamily.SweepRetiredEvidence(t.Context(), r.db, 1)
	require.NoError(t, err)
	return result
}

func TestVoiceAuthRetentionReplayOwnsGenerationUntilCopy(t *testing.T) {
	for _, variant := range []string{"connection", "registered_provisional", "unregistered_provisional", "channel_room", "dm_room"} {
		t.Run(variant, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
			var binding protocol.Binding
			if variant != "unregistered_provisional" {
				binding = r.register(t)
			}
			endpoint := protocol.EndpointConnectionRelease
			var payload any = closureRelease(binding)
			expected := protocol.ExpectedResponse{Binding: &binding}
			room := strings.HasSuffix(variant, "_room")
			roomGeneration := ""
			if room {
				p := roomBindingFixture(t, r, strings.TrimSuffix(variant, "_room"))
				roomBindingAttach(t, r, binding, p)
				roomGeneration = p.SessionGeneration
				endpoint, payload = protocol.EndpointRoomRelease, roomBindingRelease(binding, p)
			} else if strings.HasSuffix(variant, "provisional") {
				endpoint, payload, expected = protocol.EndpointProvisionalClose, closureRequest(r, registration), protocol.ExpectedResponse{}
			}
			w, request := r.request(t, endpoint, payload, "")
			r.verify(t, endpoint, w, request, expected)
			if room {
				w, request = r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
				r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
			}
			retentionReceiptRetire(t, r)
			state := &closurePausedCommit{ready: make(chan struct{}), resume: make(chan struct{})}
			connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
			require.NoError(t, err)
			query := "FROM voice_auth_connection_closures WHERE connection_generation=$1"
			if room {
				query = "FROM voice_auth_room_closures WHERE session_generation=$1"
			}
			seam := retentionReadConnector{Connector: closurePausedConnector{Connector: connector, state: state}, query: query, ready: make(chan struct{}), resume: make(chan struct{}), once: &sync.Once{}}
			db := sql.OpenDB(seam)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			var readResume, commitResume sync.Once
			t.Cleanup(func() { readResume.Do(func() { close(seam.resume) }); commitResume.Do(func() { close(state.resume) }) })
			wire, retained := r.signed(t, endpoint, payload, "")
			result := retentionReceiptStart(retentionReceiptRouter(db, r.secret), wire)
			retentionReceiptWait(t, seam.ready)
			gc := retentionReceiptGC(t, r)
			require.True(t, gc.Incomplete)
			require.Zero(t, gc.DeletedConnectionClosures)
			require.Zero(t, gc.DeletedRoomClosures)
			_, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Equal(t, 1, closed, "actual handler must already own parent generation")
			if room {
				require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_closures", roomGeneration))
			}
			readResume.Do(func() { close(seam.resume) })
			retentionReceiptWait(t, state.ready)
			select {
			case <-result:
				t.Fatal("response preceded definite commit acknowledgement")
			default:
			}
			gc = retentionReceiptGC(t, r)
			require.EqualValues(t, 1, gc.DeletedConnectionClosures)
			if room {
				require.EqualValues(t, 1, gc.DeletedRoomClosures)
			}
			commitResume.Do(func() { close(state.resume) })
			response := r.verify(t, endpoint, retentionReceiptResult(t, result), retained, expected)
			if endpoint == protocol.EndpointProvisionalClose {
				value, ok := response.ProvisionalClose()
				require.True(t, ok)
				disposition := "registered"
				if variant == "unregistered_provisional" {
					disposition = "unregistered"
				}
				require.Equal(t, disposition, value.Disposition)
			}
			require.Zero(t, state.subsequentSQL.Load(), "signing must use only the request-owned copy")
			// Reverse ordering: GC has already won. No handler may recreate evidence.
			w, request = r.request(t, endpoint, payload, "")
			if endpoint == protocol.EndpointProvisionalClose {
				value, ok := r.verify(t, endpoint, w, request, expected).ProvisionalClose()
				require.True(t, ok)
				require.Equal(t, "retired_boot", value.Disposition)
				require.Nil(t, value.Binding)
			} else {
				closureFailure(t, w, http.StatusGone)
			}
			live, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
			require.Zero(t, live)
			require.Zero(t, closed)
			if room {
				require.Zero(t, roomBindingCount(t, r, "voice_auth_room_closures", roomGeneration))
			}
		})
	}
}

func TestVoiceAuthRetentionInitialRoomReceiptSurvivesCollection(t *testing.T) {
	for _, kind := range []string{"channel", "dm"} {
		t.Run(kind, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			binding := r.register(t)
			p := roomBindingFixture(t, r, kind)
			roomBindingAttach(t, r, binding, p)
			state := &closurePausedCommit{ready: make(chan struct{}), resume: make(chan struct{})}
			connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
			require.NoError(t, err)
			db := sql.OpenDB(closurePausedConnector{Connector: connector, state: state})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			var resume sync.Once
			t.Cleanup(func() { resume.Do(func() { close(state.resume) }) })
			wire, retained := r.signed(t, protocol.EndpointRoomRelease, roomBindingRelease(binding, p), "")
			result := retentionReceiptStart(retentionReceiptRouter(db, r.secret), wire)
			retentionReceiptWait(t, state.ready)
			require.Zero(t, roomBindingCount(t, r, "voice_auth_room_bindings", p.SessionGeneration))
			require.Zero(t, roomBindingCount(t, r, "voice_enforcement_sessions", p.SessionGeneration))
			require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_closures", p.SessionGeneration))
			// Release the real parent after the child's physical commit; never fabricate
			// an absent parent while a child binding still requires its foreign key.
			w, request := r.request(t, protocol.EndpointConnectionRelease, closureRelease(binding), "")
			r.verify(t, protocol.EndpointConnectionRelease, w, request, protocol.ExpectedResponse{Binding: &binding})
			retentionReceiptRetire(t, r)
			gc := retentionReceiptGC(t, r)
			require.EqualValues(t, 1, gc.DeletedRoomClosures)
			require.EqualValues(t, 1, gc.DeletedConnectionClosures)
			select {
			case <-result:
				t.Fatal("receipt escaped before definite commit")
			default:
			}
			resume.Do(func() { close(state.resume) })
			response := r.verify(t, protocol.EndpointRoomRelease, retentionReceiptResult(t, result), retained, protocol.ExpectedResponse{Binding: &binding})
			require.True(t, response.Empty())
			require.Zero(t, state.subsequentSQL.Load())
		})
	}
}

func TestVoiceAuthRetentionAdditionalUnknownCommitWriters(t *testing.T) {
	for _, variant := range []string{"unregistered_provisional", "channel_room", "dm_room"} {
		t.Run(variant, func(t *testing.T) {
			r := newVoiceAuthRig(t)
			registration, _ := r.signed(t, protocol.EndpointConnectionRegister, r.registration(), r.token)
			endpoint := protocol.EndpointProvisionalClose
			var payload any = closureRequest(r, registration)
			expected := protocol.ExpectedResponse{}
			roomGeneration := ""
			if variant != "unregistered_provisional" {
				binding := r.register(t)
				p := roomBindingFixture(t, r, strings.TrimSuffix(variant, "_room"))
				roomBindingAttach(t, r, binding, p)
				endpoint, payload, expected = protocol.EndpointRoomRelease, roomBindingRelease(binding, p), protocol.ExpectedResponse{Binding: &binding}
				roomGeneration = p.SessionGeneration
			}
			connector, err := pq.NewConnector(os.Getenv("DATABASE_URL"))
			require.NoError(t, err)
			db := sql.OpenDB(closureUnknownConnector{connector})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			wire, _ := r.signed(t, endpoint, payload, "")
			closureFailure(t, retentionReceiptResult(t, retentionReceiptStart(retentionReceiptRouter(db, r.secret), wire)), http.StatusServiceUnavailable)
			if roomGeneration != "" {
				require.Equal(t, 1, roomBindingCount(t, r, "voice_auth_room_closures", roomGeneration))
				require.Zero(t, roomBindingCount(t, r, "voice_auth_room_bindings", roomGeneration))
			} else {
				_, closed := closureCount(t, r.db, r.target.ConnectionGeneration)
				require.Equal(t, 1, closed)
			}
			w, request := r.request(t, endpoint, payload, "")
			r.verify(t, endpoint, w, request, expected)
		})
	}
}
