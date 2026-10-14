// Package voicecommand defines the durable voice-enforcement wire contract and
// the database receipt that binds it to one broker generation.
package voicecommand

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Schema, Version and MaxCommandBytes define the closed durable wire format.
const (
	Schema          = "concord.voice-enforcement"
	Version         = 1
	MaxCommandBytes = 4096
)

// ErrInvalidCommand is deliberately independent of rejected wire values.
var ErrInvalidCommand = errors.New("invalid durable voice command")

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var credentialEpoch = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ExactTarget is frozen from the enforcement registry before the command commits.
// An empty credential epoch preserves evidence from the legacy registry.
type ExactTarget struct {
	SessionGeneration string `json:"sessionGeneration"`
	NodeBootID        string `json:"nodeBootId"`
	SocketID          string `json:"socketId"`
	CredentialEpoch   string `json:"credentialEpoch"`
}

// VoiceCommand preserves decimal strings rather than rounding PostgreSQL BIGINTs.
type VoiceCommand struct {
	Schema                string       `json:"schema"`
	Version               int          `json:"version"`
	StreamGeneration      string       `json:"streamGeneration"`
	CommandID             string       `json:"commandId"`
	Sequence              string       `json:"sequence"`
	Scope                 string       `json:"scope"`
	Kind                  string       `json:"kind"`
	Action                string       `json:"action"`
	ChannelID             *string      `json:"channelId"`
	ServerID              *string      `json:"serverId,omitempty"`
	UserID                string       `json:"userId"`
	Target                *ExactTarget `json:"target"`
	Reason                string       `json:"reason,omitempty"`
	AuthorizationRevision string       `json:"authorizationRevision,omitempty"`
	ServerMuted           *bool        `json:"serverMuted,omitempty"`
	ServerDeafened        *bool        `json:"serverDeafened,omitempty"`
}

var requiredFields = []string{
	"schema", "version", "streamGeneration", "commandId", "sequence", "scope",
	"kind", "action", "channelId", "userId", "target",
}
var optionalFields = []string{
	"serverId", "reason", "authorizationRevision", "serverMuted", "serverDeafened",
}
var targetFields = []string{"sessionGeneration", "nodeBootId", "socketId", "credentialEpoch"}

// DecodeVoiceCommand admits exactly one closed-schema envelope for generation.
// Reserved fields never acquire a permissive legacy interpretation on failure.
func DecodeVoiceCommand(data []byte, generation uuid.UUID) (VoiceCommand, error) {
	var command VoiceCommand
	if generation == uuid.Nil || len(data) > MaxCommandBytes || !utf8.Valid(data) || unpairedJSONSurrogate(data) {
		return command, ErrInvalidCommand
	}
	fields, err := closedObject(data, requiredFields, optionalFields)
	if err != nil {
		return command, err
	}
	if err := json.Unmarshal(data, &command); err != nil {
		return VoiceCommand{}, ErrInvalidCommand
	}
	if command.Schema != Schema || command.Version != Version ||
		command.StreamGeneration != generation.String() ||
		!canonicalUUID.MatchString(command.StreamGeneration) ||
		!canonicalUUID.MatchString(command.CommandID) || !canonicalUUID.MatchString(command.UserID) ||
		!positiveBIGINT(command.Sequence) || (command.Scope != "channel" && command.Scope != "dm") {
		return VoiceCommand{}, ErrInvalidCommand
	}
	if command.ChannelID != nil && !canonicalUUID.MatchString(*command.ChannelID) {
		return VoiceCommand{}, ErrInvalidCommand
	}
	if command.Target != nil {
		if command.ChannelID == nil || !validTarget(fields["target"], command.Target) {
			return VoiceCommand{}, ErrInvalidCommand
		}
	}
	if !validCommandKind(command, fields) {
		return VoiceCommand{}, ErrInvalidCommand
	}
	return command, nil
}

// closedObject rejects duplicate and noncanonical field names too: Go's ordinary
// struct decoder otherwise accepts case-insensitive aliases and last-key wins.
func closedObject(data []byte, required, optional []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrInvalidCommand
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, field := range required {
		allowed[field] = true
	}
	for _, field := range optional {
		allowed[field] = true
	}
	fields, err := readClosedFields(decoder, allowed)
	if err != nil {
		return nil, err
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, ErrInvalidCommand
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidCommand
	}
	for _, field := range required {
		if _, present := fields[field]; !present {
			return nil, ErrInvalidCommand
		}
	}
	return fields, nil
}

