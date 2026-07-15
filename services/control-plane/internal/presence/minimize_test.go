package presence

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestApplyMinimization_RemovesGranularFieldsFromSerializedBytes(t *testing.T) {
	started := int64(123)
	tests := []struct {
		name     string
		category Category
		input    any
		absent   []string
		retained []string
	}{
		{
			name: "server voice", category: CategoryServerVoice,
			input: ServerVoicePayload{
				ChannelID: uuid.New(), ChannelName: "General",
				ServerID: uuid.New(), ServerName: "Concord", StartedAt: &started,
			},
			absent:   []string{"channel_name", "server_name"},
			retained: []string{"channel_id", "server_id", "started_at"},
		},
		{
			name: "private call", category: CategoryPrivateCall,
			input:    PrivateCallPayload{CallType: "group", ParticipantCount: 3, StartedAt: &started},
			absent:   []string{"participant_count"},
			retained: []string{"call_type", "started_at"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.input)
			require.NoError(t, err)
			got, err := ApplyMinimization(test.category, raw)
			require.NoError(t, err)
			for _, key := range test.absent {
				require.NotContains(t, string(got), "\""+key+"\"")
			}
			for _, key := range test.retained {
				require.Contains(t, string(got), "\""+key+"\"")
			}
		})
	}
}

func TestApplyMinimization_StripsUnknownAndRejectsInvalidJSON(t *testing.T) {
	got, err := ApplyMinimization(
		CategoryPrivateCall,
		[]byte(`{"call_type":"dm","participant_identity":"secret"}`),
	)
	require.NoError(t, err)
	require.JSONEq(t, `{"call_type":"dm"}`, string(got))

	for _, raw := range [][]byte{nil, []byte(`null`), []byte(`[]`), []byte(`{"call_type":`), []byte(`{} {}`)} {
		out, err := ApplyMinimization(CategoryPrivateCall, raw)
		require.Error(t, err)
		require.Nil(t, out)
	}
	out, err := ApplyMinimization(Category("unknown"), []byte(`{}`))
	require.Error(t, err)
	require.Nil(t, out)
}

func TestPrivateCallPayload_HasNoParticipantIdentitySurface(t *testing.T) {
	typ := reflect.TypeOf(PrivateCallPayload{})
	for index := 0; index < typ.NumField(); index++ {
		name := strings.ToLower(typ.Field(index).Name + " " + typ.Field(index).Tag.Get("json"))
		require.NotContains(t, name, "participant_id")
		require.NotContains(t, name, "participant_name")
	}
}

func TestPolicyError_ErrorAndUnwrap(t *testing.T) {
	cause := errors.New("database unavailable")
	err := &PolicyError{class: FailureSettingsRead, cause: cause}

	require.Equal(t, "rich-presence policy failed: settings_read", err.Error())
	require.Same(t, cause, err.Unwrap())
}

func TestPolicyErrorClass_DirectWrappedAndUnknown(t *testing.T) {
	policyErr := &PolicyError{class: FailureAudienceRead, cause: errors.New("query failed")}
	tests := []struct {
		name string
		err  error
		want FailureClass
	}{
		{name: "direct", err: policyErr, want: FailureAudienceRead},
		{name: "wrapped", err: fmt.Errorf("authorize presence: %w", policyErr), want: FailureAudienceRead},
		{name: "unknown", err: errors.New("not a policy error"), want: FailureUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, PolicyErrorClass(test.err))
		})
	}
}
