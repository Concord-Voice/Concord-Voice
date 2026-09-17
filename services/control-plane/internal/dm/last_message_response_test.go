package dm

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLastMessageResponse_JSONTags pins the wire names against
// docs/api/openapi.yaml, which has no CI verifier in either direction (#2364
// spec §7). It lives in this internal `package dm` file rather than
// handlers_test.go's external `dm_test` package because lastMessageResponse
// is unexported and no export_test.go alias exists for it.
//
// This is not redundant with TestDMLastMessage_KeySetIsExactlySixFields
// (handlers_test.go): that test round-trips one fixture through JSON and
// asserts the emitted key set, so a new field added with `omitempty` that
// happens to be empty for that fixture's content would slip past it
// silently. Asserting NumField() here fails loudly the moment the struct
// grows, independent of any fixture's shape.
func TestLastMessageResponse_JSONTags(t *testing.T) {
	want := map[string]string{
		"Content":          "content",
		"UserID":           "user_id",
		"ExpiresAt":        "expires_at",
		"CreatedAt":        "created_at",
		"Type":             "type,omitempty",
		"CallEventPayload": "call_event_payload,omitempty",
		"AttachmentType":   "attachment_type,omitempty",
		"AttachmentMime":   "attachment_mime,omitempty",
	}
	typ := reflect.TypeOf(lastMessageResponse{})
	require.Equal(t, len(want), typ.NumField(), "field added or removed without updating this pin")
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		require.Equal(t, want[f.Name], f.Tag.Get("json"), "json tag drift on %s", f.Name)
	}
}
