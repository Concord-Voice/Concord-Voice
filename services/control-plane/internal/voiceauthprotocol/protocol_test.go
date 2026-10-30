package voiceauthprotocol_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	p "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/voiceauthprotocol"
	"github.com/stretchr/testify/require"
)

type vector struct {
	ID, Direction, Transport, RawHex, RawSHA256, Method, Path, RequestID string
	Headers                                                              [][2]string
	Status                                                               int
	VerifierUnixSeconds                                                  string
	Signing                                                              *struct {
		Domain, Timestamp, SigningBytesHex, DerivedKeyHex, ExpectedMacHex string
		OrderedFields                                                     []string
	}
	Expected struct {
		Decode                         string
		Accept                         bool
		MacMatches, Freshness, Binding *bool
		Decoded                        json.RawMessage
	}
}
type corpus struct {
	TestOnlySecretHex string
	Records           []vector
}

func vectors(t *testing.T) (corpus, map[string]vector, []byte) {
	t.Helper()
	b, e := os.ReadFile("../api/testdata/auth_connection_protocol.json")
	require.NoError(t, e)
	var c corpus
	require.NoError(t, json.Unmarshal(b, &c))
	require.Len(t, c.Records, 143)
	m := map[string]vector{}
	for _, v := range c.Records {
		m[v.ID] = v
	}
	return c, m, unhex(t, c.TestOnlySecretHex)
}
func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	require.NoError(t, e)
	return b
}
func clock(t *testing.T, v vector) time.Time {
	t.Helper()
	n, e := strconv.ParseInt(v.VerifierUnixSeconds, 10, 64)
	require.NoError(t, e)
	return time.Unix(n, 0)
}
func endpoint(t *testing.T, v vector) p.Endpoint {
	t.Helper()
	switch strings.TrimSuffix(strings.Split(v.Path, "?")[0], "/") {
	case "/api/v1/internal/voice/auth-connections":
		return p.EndpointConnectionRegister
	case "/api/v1/internal/voice/auth-connections/release":
		return p.EndpointConnectionRelease
	case "/api/v1/internal/voice/auth-connections/provisional-close":
		return p.EndpointProvisionalClose
	case "/api/v1/voice/enforcement-sessions":
		return p.EndpointRoomRegister
	case "/api/v1/internal/voice/enforcement-sessions/release":
		return p.EndpointRoomRelease
	case "/api/v1/internal/voice/enforcement/health":
		return p.EndpointHealthBootstrap
	case "/api/v1/internal/voice/auth-connections/retire-boot":
		return p.EndpointBootRetire
	}
	// Alias test still invokes the matched endpoint with its original, noncanonical URI.
	if strings.HasPrefix(v.ID, "path-") {
		return p.EndpointConnectionRegister
	}
	t.Fatalf("unknown fixture path %q", v.Path)
	return ""
}
func headers(v vector) []p.Header {
	h := make([]p.Header, len(v.Headers))
	for i, x := range v.Headers {
		h[i] = p.Header{Name: x[0], Value: x[1]}
	}
	return h
}
func request(t *testing.T, v vector) p.HTTPRequest {
	return p.HTTPRequest{Method: v.Method, Path: v.Path, Headers: headers(v), Body: unhex(t, v.RawHex)}
}
func goldenBinding() p.Binding {
	return p.Binding{UserID: "00000003-1111-4111-8111-111111111111", FamilyID: "00000004-1111-4111-8111-111111111111", Target: p.Target{ConnectionGeneration: "00000001-1111-4111-8111-111111111111", NodeBootID: "00000002-1111-4111-8111-111111111111", SocketID: "test-socket-é", CredentialEpoch: "0123456789abcdef0123456789abcdef"}}
}
func contextFor(t *testing.T, v vector) (p.Request, p.RequestContext) {
	t.Helper()
	ep := endpoint(t, v)
	r, e := p.DecodeHTTPRequest(ep, request(t, v))
	require.NoError(t, e)
	expected := p.ExpectedResponse{}
	b := goldenBinding()
	switch ep {
	case p.EndpointConnectionRegister:
		expected.UserID = b.UserID
	case p.EndpointConnectionRelease, p.EndpointRoomRegister, p.EndpointRoomRelease:
		expected.Binding = &b
	}
	c, e := p.NewRequestContext(r, expected)
	require.NoError(t, e)
	return r, c
}

