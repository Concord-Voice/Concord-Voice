package voiceauthprotocol

import (
	"crypto/subtle"
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/mediaproof"
)

const zeroProof = "0000000000000000000000000000000000000000000000000000000000000000"

func sign(domain, timestamp string, fields []string, secret []byte) (string, error) {
	p := mediaproof.Sign(mediaproof.DeriveKey(string(secret), domain), "v1", timestamp, fields...)
	if p == "" {
		return "", failure("invalid_proof")
	}
	return p, nil
}
func verify(domain, timestamp, proof string, fields []string, secret []byte, now time.Time, exact bool) error {
	p, err := sign(domain, timestamp, fields, secret)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(p), []byte(proof)) != 1 {
		return failure("invalid_proof")
	}
	stamp, ok := new(big.Int).SetString(timestamp, 10)
	if !ok {
		return failure("invalid_decoder_state")
	}
	age := new(big.Int).Sub(big.NewInt(now.Unix()), stamp)
	lo, hi := int64(-30), int64(30)
	if exact {
		lo, hi = -1, 6
	}
	if age.Cmp(big.NewInt(lo)) < 0 || age.Cmp(big.NewInt(hi)) > 0 {
		return failure("proof_stale")
	}
	return nil
}
func targetFields(t Target) []string {
	return []string{t.ConnectionGeneration, t.NodeBootID, t.SocketID, t.CredentialEpoch}
}
func bindingFields(b Binding) []string {
	return append([]string{b.UserID, b.FamilyID}, targetFields(b.Target)...)
}
func roomFields(r Room) []string { return []string{r.SessionGeneration, r.RoomID, r.RoomKind} }
func commandProof(c Command) (domain, timestamp, proof string, fields []string, exact bool, err error) {
	switch p := c.value.(type) {
	case AuthCommand:
		domain = "concord/auth-connection/eject/request/v1"
		timestamp = p.Timestamp
		proof = p.Proof
		exact = true
		fields = make([]string, 0, 12)
		fields = append(fields, p.Schema, "1", p.OperationID, p.Scope, p.Reason, p.UserID, p.FamilyID)
		fields = append(fields, targetFields(p.Target)...)
		fields = append(fields, p.Nonce)
	case EpochCommand:
		domain = "concord/auth-connection/credential-eject/request/v1"
		timestamp = p.Timestamp
		proof = p.Proof
		exact = true
		fields = make([]string, 0, 11)
		fields = append(fields, p.Schema, "1", p.ParentGeneration, p.UserID, p.FamilyID, p.SupersededCredentialEpoch)
		fields = append(fields, targetFields(p.Target)...)
		fields = append(fields, p.Nonce)
	case Health4:
		domain = "concord/auth-connection/health/request/v1"
		timestamp = p.Timestamp
		proof = p.Proof
		fields = []string{p.Schema, "1", "4", p.NodeBootID, p.Challenge, p.Activation, strconv.FormatBool(p.AdmissionAllowed)}
	default:
		err = failure("invalid_decoder_state")
	}
	return
}

// VerifyCommand checks the domain-separated MAC, explicit-clock freshness and command binding.
func VerifyCommand(c Command, secret []byte, now time.Time) error {
	d, t, p, f, e, err := commandProof(c)
	if err != nil {
		return err
	}
	if err := verify(d, t, p, f, secret, now, e); err != nil {
		return err
	}
	return commandBinding(c)
}

// SignCommand signs a privately constructed or decoded command in the fixed wire field order.
func SignCommand(c Command, secret []byte) ([]byte, error) {
	d, t, _, f, _, err := commandProof(c)
	if err != nil {
		return nil, err
	}
	if err := commandBinding(c); err != nil {
		return nil, err
	}
	p, err := sign(d, t, f, secret)
	if err != nil {
		return nil, err
	}
	switch v := c.value.(type) {
	case AuthCommand:
		v.Proof = p
		return json.Marshal(v)
	case EpochCommand:
		v.Proof = p
		return json.Marshal(v)
	case Health4:
		v.Proof = p
		return json.Marshal(v)
	}
	return nil, failure("invalid_decoder_state")
}

// SignAuthACK signs an auth acknowledgment that echoes every original command field.
func SignAuthACK(c Command, secret []byte) ([]byte, error) {
	v, ok := c.value.(AuthCommand)
	if !ok {
		return nil, failure("invalid_decoder_state")
	}
	if err := commandBinding(c); err != nil {
		return nil, err
	}
	d, t, _, f, _, err := commandProof(c)
	if err != nil {
		return nil, err
	}
	d = ackDomain(d)
	f = append(f, "true")
	proof, err := sign(d, t, f, secret)
	if err != nil {
		return nil, err
	}
	return json.Marshal(AuthAck{UserID: v.UserID, FamilyID: v.FamilyID, Target: v.Target, Schema: v.Schema, Version: v.Version, OperationID: v.OperationID, Scope: v.Scope, Reason: v.Reason, Timestamp: v.Timestamp, Nonce: v.Nonce, OK: true, Proof: proof})
}

