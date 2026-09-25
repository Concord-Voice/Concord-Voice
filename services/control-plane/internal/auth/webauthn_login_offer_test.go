package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// A security key offered without the options to answer it strands the user:
// the client picks it and nothing happens, and for an account whose only
// factor is a key that is no way in (silent-failure review, #3460). These
// cases pin that WebAuthn is offered only with its options, and that a
// challenge nothing can answer is an error, on every path that builds one.

// webAuthnOfferStub is the shared MFA-checker stub with a scripted WebAuthn
// ceremony.
type webAuthnOfferStub struct {
	mfaMethodsStub
	options interface{}
	err     error
}

func (s *webAuthnOfferStub) BeginWebAuthnLogin(context.Context, string, string) (interface{}, error) {
	return s.options, s.err
}

var (
	errCeremonyUnavailable = errors.New("session store unavailable")
	ceremonyOptions        = map[string]string{"challenge": "fixture"}
)

// challengesRecorded counts the challenge-required events, which may be
// recorded only for a challenge that was actually sent.
func challengesRecorded(r *securityEventRecorder) int {
	n := 0
	for _, event := range r.events {
		if event.ReasonCode == securityevent.ReasonChallengeRequired {
			n++
		}
	}
	return n
}

func TestWebAuthnLoginOfferDropsAKeyItCannotOffer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		methods []string
		options interface{}
		err     error
		offered []string
		failed  bool
	}{
		{name: "ceremony begins", methods: []string{"webauthn", "totp"}, options: ceremonyOptions, offered: []string{"webauthn", "totp"}},
		{name: "ceremony fails, another method", methods: []string{"webauthn", "totp"}, err: errCeremonyUnavailable, offered: []string{"totp"}},
		{name: "no key registered, another method", methods: []string{"totp", "webauthn"}, offered: []string{"totp"}},
		{name: "ceremony fails, key only", methods: []string{"webauthn"}, err: errCeremonyUnavailable, failed: true},
		{name: "no key registered, key only", methods: []string{"webauthn"}, failed: true},
		{name: "no key offered", methods: []string{"totp"}, err: errCeremonyUnavailable, offered: []string{"totp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{mfaChecker: &webAuthnOfferStub{options: tc.options, err: tc.err}, log: logger.NewWithWriter(io.Discard)}

			offered, options, err := h.webAuthnLoginOffer(context.Background(), "user-1", "jti-1", tc.methods)

			if tc.failed {
				require.ErrorIs(t, err, errNoAnswerableMFAMethod)
				require.Nil(t, offered)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.offered, offered)
			if containsMethod(tc.offered, "webauthn") {
				require.Equal(t, ceremonyOptions, options)
			} else {
				require.Nil(t, options, "no options may accompany an offer without webauthn")
			}
		})
	}
}

