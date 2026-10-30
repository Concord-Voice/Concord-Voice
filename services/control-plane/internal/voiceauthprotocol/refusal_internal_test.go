package voiceauthprotocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Internal refusal coverage: branches the golden corpus cannot reach through
// the public API alone (unknown endpoints, codec internals, proof faults).

type refusalVector struct {
	ID, Method, Path, RawHex, VerifierUnixSeconds string
	Headers                                       [][2]string
	Status                                        int
}

const otherUUID = "00000009-1111-4111-8111-111111111111"
const otherHex64 = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"

func refusalCorpus(t *testing.T) (map[string]refusalVector, []byte) {
	t.Helper()
	raw, err := os.ReadFile("../api/testdata/auth_connection_protocol.json")
	require.NoError(t, err)
	var c struct {
		TestOnlySecretHex string
		Records           []refusalVector
	}
	require.NoError(t, json.Unmarshal(raw, &c))
	index := make(map[string]refusalVector, len(c.Records))
	for _, v := range c.Records {
		index[v.ID] = v
	}
	secret, err := hex.DecodeString(c.TestOnlySecretHex)
	require.NoError(t, err)
	return index, secret
}

func refusalBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func refusalClock(t *testing.T, v refusalVector) time.Time {
	t.Helper()
	n, err := strconv.ParseInt(v.VerifierUnixSeconds, 10, 64)
	require.NoError(t, err)
	return time.Unix(n, 0)
}

func refusalHeaders(v refusalVector) []Header {
	h := make([]Header, len(v.Headers))
	for i, x := range v.Headers {
		h[i] = Header{Name: x[0], Value: x[1]}
	}
	return h
}

func refusalWire(t *testing.T, v refusalVector) HTTPRequest {
	t.Helper()
	return HTTPRequest{Method: v.Method, Path: v.Path, Headers: refusalHeaders(v), Body: refusalBytes(t, v.RawHex)}
}

func refusalResponse(t *testing.T, v refusalVector) HTTPResponse {
	t.Helper()
	return HTTPResponse{Status: v.Status, Headers: refusalHeaders(v), Body: refusalBytes(t, v.RawHex)}
}

func refusalRequest(t *testing.T, index map[string]refusalVector, id string, e Endpoint) Request {
	t.Helper()
	r, err := DecodeHTTPRequest(e, refusalWire(t, index[id]))
	require.NoError(t, err)
	return r
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe), "want ProtocolError %q, got %v", reason, err)
	require.Equal(t, reason, pe.ProtocolReason())
}

func withHeader(h []Header, suffix, value string) []Header {
	out := append([]Header(nil), h...)
	for i := range out {
		if strings.HasSuffix(out[i].Name, suffix) {
			out[i].Value = value
		}
	}
	return out
}

func withoutHeader(h []Header, name string) []Header {
	out := make([]Header, 0, len(h))
	for _, x := range h {
		if x.Name != name {
			out = append(out, x)
		}
	}
	return out
}

func refusalBinding() Binding {
	return Binding{UserID: "00000003-1111-4111-8111-111111111111", FamilyID: "00000004-1111-4111-8111-111111111111", Target: Target{ConnectionGeneration: "00000001-1111-4111-8111-111111111111", NodeBootID: "00000002-1111-4111-8111-111111111111", SocketID: "test-socket-é", CredentialEpoch: "0123456789abcdef0123456789abcdef"}}
}

