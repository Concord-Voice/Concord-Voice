package messages

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBindPurgeRequest_Target(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		scopeID    = "11111111-2222-4333-8444-555555555555"
		canonical  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		equivalent = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
	)

	tests := []struct {
		name       string
		body       string
		wantTarget *string
		wantStatus int
		wantOK     bool
	}{
		{
			name:       "canonical target is preserved",
			body:       `{"range":"all","target_user_id":"` + canonical + `"}`,
			wantTarget: stringPointer(canonical),
			wantStatus: http.StatusOK,
			wantOK:     true,
		},
		{
			name:       "equivalent UUID spelling is canonicalized",
			body:       `{"range":"all","target_user_id":"` + equivalent + `"}`,
			wantTarget: stringPointer(canonical),
			wantStatus: http.StatusOK,
			wantOK:     true,
		},
		{
			name:       "malformed target is rejected",
			body:       `{"range":"all","target_user_id":"not-a-uuid"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty target is rejected",
			body:       `{"range":"all","target_user_id":""}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "omitted target stays nil",
			body:       `{"range":"all"}`,
			wantStatus: http.StatusOK,
			wantOK:     true,
		},
		{
			name:       "null target stays nil",
			body:       `{"range":"all","target_user_id":null}`,
			wantStatus: http.StatusOK,
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/channels/"+scopeID+"/messages", strings.NewReader(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")

			req, _, ok := bindPurgeRequest(c, scopeID, "Invalid scope ID")

			require.Equal(t, tt.wantStatus, w.Code)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				require.Contains(t, w.Body.String(), "Invalid target user ID")
				return
			}
			if tt.wantTarget == nil {
				require.Nil(t, req.TargetUserID)
				return
			}
			require.NotNil(t, req.TargetUserID)
			require.Equal(t, *tt.wantTarget, *req.TargetUserID)
		})
	}
}

func stringPointer(value string) *string { return &value }
