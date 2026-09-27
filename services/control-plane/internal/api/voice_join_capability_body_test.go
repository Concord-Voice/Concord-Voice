package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type activatedVoiceRolloutConnector struct{}

func (activatedVoiceRolloutConnector) Connect(context.Context) (driver.Conn, error) {
	return &activatedVoiceRolloutConn{}, nil
}

func (activatedVoiceRolloutConnector) Driver() driver.Driver { return activatedVoiceRolloutDriver{} }

type activatedVoiceRolloutDriver struct{}

func (activatedVoiceRolloutDriver) Open(string) (driver.Conn, error) {
	return &activatedVoiceRolloutConn{}, nil
}

type activatedVoiceRolloutConn struct{}

func (*activatedVoiceRolloutConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*activatedVoiceRolloutConn) Close() error                        { return nil }
func (*activatedVoiceRolloutConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (*activatedVoiceRolloutConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &activatedVoiceRolloutRows{}, nil
}

type activatedVoiceRolloutRows struct {
	done bool
}

func (*activatedVoiceRolloutRows) Columns() []string { return []string{"activated"} }
func (r *activatedVoiceRolloutRows) Close() error    { return nil }
func (r *activatedVoiceRolloutRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = true
	return nil
}

func TestRequireMediaCapabilityBindsTheAdmissionBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const token = "test-token-3143"
	nodeBootID := uuid.MustParse("22222222-2222-4222-8222-222222222222").String()
	body := []byte(`{"channel_id":"33333333-3333-4333-8333-333333333333","user_id":"44444444-4444-4444-8444-444444444444"}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	requestURI := "/api/v1/channels/33333333-3333-4333-8333-333333333333/voice/join"
	digest := sha256.Sum256(body)
	proof := mediaproof.Sign(
		mediaproof.DeriveKey(voiceEnforcementProtocolTestSecret, voiceEnforcementCapabilityProofContext),
		voiceEnforcementCapabilityProofVersion, timestamp,
		http.MethodPost, requestURI, mediaproof.TokenDigest(token), nodeBootID, fmt.Sprintf("%x", digest),
	)

	for _, test := range []struct {
		name        string
		requestBody []byte
		status      int
		aborted     bool
	}{
		{name: "accepts signed channel admission body", requestBody: body, status: http.StatusOK, aborted: false},
		{name: "rejects body tampered after signing", requestBody: []byte(`{"channel_id":"33333333-3333-4333-8333-333333333333","user_id":"55555555-5555-4555-8555-555555555555"}`), status: http.StatusForbidden, aborted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, requestURI, bytes.NewReader(test.requestBody))
			ctx.Request.Header.Set("Authorization", "Bearer "+token)
			ctx.Request.Header.Set("X-Concord-Voice-Enforcement-Node-Boot-ID", nodeBootID)
			ctx.Request.Header.Set(voiceEnforcementCapabilityTimestampHeader, timestamp)
			ctx.Request.Header.Set(voiceEnforcementCapabilityProofHeader, proof)
			ctx.Set("concord_service_hop", true)

			db := sql.OpenDB(activatedVoiceRolloutConnector{})
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			newVoiceEnforcementSessionHandler(db, voiceEnforcementProtocolTestSecret).RequireMediaCapability(ctx)
			ctx.Writer.WriteHeaderNow()

			assert.Equal(t, test.status, recorder.Code)
			assert.Equal(t, test.aborted, ctx.IsAborted())
		})
	}
}