func TestUnknownEndpointFailsClosedEverywhere(t *testing.T) {
	_, secret := refusalCorpus(t)
	bogus := Endpoint("bogus")
	_, err := config(bogus)
	requireReason(t, err, "invalid_decoder_state")
	_, err = decodeRequestBody(bogus, []byte(`{}`))
	requireReason(t, err, "invalid_decoder_state")
	_, err = decodeResponseBody(bogus, nil)
	requireReason(t, err, "invalid_decoder_state")
	_, err = DecodeHTTPRequest(bogus, HTTPRequest{Method: "POST", Body: []byte(`{}`)})
	requireReason(t, err, "invalid_decoder_state")
	_, _, err = SignHTTPRequest(bogus, []byte(`{}`), "", AttemptMeta{}, secret)
	requireReason(t, err, "invalid_decoder_state")
	requireReason(t, validRequest(Request{endpoint: bogus, value: BootRetire{}}), "invalid_decoder_state")

	// Non-ASCII header names never case-fold onto a security header.
	require.False(t, headerEqual("Authorizatİon", "Authorization"))
	require.True(t, headerEqual("authorization", "Authorization"))

	// Payloads without a target, room or binding yield zero values.
	retire := Request{endpoint: EndpointBootRetire, value: BootRetire{NodeBootID: otherUUID}}
	require.Equal(t, Target{}, requestTarget(retire))
	require.Equal(t, Room{}, requestRoom(retire))
	require.Equal(t, otherUUID, requestBoot(retire))
	_, ok := requestBinding(retire)
	require.False(t, ok)
	_, ok = responseBinding(ProvisionalCloseResult{})
	require.False(t, ok)
}

func TestDecodeHTTPRequestRefusesHeaderAndBodyViolations(t *testing.T) {
	index, _ := refusalCorpus(t)

	register := refusalWire(t, index["connection-register"])
	for name, auth := range map[string]string{"basic scheme": "Basic abc", "empty bearer": "Bearer ", "crlf": "Bearer a\r\nb"} {
		t.Run(name, func(t *testing.T) {
			w := register
			w.Headers = withHeader(register.Headers, "Authorization", auth)
			_, err := DecodeHTTPRequest(EndpointConnectionRegister, w)
			requireReason(t, err, "invalid_headers")
		})
	}
	t.Run("missing bearer", func(t *testing.T) {
		w := register
		w.Headers = withoutHeader(register.Headers, "Authorization")
		_, err := DecodeHTTPRequest(EndpointConnectionRegister, w)
		requireReason(t, err, "invalid_headers")
	})

	t.Run("bearer on a non-bearer endpoint", func(t *testing.T) {
		w := refusalWire(t, index["connection-release"])
		w.Headers = append(w.Headers, Header{Name: "authorization", Value: "Bearer x"})
		_, err := DecodeHTTPRequest(EndpointConnectionRelease, w)
		requireReason(t, err, "invalid_headers")
	})

	t.Run("bootstrap body nonce differs from header nonce", func(t *testing.T) {
		w := refusalWire(t, index["bootstrap-active-true"])
		w.Headers = withHeader(w.Headers, "-Nonce", otherHex64)
		_, err := DecodeHTTPRequest(EndpointHealthBootstrap, w)
		requireReason(t, err, "invalid_headers")
	})

	t.Run("body of another endpoint", func(t *testing.T) {
		w := refusalWire(t, index["connection-release"])
		_, err := DecodeHTTPRequest(EndpointBootRetire, w)
		requireReason(t, err, "invalid_variant")
	})
}

func TestSignHTTPRequestRefusals(t *testing.T) {
	index, secret := refusalCorpus(t)
	v := index["connection-release"]
	body := refusalBytes(t, v.RawHex)
	h := refusalHeaders(v)
	meta := AttemptMeta{Timestamp: h[0].Value, Nonce: h[1].Value}

	_, _, err := SignHTTPRequest(EndpointConnectionRelease, body, "token", meta, secret)
	requireReason(t, err, "invalid_decoder_state")
	_, _, err = SignHTTPRequest(EndpointConnectionRelease, body, "", AttemptMeta{Timestamp: "0", Nonce: meta.Nonce}, secret)
	requireReason(t, err, "invalid_headers")
	_, _, err = SignHTTPRequest(EndpointConnectionRelease, body, "", meta, nil)
	requireReason(t, err, "invalid_proof")
	_, _, err = SignHTTPRequest(EndpointConnectionRelease, body, "", meta, secret)
	require.NoError(t, err)
}

