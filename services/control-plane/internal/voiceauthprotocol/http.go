package voiceauthprotocol

import (
	"encoding/json"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
	"strings"
	"time"
	"unicode/utf8"
)

type endpointConfig struct {
	path, domain, schema, header string
	status                       int
	bearer                       bool
}

func config(e Endpoint) (endpointConfig, error) {
	c := endpointConfig{header: "X-Concord-Auth-Connection", status: 200, domain: "concord/auth-connection/"}
	switch e {
	case EndpointConnectionRegister:
		c.path = "/api/v1/internal/voice/auth-connections"
		c.domain += "register"
		c.bearer = true
	case EndpointConnectionRelease:
		c.path = "/api/v1/internal/voice/auth-connections/release"
		c.domain += "release"
		c.status = 204
	case EndpointProvisionalClose:
		c.path = "/api/v1/internal/voice/auth-connections/provisional-close"
		c.domain += "provisional-close"
		c.schema = provisionalSchema
	case EndpointRoomRegister:
		c.path = "/api/v1/voice/enforcement-sessions"
		c.domain = "concord/auth-room-binding/register"
		c.schema = roomSchema
		c.header = "X-Concord-Auth-Room"
		c.bearer = true
	case EndpointRoomRelease:
		c.path = "/api/v1/internal/voice/enforcement-sessions/release"
		c.domain = "concord/auth-room-binding/release"
		c.schema = roomSchema
		c.header = "X-Concord-Auth-Room"
		c.status = 204
	case EndpointHealthBootstrap:
		c.path = "/api/v1/internal/voice/enforcement/health"
		c.domain += "health/bootstrap"
	case EndpointBootRetire:
		c.path = "/api/v1/internal/voice/auth-connections/retire-boot"
		c.domain += "retire-boot"
		c.schema = retireSchema
		c.status = 204
	default:
		return c, failure("invalid_decoder_state")
	}
	return c, nil
}

// HTTP field names are ASCII tokens; Unicode case folding must not create an
// alternate spelling of a security header.
func headerEqual(name, expected string) bool {
	for i := range len(name) {
		if name[i] > 127 {
			return false
		}
	}
	return strings.EqualFold(name, expected)
}
func singleHeader(headers []Header, name string) (string, error) {
	s := ""
	n := 0
	for _, h := range headers {
		if headerEqual(h.Name, name) {
			s = h.Value
			n++
		}
	}
	if n != 1 {
		return "", failure("invalid_headers")
	}
	return s, nil
}
func proofHeaders(headers []Header, prefix string) (string, string, string, error) {
	t, err := singleHeader(headers, prefix+"-Timestamp")
	if err != nil || !validTimestamp(t) {
		return "", "", "", failure("invalid_headers")
	}
	n, err := singleHeader(headers, prefix+"-Nonce")
	if err != nil || !hex64Pattern.MatchString(n) {
		return "", "", "", failure("invalid_headers")
	}
	p, err := singleHeader(headers, prefix+"-Proof")
	if err != nil || !hex64Pattern.MatchString(p) {
		return "", "", "", failure("invalid_headers")
	}
	return t, n, p, nil
}
func makeHeaders(prefix, t, n, p string) []Header {
	return []Header{{prefix + "-Timestamp", t}, {prefix + "-Nonce", n}, {prefix + "-Proof", p}}
}
func copyRequest(r HTTPRequest) HTTPRequest {
	r.Body = append([]byte(nil), r.Body...)
	r.Headers = append([]Header(nil), r.Headers...)
	return r
}
func decodeRequestBody(e Endpoint, raw []byte) (any, error) {
	switch e {
	case EndpointConnectionRegister:
		var p ConnectionRegister
		err := decodeValue(raw, &p, "")
		return p, err
	case EndpointConnectionRelease:
		var p ConnectionRelease
		err := decodeValue(raw, &p, "")
		return p, err
	case EndpointProvisionalClose:
		var p ProvisionalClose
		err := decodeValue(raw, &p, provisionalSchema)
		return p, err
	case EndpointRoomRegister:
		var p RoomRegister
		err := decodeValue(raw, &p, roomSchema)
		return p, err
	case EndpointRoomRelease:
		var p RoomRelease
		err := decodeValue(raw, &p, roomSchema)
		return p, err
	case EndpointHealthBootstrap:
		var p HealthBootstrap
		err := decodeValue(raw, &p, "")
		return p, err
	case EndpointBootRetire:
		var p BootRetire
		err := decodeValue(raw, &p, retireSchema)
		return p, err
	}
	return nil, failure("invalid_decoder_state")
}

