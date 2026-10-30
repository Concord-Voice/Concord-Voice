package voiceauthprotocol

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxPayload = 4096

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var hex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var epochPattern = regexp.MustCompile(`^(|[0-9a-f]{32})$`)
var timestampPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// encoding/json replaces malformed Unicode. Check the original bytes first so
// that replacement cannot erase a distinct spelling at the authentication boundary.
func strictRaw(raw []byte) error {
	if len(raw) > maxPayload {
		return failure("payload_too_large")
	}
	if !utf8.Valid(raw) || !json.Valid(raw) || !validSurrogates(raw) {
		return failure("malformed_body")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return failure("malformed_body")
	}
	return nil
}
func validSurrogates(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		var ok bool
		if i, ok = unicodeEscapeEnd(raw, i); !ok {
			return false
		}
	}
	return true
}

// unicodeEscapeEnd validates the \u escape whose 'u' is at i, including a
// following low surrogate, and returns the index of its last byte.
func unicodeEscapeEnd(raw []byte, i int) (int, bool) {
	if i+4 >= len(raw) {
		return i, false
	}
	n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
	if err != nil {
		return i, false
	}
	i += 4
	if n >= 0xdc00 && n <= 0xdfff {
		return i, false
	}
	if n < 0xd800 || n > 0xdbff {
		return i, true
	}
	if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
		return i, false
	}
	low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
	if err != nil || low < 0xdc00 || low > 0xdfff {
		return i, false
	}
	return i + 6, true
}
func uniqueValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return failure("malformed_body")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return failure("malformed_body")
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return failure("malformed_body")
			}
			seen[s] = true
			if err := uniqueValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueValue(d); err != nil {
				return err
			}
		}
	default:
		return failure("malformed_body")
	}
	if _, err := d.Token(); err != nil {
		return failure("malformed_body")
	}
	return nil
}
func validTimestamp(s string) bool {
	if !timestampPattern.MatchString(s) {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}
func validSocket(s string) bool {
	if len(s) < 1 || len(s) > 128 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func scalarOK(name, s string) bool {
	switch name {
	case "UserID", "FamilyID", "ConnectionGeneration", "NodeBootID", "SessionGeneration", "RoomID", "OperationID", "ParentGeneration":
		return uuidPattern.MatchString(s)
	case "CredentialEpoch", "SupersededCredentialEpoch":
		return epochPattern.MatchString(s)
	case "SocketID":
		return validSocket(s)
	case "Timestamp":
		return validTimestamp(s)
	case "Nonce", "Proof", "Challenge", "RegistrationTokenDigest", "RegistrationBodyDigest":
		return hex64Pattern.MatchString(s)
	case "Scope":
		return s == "session" || s == "user"
	case "Reason":
		return s == "logout" || s == "explicit_revoke" || s == "displacement" || s == "theft" || s == "disablement" || s == "erasure"
	case "Activation":
		return s == "prepared" || s == "active"
	case "Mode":
		return s == "attach" || s == "renew"
	case "RoomKind":
		return s == "channel" || s == "dm"
	}
	return true
}

// All structure/discriminator checks finish before scalar predicates. Maps are
// confined to this parser; no generic payload escapes the typed wrapper API.
func shape(raw []byte, t reflect.Type, schema string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return failure("invalid_variant")
	}
	required, err := requiredFields(obj, t)
	if err != nil {
		return err
	}
	if len(obj) != required {
		return failure("invalid_variant")
	}
	optionalAbsent := required < t.NumField()
	for i := 0; i < t.NumField(); i++ {
		if err := shapeField(obj, t.Field(i), optionalAbsent, schema); err != nil {
			return err
		}
	}
	return nil
}

// requiredFields counts the keys t requires; only the provisional-close result
// has a variant (non-registered dispositions carry no binding).
func requiredFields(obj map[string]json.RawMessage, t reflect.Type) (int, error) {
	if t != reflect.TypeFor[ProvisionalCloseResult]() {
		return t.NumField(), nil
	}
	var disposition string
	if err := json.Unmarshal(obj["disposition"], &disposition); err != nil {
		return 0, failure("invalid_variant")
	}
	switch disposition {
	case "registered":
		return t.NumField(), nil
	case "unregistered", "retired_boot":
		if _, ok := obj["binding"]; ok {
			return 0, failure("invalid_variant")
		}
		return t.NumField() - 1, nil
	}
	return 0, failure("invalid_variant")
}

func shapeField(obj map[string]json.RawMessage, f reflect.StructField, optionalAbsent bool, schema string) error {
	v, ok := obj[strings.Split(f.Tag.Get("json"), ",")[0]]
	if !ok && f.Type.Kind() == reflect.Pointer && optionalAbsent {
		return nil
	}
	if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return failure("invalid_variant")
	}
	typ := f.Type
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Struct {
		return shape(v, typ, "")
	}
	if f.Name == "Schema" {
		var s string
		if json.Unmarshal(v, &s) != nil || s != schema {
			return failure("invalid_variant")
		}
	}
	if typ.Kind() == reflect.Int && string(v) != fixedInt(f.Name) {
		return failure("invalid_variant")
	}
	return nil
}

// fixedInt is the only accepted literal for a closed integer field.
func fixedInt(name string) string {
	if name == "Version" {
		return "1"
	}
	return "4"
}

func scalarFields(v reflect.Value) error {
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		x := v.Field(i)
		if x.Kind() == reflect.Pointer {
			if x.IsNil() {
				continue
			}
			x = x.Elem()
		}
		if x.Kind() == reflect.Struct {
			if err := scalarFields(x); err != nil {
				return err
			}
		} else if x.Kind() == reflect.String && !scalarOK(f.Name, x.String()) {
			return failure("invalid_value")
		} else if (f.Name == "OK" || f.Name == "TransportOK") && !x.Bool() {
			return failure("invalid_value")
		}
	}
	return nil
}
func decodeValue(raw []byte, dst any, schema string) error {
	if err := strictRaw(raw); err != nil {
		return err
	}
	if err := shape(raw, reflect.TypeOf(dst).Elem(), schema); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return failure("invalid_value")
	}
	if err := scalarFields(reflect.ValueOf(dst).Elem()); err != nil {
		return err
	}
	return healthBounds(reflect.ValueOf(dst).Elem())
}
func healthBounds(v reflect.Value) error {
	activation := v.FieldByName("Activation")
	if !activation.IsValid() {
		return nil
	}
	allowed := v.FieldByName("AdmissionAllowed")
	granted := v.FieldByName("AdmissionGranted")
	if activation.String() == "prepared" && ((allowed.IsValid() && allowed.Bool()) || (granted.IsValid() && granted.Bool())) {
		return failure("invalid_value")
	}
	if allowed.IsValid() && granted.IsValid() && granted.Bool() && !allowed.Bool() {
		return failure("invalid_value")
	}
	return nil
}