func TestNewRequestContextRefusesInconsistentExpectations(t *testing.T) {
	index, _ := refusalCorpus(t)
	register := refusalRequest(t, index, "connection-register", EndpointConnectionRegister)
	release := refusalRequest(t, index, "connection-release", EndpointConnectionRelease)
	room := refusalRequest(t, index, "room-attach-channel", EndpointRoomRegister)
	bootstrap := refusalRequest(t, index, "bootstrap-active-true", EndpointHealthBootstrap)
	retire := refusalRequest(t, index, "boot-retirement", EndpointBootRetire)
	provisional := refusalRequest(t, index, "provisional-close", EndpointProvisionalClose)

	golden := refusalBinding()
	foreignFamily := golden
	foreignFamily.FamilyID = otherUUID
	otherTarget := golden
	otherTarget.Target.SocketID = "another-socket"

	cases := []struct {
		name     string
		r        Request
		expected ExpectedResponse
	}{
		{"malformed user", register, ExpectedResponse{UserID: "not-a-uuid"}},
		{"malformed binding", release, ExpectedResponse{Binding: &Binding{}}},
		{"binding for another target", release, ExpectedResponse{Binding: &otherTarget}},
		{"user disagrees with binding", release, ExpectedResponse{UserID: otherUUID, Binding: &golden}},
		{"register without user", register, ExpectedResponse{}},
		{"register with binding", register, ExpectedResponse{UserID: golden.UserID, Binding: &golden}},
		{"room register without binding", room, ExpectedResponse{}},
		{"release binding disagrees with request", release, ExpectedResponse{Binding: &foreignFamily}},
		{"bootstrap with user", bootstrap, ExpectedResponse{UserID: golden.UserID}},
		{"retire with user", retire, ExpectedResponse{UserID: golden.UserID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRequestContext(tc.r, tc.expected)
			requireReason(t, err, "invalid_decoder_state")
		})
	}

	ctx, err := NewRequestContext(provisional, ExpectedResponse{})
	require.NoError(t, err)
	require.True(t, ctx.valid)
	ctx, err = NewRequestContext(release, ExpectedResponse{Binding: &golden})
	require.NoError(t, err)
	require.NotSame(t, &golden, ctx.expected.Binding)
}

func TestVerifyHTTPResponseRefusals(t *testing.T) {
	index, secret := refusalCorpus(t)
	r := refusalRequest(t, index, "connection-register", EndpointConnectionRegister)
	golden := refusalBinding()
	ctx, err := NewRequestContext(r, ExpectedResponse{UserID: golden.UserID})
	require.NoError(t, err)
	v := index["connection-register-response"]
	now := refusalClock(t, v)
	good := refusalResponse(t, v)
	_, err = VerifyHTTPResponse(EndpointConnectionRegister, good, ctx, secret, now)
	require.NoError(t, err)

	mutate := func(f func(*HTTPResponse)) HTTPResponse {
		w := good
		w.Headers = append([]Header(nil), good.Headers...)
		w.Body = append([]byte(nil), good.Body...)
		f(&w)
		return w
	}
	cases := []struct {
		name, reason string
		wire         HTTPResponse
		ep           Endpoint
	}{
		{"oversized body", "payload_too_large", mutate(func(w *HTTPResponse) { w.Body = bytes.Repeat([]byte(" "), maxPayload+1) }), EndpointConnectionRegister},
		{"endpoint differs from context", "invalid_decoder_state", good, EndpointConnectionRelease},
		{"unexpected status", "unexpected_status", mutate(func(w *HTTPResponse) { w.Status = 201 }), EndpointConnectionRegister},
		{"undecodable body", "invalid_variant", mutate(func(w *HTTPResponse) { w.Body = []byte(`{}`) }), EndpointConnectionRegister},
		{"missing proof header", "invalid_headers", mutate(func(w *HTTPResponse) { w.Headers = w.Headers[:2] }), EndpointConnectionRegister},
		{"nonce differs from request", "invalid_headers", mutate(func(w *HTTPResponse) { w.Headers = withHeader(w.Headers, "-Nonce", otherHex64) }), EndpointConnectionRegister},
		{"forged proof", "invalid_proof", mutate(func(w *HTTPResponse) { w.Headers = withHeader(w.Headers, "-Proof", otherHex64) }), EndpointConnectionRegister},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifyHTTPResponse(tc.ep, tc.wire, ctx, secret, now)
			requireReason(t, err, tc.reason)
		})
	}

	t.Run("authentic response for another user", func(t *testing.T) {
		other, err := NewRequestContext(r, ExpectedResponse{UserID: otherUUID})
		require.NoError(t, err)
		_, err = VerifyHTTPResponse(EndpointConnectionRegister, good, other, secret, now)
		requireReason(t, err, "binding_mismatch")
	})
}