// SignEpochACK signs an epoch acknowledgment that echoes every original command field.
func SignEpochACK(c Command, secret []byte) ([]byte, error) {
	v, ok := c.value.(EpochCommand)
	if !ok {
		return nil, failure("invalid_decoder_state")
	}
	if err := commandBinding(c); err != nil {
		return nil, err
	}
	d, t, _, f, _, err := commandProof(c)
	if err != nil {
		return nil, err
	}
	d = ackDomain(d)
	f = append(f, "true")
	proof, err := sign(d, t, f, secret)
	if err != nil {
		return nil, err
	}
	return json.Marshal(EpochAck{UserID: v.UserID, FamilyID: v.FamilyID, Target: v.Target, Schema: v.Schema, Version: v.Version, ParentGeneration: v.ParentGeneration, SupersededCredentialEpoch: v.SupersededCredentialEpoch, Timestamp: v.Timestamp, Nonce: v.Nonce, OK: true, Proof: proof})
}

// SignHealthACK signs a health acknowledgment without granting beyond the challenge permission.
func SignHealthACK(c Command, granted bool, secret []byte) ([]byte, error) {
	v, ok := c.value.(Health4)
	if !ok {
		return nil, failure("invalid_decoder_state")
	}
	if granted && (!v.AdmissionAllowed || v.Activation != "active") {
		return nil, failure("invalid_value")
	}
	if err := commandBinding(c); err != nil {
		return nil, err
	}
	d, t, _, f, _, err := commandProof(c)
	if err != nil {
		return nil, err
	}
	d = ackDomain(d)
	f = append(f, "true")
	f = append(f, strconv.FormatBool(granted))
	proof, err := sign(d, t, f, secret)
	if err != nil {
		return nil, err
	}
	return json.Marshal(HealthAck4{Schema: v.Schema, Version: v.Version, Protocol: v.Protocol, NodeBootID: v.NodeBootID, Challenge: v.Challenge, Activation: v.Activation, AdmissionAllowed: v.AdmissionAllowed, Timestamp: v.Timestamp, TransportOK: true, AdmissionGranted: granted, Proof: proof})
}

// VerifyACK verifies an acknowledgment proof and its exact original-command binding.
func VerifyACK(raw []byte, original Command, secret []byte, now time.Time) (ACK, error) {
	if err := strictRaw(raw); err != nil {
		return ACK{}, err
	}
	if original.Kind() == "" {
		return ACK{}, failure("invalid_decoder_state")
	}
	var c Command
	var result ACK
	var extra []string
	switch original.Kind() {
	case authSchema:
		var v AuthAck
		if err := decodeValue(raw, &v, authSchema); err != nil {
			return ACK{}, err
		}
		result = ACK{value: v}
		c = Command{value: AuthCommand{UserID: v.UserID, FamilyID: v.FamilyID, Target: v.Target, Schema: v.Schema, Version: v.Version, OperationID: v.OperationID, Scope: v.Scope, Reason: v.Reason, Timestamp: v.Timestamp, Nonce: v.Nonce, Proof: v.Proof}}
		extra = []string{"true"}
	case epochSchema:
		var v EpochAck
		if err := decodeValue(raw, &v, epochSchema); err != nil {
			return ACK{}, err
		}
		result = ACK{value: v}
		c = Command{value: EpochCommand{UserID: v.UserID, FamilyID: v.FamilyID, Target: v.Target, Schema: v.Schema, Version: v.Version, ParentGeneration: v.ParentGeneration, SupersededCredentialEpoch: v.SupersededCredentialEpoch, Timestamp: v.Timestamp, Nonce: v.Nonce, Proof: v.Proof}}
		extra = []string{"true"}
	case healthSchema:
		var v HealthAck4
		if err := decodeValue(raw, &v, healthSchema); err != nil {
			return ACK{}, err
		}
		result = ACK{value: v}
		c = Command{value: Health4{Schema: v.Schema, Version: v.Version, Protocol: v.Protocol, NodeBootID: v.NodeBootID, Challenge: v.Challenge, Activation: v.Activation, AdmissionAllowed: v.AdmissionAllowed, Timestamp: v.Timestamp, Proof: v.Proof}}
		extra = []string{"true", strconv.FormatBool(v.AdmissionGranted)}
	default:
		return ACK{}, failure("invalid_decoder_state")
	}
	d, t, p, f, e, err := commandProof(c)
	if err != nil {
		return ACK{}, err
	}
	d = ackDomain(d)
	if err := verify(d, t, p, append(f, extra...), secret, now, e); err != nil {
		return ACK{}, err
	}
	if err := commandBinding(c); err != nil {
		return ACK{}, err
	}
	_, ot, _, of, _, err := commandProof(original)
	if err != nil {
		return ACK{}, err
	}
	if t != ot || !reflect.DeepEqual(f, of) {
		return ACK{}, failure("binding_mismatch")
	}
	return result, nil
}

// ackDomain turns a request proof domain into its acknowledgement domain.
func ackDomain(d string) string { return strings.Replace(d, "/request/", "/ack/", 1) }
