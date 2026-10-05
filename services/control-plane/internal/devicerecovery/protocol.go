// Package devicerecovery implements the immutable trusted-device recovery v2 contract.
package devicerecovery

import (
	"bytes"
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/net/idna"
)

// Version is the only accepted trusted-device protocol.
const Version = 2

// MaxBody caps the complete JSON request in bytes.
const MaxBody = 16 * 1024

// UpdateMessage tells legacy clients to update both devices and restart.
const UpdateMessage = "Trusted recovery requires protocol v2. Update both devices and restart recovery."

// ErrInvalid rejects noncanonical protocol material.
var ErrInvalid = errors.New("invalid recovery context")

// Context is fixed at creation. All binary wire fields are canonical padded base64.
type Context struct {
	RequestID            string `json:"request_id"`
	ProtocolVersion      int    `json:"protocol_version"`
	ServerOrigin         string `json:"server_origin"`
	AccountBinding       string `json:"account_binding"`
	ExpiresAt            int64  `json:"expires_at"`
	RequesterNonce       string `json:"requester_nonce"`
	RequesterPublicKey   string `json:"requester_public_key"`
	RecoveryTokenJTIHash string `json:"recovery_token_jti_hash"`
}

// Offer contains the immutable responder fields added by the first offer.
type Offer struct {
	ResponderPublicKey string `json:"responder_public_key"`
	ResponderNonce     string `json:"responder_nonce"`
	TranscriptHash     string `json:"transcript_hash"`
}

// CreateBody is the exact requester creation body.
type CreateBody struct {
	ProtocolVersion    int    `json:"protocol_version"`
	RecoveryToken      string `json:"recovery_token"`
	ServerOrigin       string `json:"server_origin"`
	AccountBinding     string `json:"account_binding"`
	RequesterNonce     string `json:"requester_nonce"`
	RequesterPublicKey string `json:"requester_public_key"`
}

// RespondBody carries action-specific fields; ParseRespond enforces the exclusive shape.
type RespondBody struct {
	Action             string `json:"action"`
	ProtocolVersion    int    `json:"protocol_version"`
	ResponderPublicKey string `json:"responder_public_key,omitempty"`
	ResponderNonce     string `json:"responder_nonce,omitempty"`
	TranscriptHash     string `json:"transcript_hash,omitempty"`
	EncryptedPayload   string `json:"encrypted_payload,omitempty"`
}

// CompleteBody acknowledges import of the exact approved transcript.
type CompleteBody struct {
	ProtocolVersion int    `json:"protocol_version"`
	TranscriptHash  string `json:"transcript_hash"`
}

// ResponseResult acknowledges a single authoritative transition.
type ResponseResult struct {
	RequestID       string `json:"request_id"`
	ProtocolVersion int    `json:"protocol_version"`
	Status          string `json:"status"`
}

// CanonicalUUID recognizes lowercase UUIDs in their full canonical representation.
func CanonicalUUID(s string) bool { id, err := uuid.Parse(s); return err == nil && id.String() == s }

// Hash returns the SHA-256 digest of UTF-8 protocol input.
func Hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// AccountBinding binds the ceremony to the canonical account UUID.
func AccountBinding(userID string) string {
	return Encode(Hash("concord-trusted-device-recovery/account/v2\x00" + userID))
}

// Encode produces canonical padded standard base64.
func Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Decode requires canonical base64 of an exact binary size.
func Decode(s string, size int) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != size || Encode(b) != s {
		return nil, ErrInvalid
	}
	return b, nil
}

// PublicKey validates an uncompressed P-384 point using the standard library.
func PublicKey(s string) ([]byte, error) {
	b, err := Decode(s, 97)
	if err != nil {
		return nil, err
	}
	if _, err := ecdh.P384().NewPublicKey(b); err != nil {
		return nil, ErrInvalid
	}
	return b, nil
}

// Envelope accepts only bounded v2 ciphertext envelopes.
func Envelope(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) < 30 || len(b) > 8192 || b[0] != 2 || Encode(b) != s {
		return nil, ErrInvalid
	}
	return b, nil
}

// Equal compares protocol bindings in constant time.
func Equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// URL hosts use nontransitional UTS #46 with WHATWG's relaxed ASCII, hyphen,
// and DNS-length checks. Validate decoded A-labels, context joiners, and Bidi.
var originIDNA = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.VerifyDNSLength(false),
	idna.BidiRule(),
)