func TestSignHTTPResponseRefusesMismatchedPayloads(t *testing.T) {
	index, secret := refusalCorpus(t)
	const stamp = "1793000000"

	register := refusalRequest(t, index, "connection-register", EndpointConnectionRegister)
	var reg RegistrationResult
	require.NoError(t, json.Unmarshal(refusalBytes(t, index["connection-register-response"].RawHex), &reg))
	_, err := SignHTTPResponse(register, reg, "0", secret)
	requireReason(t, err, "invalid_value")
	_, err = SignHTTPResponse(register, reg, stamp, nil)
	requireReason(t, err, "invalid_proof")
	wrongTarget := reg
	wrongTarget.Target.SocketID = "another-socket"
	_, err = SignHTTPResponse(register, wrongTarget, stamp, secret)
	requireReason(t, err, "binding_mismatch")
	_, err = SignHTTPResponse(register, nil, stamp, secret)
	requireReason(t, err, "invalid_decoder_state")

	provisional := refusalRequest(t, index, "provisional-close", EndpointProvisionalClose)
	var closed ProvisionalCloseResult
	require.NoError(t, json.Unmarshal(refusalBytes(t, index["provisional-close-unregistered-response"].RawHex), &closed))
	closed.RegistrationBodyDigest = otherHex64
	_, err = SignHTTPResponse(provisional, closed, stamp, secret)
	requireReason(t, err, "binding_mismatch")

	room := refusalRequest(t, index, "room-attach-channel", EndpointRoomRegister)
	var attached RoomRegistrationResult
	require.NoError(t, json.Unmarshal(refusalBytes(t, index["room-attach-channel-response"].RawHex), &attached))
	attached.Room.RoomID = otherUUID
	_, err = SignHTTPResponse(room, attached, stamp, secret)
	requireReason(t, err, "binding_mismatch")

	bootstrap := refusalRequest(t, index, "bootstrap-active-true", EndpointHealthBootstrap)
	var health HealthBootstrapResult
	require.NoError(t, json.Unmarshal(refusalBytes(t, index["bootstrap-active-true-response"].RawHex), &health))
	health.NodeBootID = otherUUID
	_, err = SignHTTPResponse(bootstrap, health, stamp, secret)
	requireReason(t, err, "binding_mismatch")
}

func TestProofPrimitivesFailClosed(t *testing.T) {
	_, err := sign("d", "1", []string{"a"}, nil)
	requireReason(t, err, "invalid_proof")
	requireReason(t, verify("d", "1", zeroProof, []string{"a"}, nil, time.Unix(1, 0), false), "invalid_proof")
	// A signable but non-decimal timestamp never reaches the freshness window.
	p, err := sign("d", "not-a-number", []string{"a"}, []byte("secret"))
	require.NoError(t, err)
	requireReason(t, verify("d", "not-a-number", p, []string{"a"}, []byte("secret"), time.Unix(1, 0), false), "invalid_decoder_state")
	_, _, _, _, _, err = commandProof(Command{value: AuthAck{}})
	requireReason(t, err, "invalid_decoder_state")
}

