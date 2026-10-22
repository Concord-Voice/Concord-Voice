package managedredis

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
)

// SecretScope is the complete selector for one named, shared provider secret.
type SecretScope struct {
	Project     string `json:"projectId"`
	Environment string `json:"environment"`
	Path        string `json:"path"`
	Name        string `json:"name"`
}

// Credential is one bounded application identity for an exact secret scope.
type Credential struct {
	Version    int         `json:"version"`
	Scope      SecretScope `json:"scope"`
	RotationID string      `json:"rotationId"`
	Service    string      `json:"service"`
	Generation uint64      `json:"generation"`
	Username   string      `json:"username"`
	Password   string      `json:"password"`
	ValidFrom  time.Time   `json:"validFrom"`
	ValidUntil time.Time   `json:"validUntil"`
}

var (
	aclUsername = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	rotationID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	errInvalid  = errors.New("invalid managed Redis credential")
)

// decodeExact checks both unknown and repeated fields; encoding/json otherwise
// accepts duplicate keys, including duplicates nested inside the scope object.
func decodeExact(raw []byte, dst any) error {
	if len(raw) == 0 || len(raw) > 16*1024 {
		return errInvalid
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateKeys(check); err != nil {
		return errInvalid
	}
	if _, err := check.Token(); !errors.Is(err, io.EOF) {
		return errInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errInvalid
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errInvalid
	}
	return nil
}

// DecodeBoundedJSON rejects duplicate and unknown fields without returning input data.
func DecodeBoundedJSON(raw []byte, dst any) error { return decodeExact(raw, dst) }

func rejectDuplicateKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := make(map[string]struct{})
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errInvalid
			}
			if _, exists := seen[name]; exists {
				return errInvalid
			}
			seen[name] = struct{}{}
			if err := rejectDuplicateKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := rejectDuplicateKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return nil
	}
}

// ParseCredential validates one scoped generation and its validity window.
func ParseCredential(raw []byte, expectedScope SecretScope, expectedService string, now time.Time) (Credential, error) {
	var value Credential
	if err := decodeExact(raw, &value); err != nil ||
		value.Version != 1 || value.Scope != expectedScope || !validScope(expectedScope) ||
		value.Service != expectedService || (value.Service != "control-plane" && value.Service != "media-plane") ||
		value.Generation == 0 || value.Generation > maxJSONSafeGeneration || value.Username != canonicalUsername(value.Service, value.Generation) ||
		!rotationID.MatchString(value.RotationID) || value.Password == "" ||
		value.ValidFrom.IsZero() || value.ValidUntil.IsZero() ||
		!value.ValidUntil.After(value.ValidFrom) || now.Before(value.ValidFrom) || !now.Before(value.ValidUntil) {
		return Credential{}, errInvalid
	}
	return value, nil
}

func validScope(scope SecretScope) bool {
	return scope.Project != "" && scope.Environment != "" && scope.Path != "" &&
		scope.Path[0] == '/' && scope.Name != ""
}
