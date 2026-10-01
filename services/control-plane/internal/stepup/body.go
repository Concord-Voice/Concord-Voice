package stepup

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// ErrMsgInvalidRequestBody is the 400 body ReadOptionalStepUp answers a body
// it cannot read with.
const ErrMsgInvalidRequestBody = "Invalid request body"

const (
	// maxOptionalStepUpBodyBytes caps the body. Both fields at their caps fit
	// with room for JSON escaping and a client that sends other fields beside
	// them.
	maxOptionalStepUpBodyBytes = 4 << 10

	// maxStepUpFieldLength caps mfa_code and step_up_token alike. It is the
	// MFA-enforcement toggle's binding:"max=256" on mfa_code, counted in
	// characters as that validator counts. A minted token is 43.
	maxStepUpFieldLength = 256
)

// Input is the step-up an own-rule route's body carries. Either field may be
// empty, meaning the client sent no such factor; which one the route then
// demands is the route's decision, not this reader's. There is no password
// field: an own-rule route's password reaches only the mint endpoint, and the
// route takes the single-use StepUpToken it returned (#3509).
type Input struct {
	MFACode     string
	StepUpToken string
}

// Fields is the wire shape of an own-rule route's step-up, for a route that
// decodes its own body (the self-purges, DM Clear) and embeds it there, and for
// ReadOptionalStepUp. Every own-rule body reader goes through Input, so the
// caps and the current_password refusal live in one place.
//
// CurrentPassword is captured only to be refused: a RawMessage is non-nil for
// any value, null included, so "present as a key" is what refuses, never what
// the value is.
type Fields struct {
	MFACode         string          `json:"mfa_code"`
	StepUpToken     string          `json:"step_up_token"`
	CurrentPassword json.RawMessage `json:"current_password"`
}

// Input validates f and returns the step-up it carries. A body that carries
// current_password at all is a 400 whatever the account or the soft-lock
// position, so a client that still sends the password to the route fails
// loudly rather than looping on password_required; so is an mfa_code or a
// step_up_token longer than 256 characters. The *Error is a 4xx with no Cause.
func (f Fields) Input() (Input, *Error) {
	if f.CurrentPassword != nil || // pragma: allowlist secret
		utf8.RuneCountInString(f.MFACode) > maxStepUpFieldLength ||
		utf8.RuneCountInString(f.StepUpToken) > maxStepUpFieldLength {
		return Input{}, invalidRequestBody()
	}
	return Input{MFACode: f.MFACode, StepUpToken: f.StepUpToken}, nil
}

// ReadOptionalStepUp reads the {"mfa_code":"...","step_up_token":"..."} body
// of a route that needs a step-up only SOMETIMES: the delete-rate soft-lock's
// single-message deletes (#3455), whose own-rule confirmation runs through
// VerifyOwnRuleTx. It returns a zero Input and nil when the body carries
// neither field (empty, JSON whitespace only, or an object whose fields are
// absent, null or empty), and the fields otherwise. Unknown fields are ignored
// and no Content-Type is required: a DELETE body has no established type.
//
// Anything else is a 400 with ErrMsgInvalidRequestBody: a body over 4 KiB, a
// JSON value that is not an object (null included), trailing data after the
// object, a non-string field, and every refusal Fields.Input makes. None of
// these is read as "no step-up", because a body that tried to carry a factor
// and failed must not become a request that never sent one. The *Error is a
// 4xx, has no Cause, and is the caller's to write.
func ReadOptionalStepUp(c *gin.Context) (Input, *Error) {
	var body Fields
	present, e := readOneObject(c, &body)
	if e != nil || !present {
		return Input{}, e
	}
	return body.Input()
}

// readOneObject reads the whole body, at most 4 KiB, into dst as exactly one
// JSON object. present is false for an empty or whitespace-only body, which
// each caller decides about; anything else that is not one object is the 400.
func readOneObject(c *gin.Context, dst any) (present bool, e *Error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOptionalStepUpBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return false, invalidRequestBody()
	}
	// JSON's whitespace, not Unicode's: a body of U+00A0 is not empty.
	trimmed := bytes.Trim(raw, " \t\r\n")
	if len(trimmed) == 0 {
		return false, nil
	}
	// json.Unmarshal rejects trailing data and a wrong type, but decodes null
	// into a struct as a no-op; the leading brace is what refuses null and
	// every other non-object.
	if trimmed[0] != '{' || json.Unmarshal(trimmed, dst) != nil {
		return false, invalidRequestBody()
	}
	return true, nil
}

func invalidRequestBody() *Error {
	return &Error{Status: http.StatusBadRequest, Body: gin.H{"error": ErrMsgInvalidRequestBody}}
}