func TestCommandAndACKSignersRefuseMismatchedOrUnkeyedCommands(t *testing.T) {
	index, secret := refusalCorpus(t)
	auth, err := DecodeCommand(refusalBytes(t, index["eject-session-logout"].RawHex))
	require.NoError(t, err)
	epoch, err := DecodeCommand(refusalBytes(t, index["credential-eject"].RawHex))
	require.NoError(t, err)
	health, err := DecodeCommand(refusalBytes(t, index["health-active-true-true"].RawHex))
	require.NoError(t, err)

	mismatched, ok := epoch.Epoch()
	require.True(t, ok)
	mismatched.SupersededCredentialEpoch = ""
	broken := Command{value: mismatched}

	_, err = SignCommand(broken, secret)
	requireReason(t, err, "binding_mismatch")
	_, err = SignEpochACK(broken, secret)
	requireReason(t, err, "binding_mismatch")
	for name, c := range map[string]Command{"auth": auth, "epoch": epoch, "health": health} {
		t.Run(name, func(t *testing.T) {
			_, err := SignCommand(c, nil)
			requireReason(t, err, "invalid_proof")
		})
	}
	_, err = SignAuthACK(auth, nil)
	requireReason(t, err, "invalid_proof")
	_, err = SignEpochACK(epoch, nil)
	requireReason(t, err, "invalid_proof")
	_, err = SignHealthACK(health, true, nil)
	requireReason(t, err, "invalid_proof")
}

func TestVerifyACKRefusals(t *testing.T) {
	index, secret := refusalCorpus(t)
	authAckVector := index["eject-session-logout-ack"]
	now := refusalClock(t, authAckVector)
	authAck := refusalBytes(t, authAckVector.RawHex)
	auth, err := DecodeCommand(refusalBytes(t, index["eject-session-logout"].RawHex))
	require.NoError(t, err)
	epoch, err := DecodeCommand(refusalBytes(t, index["credential-eject"].RawHex))
	require.NoError(t, err)
	health, err := DecodeCommand(refusalBytes(t, index["health-active-true-true"].RawHex))
	require.NoError(t, err)

	_, err = VerifyACK(authAck, auth, secret, now)
	require.NoError(t, err)

	_, err = VerifyACK([]byte(`{`), auth, secret, now)
	requireReason(t, err, "malformed_body")
	_, err = VerifyACK(authAck, Command{}, secret, now)
	requireReason(t, err, "invalid_decoder_state")
	for name, original := range map[string]Command{"auth": auth, "epoch": epoch, "health": health} {
		t.Run("undecodable "+name+" ack", func(t *testing.T) {
			_, err := VerifyACK([]byte(`{}`), original, secret, now)
			requireReason(t, err, "invalid_variant")
		})
	}
	_, err = VerifyACK(authAck, Command{value: AuthCommand{Schema: "concord.unknown"}}, secret, now)
	requireReason(t, err, "invalid_decoder_state")
	_, err = VerifyACK(authAck, auth, []byte("wrong secret"), now)
	requireReason(t, err, "invalid_proof")
	// An original that is itself an ACK has no command proof to compare.
	_, err = VerifyACK(authAck, Command{value: AuthAck{Schema: authSchema}}, secret, now)
	requireReason(t, err, "invalid_decoder_state")

	t.Run("authentic epoch ack with a mismatched superseded epoch", func(t *testing.T) {
		v := index["credential-eject-ack"]
		var ack EpochAck
		require.NoError(t, json.Unmarshal(refusalBytes(t, v.RawHex), &ack))
		ack.SupersededCredentialEpoch = ""
		c := Command{value: EpochCommand{UserID: ack.UserID, FamilyID: ack.FamilyID, Target: ack.Target, Schema: ack.Schema, Version: ack.Version, ParentGeneration: ack.ParentGeneration, SupersededCredentialEpoch: ack.SupersededCredentialEpoch, Timestamp: ack.Timestamp, Nonce: ack.Nonce}}
		d, ts, _, f, _, err := commandProof(c)
		require.NoError(t, err)
		ack.Proof, err = sign(ackDomain(d), ts, append(f, "true"), secret)
		require.NoError(t, err)
		raw, err := json.Marshal(ack)
		require.NoError(t, err)
		_, err = VerifyACK(raw, epoch, secret, refusalClock(t, v))
		requireReason(t, err, "binding_mismatch")
	})
}