// DecodeHTTPRequest strictly decodes a fixed-route request and copies its wire commitments.
func DecodeHTTPRequest(e Endpoint, wire HTTPRequest) (Request, error) {
	if len(wire.Body) > maxPayload {
		return Request{}, failure("payload_too_large")
	}
	v, err := decodeRequestBody(e, wire.Body)
	if err != nil {
		return Request{}, err
	}
	c, err := config(e)
	if err != nil {
		return Request{}, err
	}
	if wire.Method != "POST" || wire.Path != c.path {
		return Request{}, failure("noncanonical_endpoint")
	}
	t, n, p, err := proofHeaders(wire.Headers, c.header)
	if err != nil {
		return Request{}, err
	}
	token := ""
	if c.bearer {
		a, err := singleHeader(wire.Headers, "Authorization")
		if err != nil || !strings.HasPrefix(a, "Bearer ") || len(a) <= 7 || !utf8.ValidString(a) || strings.ContainsAny(a, "\r\n") {
			return Request{}, failure("invalid_headers")
		}
		token = a[7:]
	} else {
		for _, h := range wire.Headers {
			if headerEqual(h.Name, "Authorization") {
				return Request{}, failure("invalid_headers")
			}
		}
	}
	if b, ok := v.(HealthBootstrap); ok && b.Nonce != n {
		return Request{}, failure("invalid_headers")
	}
	return Request{endpoint: e, value: v, wire: copyRequest(wire), timestamp: t, nonce: n, proof: p, tokenDigest: mediaproof.TokenDigest(token), bodyDigest: mediaproof.TokenDigest(string(wire.Body))}, nil
}
func requestTarget(r Request) Target {
	switch p := r.value.(type) {
	case ConnectionRegister:
		return Target{p.ConnectionGeneration, p.NodeBootID, p.SocketID, p.CredentialEpoch}
	case ConnectionRelease:
		return p.Target
	case ProvisionalClose:
		return p.Target
	case RoomRegister:
		return Target{p.ConnectionGeneration, p.NodeBootID, p.SocketID, p.CredentialEpoch}
	case RoomRelease:
		return p.Binding.Target
	}
	return Target{}
}
func requestBoot(r Request) string {
	switch p := r.value.(type) {
	case HealthBootstrap:
		return p.NodeBootID
	case BootRetire:
		return p.NodeBootID
	}
	return requestTarget(r).NodeBootID
}
func requestBinding(r Request) (Binding, bool) {
	switch p := r.value.(type) {
	case ConnectionRelease:
		return Binding{p.UserID, p.FamilyID, p.Target}, true
	case RoomRelease:
		return p.Binding, true
	}
	return Binding{}, false
}
func requestRoom(r Request) Room {
	switch p := r.value.(type) {
	case RoomRegister:
		return Room{p.SessionGeneration, p.RoomID, p.RoomKind}
	case RoomRelease:
		return p.Room
	}
	return Room{}
}
func requestProofFields(r Request, c endpointConfig) []string {
	f := []string{}
	if c.schema != "" {
		f = append(f, c.schema)
	}
	f = append(f, "1", "POST", c.path)
	if c.bearer {
		f = append(f, r.tokenDigest)
	}
	return append(f, r.bodyDigest, requestBoot(r), r.nonce)
}
func validRequest(r Request) error {
	if r.value == nil {
		return failure("invalid_decoder_state")
	}
	_, err := config(r.endpoint)
	return err
}

// VerifyHTTPRequest checks a retained request proof against an explicit verification clock.
func VerifyHTTPRequest(r Request, secret []byte, now time.Time) error {
	if err := validRequest(r); err != nil {
		return err
	}
	c, err := config(r.endpoint)
	if err != nil {
		return err
	}
	return verify(c.domain+"/request/v1", r.timestamp, r.proof, requestProofFields(r, c), secret, now, false)
}