func readClosedFields(decoder *json.Decoder, allowed map[string]bool) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok || !allowed[key] {
			return nil, ErrInvalidCommand
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrInvalidCommand
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrInvalidCommand
		}
		fields[key] = value
	}
	return fields, nil
}

func validTarget(raw json.RawMessage, target *ExactTarget) bool {
	fields, err := closedObject(raw, targetFields, nil)
	if err != nil || !canonicalUUID.MatchString(target.SessionGeneration) ||
		!canonicalUUID.MatchString(target.NodeBootID) || target.SocketID == "" ||
		len(target.SocketID) > 128 {
		return false
	}
	var epoch *string
	if err := json.Unmarshal(fields["credentialEpoch"], &epoch); err != nil || epoch == nil {
		return false
	}
	if target.CredentialEpoch != "" && !credentialEpoch.MatchString(target.CredentialEpoch) {
		return false
	}
	for _, character := range target.SocketID {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func positiveBIGINT(value string) bool {
	if value == "" || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}

func validCommandKind(command VoiceCommand, fields map[string]json.RawMessage) bool {
	switch command.Kind {
	case "server_moderation":
		return validSnapshot(command, fields)
	case "user_moderation":
		return command.Target != nil && (command.Action == "mute" || command.Action == "deafen") &&
			!hasAny(fields, optionalFields...)
	case "disconnect":
		if command.Action != "disconnect" ||
			(command.Reason != "membership_removed" && command.Reason != "moderator_request") ||
			hasAny(fields, "serverId", "authorizationRevision", "serverMuted", "serverDeafened") {
			return false
		}
		return command.Target != nil ||
			(command.Scope == "dm" && command.ChannelID != nil && command.Reason == "membership_removed")
	default:
		return false
	}
}

func validSnapshot(command VoiceCommand, fields map[string]json.RawMessage) bool {
	if (command.Action != "mute" && command.Action != "unmute" &&
		command.Action != "deafen" && command.Action != "undeafen") ||
		!positiveBIGINT(command.AuthorizationRevision) || command.ServerMuted == nil ||
		command.ServerDeafened == nil || (*command.ServerDeafened && !*command.ServerMuted) ||
		hasAny(fields, "reason") {
		return false
	}
	if command.Scope == "channel" {
		return command.ServerID != nil && canonicalUUID.MatchString(*command.ServerID)
	}
	return command.ChannelID != nil && !hasAny(fields, "serverId")
}

func hasAny(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, present := fields[name]; present {
			return true
		}
	}
	return false
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. Reject them before
// decoding so a frozen socket identity cannot silently change on the Go side.
func unpairedJSONSurrogate(data []byte) bool {
	inside := false
	for index := 0; index < len(data); index++ {
		if data[index] == '"' {
			inside = !inside
			continue
		}
		if !inside || data[index] != '\\' {
			continue
		}
		index++
		if index >= len(data) || data[index] != 'u' {
			continue
		}
		end, ok := pairedJSONEscapeEnd(data, index)
		if !ok {
			return true
		}
		index = end
	}
	return false
}

func pairedJSONEscapeEnd(data []byte, index int) (int, bool) {
	code, ok := jsonEscapeCode(data, index)
	if !ok || (code >= 0xdc00 && code <= 0xdfff) {
		return index, false
	}
	end := index + 4
	if code < 0xd800 || code > 0xdbff {
		return end, true
	}
	if end+6 >= len(data) || data[end+1] != '\\' || data[end+2] != 'u' {
		return end, false
	}
	low, ok := jsonEscapeCode(data, end+2)
	return end + 6, ok && low >= 0xdc00 && low <= 0xdfff
}

func jsonEscapeCode(data []byte, index int) (uint64, bool) {
	if index+4 >= len(data) {
		return 0, false
	}
	code, err := strconv.ParseUint(string(data[index+1:index+5]), 16, 16)
	return code, err == nil
}
