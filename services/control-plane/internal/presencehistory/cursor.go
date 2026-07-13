// Package presencehistory implements the private, self-only Activity History
// domain and persistence boundary.
package presencehistory

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	maxEncodedCursorBytes = 512
	maxDecodedCursorBytes = 256
)

// ErrInvalidCursor identifies every client-supplied cursor validation failure
// without including the opaque cursor value in an error or log.
var ErrInvalidCursor = errors.New("invalid activity history cursor")

// PageCursor is the versioned keyset boundary encoded into the public opaque
// cursor string.
type PageCursor struct {
	Version    int       `json:"v"`
	RecordedAt time.Time `json:"recorded_at"`
	ID         uuid.UUID `json:"id"`
}

type cursorWire struct {
	Version    int    `json:"v"`
	RecordedAt string `json:"recorded_at"`
	ID         string `json:"id"`
}

// EncodeCursor returns an unpadded base64url cursor.
func EncodeCursor(cursor PageCursor) (string, error) {
	if cursor.Version != 1 || cursor.RecordedAt.IsZero() || cursor.ID == uuid.Nil {
		return "", ErrInvalidCursor
	}
	cursor.RecordedAt = cursor.RecordedAt.UTC()
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("%w: encode", ErrInvalidCursor)
	}
	if len(raw) > maxDecodedCursorBytes {
		return "", fmt.Errorf("%w: decoded size", ErrInvalidCursor)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > maxEncodedCursorBytes {
		return "", fmt.Errorf("%w: encoded size", ErrInvalidCursor)
	}
	return encoded, nil
}

// DecodeCursor validates and decodes an untrusted opaque cursor.
func DecodeCursor(encoded string) (PageCursor, error) {
	if encoded == "" || len(encoded) > maxEncodedCursorBytes || !rawBase64URL(encoded) {
		return PageCursor{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > maxDecodedCursorBytes {
		return PageCursor{}, ErrInvalidCursor
	}

	fields, err := decodeExactJSONObject(raw, "v", "recorded_at", "id")
	if err != nil || len(fields) != 3 {
		return PageCursor{}, ErrInvalidCursor
	}
	var version *int
	var recordedAtRaw *string
	var idRaw *string
	if json.Unmarshal(fields["v"], &version) != nil || version == nil ||
		json.Unmarshal(fields["recorded_at"], &recordedAtRaw) != nil || recordedAtRaw == nil ||
		json.Unmarshal(fields["id"], &idRaw) != nil || idRaw == nil {
		return PageCursor{}, ErrInvalidCursor
	}
	wire := cursorWire{Version: *version, RecordedAt: *recordedAtRaw, ID: *idRaw}
	if wire.Version != 1 || wire.RecordedAt == "" || wire.ID == "" ||
		!strings.HasSuffix(wire.RecordedAt, "Z") || strings.Contains(wire.RecordedAt, ",") {
		return PageCursor{}, ErrInvalidCursor
	}

	recordedAt, err := time.Parse(time.RFC3339Nano, wire.RecordedAt)
	if err != nil || recordedAt.IsZero() || recordedAt.Location() != time.UTC {
		return PageCursor{}, ErrInvalidCursor
	}
	id, err := uuid.Parse(wire.ID)
	if err != nil || id == uuid.Nil || id.String() != wire.ID {
		return PageCursor{}, ErrInvalidCursor
	}
	return PageCursor{Version: 1, RecordedAt: recordedAt, ID: id}, nil
}

func rawBase64URL(value string) bool {
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}