// SignHTTPRequest signs exact supplied body bytes and returns independent wire and request copies.
func SignHTTPRequest(e Endpoint, body []byte, token string, a AttemptMeta, secret []byte) (HTTPRequest, Request, error) {
	c, err := config(e)
	if err != nil {
		return HTTPRequest{}, Request{}, err
	}
	if c.bearer != (token != "") {
		return HTTPRequest{}, Request{}, failure("invalid_decoder_state")
	}
	wire := HTTPRequest{Method: "POST", Path: c.path, Body: append([]byte(nil), body...), Headers: makeHeaders(c.header, a.Timestamp, a.Nonce, zeroProof)}
	if c.bearer {
		wire.Headers = append(wire.Headers, Header{"Authorization", "Bearer " + token})
	}
	r, err := DecodeHTTPRequest(e, wire)
	if err != nil {
		return HTTPRequest{}, Request{}, err
	}
	p, err := sign(c.domain+"/request/v1", a.Timestamp, requestProofFields(r, c), secret)
	if err != nil {
		return HTTPRequest{}, Request{}, err
	}
	wire.Headers[2].Value = p
	r.proof = p
	r.wire = copyRequest(wire)
	return wire, r, nil
}
func validateBinding(b Binding) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return failure("invalid_decoder_state")
	}
	var value Binding
	return decodeValue(raw, &value, "")
}

// NewRequestContext copies independently known expectations into an original-request context.
func NewRequestContext(r Request, e ExpectedResponse) (RequestContext, error) {
	if err := validRequest(r); err != nil {
		return RequestContext{}, err
	}
	bad := func() (RequestContext, error) { return RequestContext{}, failure("invalid_decoder_state") }
	if e.UserID != "" && !uuidPattern.MatchString(e.UserID) {
		return bad()
	}
	if e.Binding != nil {
		if validateBinding(*e.Binding) != nil || e.Binding.Target != requestTarget(r) || (e.UserID != "" && e.UserID != e.Binding.UserID) {
			return bad()
		}
		b := *e.Binding
		e.Binding = &b
	}
	if !expectedFitsEndpoint(r, e) {
		return bad()
	}
	r.wire = copyRequest(r.wire)
	return RequestContext{request: r, expected: e, valid: true}, nil
}

// expectedFitsEndpoint reports whether e carries exactly the expectations
// r's endpoint requires: a user for registration, a matching binding for
// room and release calls, and none for health and boot retirement.
func expectedFitsEndpoint(r Request, e ExpectedResponse) bool {
	switch r.endpoint {
	case EndpointConnectionRegister:
		return e.UserID != "" && e.Binding == nil
	case EndpointRoomRegister, EndpointConnectionRelease, EndpointRoomRelease:
		if e.Binding == nil {
			return false
		}
		b, ok := requestBinding(r)
		return !ok || b == *e.Binding
	case EndpointProvisionalClose:
		return true
	case EndpointHealthBootstrap, EndpointBootRetire:
		return e.UserID == "" && e.Binding == nil
	default:
		return false
	}
}
func decodeResponseBody(e Endpoint, raw []byte) (any, error) {
	switch e {
	case EndpointConnectionRegister:
		var p RegistrationResult
		err := decodeValue(raw, &p, "")
		return p, err
	case EndpointProvisionalClose:
		var p ProvisionalCloseResult
		err := decodeValue(raw, &p, provisionalResultSchema)
		return p, err
	case EndpointRoomRegister:
		var p RoomRegistrationResult
		err := decodeValue(raw, &p, roomSchema)
		return p, err
	case EndpointHealthBootstrap:
		var p HealthBootstrapResult
		err := decodeValue(raw, &p, "")
		return p, err
	case EndpointConnectionRelease, EndpointRoomRelease, EndpointBootRetire:
		if len(raw) != 0 {
			return nil, failure("invalid_variant")
		}
		return nil, nil
	}
	return nil, failure("invalid_decoder_state")
}
func responseBinding(v any) (Binding, bool) {
	switch p := v.(type) {
	case RegistrationResult:
		return Binding{p.UserID, p.FamilyID, p.Target}, true
	case RoomRegistrationResult:
		return p.Binding, true
	case ProvisionalCloseResult:
		if p.Binding != nil {
			return *p.Binding, true
		}
	}
	return Binding{}, false
}
func responseMatch(r Request, v any, e ExpectedResponse) error {
	bad := failure("binding_mismatch")
	if b, ok := responseBinding(v); ok {
		if b.Target != requestTarget(r) || (e.UserID != "" && b.UserID != e.UserID) || (e.Binding != nil && b != *e.Binding) {
			return bad
		}
	}
	switch p := v.(type) {
	case ProvisionalCloseResult:
		q, ok := r.ProvisionalClose()
		if !ok || p.Target != q.Target || p.RegistrationBodyDigest != q.RegistrationBodyDigest || p.RegistrationTokenDigest != q.RegistrationTokenDigest {
			return bad
		}
	case RoomRegistrationResult:
		if p.Room != requestRoom(r) {
			return bad
		}
	case HealthBootstrapResult:
		if p.NodeBootID != requestBoot(r) {
			return bad
		}
	}
	return nil
}
func responseFields(r Request, v any, raw []byte, c endpointConfig) []string {
	f := []string{}
	schema := c.schema
	if r.endpoint == EndpointProvisionalClose {
		schema = provisionalResultSchema
	}
	if schema != "" {
		f = append(f, schema)
	}
	f = append(f, "1", c.path, r.timestamp, r.nonce, r.bodyDigest)
	switch p := v.(type) {
	case RegistrationResult:
		f = append(f, "4")
		return append(f, bindingFields(Binding{p.UserID, p.FamilyID, p.Target})...)
	case RoomRegistrationResult:
		f = append(f, "200", "4")
		f = append(f, bindingFields(p.Binding)...)
		return append(f, roomFields(p.Room)...)
	case ProvisionalCloseResult, HealthBootstrapResult:
		return append(f, "200", mediaproof.TokenDigest(string(raw)))
	}
	f = append(f, "204")
	if r.endpoint == EndpointBootRetire {
		return append(f, requestBoot(r))
	}
	b, _ := requestBinding(r)
	f = append(f, bindingFields(b)...)
	if r.endpoint == EndpointRoomRelease {
		f = append(f, roomFields(requestRoom(r))...)
	}
	return f
}

