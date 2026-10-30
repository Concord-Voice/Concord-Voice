package voiceauthprotocol

import "encoding/json"

const authSchema = "concord.auth-revocation"
const epochSchema = "concord.credential-connection-revocation"
const healthSchema = "concord.voice-enforcement-health"
const provisionalSchema = "concord.auth-connection-provisional-close"
const provisionalResultSchema = "concord.auth-connection-provisional-close-result"
const roomSchema = "concord.auth-room-binding"
const retireSchema = "concord.auth-boot-retirement"

func payloadSchema(v any) string {
	switch p := v.(type) {
	case AuthCommand:
		return p.Schema
	case EpochCommand:
		return p.Schema
	case Health4:
		return p.Schema
	case AuthAck:
		return p.Schema
	case EpochAck:
		return p.Schema
	case HealthAck4:
		return p.Schema
	}
	return ""
}

// DecodeCommand strictly decodes a command while retaining its original raw bytes.
func DecodeCommand(raw []byte) (Command, error) {
	if err := strictRaw(raw); err != nil {
		return Command{}, err
	}
	var discriminator struct {
		Schema string `json:"schema"`
	}
	if json.Unmarshal(raw, &discriminator) != nil {
		return Command{}, failure("invalid_variant")
	}
	var v any
	switch discriminator.Schema {
	case authSchema:
		var p AuthCommand
		if err := decodeValue(raw, &p, authSchema); err != nil {
			return Command{}, err
		}
		v = p
	case epochSchema:
		var p EpochCommand
		if err := decodeValue(raw, &p, epochSchema); err != nil {
			return Command{}, err
		}
		v = p
	case healthSchema:
		var p Health4
		if err := decodeValue(raw, &p, healthSchema); err != nil {
			return Command{}, err
		}
		v = p
	default:
		return Command{}, failure("invalid_variant")
	}
	c := Command{value: v, raw: append([]byte(nil), raw...)}
	if err := commandBinding(c); err != nil {
		return Command{}, err
	}
	return c, nil
}
func construct(v any) (Command, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Command{}, failure("invalid_decoder_state")
	}
	c, err := DecodeCommand(raw)
	if err != nil {
		return Command{}, err
	}
	if err := commandBinding(c); err != nil {
		return Command{}, err
	}
	return c, nil
}

// NewAuthCommand constructs a validated auth command using caller-selected attempt metadata.
func NewAuthCommand(b Binding, operationID, scope, reason string, a AttemptMeta) (Command, error) {
	return construct(AuthCommand{UserID: b.UserID, FamilyID: b.FamilyID, Target: b.Target, Schema: authSchema, Version: 1, OperationID: operationID, Scope: scope, Reason: reason, Timestamp: a.Timestamp, Nonce: a.Nonce, Proof: zeroProof})
}

// NewEpochCommand constructs a validated epoch command bound to its superseded target epoch.
func NewEpochCommand(b Binding, parentGeneration, supersededCredentialEpoch string, a AttemptMeta) (Command, error) {
	return construct(EpochCommand{UserID: b.UserID, FamilyID: b.FamilyID, Target: b.Target, Schema: epochSchema, Version: 1, ParentGeneration: parentGeneration, SupersededCredentialEpoch: supersededCredentialEpoch, Timestamp: a.Timestamp, Nonce: a.Nonce, Proof: zeroProof})
}

// NewHealthCommand constructs a validated protocol-4 health challenge without reading a clock.
func NewHealthCommand(boot, challenge, activation string, allowed bool, timestamp string) (Command, error) {
	return construct(Health4{Schema: healthSchema, Version: 1, Protocol: 4, NodeBootID: boot, Challenge: challenge, Activation: activation, AdmissionAllowed: allowed, Timestamp: timestamp, Proof: zeroProof})
}
func commandBinding(c Command) error {
	if p, ok := c.Epoch(); ok && p.SupersededCredentialEpoch != p.Target.CredentialEpoch {
		return failure("binding_mismatch")
	}
	return nil
}