func TestHandleMFAChallengeNeverOffersAKeyWithoutItsOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		methods []string
		options interface{}
		err     error
		status  int
		offered []string
	}{
		{name: "ceremony begins", methods: []string{"webauthn"}, options: ceremonyOptions, status: http.StatusOK, offered: []string{"webauthn"}},
		{name: "ceremony fails, TOTP remains", methods: []string{"webauthn", "totp"}, err: errCeremonyUnavailable, status: http.StatusOK, offered: []string{"totp"}},
		{name: "ceremony fails, key only", methods: []string{"webauthn"}, err: errCeremonyUnavailable, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &webAuthnOfferStub{
				mfaMethodsStub: mfaMethodsStub{loginMethods: tc.methods, enabledMethods: tc.methods, challengeTok: "challenge-token"},
				options:        tc.options,
				err:            tc.err,
			}
			rdb := rememberMeRedis(t)
			h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard), redis: rdb}
			recorder := &securityEventRecorder{}
			h.SetSecurityEvents(recorder)
			c, rec := newMFAChallengeRecorder()

			handled := h.handleMFAChallenge(c.Request.Context(), c, "user-1", false, "epoch-1", securityevent.AuthPassword)

			require.True(t, handled)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.status != http.StatusOK {
				require.Contains(t, rec.Body.String(), errMsgMFAUnavailable)
				require.NotContains(t, rec.Body.String(), "challenge-token", "no unanswerable challenge may be handed out")
				require.Zero(t, challengesRecorded(recorder), "a challenge that was not sent must not be recorded")
				require.False(t, rememberMeStored(t, rdb), "an unsent challenge's remember-me state must be discarded")
				return
			}
			require.True(t, rememberMeStored(t, rdb), "a sent challenge keeps its remember-me state")
			require.Equal(t, 1, challengesRecorded(recorder))
			var body struct {
				Methods         []string    `json:"methods"`
				WebAuthnOptions interface{} `json:"webauthn_options"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, tc.offered, body.Methods)
			require.Equal(t, containsMethod(tc.offered, "webauthn"), body.WebAuthnOptions != nil,
				"webauthn is offered exactly when its options are")
		})
	}
}

func TestBuildMFAChallengeResponseNeverOffersAKeyWithoutItsOptions(t *testing.T) {
	stub := &webAuthnOfferStub{
		mfaMethodsStub: mfaMethodsStub{loginMethods: []string{"webauthn", "totp"}, enabledMethods: []string{"webauthn", "totp"}},
		err:            errCeremonyUnavailable,
	}
	h := &Handler{mfaChecker: stub, log: logger.NewWithWriter(io.Discard)}

	resp, err := h.buildMFAChallengeResponse(context.Background(), "mfa_upgrade_required", "Verify", "tok", "user-1", "jti-1")

	require.NoError(t, err)
	require.Equal(t, []string{"totp"}, resp["methods"])
	require.NotContains(t, resp, "webauthn_options")

	stub.loginMethods = []string{"webauthn"}
	resp, err = h.buildMFAChallengeResponse(context.Background(), "mfa_upgrade_required", "Verify", "tok", "user-1", "jti-1")

	require.ErrorIs(t, err, errNoAnswerableMFAMethod)
	require.Nil(t, resp)
}

func TestIssueMFAChallengeNeverOffersAKeyWithoutItsOptions(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	userID := dbtest.CreateUser(t, db).String()
	stub := &webAuthnOfferStub{
		mfaMethodsStub: mfaMethodsStub{loginMethods: []string{"webauthn", "totp"}, enabledMethods: []string{"webauthn", "totp"}, challengeTok: "sso-challenge"},
		err:            errCeremonyUnavailable,
	}
	rdb := rememberMeRedis(t)
	h := NewHandler(db, rdb, logger.NewWithWriter(io.Discard), "", nil)
	h.SetMFAChecker(stub)
	recorder := &securityEventRecorder{}
	h.SetSecurityEvents(recorder)

	token, methods, _, options, mfaEnabled, err := h.IssueMFAChallenge(context.Background(), userID)

	require.NoError(t, err)
	require.True(t, mfaEnabled)
	require.Equal(t, "sso-challenge", token)
	require.Equal(t, []string{"totp"}, methods)
	require.Nil(t, options)
	require.Equal(t, 1, challengesRecorded(recorder))
	require.True(t, rememberMeStored(t, rdb), "a sent challenge keeps its remember-me state")

	stub.loginMethods = []string{"webauthn"}
	_, _, _, _, _, err = h.IssueMFAChallenge(context.Background(), userID)

	require.ErrorIs(t, err, errNoAnswerableMFAMethod)
	require.Equal(t, 1, challengesRecorded(recorder), "a challenge that was not sent must not be recorded")
	require.False(t, rememberMeStored(t, rdb), "an unsent challenge's remember-me state must be discarded")
}

// rememberMeRedis is a miniredis client holding the remember-me state the MFA
// package would have stored for the stub's challenge ("stub-jti"), since the
// stub itself writes nothing.
func rememberMeRedis(t *testing.T) *redis.Client {
	t.Helper()
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	require.NoError(t, rdb.Set(context.Background(), MFAChallengeRememberMeKey("stub-jti"), "1", time.Minute).Err())
	return rdb
}

func rememberMeStored(t *testing.T, rdb *redis.Client) bool {
	t.Helper()
	n, err := rdb.Exists(context.Background(), MFAChallengeRememberMeKey("stub-jti")).Result()
	require.NoError(t, err)
	return n == 1
}
