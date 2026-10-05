package devicerecovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// APIError carries only public, fixed outcome text.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }
func invalidBody() error          { return &APIError{http.StatusBadRequest, "Invalid recovery request body"} }

// object reads the complete capped body before semantic decoding. Gin caches the
// full read; json.Valid rejects trailing documents, and tokens detect duplicates.
func object(c *gin.Context) (map[string]json.RawMessage, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBody)
	var first json.RawMessage
	bindErr := c.ShouldBindBodyWithJSON(&first)
	var tooLarge *http.MaxBytesError
	if errors.As(bindErr, &tooLarge) {
		return nil, &APIError{http.StatusRequestEntityTooLarge, "Recovery request body exceeds 16 KiB"}
	}
	cached, ok := c.Get(gin.BodyBytesKey)
	b, bytesOK := cached.([]byte)
	if bindErr != nil || !ok || !bytesOK || !json.Valid(b) {
		return nil, invalidBody()
	}
	d := json.NewDecoder(bytes.NewReader(b))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, invalidBody()
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, invalidBody()
		}
		name, ok := tok.(string)
		if !ok {
			return nil, invalidBody()
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, invalidBody()
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, invalidBody()
		}
		fields[name] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, invalidBody()
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidBody()
	}
	return fields, nil
}
func exact(fields map[string]json.RawMessage, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}
func version(fields map[string]json.RawMessage) error {
	var v int
	if err := json.Unmarshal(fields["protocol_version"], &v); err != nil || v != Version {
		return &APIError{http.StatusBadRequest, UpdateMessage}
	}
	return nil
}

// ParseCreate validates the complete exact v2 creation body.
func ParseCreate(c *gin.Context) (CreateBody, error) {
	var r CreateBody
	f, err := object(c)
	if err != nil {
		return r, err
	}
	if err := version(f); err != nil {
		return r, err
	}
	if !exact(f, "protocol_version", "recovery_token", "server_origin", "account_binding", "requester_nonce", "requester_public_key") {
		return r, invalidBody()
	}
	b, err := json.Marshal(f)
	if err != nil {
		return r, invalidBody()
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, invalidBody()
	}
	if r.RecoveryToken == "" || len(r.RecoveryToken) > 4096 || !Origin(r.ServerOrigin) {
		return r, invalidBody()
	}
	for _, v := range []string{r.AccountBinding, r.RequesterNonce} {
		if _, err := Decode(v, 32); err != nil {
			return r, invalidBody()
		}
	}
	if _, err := PublicKey(r.RequesterPublicKey); err != nil {
		return r, invalidBody()
	}
	return r, nil
}

// ParseRespond validates the mutually exclusive offer, approve, and reject bodies.
func ParseRespond(c *gin.Context) (RespondBody, error) {
	var r RespondBody
	f, err := object(c)
	if err != nil {
		return r, err
	}
	if err := version(f); err != nil {
		return r, err
	}
	if err := json.Unmarshal(f["action"], &r.Action); err != nil {
		return r, invalidBody()
	}
	names := []string{"action", "protocol_version"}
	switch r.Action {
	case "offer":
		names = append(names, "responder_public_key", "responder_nonce", "transcript_hash")
	case "approve":
		names = append(names, "transcript_hash", "encrypted_payload")
	case "reject":
	default:
		return r, invalidBody()
	}
	if !exact(f, names...) {
		return r, invalidBody()
	}
	b, err := json.Marshal(f)
	if err != nil {
		return r, invalidBody()
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, invalidBody()
	}
	if r.Action != "reject" {
		if _, err := Decode(r.TranscriptHash, 32); err != nil {
			return r, invalidBody()
		}
	}
	if r.Action == "offer" {
		if _, err := PublicKey(r.ResponderPublicKey); err != nil {
			return r, invalidBody()
		}
		if _, err := Decode(r.ResponderNonce, 32); err != nil {
			return r, invalidBody()
		}
	}
	if r.Action == "approve" {
		if _, err := Envelope(r.EncryptedPayload); err != nil {
			return r, invalidBody()
		}
	}
	return r, nil
}

// ParseComplete validates the exact requester completion body.
func ParseComplete(c *gin.Context) (CompleteBody, error) {
	var r CompleteBody
	f, err := object(c)
	if err != nil {
		return r, err
	}
	if err := version(f); err != nil {
		return r, err
	}
	if !exact(f, "protocol_version", "transcript_hash") {
		return r, invalidBody()
	}
	b, err := json.Marshal(f)
	if err != nil {
		return r, invalidBody()
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, invalidBody()
	}
	if _, err := Decode(r.TranscriptHash, 32); err != nil {
		return r, invalidBody()
	}
	return r, nil
}

// WriteError emits sanitized fixed text and the prescribed HTTP outcome.
func WriteError(c *gin.Context, err error) {
	status, message := http.StatusInternalServerError, "Recovery is unavailable. Try again."
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		status, message = apiErr.Status, apiErr.Message
	}
	c.JSON(status, gin.H{"error": message})
}
