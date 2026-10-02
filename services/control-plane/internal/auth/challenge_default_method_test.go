package auth

// The advisory default_method on the login, refresh and SSO MFA challenges
// (MFA picker spec §2). It is presentation input for the challenge modal, so it
// may never fail a challenge, and the value — derived from how recently each
// factor was used — may never reach a log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/require"
)

// fixedDefaultMethod is a DefaultMethodReader returning method/err and
// recording what it was asked.
func fixedDefaultMethod(method string, err error, gotUser *string, gotOffered *[]string) DefaultMethodReader {
	return func(_ context.Context, userID string, offered []string) (string, error) {
		*gotUser = userID
		*gotOffered = offered
		return method, err
	}
}

func TestChallengeDefaultMethod(t *testing.T) {
	ctx := context.Background()

	t.Run("no reader wired: omitted", func(t *testing.T) {
		h := &Handler{log: logger.NewWithWriter(io.Discard)}
		require.Equal(t, "", h.ChallengeDefaultMethod(ctx, "user-1", []string{"totp"}))
	})

	t.Run("reads within the offered methods", func(t *testing.T) {
		var user string
		var offered []string
		h := &Handler{log: logger.NewWithWriter(io.Discard)}
		h.SetDefaultMethodReader(fixedDefaultMethod("totp", nil, &user, &offered))

		require.Equal(t, "totp", h.ChallengeDefaultMethod(ctx, "user-1", []string{"totp", "email"}))
		require.Equal(t, "user-1", user)
		require.Equal(t, []string{"totp", "email"}, offered)
	})

	t.Run("a read failure is logged and omitted, never fatal", func(t *testing.T) {
		var logs bytes.Buffer
		var user string
		var offered []string
		h := &Handler{log: logger.NewWithWriter(&logs)}
		h.SetDefaultMethodReader(fixedDefaultMethod("webauthn", errors.New("db unavailable"), &user, &offered))

		require.Equal(t, "", h.ChallengeDefaultMethod(ctx, "user-1", []string{"webauthn"}))
		require.Contains(t, logs.String(), "Failed to read step-up factors")
		require.Contains(t, logs.String(), "db unavailable")
	})
}

func TestHandleMFAChallengeCarriesDefaultMethod(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		readErr error
		want    any // nil = field absent
	}{
		{name: "read succeeds", method: "totp", want: "totp"},
		{name: "no default for this challenge", method: "", want: nil},
		{name: "read fails", method: "totp", readErr: errors.New("db unavailable"), want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			var user string
			var offered []string
			stub := &mfaMethodsStub{loginMethods: []string{"totp"}, enabledMethods: []string{"totp"}, challengeTok: "challenge-token"}
			h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(&logs)}
			h.SetDefaultMethodReader(fixedDefaultMethod(tc.method, tc.readErr, &user, &offered))
			c, rec := newMFAChallengeRecorder()

			h.handleMFAChallenge(c.Request.Context(), c, "44444444-4444-4444-4444-444444444444", false, "epoch-1", securityevent.AuthPassword)

			require.Equal(t, http.StatusOK, rec.Code, "the default never decides whether a login proceeds")
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, "challenge-token", body["mfa_challenge_token"])
			got, present := body["default_method"]
			if tc.want == nil {
				require.False(t, present, "default_method must be omitted, not sent empty")
			} else {
				require.Equal(t, tc.want, got)
			}
			require.Equal(t, "44444444-4444-4444-4444-444444444444", user)
			require.Equal(t, []string{"totp"}, offered, "the reader is asked about exactly what the challenge offers")
			require.NotContains(t, logs.String(), "default_method", "the chosen factor never reaches a log")
		})
	}
}

func TestBuildMFAChallengeResponseCarriesDefaultMethod(t *testing.T) {
	var user string
	var offered []string
	stub := &mfaMethodsStub{loginMethods: []string{"totp"}, enabledMethods: []string{"totp"}}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	resp, err := h.buildMFAChallengeResponse(context.Background(),
		"mfa_upgrade_required", "Verify your identity", "tok", "user-5", "jti-5")
	require.NoError(t, err)
	require.NotContains(t, resp, "default_method", "no reader wired: omitted")

	h.SetDefaultMethodReader(fixedDefaultMethod("totp", nil, &user, &offered))
	resp, err = h.buildMFAChallengeResponse(context.Background(),
		"mfa_upgrade_required", "Verify your identity", "tok", "user-5", "jti-5")
	require.NoError(t, err)
	require.Equal(t, "totp", resp["default_method"])
	require.Equal(t, []string{"totp"}, offered)

	h.SetDefaultMethodReader(fixedDefaultMethod("totp", errors.New("db unavailable"), &user, &offered))
	resp, err = h.buildMFAChallengeResponse(context.Background(),
		"mfa_upgrade_required", "Verify your identity", "tok", "user-5", "jti-5")
	require.NoError(t, err, "a failed default read never fails the refresh challenge")
	require.NotContains(t, resp, "default_method")
}
