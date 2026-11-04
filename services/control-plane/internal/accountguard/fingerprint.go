package accountguard

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

var (
	// ErrEncodingTooLong means a credential ID, or a ref or target count, does not fit the
	// uint16 length prefix of the fingerprint or guard-token encoding. Nothing is
	// truncated or wrapped: a prefix that lied about its length would let two different
	// inputs encode alike, so the work is refused instead.
	ErrEncodingTooLong = errors.New("accountguard: encoding too long")
	// ErrUnknownCredKind means a credential reference carries a kind outside decide's
	// set (CredUnknown, the zero value, included). Every encoder refuses it rather than
	// fingerprint or seal a kind no credential can have.
	ErrUnknownCredKind = errors.New("accountguard: unknown credential kind")
	// ErrDuplicateTarget means a request named the same credential (equal kind, ID and
	// version) twice. A target set is a set, so the request is refused rather than
	// fingerprinted as if the repeat were not there.
	ErrDuplicateTarget = errors.New("accountguard: duplicate target")
)

// The domain prefixes. Each is versioned and frozen, and the two differ so a digest made
// over one encoding can never equal a digest made over the other: changing one re-keys
// every fingerprint made under it. The trailing NUL ends the text.
const (
	originDomain  = "concord/accountguard/origin/v1\x00"
	requestDomain = "concord/accountguard/request/v1\x00"
)

const (
	// maxPrefixed is the largest length or count a uint16 prefix carries.
	maxPrefixed = math.MaxUint16
	// refSizeHint is a capacity guess for one encoded ref: the fixed 11 bytes plus a UUID.
	refSizeHint = 11 + 36
)

// validCredKind reports whether k is one of decide's credential kinds, CredPassword
// through CredRecoveryDevice. CredUnknown, the zero value, is not a kind.
// TestValidCredKind_MatchesDecideSpellings holds this range to decide's spelling table
// for every byte, so a kind added there fails that test until the bound here moves too.
func validCredKind(k decide.CredKind) bool {
	return decide.CredPassword <= k && k <= decide.CredRecoveryDevice
}

// checkCredKinds wraps ErrUnknownCredKind for the first ref whose kind is not valid.
func checkCredKinds(refs []decide.CredRef) error {
	for _, ref := range refs {
		if !validCredKind(ref.Kind) {
			return fmt.Errorf("%w: kind %d", ErrUnknownCredKind, ref.Kind)
		}
	}
	return nil
}

// appendRef appends the encoding of one credential reference to buf:
//
//	kind (1 byte) || len(ID) (uint16, big-endian) || ID || version (int64, big-endian)
//
// A negative version encodes as its two's-complement 8 bytes. A kind outside decide's
// set wraps ErrUnknownCredKind; an ID longer than a uint16 can count wraps
// ErrEncodingTooLong. On any error the returned slice is not meaningful. Every encoder
// that writes a ref goes through here, so this is where an unknown kind is refused.
func appendRef(buf []byte, ref decide.CredRef) ([]byte, error) {
	if !validCredKind(ref.Kind) {
		return nil, fmt.Errorf("encode credential reference: %w: kind %d", ErrUnknownCredKind, ref.Kind)
	}
	idLen := len(ref.ID)
	if idLen > maxPrefixed {
		return nil, fmt.Errorf("encode credential reference: %w: id of %d bytes", ErrEncodingTooLong, idLen)
	}
	buf = append(buf, byte(ref.Kind))
	buf = binary.BigEndian.AppendUint16(buf, uint16(idLen))
	buf = append(buf, ref.ID...)
	// binary.Append writes an int64 as its two's-complement bytes, so no integer
	// conversion is needed. Its error is nil for a fixed-size value like this one, so
	// the branch below is unreachable today; it is checked rather than ignored.
	buf, err := binary.Append(buf, binary.BigEndian, ref.Version)
	if err != nil {
		return nil, fmt.Errorf("encode credential reference version: %w", err)
	}
	return buf, nil
}

