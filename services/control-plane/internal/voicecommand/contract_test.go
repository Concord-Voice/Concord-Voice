package voicecommand

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type namedEnvelope struct {
	Name    string          `json:"name"`
	Command json.RawMessage `json:"command"`
}

func TestDecodeVoiceCommandSharedFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/envelopes.json")
	require.NoError(t, err)
	var fixtures struct {
		Valid   []namedEnvelope `json:"valid"`
		Invalid []namedEnvelope `json:"invalid"`
	}
	require.NoError(t, json.Unmarshal(data, &fixtures))
	require.NotEmpty(t, fixtures.Valid)
	require.NotEmpty(t, fixtures.Invalid)
	generation := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	for _, fixture := range fixtures.Valid {
		t.Run("accept "+fixture.Name, func(t *testing.T) {
			command, decodeErr := DecodeVoiceCommand(fixture.Command, generation)
			require.NoError(t, decodeErr)
			encoded, encodeErr := json.Marshal(command)
			require.NoError(t, encodeErr)
			assert.JSONEq(t, string(fixture.Command), string(encoded), "immutable wire fields and decimal precision must survive decoding")
		})
	}
	for _, fixture := range fixtures.Invalid {
		t.Run("reject "+fixture.Name, func(t *testing.T) {
			_, decodeErr := DecodeVoiceCommand(fixture.Command, generation)
			require.Error(t, decodeErr)
		})
	}
}

func TestDecodeVoiceCommandRejectsMalformedJSON(t *testing.T) {
	generation := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	for _, payload := range []string{"", "null", "[]", "{}", "{", "{} {}", "true"} {
		t.Run(payload, func(t *testing.T) {
			_, err := DecodeVoiceCommand([]byte(payload), generation)
			require.Error(t, err)
		})
	}
}

func TestDecodeVoiceCommandRejectsAliasesAndDuplicateFields(t *testing.T) {
	data, err := os.ReadFile("testdata/envelopes.json")
	require.NoError(t, err)
	var fixtures struct {
		Valid []namedEnvelope `json:"valid"`
	}
	require.NoError(t, json.Unmarshal(data, &fixtures))
	require.NotEmpty(t, fixtures.Valid)
	var original map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fixtures.Valid[0].Command, &original))
	generation := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	for _, field := range []struct{ canonical, alias string }{
		{"schema", "Schema"}, {"version", "VERSION"}, {"userId", "UserId"},
		{"streamGeneration", "StreamGeneration"},
	} {
		t.Run("reject envelope alias "+field.alias, func(t *testing.T) {
			command := make(map[string]json.RawMessage, len(original))
			for key, value := range original {
				command[key] = value
			}
			command[field.alias] = command[field.canonical]
			delete(command, field.canonical)
			encoded, encodeErr := json.Marshal(command)
			require.NoError(t, encodeErr)
			_, decodeErr := DecodeVoiceCommand(encoded, generation)
			require.Error(t, decodeErr)
		})
	}
	for _, field := range []struct{ canonical, alias string }{
		{"socketId", "SocketID"}, {"nodeBootId", "NodeBootID"}, {"credentialEpoch", "CredentialEpoch"},
	} {
		t.Run("reject target alias "+field.alias, func(t *testing.T) {
			command := make(map[string]json.RawMessage, len(original))
			for key, value := range original {
				command[key] = value
			}
			var target map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(command["target"], &target))
			target[field.alias] = target[field.canonical]
			delete(target, field.canonical)
			encodedTarget, encodeErr := json.Marshal(target)
			require.NoError(t, encodeErr)
			command["target"] = encodedTarget
			encoded, encodeErr := json.Marshal(command)
			require.NoError(t, encodeErr)
			_, decodeErr := DecodeVoiceCommand(encoded, generation)
			require.Error(t, decodeErr)
		})
	}
	t.Run("reject duplicate envelope field with identical value", func(t *testing.T) {
		encoded := bytes.TrimSpace(fixtures.Valid[0].Command)
		duplicate := append(append([]byte{}, encoded[:len(encoded)-1]...), []byte(`,"version":1}`)...)
		_, decodeErr := DecodeVoiceCommand(duplicate, generation)
		require.Error(t, decodeErr)
	})
	t.Run("reject duplicate target field with identical value", func(t *testing.T) {
		command := make(map[string]json.RawMessage, len(original))
		for key, value := range original {
			command[key] = value
		}
		target := bytes.TrimSpace(command["target"])
		command["target"] = append(append([]byte{}, target[:len(target)-1]...), []byte(`,"socketId":"socket-fixture"}`)...)
		encoded, encodeErr := json.Marshal(command)
		require.NoError(t, encodeErr)
		_, decodeErr := DecodeVoiceCommand(encoded, generation)
		require.Error(t, decodeErr)
	})
}