// These are authenticated wire messages whose refusal needs persisted commitments or
// actual native ownership. Codec success does not claim that the operation is authorized.
func stateOnly(id string) bool {
	switch id {
	case "valid-proof-wrong-owned-boot", "valid-proof-wrong-owned-generation", "valid-proof-wrong-owned-family", "provisional-wrong-original-token-digest", "provisional-wrong-original-body-digest", "provisional-wrong-target":
		return true
	}
	return false
}
func TestSharedRawProtocolVectors(t *testing.T) {
	c, index, secret := vectors(t)
	domains := map[string]bool{}
	for _, v := range c.Records {
		t.Run(v.ID, func(t *testing.T) {
			raw := unhex(t, v.RawHex)
			digest := sha256.Sum256(raw)
			require.Equal(t, v.RawSHA256, hex.EncodeToString(digest[:]))
			if v.Signing != nil {
				s := v.Signing
				domains[s.Domain] = true
				require.Equal(t, unhex(t, s.SigningBytesHex), []byte(strings.Join(append([]string{"v1", s.Timestamp}, s.OrderedFields...), "\n")))
				k := hmac.New(sha256.New, secret)
				_, e := k.Write([]byte(s.Domain))
				require.NoError(t, e)
				require.Equal(t, s.DerivedKeyHex, hex.EncodeToString(k.Sum(nil)))
				mac := hmac.New(sha256.New, k.Sum(nil))
				_, e = mac.Write(unhex(t, s.SigningBytesHex))
				require.NoError(t, e)
				require.Equal(t, s.ExpectedMacHex, hex.EncodeToString(mac.Sum(nil)))
			}
			var err error
			if v.Transport == "nats" {
				if v.Direction == "request" {
					var cmd p.Command
					cmd, err = p.DecodeCommand(raw)
					if v.Expected.Decode == "reject" {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					assertCommandPayload(t, cmd, v.Expected.Decoded)
					err = p.VerifyCommand(cmd, secret, clock(t, v))
				} else {
					origin, e := p.DecodeCommand(unhex(t, index[v.RequestID].RawHex))
					require.NoError(t, e)
					_, err = p.VerifyACK(raw, origin, secret, clock(t, v))
				}
			} else if v.Direction == "request" {
				var r p.Request
				r, err = p.DecodeHTTPRequest(endpoint(t, v), request(t, v))
				if err == nil {
					assertRequestPayload(t, r, v.Expected.Decoded)
					err = p.VerifyHTTPRequest(r, secret, clock(t, v))
				}
			} else {
				_, ctx := contextFor(t, index[v.RequestID])
				_, err = p.VerifyHTTPResponse(endpoint(t, v), p.HTTPResponse{Status: v.Status, Headers: headers(v), Body: raw}, ctx, secret, clock(t, v))
			}
			if v.Expected.Accept || stateOnly(v.ID) {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	require.Len(t, domains, 20)
}

func TestRequestSigningUsesOriginalBytesAndGoldenProof(t *testing.T) {
	c, _, secret := vectors(t)
	for _, v := range c.Records {
		if v.Transport != "http" || v.Direction != "request" || !v.Expected.Accept {
			continue
		}
		t.Run(v.ID, func(t *testing.T) {
			raw := unhex(t, v.RawHex)
			token := ""
			for _, h := range v.Headers {
				if h[0] == "Authorization" {
					token = strings.TrimPrefix(h[1], "Bearer ")
				}
			}
			wire, r, e := p.SignHTTPRequest(endpoint(t, v), raw, token, p.AttemptMeta{Timestamp: v.Signing.Timestamp, Nonce: v.Headers[1][1]}, secret)
			require.NoError(t, e)
			require.Equal(t, raw, wire.Body)
			require.Equal(t, v.Path, wire.Path)
			require.NoError(t, p.VerifyHTTPRequest(r, secret, clock(t, v)))
			proof := ""
			for _, h := range wire.Headers {
				if strings.HasSuffix(h.Name, "-Proof") {
					proof = h.Value
				}
			}
			require.Equal(t, v.Signing.ExpectedMacHex, proof)
			raw[0] = '!'
			wire.Body[0] = '!'
			wire.Headers[0].Value = "0"
			require.NoError(t, p.VerifyHTTPRequest(r, secret, clock(t, v)))
		})
	}
}
func TestPrivateStateAndRequestContextRejectInvalidInputs(t *testing.T) {
	_, index, secret := vectors(t)
	now := clock(t, index["connection-register"])
	require.Error(t, p.VerifyCommand(p.Command{}, secret, now))
	require.Error(t, p.VerifyHTTPRequest(p.Request{}, secret, now))
	_, e := p.SignCommand(p.Command{}, secret)
	require.Error(t, e)
	_, e = p.SignAuthACK(p.Command{}, secret)
	require.Error(t, e)
	_, e = p.SignEpochACK(p.Command{}, secret)
	require.Error(t, e)
	_, e = p.SignHealthACK(p.Command{}, false, secret)
	require.Error(t, e)
	_, e = p.NewRequestContext(p.Request{}, p.ExpectedResponse{})
	require.Error(t, e)
	_, e = p.SignHTTPResponse(p.Request{}, nil, "1793000000", secret)
	require.Error(t, e)
	v := index["connection-register-response"]
	_, e = p.VerifyHTTPResponse(p.EndpointConnectionRegister, p.HTTPResponse{Status: v.Status, Headers: headers(v), Body: unhex(t, v.RawHex)}, p.RequestContext{}, secret, now)
	require.Error(t, e)
	r, e := p.DecodeHTTPRequest(p.EndpointConnectionRegister, request(t, index["connection-register"]))
	require.NoError(t, e)
	_, e = p.NewRequestContext(r, p.ExpectedResponse{})
	require.Error(t, e)
	_, _, e = p.SignHTTPRequest(p.EndpointConnectionRegister, []byte("{}"), "", p.AttemptMeta{}, secret)
	require.Error(t, e)
	_, e = p.NewAuthCommand(p.Binding{}, "", "", "", p.AttemptMeta{})
	require.Error(t, e)
	_, e = p.NewEpochCommand(p.Binding{}, "", "", p.AttemptMeta{})
	require.Error(t, e)
	_, e = p.NewHealthCommand("", "", "", true, "")
	require.Error(t, e)
}

func TestCommandFactoriesAndACKSignersMatchIndependentGoldenMACs(t *testing.T) {
	c, index, secret := vectors(t)
	for _, v := range c.Records {
		if v.Transport != "nats" || v.Direction != "request" || !v.Expected.Accept {
			continue
		}
		t.Run(v.ID, func(t *testing.T) {
			original, e := p.DecodeCommand(unhex(t, v.RawHex))
			require.NoError(t, e)
			var built p.Command
			if a, ok := original.Auth(); ok {
				built, e = p.NewAuthCommand(p.Binding{UserID: a.UserID, FamilyID: a.FamilyID, Target: a.Target}, a.OperationID, a.Scope, a.Reason, p.AttemptMeta{Timestamp: a.Timestamp, Nonce: a.Nonce})
			} else if a, ok := original.Epoch(); ok {
				built, e = p.NewEpochCommand(p.Binding{UserID: a.UserID, FamilyID: a.FamilyID, Target: a.Target}, a.ParentGeneration, a.SupersededCredentialEpoch, p.AttemptMeta{Timestamp: a.Timestamp, Nonce: a.Nonce})
			} else {
				a, ok := original.Health()
				require.True(t, ok)
				built, e = p.NewHealthCommand(a.NodeBootID, a.Challenge, a.Activation, a.AdmissionAllowed, a.Timestamp)
			}
			require.NoError(t, e)
			signed, e := p.SignCommand(built, secret)
			require.NoError(t, e)
			require.JSONEq(t, string(unhex(t, v.RawHex)), string(signed))
			// Accessor payloads are copies: changing one cannot alter the retained MAC input.
			if a, ok := original.Auth(); ok {
				a.Target.SocketID = "changed"
				require.NoError(t, p.VerifyCommand(original, secret, clock(t, v)))
			}
			ackVector, ok := index[v.ID+"-ack"]
			if !ok {
				return
			}
			var ack []byte
			if _, ok := original.Auth(); ok {
				ack, e = p.SignAuthACK(original, secret)
				_, wrong := p.SignEpochACK(original, secret)
				require.Error(t, wrong)
			} else if _, ok := original.Epoch(); ok {
				ack, e = p.SignEpochACK(original, secret)
				_, wrong := p.SignAuthACK(original, secret)
				require.Error(t, wrong)
			} else {
				var payload struct{ AdmissionGranted bool }
				require.NoError(t, json.Unmarshal(unhex(t, ackVector.RawHex), &payload))
				ack, e = p.SignHealthACK(original, payload.AdmissionGranted, secret)
			}
			require.NoError(t, e)
			require.JSONEq(t, string(unhex(t, ackVector.RawHex)), string(ack))
			verified, e := p.VerifyACK(ack, original, secret, clock(t, ackVector))
			require.NoError(t, e)
			require.Equal(t, original.Kind(), verified.Kind())
		})
	}
}
func TestHTTPResponseSignersUseGoldenRequestContext(t *testing.T) {
	c, index, secret := vectors(t)
	for _, v := range c.Records {
		if v.Transport != "http" || v.Direction != "response" || !v.Expected.Accept {
			continue
		}
		t.Run(v.ID, func(t *testing.T) {
			r, ctx := contextFor(t, index[v.RequestID])
			var payload p.ResponsePayload
			switch endpoint(t, v) {
			case p.EndpointConnectionRegister:
				var x p.RegistrationResult
				require.NoError(t, json.Unmarshal(unhex(t, v.RawHex), &x))
				payload = x
			case p.EndpointProvisionalClose:
				var x p.ProvisionalCloseResult
				require.NoError(t, json.Unmarshal(unhex(t, v.RawHex), &x))
				payload = x
			case p.EndpointRoomRegister:
				var x p.RoomRegistrationResult
				require.NoError(t, json.Unmarshal(unhex(t, v.RawHex), &x))
				payload = x
			case p.EndpointHealthBootstrap:
				var x p.HealthBootstrapResult
				require.NoError(t, json.Unmarshal(unhex(t, v.RawHex), &x))
				payload = x
			}
			wire, e := p.SignHTTPResponse(r, payload, v.Signing.Timestamp, secret)
			require.NoError(t, e)
			require.Equal(t, v.Status, wire.Status)
			if len(v.RawHex) == 0 {
				require.Empty(t, wire.Body)
			} else {
				require.JSONEq(t, string(unhex(t, v.RawHex)), string(wire.Body))
			}
			// Bootstrap's response-body commitment must hash the actual serialized bytes.
			fields := append([]string{}, v.Signing.OrderedFields...)
			if endpoint(t, v) == p.EndpointHealthBootstrap {
				digest := sha256.Sum256(wire.Body)
				fields[len(fields)-1] = hex.EncodeToString(digest[:])
			}
			key := hmac.New(sha256.New, secret)
			_, e = key.Write([]byte(v.Signing.Domain))
			require.NoError(t, e)
			mac := hmac.New(sha256.New, key.Sum(nil))
			_, e = mac.Write([]byte(strings.Join(append([]string{"v1", v.Signing.Timestamp}, fields...), "\n")))
			require.NoError(t, e)
			proof := ""
			for _, h := range wire.Headers {
				if strings.HasSuffix(h.Name, "-Proof") {
					proof = h.Value
				}
			}
			require.Equal(t, hex.EncodeToString(mac.Sum(nil)), proof)
			verified, e := p.VerifyHTTPResponse(endpoint(t, v), wire, ctx, secret, clock(t, v))
			require.NoError(t, e)
			require.Equal(t, v.Status == 204, verified.Empty())
			require.Equal(t, endpoint(t, v), verified.Endpoint())
			_, e = p.SignHTTPResponse(r, p.RegistrationResult{}, v.Signing.Timestamp, secret)
			require.Error(t, e)
		})
	}
}

func TestRejectionPriorityAndHealthGrantConstraints(t *testing.T) {
	_, index, secret := vectors(t)
	for id, reason := range map[string]string{"raw-duplicate-version": "malformed_body", "raw-escaped-duplicate-version": "malformed_body", "raw-nested-escaped-duplicate-boot": "malformed_body", "raw-invalid-utf8": "malformed_body", "raw-unknown-schema": "invalid_variant", "raw-health-spoof-unknown-schema": "invalid_variant", "raw-missing-version": "invalid_variant", "raw-noncanonical-uuid": "invalid_value", "exact-age-7": "proof_stale", "proof-wrong-domain": "invalid_proof", "headers-duplicate-timestamp": "invalid_headers", "headers-comma-nonce": "invalid_headers", "headers-missing-proof": "invalid_headers", "path-query": "noncanonical_endpoint", "body-size-4097": "payload_too_large"} {
		t.Run(id, func(t *testing.T) {
			v := index[id]
			var e error
			if v.Transport == "nats" {
				var c p.Command
				c, e = p.DecodeCommand(unhex(t, v.RawHex))
				if e == nil {
					e = p.VerifyCommand(c, secret, clock(t, v))
				}
			} else {
				var r p.Request
				r, e = p.DecodeHTTPRequest(endpoint(t, v), request(t, v))
				if e == nil {
					e = p.VerifyHTTPRequest(r, secret, clock(t, v))
				}
			}
			var protocolError *p.ProtocolError
			require.ErrorAs(t, e, &protocolError)
			require.Equal(t, reason, protocolError.ProtocolReason())
			require.NotContains(t, protocolError.Error(), string(secret))
		})
	}
	v := index["proof-wrong-domain"]
	c, e := p.DecodeCommand(unhex(t, v.RawHex))
	require.NoError(t, e)
	var pe *p.ProtocolError
	e = p.VerifyCommand(c, secret, time.Unix(1793000100, 0))
	require.ErrorAs(t, e, &pe)
	require.Equal(t, "invalid_proof", pe.ProtocolReason())
	prepared, e := p.DecodeCommand(unhex(t, index["health-prepared-false-false"].RawHex))
	require.NoError(t, e)
	_, e = p.SignHealthACK(prepared, true, secret)
	require.Error(t, e)
	_, e = p.NewHealthCommand(goldenBinding().Target.NodeBootID, strings.Repeat("ab", 32), "prepared", true, "1793000000")
	require.Error(t, e)
	// Exact signed ACKs may not acquire a new nonce or timestamp.
	original, e := p.DecodeCommand(unhex(t, index["eject-session-logout"].RawHex))
	require.NoError(t, e)
	_, e = p.VerifyACK(unhex(t, index["valid-proof-ack-request-mismatch"].RawHex), original, secret, time.Unix(1793000000, 0))
	require.Error(t, e)
}

func assertPayload[T any](t *testing.T, payload T, expected json.RawMessage) {
	t.Helper()
	b, e := json.Marshal(payload)
	require.NoError(t, e)
	require.JSONEq(t, string(expected), string(b))
}
func assertCommandPayload(t *testing.T, c p.Command, expected json.RawMessage) {
	t.Helper()
	if x, ok := c.Auth(); ok {
		assertPayload(t, x, expected)
		_, wrong := c.Health()
		require.False(t, wrong)
		return
	}
	if x, ok := c.Epoch(); ok {
		assertPayload(t, x, expected)
		_, wrong := c.Auth()
		require.False(t, wrong)
		return
	}
	x, ok := c.Health()
	require.True(t, ok)
	assertPayload(t, x, expected)
	_, wrong := c.Epoch()
	require.False(t, wrong)
}
func assertRequestPayload(t *testing.T, r p.Request, expected json.RawMessage) {
	t.Helper()
	switch r.Endpoint() {
	case p.EndpointConnectionRegister:
		x, ok := r.ConnectionRegister()
		require.True(t, ok)
		assertPayload(t, x, expected)
	case p.EndpointConnectionRelease:
		x, ok := r.ConnectionRelease()
		require.True(t, ok)
		assertPayload(t, x, expected)
	case p.EndpointProvisionalClose:
		x, ok := r.ProvisionalClose()
		require.True(t, ok)
		assertPayload(t, x, expected)
	case p.EndpointRoomRegister:
		x, ok := r.RoomRegister()
		require.True(t, ok)
		assertPayload(t, x, expected)
	case p.EndpointRoomRelease:
		x, ok := r.RoomRelease()
		require.True(t, ok)
		assertPayload(t, x, expected)
	case p.EndpointHealthBootstrap:
		x, ok := r.HealthBootstrap()
		require.True(t, ok)
		assertPayload(t, x, expected)
	case p.EndpointBootRetire:
		x, ok := r.BootRetire()
		require.True(t, ok)
		assertPayload(t, x, expected)
	default:
		t.Fatal("missing typed request accessor")
	}
}

func TestAccessorCopiesAndResponseContextBindings(t *testing.T) {
	_, index, secret := vectors(t)
	for _, id := range []string{"eject-session-logout-ack", "credential-eject-ack", "health-active-true-true-ack"} {
		t.Run(id, func(t *testing.T) {
			v := index[id]
			cmd, e := p.DecodeCommand(unhex(t, index[v.RequestID].RawHex))
			require.NoError(t, e)
			ack, e := p.VerifyACK(unhex(t, v.RawHex), cmd, secret, clock(t, v))
			require.NoError(t, e)
			if x, ok := ack.Auth(); ok {
				assertPayload(t, x, v.Expected.Decoded)
				x.Target.SocketID = "changed"
				next, valid := ack.Auth()
				require.True(t, valid)
				require.Equal(t, goldenBinding().Target.SocketID, next.Target.SocketID)
				_, wrong := ack.Epoch()
				require.False(t, wrong)
			} else if x, ok := ack.Epoch(); ok {
				assertPayload(t, x, v.Expected.Decoded)
				_, wrong := ack.Health()
				require.False(t, wrong)
			} else {
				x, ok := ack.Health()
				require.True(t, ok)
				assertPayload(t, x, v.Expected.Decoded)
				_, wrong := ack.Auth()
				require.False(t, wrong)
			}
		})
	}
	for _, id := range []string{"connection-register-response", "room-attach-channel-response", "provisional-close-registered-response", "bootstrap-active-true-response"} {
		t.Run(id, func(t *testing.T) {
			v := index[id]
			_, ctx := contextFor(t, index[v.RequestID])
			response, e := p.VerifyHTTPResponse(endpoint(t, v), p.HTTPResponse{Status: v.Status, Headers: headers(v), Body: unhex(t, v.RawHex)}, ctx, secret, clock(t, v))
			require.NoError(t, e)
			require.False(t, response.Empty())
			if x, ok := response.Registration(); ok {
				assertPayload(t, x, v.Expected.Decoded)
				_, wrong := response.ProvisionalClose()
				require.False(t, wrong)
			} else if x, ok := response.RoomRegistration(); ok {
				assertPayload(t, x, v.Expected.Decoded)
				_, wrong := response.HealthBootstrap()
				require.False(t, wrong)
			} else if x, ok := response.ProvisionalClose(); ok {
				assertPayload(t, x, v.Expected.Decoded)
				require.NotNil(t, x.Binding)
				x.Binding.Target.SocketID = "changed"
				next, valid := response.ProvisionalClose()
				require.True(t, valid)
				require.Equal(t, goldenBinding().Target.SocketID, next.Binding.Target.SocketID)
			} else {
				x, ok := response.HealthBootstrap()
				require.True(t, ok)
				assertPayload(t, x, v.Expected.Decoded)
				_, wrong := response.Registration()
				require.False(t, wrong)
			}
		})
	}
	rv := index["room-attach-channel"]
	r, e := p.DecodeHTTPRequest(endpoint(t, rv), request(t, rv))
	require.NoError(t, e)
	b := goldenBinding()
	ctx, e := p.NewRequestContext(r, p.ExpectedResponse{Binding: &b})
	require.NoError(t, e)
	b.Target.SocketID = "changed"
	b.FamilyID = "00000014-1111-4111-8111-111111111111"
	v := index["room-attach-channel-response"]
	_, e = p.VerifyHTTPResponse(endpoint(t, v), p.HTTPResponse{Status: v.Status, Headers: headers(v), Body: unhex(t, v.RawHex)}, ctx, secret, clock(t, v))
	require.NoError(t, e)
	rv = index["connection-register"]
	r, e = p.DecodeHTTPRequest(endpoint(t, rv), request(t, rv))
	require.NoError(t, e)
	ctx, e = p.NewRequestContext(r, p.ExpectedResponse{UserID: "00000014-1111-4111-8111-111111111111"})
	require.NoError(t, e)
	v = index["connection-register-response"]
	_, e = p.VerifyHTTPResponse(endpoint(t, v), p.HTTPResponse{Status: v.Status, Headers: headers(v), Body: unhex(t, v.RawHex)}, ctx, secret, clock(t, v))
	var pe *p.ProtocolError
	require.ErrorAs(t, e, &pe)
	require.Equal(t, "binding_mismatch", pe.ProtocolReason())
	var typedNil *p.RegistrationResult
	_, e = p.SignHTTPResponse(r, typedNil, "1793000000", secret)
	require.Error(t, e)
}
func TestExactFreshnessUsesIntegerSecondsNearInt64Limit(t *testing.T) {
	_, _, secret := vectors(t)
	const stamp = "9223372036854775800"
	const ts int64 = 9223372036854775800
	cmd, e := p.NewAuthCommand(goldenBinding(), "00000007-1111-4111-8111-111111111111", "session", "logout", p.AttemptMeta{Timestamp: stamp, Nonce: strings.Repeat("ab", 32)})
	require.NoError(t, e)
	raw, e := p.SignCommand(cmd, secret)
	require.NoError(t, e)
	cmd, e = p.DecodeCommand(raw)
	require.NoError(t, e)
	for _, age := range []int64{-2, -1, 0, 6, 7} {
		t.Run(strconv.FormatInt(age, 10), func(t *testing.T) {
			e := p.VerifyCommand(cmd, secret, time.Unix(ts+age, 0))
			if age >= -1 && age <= 6 {
				require.NoError(t, e)
			} else {
				require.Error(t, e)
			}
		})
	}
}

func TestHTTPResponseBoundsStatusAndEmptyBodyPriority(t *testing.T) {
	_, index, secret := vectors(t)
	v := index["connection-release-response"]
	_, ctx := contextFor(t, index[v.RequestID])
	now := clock(t, v)
	for _, body := range []string{" ", "null", "{}"} {
		t.Run("204_body_"+body, func(t *testing.T) {
			_, e := p.VerifyHTTPResponse(endpoint(t, v), p.HTTPResponse{Status: 204, Headers: headers(v), Body: []byte(body)}, ctx, secret, now)
			require.Error(t, e)
		})
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		reason string
	}{{"size before status", 201, strings.Repeat("x", 4097), "payload_too_large"}, {"status before malformed body", 201, "{", "unexpected_status"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := p.VerifyHTTPResponse(endpoint(t, v), p.HTTPResponse{Status: tc.status, Headers: headers(v), Body: []byte(tc.body)}, ctx, secret, now)
			var pe *p.ProtocolError
			require.ErrorAs(t, e, &pe)
			require.Equal(t, tc.reason, pe.ProtocolReason())
		})
	}
	r, e := p.DecodeHTTPRequest(endpoint(t, index["connection-register"]), request(t, index["connection-register"]))
	require.NoError(t, e)
	b := goldenBinding()
	b.Target.SocketID = "wrong-local-socket"
	_, e = p.NewRequestContext(r, p.ExpectedResponse{UserID: goldenBinding().UserID, Binding: &b})
	require.Error(t, e)
}

func TestSecurityScalarsRequireEndOfInputWithoutTrailingNewline(t *testing.T) {
	_, index, _ := vectors(t)
	v := index["eject-session-logout"]
	raw := string(unhex(t, v.RawHex))
	for _, value := range []string{goldenBinding().UserID, goldenBinding().Target.CredentialEpoch, "1793000000"} {
		t.Run(value, func(t *testing.T) {
			changed := strings.Replace(raw, strconv.Quote(value), strconv.Quote(value+"\n"), 1)
			require.NotEqual(t, raw, changed)
			_, e := p.DecodeCommand([]byte(changed))
			var pe *p.ProtocolError
			require.ErrorAs(t, e, &pe)
			require.Equal(t, "invalid_value", pe.ProtocolReason())
		})
	}
	wire := request(t, index["connection-register"])
	for i := range wire.Headers {
		if strings.HasSuffix(wire.Headers[i].Name, "-Nonce") {
			wire.Headers[i].Value += "\n"
		}
	}
	_, e := p.DecodeHTTPRequest(p.EndpointConnectionRegister, wire)
	var pe *p.ProtocolError
	require.ErrorAs(t, e, &pe)
	require.Equal(t, "invalid_headers", pe.ProtocolReason())
}
