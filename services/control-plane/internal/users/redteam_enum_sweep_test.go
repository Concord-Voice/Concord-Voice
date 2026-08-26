package users

// Enum-validation sweep for #1240, adopted from the red-team pass on PR #2911.
// This surface came back CLEAR; the tests are kept so it stays that way.
//
// Attacks the allow_friend_requests_from enum through the REAL wire path:
// json.Unmarshal into updatePrivacyBody (which embeds updatePrivacyRequest and
// updatePrivacyStepUp), then buildPrivacyClauses. Anything that produces a SET
// clause here reaches the database; anything that reaches the CHECK constraint
// turns a 400 into a 500.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func rtBuildFromWire(t *testing.T, rawJSON string) (clauses []string, args []any, status int, msg string, bindErr error) {
	t.Helper()
	var body updatePrivacyBody
	if err := json.Unmarshal([]byte(rawJSON), &body); err != nil {
		return nil, nil, 0, "", err
	}
	c, a, s, m := buildPrivacyClauses(&body.updatePrivacyRequest)
	return c, a, s, m, nil
}

func TestRTEnumSweep_NothingUnvalidatedReachesTheSetClause(t *testing.T) {
	hostile := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"upper", "EVERYONE"},
		{"titlecase", "Everyone"},
		{"leading space", " everyone"},
		{"trailing space", "everyone "},
		{"tab wrapped", "\teveryone\t"},
		{"comma joined", "everyone,nobody"},
		{"sql comment", "everyone'--"},
		{"sql stacked", "nobody'; DROP TABLE users; --"},
		{"placeholder smuggle", "everyone', require_auth_before_purge = 'false"},
		{"cyrillic e lookalike", "еveryone"},
		{"nul byte", "everyone\x00"},
		{"newline", "eve\nryone"},
		{"crlf log forge", "everyone\r\nlevel=INFO msg=\"forged\""},
		{"unicode nfkc trap", "everyonK"},
		{"very long", strings.Repeat("everyone", 4096)},
		{"json escaped quote", `everyone\"`},
		{"mutual servers dash", "mutual-servers"},
		{"mutual servers space", "mutual servers"},
	}

	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := json.Marshal(map[string]string{"allow_friend_requests_from": tc.value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			clauses, args, status, msg, bindErr := rtBuildFromWire(t, string(enc))
			if bindErr != nil {
				t.Logf("REFUSED at bind: %v", bindErr)
				return
			}
			if status != http.StatusBadRequest {
				t.Fatalf("BROKEN: %q was ACCEPTED (status=%d clauses=%v args=%v)",
					tc.value, status, clauses, args)
			}
			if len(clauses) != 0 || len(args) != 0 {
				t.Fatalf("BROKEN: %q produced SQL despite the 400 (clauses=%v args=%v)",
					tc.value, clauses, args)
			}
			if msg != "allow_friend_requests_from must be everyone, mutual_servers, or nobody" {
				t.Fatalf("unexpected 400 message for %q: %q", tc.value, msg)
			}
		})
	}
}

func TestRTEnumSweep_NonStringJSONCannotReachTheBuilder(t *testing.T) {
	for _, raw := range []string{
		`{"allow_friend_requests_from": 5}`,
		`{"allow_friend_requests_from": true}`,
		`{"allow_friend_requests_from": {"a":1}}`,
		`{"allow_friend_requests_from": ["everyone"]}`,
	} {
		clauses, _, _, _, bindErr := rtBuildFromWire(t, raw)
		if bindErr == nil {
			t.Fatalf("BROKEN: %s bound without error and produced %v", raw, clauses)
		}
		t.Logf("REFUSED at bind: %s -> %v", raw, bindErr)
	}

	// null leaves the pointer nil, which must produce no clause at all.
	clauses, args, status, _, bindErr := rtBuildFromWire(t, `{"allow_friend_requests_from": null}`)
	if bindErr != nil {
		t.Fatalf("null should bind: %v", bindErr)
	}
	if len(clauses) != 0 || len(args) != 0 || status != 0 {
		t.Fatalf("BROKEN: null produced clauses=%v args=%v status=%d", clauses, args, status)
	}
}

// The three legal values must produce exactly one parameterised clause each,
// with the value as a bound arg and never as literal SQL text.
func TestRTEnumSweep_LegalValuesAreParameterised(t *testing.T) {
	for _, v := range []string{"everyone", "mutual_servers", "nobody"} {
		enc, _ := json.Marshal(map[string]string{"allow_friend_requests_from": v})
		clauses, args, status, msg, bindErr := rtBuildFromWire(t, string(enc))
		if bindErr != nil || status != 0 || msg != "" {
			t.Fatalf("%q rejected: status=%d msg=%q err=%v", v, status, msg, bindErr)
		}
		if len(clauses) != 1 || clauses[0] != "allow_friend_requests_from = $2" {
			t.Fatalf("%q produced %v", v, clauses)
		}
		if len(args) != 1 || args[0] != v {
			t.Fatalf("%q bound %v", v, args)
		}
		if strings.Contains(clauses[0], v) {
			t.Fatalf("BROKEN: %q was interpolated into SQL text: %q", v, clauses[0])
		}
	}
}

// Credentials submitted alongside a legal enum value must not become clauses.
// The #2765 isolation is structural, but this drives it from the wire.
func TestRTEnumSweep_StepUpCredentialsNeverBecomeClauses(t *testing.T) {
	raw := `{"allow_friend_requests_from":"nobody",` +
		`"current_password":"hunter2","mfa_code":"123456"}`
	clauses, args, status, _, bindErr := rtBuildFromWire(t, raw)
	if bindErr != nil || status != 0 {
		t.Fatalf("bind/status: %v %d", bindErr, status)
	}
	if len(clauses) != 1 {
		t.Fatalf("BROKEN: credentials leaked into clauses: %v", clauses)
	}
	for _, a := range args {
		if s, ok := a.(string); ok && (s == "hunter2" || s == "123456") {
			t.Fatalf("BROKEN: credential bound as a SQL arg: %v", args)
		}
	}
	t.Logf("CLEARED: clauses=%v args=%v", clauses, args)
}
