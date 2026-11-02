package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Secure Enclave session binding (ADR-0050). A bound refresh token must be
// presented with a DPoP-shaped proof: a compact ES256 JWS whose header carries
// the device's P-256 public key and whose claims tie it to this request and,
// on refresh, to this exact refresh token. The wire contract is frozen with the
// mobile client; see docs/api/openapi.yaml (DPoP header) for the shape.
const (
	dpopHeader    = "DPoP"
	dpopTyp       = "dpop+jwt"
	dpopAlgES256  = "ES256"
	dpopMaxLen    = 2048
	dpopMaxJTILen = 64
	dpopMaxSkew   = 60 * time.Second
	dpopJTITTL    = 5 * time.Minute

	errCodePoPRequired  = "refresh_pop_required"
	errCodePoPInvalid   = "refresh_pop_invalid"
	errCodePoPStaleIAT  = "refresh_pop_stale_iat"
	errCodePoPReplayed  = "refresh_pop_replayed"
	errMsgSessionProof  = "Session proof rejected"
	tokenTypeDPoP       = "DPoP"
	tokenTypeBearer     = "Bearer"
	dpopJTIRedisPrefix  = "dpop_jti:"
	errMsgProofInternal = "Session proof check failed"
)

// popError is a refused proof. Its code is the client-facing 401 body code.
type popError struct {
	code       string
	serverTime int64 // set for errCodePoPStaleIAT only
}

func (e *popError) Error() string { return e.code }

var (
	errPoPRequired = &popError{code: errCodePoPRequired}
	errPoPInvalid  = &popError{code: errCodePoPInvalid}
	errPoPReplayed = &popError{code: errCodePoPReplayed}
)

type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type dpopJOSE struct {
	Typ string   `json:"typ"`
	Alg string   `json:"alg"`
	JWK *dpopJWK `json:"jwk"`
}

type dpopClaims struct {
	JTI string  `json:"jti"`
	HTM string  `json:"htm"`
	HTU string  `json:"htu"`
	IAT *int64  `json:"iat"`
	RTH *string `json:"rth"`
}

var b64 = base64.RawURLEncoding.Strict()

// decodeStrictJSON refuses unknown members, so a private "d" in the JWK, a
// "kid", a "crit" or any claim outside the contract is a rejection, not noise.
func decodeStrictJSON(segment string, v any) error {
	raw, err := b64.DecodeString(segment)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// verifyDPoP checks a proof against the request it arrived on and returns the
// key thumbprint (jkt) and the jti. rthDigest is the SHA-256 of the presented
// refresh token, or nil where no refresh token is presented (login), in which
// case the proof must not carry rth. It does not consume the jti.
func verifyDPoP(proof, method, path string, rthDigest []byte, now time.Time) (jkt, jti string, err error) {
	if len(proof) > dpopMaxLen {
		return "", "", errPoPInvalid
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return "", "", errPoPInvalid
	}
	pub, jkt, ok := parseDPoPHeader(parts[0])
	if !ok {
		return "", "", errPoPInvalid
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return "", "", errPoPInvalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return "", "", errPoPInvalid
	}

	var cl dpopClaims
	if decodeStrictJSON(parts[1], &cl) != nil || !dpopClaimsMatch(&cl, method, path, rthDigest) {
		return "", "", errPoPInvalid
	}
	// Skew is checked last, so a stale answer is only ever given to a proof
	// that is otherwise valid: the server_time it carries helps the device's
	// clock, not a forger probing the verifier.
	if skew := now.Sub(time.Unix(*cl.IAT, 0)); skew > dpopMaxSkew || skew < -dpopMaxSkew {
		return "", "", &popError{code: errCodePoPStaleIAT, serverTime: now.Unix()}
	}
	return jkt, cl.JTI, nil
}

// parseDPoPHeader decodes the protected header and returns the embedded P-256
// public key with its RFC 7638 thumbprint.
func parseDPoPHeader(segment string) (*ecdsa.PublicKey, string, bool) {
	var jose dpopJOSE
	if decodeStrictJSON(segment, &jose) != nil || jose.Typ != dpopTyp || jose.Alg != dpopAlgES256 || jose.JWK == nil {
		return nil, "", false
	}
	k := jose.JWK
	if k.Kty != "EC" || k.Crv != "P-256" {
		return nil, "", false
	}
	x, xErr := b64.DecodeString(k.X)
	y, yErr := b64.DecodeString(k.Y)
	if xErr != nil || yErr != nil || len(x) != 32 || len(y) != 32 {
		return nil, "", false
	}
	// ParseUncompressedPublicKey rejects points that are not on the curve.
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return nil, "", false
	}
	// The thumbprint covers the required members in lexical order, from the
	// re-encoded coordinates so one key has exactly one thumbprint.
	thumb := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + b64.EncodeToString(x) + `","y":"` + b64.EncodeToString(y) + `"}`))
	return pub, b64.EncodeToString(thumb[:]), true
}

