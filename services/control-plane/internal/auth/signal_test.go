package auth

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// regression for #3290
//
// Exhaustive over {"", "a", "b"}^2. The only false verdict is
// both-known-and-disagreeing; every absence is a pass, because absence is not
// evidence. If a future change makes an absent value compare as a mismatch,
// this table is what fails.
func TestSignalMatch(t *testing.T) {
	cases := []struct {
		name              string
		stored, presented string
		wantKnown, wantOK bool
	}{
		{"neither side knows", "", "", false, true},
		{"never learned it", "", "a", false, true},
		{"never learned it, other value", "", "b", false, true},
		{"caller did not present it", "a", "", false, true},
		{"both known and agree", "a", "a", true, true},
		{"both known and disagree", "a", "b", true, false},
		{"both known and disagree, reversed", "b", "a", true, false},
		{"caller did not present it, other value", "b", "", false, true},
		{"both known and agree, other value", "b", "b", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			known, ok := signalMatch(c.stored, c.presented)
			if known != c.wantKnown || ok != c.wantOK {
				t.Errorf("signalMatch(%q, %q) = (known=%v, ok=%v), want (known=%v, ok=%v)",
					c.stored, c.presented, known, ok, c.wantKnown, c.wantOK)
			}
		})
	}
}

func TestRefresh_GraceLookupErrorReturns500(t *testing.T) {
	db := sql.OpenDB(noRowsConnector{})
	require.NoError(t, db.Close())
	h := &Handler{db: db, log: logger.New("test")}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	require.True(t, h.attemptGracePeriodRecovery(c, "replayed-hash"))
	require.Equal(t, http.StatusInternalServerError, w.Code)
}
