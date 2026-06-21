// Package e2eekeys defines the canonical response envelope for the e2ee
// key-fetch endpoint (GET /api/v1/e2ee/keys/:context_id).
//
// The envelope is shared between the control-plane handler and any future
// consumer (e.g., the bot/MCP visibility work in #676). Every 4xx from the
// endpoint returns ErrorResponse; the happy path returns KeyResponse.
//
// Per [internal]rules/e2ee.md: no wrapped-key material or epoch numbers
// appear in this envelope or in the server logs that accompany it. The
// Code enum is the full failure signal; Kind disambiguates the context
// type for client-side UX routing.
package e2eekeys

// Kind indicates which context type resolved the request.
type Kind string

// Kind values returned by the key-fetch endpoint. KindUnknown is used on
// 500s where context resolution itself failed.
const (
	KindChannel Kind = "channel"
	KindDM      Kind = "dm"
	KindUnknown Kind = "unknown"
)

// Code is the machine-readable error classification emitted by the server.
//
// The client mirrors this enum in e2eeErrors.ts. Adding a code here requires
// a corresponding addition on the client and a note in the spec.
type Code string

// Code values returned in ErrorResponse.Code. Each represents a distinct
// failure mode of the key-fetch endpoint:
//   - CodeNotMember: caller is not a member of the context
//   - CodeNoKeyYet: caller is a member but no key row exists
//   - CodeRevokedEpoch: caller's epoch appears in dm_key_revocations (DM path
//     only — server-channel revocation uses a separate ledger and does not
//     currently emit this code)
//   - CodeInvalidRequest: malformed UUID or version parameter
//   - CodeInternalError: server-side 500 (e.g., DB error); client may retry
const (
	CodeNotMember      Code = "NOT_MEMBER"
	CodeNoKeyYet       Code = "NO_KEY_YET"
	CodeRevokedEpoch   Code = "REVOKED_EPOCH"
	CodeInvalidRequest Code = "INVALID_REQUEST"
	CodeInternalError  Code = "INTERNAL_ERROR"
)

// ErrorResponse is emitted on every 4xx from GET /api/v1/e2ee/keys/:context_id.
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    Code   `json:"code"`
	Kind    Kind   `json:"kind"`
	Pending bool   `json:"pending,omitempty"`
}

// KeyPayload carries the wrapped key material on a 200 response.
type KeyPayload struct {
	WrappedKey string `json:"wrapped_key"`
	KeyVersion int    `json:"key_version"`
}

// KeyResponse is the 200-path envelope.
type KeyResponse struct {
	Key  KeyPayload `json:"key"`
	Kind Kind       `json:"kind"`
}