// dpopClaimsMatch checks every claim except iat skew against the request.
func dpopClaimsMatch(cl *dpopClaims, method, path string, rthDigest []byte) bool {
	if cl.IAT == nil || cl.JTI == "" || len(cl.JTI) > dpopMaxJTILen || cl.HTM != method || !dpopHTUMatches(cl.HTU, path) {
		return false
	}
	if rthDigest == nil {
		return cl.RTH == nil
	}
	return cl.RTH != nil && subtle.ConstantTimeCompare([]byte(*cl.RTH), []byte(b64.EncodeToString(rthDigest))) == 1
}

// dpopHTUMatches accepts an absolute http(s) URL without query or fragment
// whose path is the request path. Only the path is compared (ADR-0050 item 3):
// the key is per server, so the origin adds nothing a proxy could not break.
func dpopHTUMatches(htu, path string) bool {
	u, err := url.Parse(htu)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return false
	}
	return u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Path == path
}

// consumeDPoPJTI records the jti as used for its acceptance window. Keyed by
// thumbprint too, so one device cannot burn another's jti values.
func (h *Handler) consumeDPoPJTI(ctx context.Context, jkt, jti string) error {
	key := sha256.Sum256([]byte(jkt + "\x00" + jti))
	set, err := h.redis.SetNX(ctx, dpopJTIRedisPrefix+hex.EncodeToString(key[:]), 1, dpopJTITTL).Result()
	if err != nil {
		return err
	}
	if !set {
		return errPoPReplayed
	}
	return nil
}

// sessionProof checks the request's DPoP header for a refresh token row bound
// to boundJKT ("" when unbound) and returns the thumbprint the successor row
// must carry. It writes the response and returns ok=false on refusal.
//
// A bound row always needs a valid proof by its own key, whatever the
// SESSION_BINDING_ENABLED setting: turning the setting off stops new binding,
// never enforcement. An unbound row ignores a proof while binding is off.
func (h *Handler) sessionProof(c *gin.Context, boundJKT string, rthDigest []byte) (jkt string, ok bool) {
	header := c.GetHeader(dpopHeader)
	if header == "" {
		if boundJKT != "" {
			writePoPError(c, errPoPRequired)
			return "", false
		}
		return "", true
	}
	if boundJKT == "" && !h.sessionBinding {
		return "", true
	}
	jkt, jti, err := verifyDPoP(header, c.Request.Method, c.Request.URL.Path, rthDigest, time.Now())
	if err == nil && boundJKT != "" && subtle.ConstantTimeCompare([]byte(jkt), []byte(boundJKT)) != 1 {
		err = errPoPInvalid
	}
	if err == nil {
		err = h.consumeDPoPJTI(c.Request.Context(), jkt, jti)
	}
	if err != nil {
		writePoPError(c, err)
		return "", false
	}
	return jkt, true
}

// writePoPError answers a refused proof. A refusal never revokes anything and
// never logs the proof, its thumbprint or rth (AC14).
func writePoPError(c *gin.Context, err error) {
	var pe *popError
	if !errors.As(err, &pe) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgProofInternal})
		return
	}
	body := gin.H{"error": errMsgSessionProof, "code": pe.code}
	if pe.code == errCodePoPStaleIAT {
		body["server_time"] = pe.serverTime
	}
	c.JSON(http.StatusUnauthorized, body)
}

// popColumns maps a thumbprint to the (pop_jkt, pop_alg) insert arguments.
func popColumns(jkt string) (any, any) {
	if jkt == "" {
		return nil, nil
	}
	return jkt, dpopAlgES256
}

// tokenTypeFor names the token type of a minted session for the response.
func tokenTypeFor(jkt string) string {
	if jkt == "" {
		return tokenTypeBearer
	}
	return tokenTypeDPoP
}