// OriginFingerprint returns the fingerprint of the credentials an action stood on:
// SHA-256 over
//
//	"concord/accountguard/origin/v1\x00" || ref...
//
// where each ref is encoded as
//
//	kind (1 byte) || len(ID) (uint16, big-endian) || ID || version (int64, big-endian)
//
// The refs are hashed in the order given and are not sorted: decide emits them in
// canonical order. The digest is unkeyed. A kind outside decide's set wraps
// ErrUnknownCredKind and an ID over 0xFFFF bytes wraps ErrEncodingTooLong; either way the
// result is the zero value.
func OriginFingerprint(origins []decide.CredRef) ([32]byte, error) {
	buf := make([]byte, 0, len(originDomain)+len(origins)*refSizeHint)
	buf = append(buf, originDomain...)
	for _, ref := range origins {
		var err error
		if buf, err = appendRef(buf, ref); err != nil {
			return [32]byte{}, fmt.Errorf("origin fingerprint: %w", err)
		}
	}
	return sha256.Sum256(buf), nil
}

// NewCandidate returns the request fingerprint of an opening action. The encoding E is
//
//	"concord/accountguard/request/v1\x00" || kind (1 byte) || count (uint16, big-endian)
//	    || ref... || resetFactors (1 byte, 0 or 1)
//
// with each ref encoded as in OriginFingerprint and the refs in canonical order: by kind,
// then by ID compared byte for byte, then by version. Coarse is SHA-256(E) and Keyed is
// HMAC-SHA256(E) under the ring's fingerprint key; each is 32 bytes, and KeyID is the
// registry id of the secret that made Keyed.
//
// The ring is asked for its key id first, so a ring that cannot name its key refuses
// the work before anything is computed (the error wraps ErrKeyUnavailable and the
// candidate is the zero value): the caller never receives a Coarse it could use unkeyed,
// and a ring fault is reported ahead of any fault in the input. Targets are sorted on a
// copy, so the caller's slice keeps its order. More than 0xFFFF targets, or an ID over
// 0xFFFF bytes, wraps ErrEncodingTooLong; a target of a kind outside decide's set wraps
// ErrUnknownCredKind, checked before duplicates so that two equal targets beside an
// unknown kind still report the kind; a target repeated with the same kind, ID and
// version wraps ErrDuplicateTarget.
func NewCandidate(ctx context.Context, keys *KeyRing, kind decide.ReqKind, targets []decide.CredRef, resetFactors bool) (decide.Candidate, error) {
	keyID, err := keys.CurrentKeyID(ctx)
	if err != nil {
		return decide.Candidate{}, fmt.Errorf("new candidate: %w", err)
	}
	key, err := keys.Key(LabelFingerprint)
	if err != nil {
		return decide.Candidate{}, fmt.Errorf("new candidate: %w", err)
	}

	count := len(targets)
	if count > maxPrefixed {
		return decide.Candidate{}, fmt.Errorf("new candidate: %w: %d targets", ErrEncodingTooLong, count)
	}
	if err := checkCredKinds(targets); err != nil {
		return decide.Candidate{}, fmt.Errorf("new candidate: %w", err)
	}
	sorted := slices.Clone(targets)
	slices.SortFunc(sorted, compareRefs)
	// After a total-order sort equal refs are adjacent.
	for i := 1; i < len(sorted); i++ {
		if compareRefs(sorted[i-1], sorted[i]) == 0 {
			return decide.Candidate{}, fmt.Errorf("new candidate: %w: kind %d", ErrDuplicateTarget, sorted[i].Kind)
		}
	}

	buf := make([]byte, 0, len(requestDomain)+4+len(sorted)*refSizeHint)
	buf = append(buf, requestDomain...)
	buf = append(buf, byte(kind))
	buf = binary.BigEndian.AppendUint16(buf, uint16(count))
	for _, ref := range sorted {
		if buf, err = appendRef(buf, ref); err != nil {
			return decide.Candidate{}, fmt.Errorf("new candidate: %w", err)
		}
	}
	if resetFactors {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}

	coarse := sha256.Sum256(buf)
	mac := hmac.New(sha256.New, key)
	mac.Write(buf) // hash.Hash.Write never returns an error
	return decide.Candidate{Keyed: mac.Sum(nil), Coarse: coarse[:], KeyID: keyID}, nil
}

// compareRefs orders credential references by kind, then by ID byte for byte, then by
// version.
func compareRefs(a, b decide.CredRef) int {
	return cmp.Or(
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.ID, b.ID),
		cmp.Compare(a.Version, b.Version),
	)
}