func TestStrictJSONRefusals(t *testing.T) {
	requireReason(t, strictRaw(bytes.Repeat([]byte(" "), maxPayload+1)), "payload_too_large")

	for name, raw := range map[string]string{
		"dangling backslash":                 `\`,
		"short unicode escape":               `\u12`,
		"non-hex unicode escape":             `\uzzzz"`,
		"high surrogate then non-low escape": `\ud800A`,
		"high surrogate then non-hex escape": `\ud800\uzzzz`,
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, validSurrogates([]byte(raw)))
		})
	}
	require.True(t, validSurrogates([]byte(`😀 \n`)))

	for name, raw := range map[string]string{
		"empty input":            ``,
		"non-string key":         `{1:2}`,
		"duplicate key in array": `[{"a":1,"a":2}]`,
		"unterminated array":     `[1`,
		"unterminated object":    `{"a":1`,
		"truncated nested value": `{"a":`,
	} {
		t.Run(name, func(t *testing.T) {
			d := json.NewDecoder(strings.NewReader(raw))
			d.UseNumber()
			requireReason(t, uniqueValue(d), "malformed_body")
		})
	}
	d := json.NewDecoder(strings.NewReader(`[1,[2,{"a":[]}],"x"]`))
	require.NoError(t, uniqueValue(d))

	requireReason(t, shape([]byte(`null`), reflect.TypeFor[Binding](), ""), "invalid_variant")
	closeResult := reflect.TypeFor[ProvisionalCloseResult]()
	requireReason(t, shape([]byte(`{"disposition":1}`), closeResult, provisionalResultSchema), "invalid_variant")
	requireReason(t, shape([]byte(`{"disposition":"pending"}`), closeResult, provisionalResultSchema), "invalid_variant")
	requireReason(t, decodeValue([]byte(`{`), &Binding{}, ""), "malformed_body")
}

func TestDecodeValueRefusesSchemaAndFalseAcknowledgements(t *testing.T) {
	index, _ := refusalCorpus(t)
	raw := refusalBytes(t, index["eject-session-logout"].RawHex)
	requireReason(t, shape(raw, reflect.TypeFor[AuthCommand](), "concord.other"), "invalid_variant")

	var ack map[string]any
	require.NoError(t, json.Unmarshal(refusalBytes(t, index["eject-session-logout-ack"].RawHex), &ack))
	ack["ok"] = false
	falseAck, err := json.Marshal(ack)
	require.NoError(t, err)
	requireReason(t, decodeValue(falseAck, &AuthAck{}, authSchema), "invalid_value")

	requireReason(t, healthBounds(reflect.ValueOf(HealthAck4{Activation: "active", AdmissionAllowed: false, AdmissionGranted: true})), "invalid_value")
	requireReason(t, healthBounds(reflect.ValueOf(HealthAck4{Activation: "prepared", AdmissionGranted: true})), "invalid_value")
	require.NoError(t, healthBounds(reflect.ValueOf(HealthAck4{Activation: "active", AdmissionAllowed: true, AdmissionGranted: true})))
	require.NoError(t, healthBounds(reflect.ValueOf(Binding{})))
}