// VerifyHTTPResponse requires exact success status, proof, freshness and original-request binding.
func VerifyHTTPResponse(e Endpoint, wire HTTPResponse, ctx RequestContext, secret []byte, now time.Time) (Response, error) {
	if len(wire.Body) > maxPayload {
		return Response{}, failure("payload_too_large")
	}
	if !ctx.valid || ctx.request.endpoint != e {
		return Response{}, failure("invalid_decoder_state")
	}
	c, err := config(e)
	if err != nil {
		return Response{}, err
	}
	if wire.Status != c.status {
		return Response{}, failure("unexpected_status")
	}
	v, err := decodeResponseBody(e, wire.Body)
	if err != nil {
		return Response{}, err
	}
	t, n, p, err := proofHeaders(wire.Headers, c.header+"-Response")
	if err != nil {
		return Response{}, err
	}
	if n != ctx.request.nonce {
		return Response{}, failure("invalid_headers")
	}
	if err := verify(c.domain+"/response/v1", t, p, responseFields(ctx.request, v, wire.Body, c), secret, now, false); err != nil {
		return Response{}, err
	}
	if err := responseMatch(ctx.request, v, ctx.expected); err != nil {
		return Response{}, err
	}
	return Response{endpoint: e, value: v, empty: c.status == 204}, nil
}

// SignHTTPResponse signs a validated success payload using the retained original request.
func SignHTTPResponse(r Request, payload ResponsePayload, timestamp string, secret []byte) (HTTPResponse, error) {
	if err := validRequest(r); err != nil {
		return HTTPResponse{}, err
	}
	c, err := config(r.endpoint)
	if err != nil {
		return HTTPResponse{}, err
	}
	if !validTimestamp(timestamp) {
		return HTTPResponse{}, failure("invalid_value")
	}
	// Accept concrete values only; a typed-nil pointer is not a 204 payload.
	valid := false
	switch payload.(type) {
	case nil:
		valid = c.status == 204
	case RegistrationResult:
		valid = r.endpoint == EndpointConnectionRegister
	case ProvisionalCloseResult:
		valid = r.endpoint == EndpointProvisionalClose
	case RoomRegistrationResult:
		valid = r.endpoint == EndpointRoomRegister
	case HealthBootstrapResult:
		valid = r.endpoint == EndpointHealthBootstrap
	}
	if !valid {
		return HTTPResponse{}, failure("invalid_decoder_state")
	}
	var raw []byte
	if payload != nil {
		raw, err = json.Marshal(payload)
		if err != nil {
			return HTTPResponse{}, failure("invalid_decoder_state")
		}
	}
	v, err := decodeResponseBody(r.endpoint, raw)
	if err != nil {
		return HTTPResponse{}, err
	}
	expected := ExpectedResponse{}
	if b, ok := requestBinding(r); ok {
		expected.Binding = &b
	}
	if err := responseMatch(r, v, expected); err != nil {
		return HTTPResponse{}, err
	}
	p, err := sign(c.domain+"/response/v1", timestamp, responseFields(r, v, raw, c), secret)
	if err != nil {
		return HTTPResponse{}, err
	}
	return HTTPResponse{Status: c.status, Headers: makeHeaders(c.header+"-Response", timestamp, r.nonce, p), Body: raw}, nil
}