// Origin accepts only already-normalized ASCII HTTP(S) origins; it is context, never a destination.
func Origin(s string) bool {
	if !validOriginText(s) {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Opaque != "" {
		return false
	}
	return validOriginHost(u.Hostname(), u.Host) && validOriginPort(u.Port(), u.Scheme) && u.Scheme+"://"+u.Host == s
}

func validOriginText(s string) bool {
	if len(s) == 0 || len(s) > 512 {
		return false
	}
	for _, c := range s {
		if c >= 127 || c <= 32 {
			return false
		}
	}
	return true
}

func validOriginHost(host, authority string) bool {
	if host == "" || host != strings.ToLower(host) || strings.HasSuffix(authority, ":") {
		return false
	}
	if strings.Contains(host, ":") {
		ip := net.ParseIP(host)
		return ip != nil && canonicalIPv6(ip) == host
	}
	return validOriginDomain(host)
}

func validOriginDomain(host string) bool {
	for _, c := range host {
		if strings.ContainsRune("#%/:<>?@[\\]^|", c) {
			return false
		}
	}
	ascii, err := originIDNA.ToASCII(host)
	if err != nil || ascii != host {
		return false
	}
	// WHATWG normalizes numeric-ending hosts as IPv4, including shorthand forms.
	numericHost := strings.TrimSuffix(host, ".")
	tail := numericHost[strings.LastIndex(numericHost, ".")+1:]
	_, numeric := strconv.ParseUint(tail, 0, 32)
	if numeric == nil || allDigits(tail) {
		ip := net.ParseIP(host)
		return ip != nil && ip.To4() != nil && ip.String() == host
	}
	return true
}

func validOriginPort(port, scheme string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != port {
		return false
	}
	return (scheme != "http" || n != 80) && (scheme != "https" || n != 443)
}

// canonicalIPv6 uses WHATWG's hexadecimal serialization, including IPv4-mapped
// addresses that net.IP.String otherwise formats as dotted IPv4.
func canonicalIPv6(ip net.IP) string {
	b := ip.To16()
	groups := make([]string, 8)
	start, length := -1, 0
	for i := 0; i < 8; i++ {
		n := uint64(b[2*i])<<8 | uint64(b[2*i+1])
		groups[i] = strconv.FormatUint(n, 16)
		if n == 0 {
			end := i
			for end < 8 && b[2*end] == 0 && b[2*end+1] == 0 {
				end++
			}
			if end-i > length {
				start, length = i, end-i
			}
		}
	}
	if length < 2 {
		return strings.Join(groups, ":")
	}
	return strings.Join(groups[:start], ":") + "::" + strings.Join(groups[start+length:], ":")
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Validate rejects malformed immutable requester context.
func (c Context) Validate() error {
	if c.ProtocolVersion != Version || !CanonicalUUID(c.RequestID) || !Origin(c.ServerOrigin) || c.ExpiresAt <= 0 {
		return ErrInvalid
	}
	for _, s := range []string{c.AccountBinding, c.RequesterNonce, c.RecoveryTokenJTIHash} {
		if _, err := Decode(s, 32); err != nil {
			return err
		}
	}
	_, err := PublicKey(c.RequesterPublicKey)
	return err
}

// Transcript returns the canonical array bytes and their SHA-256 digest.
func Transcript(c Context, o Offer) ([]byte, []byte, error) {
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	if _, err := PublicKey(o.ResponderPublicKey); err != nil {
		return nil, nil, err
	}
	if _, err := Decode(o.ResponderNonce, 32); err != nil {
		return nil, nil, err
	}
	// ASCII canonical fields make Go's HTML escaping irrelevant; escaping is disabled explicitly.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode([]any{"concord-trusted-device-recovery", 2, c.ServerOrigin, c.RequestID, c.AccountBinding, c.ExpiresAt, c.RequesterNonce, o.ResponderNonce, c.RecoveryTokenJTIHash, "requester", c.RequesterPublicKey, "responder", o.ResponderPublicKey}); err != nil {
		return nil, nil, fmt.Errorf("serialize transcript: %w", err)
	}
	b := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	h := sha256.Sum256(b)
	return b, h[:], nil
}
