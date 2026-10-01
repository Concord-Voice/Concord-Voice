package stepup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// optionalStepUpContext builds a DELETE carrying body and no Content-Type: the
// helper must not depend on one.
func optionalStepUpContext(body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/messages/x", strings.NewReader(body))
	return c
}

// paddedStepUpBody is a valid body carrying the code 123456, padded to exactly
// n bytes. Only its SIZE can make it invalid, which is what lets the oversize
// case prove the cap rather than some other rejection.
func paddedStepUpBody(t *testing.T, n int) string {
	t.Helper()
	const prefix, suffix = `{"mfa_code":"123456","pad":"`, `"}`
	pad := n - len(prefix) - len(suffix)
	require.GreaterOrEqual(t, pad, 0)
	body := prefix + strings.Repeat("x", pad) + suffix
	require.Len(t, body, n)
	return body
}

// stepUpBody is a body carrying both fields, raw (no JSON escaping needed:
// every test value here is plain ASCII or UTF-8 with no quote or backslash).
func stepUpBody(code, token string) string {
	return `{"mfa_code":"` + code + `","step_up_token":"` + token + `"}`
}

// The caps are the contract, so they are pinned as literals: a case sized from
// the constant would move with a mutated constant and prove nothing. Kills:
// the body cap +1 or -1, the field cap moved.
func TestReadOptionalStepUp_CapsArePinned(t *testing.T) {
	require.Equal(t, 4096, maxOptionalStepUpBodyBytes, "body cap")
	require.Equal(t, 256, maxStepUpFieldLength, "mfa_code and step_up_token cap, in characters")

	_, e := ReadOptionalStepUp(optionalStepUpContext(paddedStepUpBody(t, 4096)))
	require.Nil(t, e, "a 4096-byte body is inside the cap")
	_, e = ReadOptionalStepUp(optionalStepUpContext(paddedStepUpBody(t, 4097)))
	require.NotNil(t, e, "a 4097-byte body is over it")
}

func TestReadOptionalStepUp_AbsentFields(t *testing.T) {
	for name, body := range map[string]string{ // #nosec G101 -- test case names and request bodies, not credentials
		"empty":              "",
		"whitespace":         " \t\r\n ",
		"empty object":       "{}",
		"unknown field only": `{"reason":"spam"}`,
		"null code":          `{"mfa_code":null}`,
		"empty code":         `{"mfa_code":""}`,
		"null token":         `{"step_up_token":null}`,
		"empty token":        `{"step_up_token":""}`,
		"both null":          `{"mfa_code":null,"step_up_token":null}`,
	} {
		in, e := ReadOptionalStepUp(optionalStepUpContext(body))
		require.Nil(t, e, name)
		require.Equal(t, Input{}, in, name)
	}
}

func TestReadOptionalStepUp_ReturnsTheFields(t *testing.T) {
	longCode := strings.Repeat("a", maxStepUpFieldLength)
	longToken := strings.Repeat("t", maxStepUpFieldLength)
	multibyteCode := strings.Repeat("é", maxStepUpFieldLength)
	for name, tc := range map[string]struct {
		body string
		want Input
	}{
		"code only":              {`{"mfa_code":"123456"}`, Input{MFACode: "123456"}},
		"token only":             {`{"step_up_token":"tok"}`, Input{StepUpToken: "tok"}},
		"both":                   {stepUpBody("123456", "tok"), Input{MFACode: "123456", StepUpToken: "tok"}},
		"unknown fields ignored": {`{"mfa_code":"123456","extra":{"a":[1,2]}}`, Input{MFACode: "123456"}},
		"surrounding whitespace": {"\n {\"mfa_code\":\"123456\"} \n", Input{MFACode: "123456"}},
		"256-character code":     {`{"mfa_code":"` + longCode + `"}`, Input{MFACode: longCode}},
		"256-character token":    {`{"step_up_token":"` + longToken + `"}`, Input{StepUpToken: longToken}},
		// 256 characters in 512 bytes: the cap counts characters.
		"256 two-byte characters": {`{"mfa_code":"` + multibyteCode + `"}`, Input{MFACode: multibyteCode}},
		"both at their caps":      {stepUpBody(longCode, longToken), Input{MFACode: longCode, StepUpToken: longToken}},
		"exactly 4 KiB":           {paddedStepUpBody(t, maxOptionalStepUpBodyBytes), Input{MFACode: "123456"}},
	} {
		in, e := ReadOptionalStepUp(optionalStepUpContext(tc.body))
		require.Nil(t, e, name)
		require.Equal(t, tc.want, in, name)
	}

	c := optionalStepUpContext(`{"mfa_code":"123456"}`)
	c.Request.Header.Set("Content-Type", "text/plain")
	in, e := ReadOptionalStepUp(c)
	require.Nil(t, e, "no Content-Type requirement")
	require.Equal(t, Input{MFACode: "123456"}, in)
}

// Every malformed body is the same 400, and none is silently read as "no
// step-up": a body the client meant to carry a factor must not become a
// request that did not. Kills: dropping or loosening the byte cap (4 KiB + 1),
// accepting trailing data, dropping the object check (null), dropping either
// field's length check, and dropping the current_password refusal (#3509) —
// whose every arm, null and empty included, is a 400 because the key's
// PRESENCE is what refuses.
func TestReadOptionalStepUp_InvalidBodyIs400(t *testing.T) {
	require.Equal(t, "Invalid request body", ErrMsgInvalidRequestBody)
	for name, body := range map[string]string{ // #nosec G101 -- test case names and request bodies, not credentials
		"257-character code":          `{"mfa_code":"` + strings.Repeat("a", maxStepUpFieldLength+1) + `"}`,
		"257-character token":         `{"step_up_token":"` + strings.Repeat("t", maxStepUpFieldLength+1) + `"}`,
		"over-long code, good token":  stepUpBody(strings.Repeat("a", maxStepUpFieldLength+1), "tok"),
		"current_password":            `{"current_password":"hunter22"}`,
		"current_password null":       `{"current_password":null}`,
		"current_password empty":      `{"current_password":""}`,
		"current_password with code":  `{"mfa_code":"123456","current_password":"hunter22"}`,
		"current_password with token": `{"step_up_token":"tok","current_password":"hunter22"}`,
		"current_password non-string": `{"current_password":{"a":1}}`,
		"4 KiB + 1":                   paddedStepUpBody(t, maxOptionalStepUpBodyBytes+1),
		"null":                        "null",
		"array":                       "[]",
		"string":                      `"123456"`,
		"number":                      "123456",
		"trailing object":             `{"mfa_code":"123456"}{}`,
		"trailing garbage":            `{"mfa_code":"123456"} x`,
		"trailing after token":        `{"step_up_token":"tok"} x`,
		"wrong type code":             `{"mfa_code":123456}`,
		"wrong type token":            `{"step_up_token":true}`,
		"truncated":                   `{"mfa_code":`,
	} {
		in, e := ReadOptionalStepUp(optionalStepUpContext(body))
		require.Equal(t, Input{}, in, name)
		require.NotNil(t, e, name)
		require.Equal(t, http.StatusBadRequest, e.Status, name)
		require.Equal(t, gin.H{"error": ErrMsgInvalidRequestBody}, e.Body, name)
		require.Nil(t, e.Cause, "%s: a malformed body is an outcome, not a fault", name)
	}
}
