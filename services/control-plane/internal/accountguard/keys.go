package accountguard

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"
)

// keyLen is the length of every derived key and of the registry's key_check digest
// (accountguard_hmac_keys.key_check is CHECKed to 32 bytes).
const keyLen = 32

var (
	// ErrKeyUnavailable means no key can be derived or resolved: the input secret is
	// empty, or the registry could not name its key. It fails closed: a caller must
	// refuse the work, never fall back to an unkeyed value.
	ErrKeyUnavailable = errors.New("accountguard: key unavailable")
	// ErrUnknownLabel means a Label outside the closed set was asked for a key.
	ErrUnknownLabel = errors.New("accountguard: unknown key label")
)

// Label names one purpose a derived key may serve. Every purpose derives its own key
// from the one input secret, so a key made for one purpose is useless for another. The
// set is closed: adding a label is a deliberate edit here and in the test that pins it.
// The zero value is not a label, so a Label that was never set derives nothing.
type Label uint8

// The seven labels, one per purpose a key serves.
const (
	LabelIdentifier  Label = iota + 1 // keyed digest of a normalized identifier
	LabelFingerprint                  // request and origin fingerprints
	LabelGuardToken                   // guard tokens
	LabelChallenge                    // challenge values
	LabelAddress                      // address digests
	LabelJob                          // runner job identity
	LabelKeyCheck                     // the registry's key_check digest
)

// info returns the HKDF info string for the label, or "" for any value outside the
// closed set. The strings are versioned and frozen: changing one re-keys every
// digest made under it.
func (l Label) info() string {
	switch l {
	case LabelIdentifier:
		return "concord/accountguard/identifier/v1"
	case LabelFingerprint:
		return "concord/accountguard/fingerprint/v1"
	case LabelGuardToken:
		return "concord/accountguard/guard-token/v1"
	case LabelChallenge:
		return "concord/accountguard/challenge/v1"
	case LabelAddress:
		return "concord/accountguard/address/v1"
	case LabelJob:
		return "concord/accountguard/job/v1"
	case LabelKeyCheck:
		return "concord/accountguard/key-check/v1"
	default:
		return ""
	}
}

// DeriveKey derives the 32-byte key for label l from secret with HKDF-SHA256 (nil
// salt, the label's info string). An empty secret wraps ErrKeyUnavailable; a label
// outside the closed set wraps ErrUnknownLabel. The returned slice is the caller's.
func DeriveKey(secret string, l Label) ([]byte, error) {
	if secret == "" {
		return nil, fmt.Errorf("derive key: %w: empty secret", ErrKeyUnavailable)
	}
	info := l.info()
	if info == "" {
		return nil, fmt.Errorf("derive key: %w: %d", ErrUnknownLabel, uint8(l))
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, info, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	return key, nil
}

const (
	selectKeyIDSQL = `SELECT id FROM accountguard_hmac_keys WHERE key_check = $1`
	insertKeySQL   = `INSERT INTO accountguard_hmac_keys (key_check) VALUES ($1) ON CONFLICT (key_check) DO NOTHING`
)

// redactedRing is what formatting a KeyRing prints, whatever the verb.
const redactedRing = "accountguard.KeyRing{secret:redacted}"

// KeyRing derives the account guard's keys from one input secret and resolves the id
// that names that secret in accountguard_hmac_keys. The id is what a stored
// fingerprint carries (fingerprint_key_id), so a rotated secret reads as a different
// key rather than as a corrupt value. The raw secret never reaches the database: the
// registry stores only DeriveKey(secret, LabelKeyCheck).
//
// All state sits behind one pointer, so a KeyRing copied by value shares the original's
// cache and the mutex is never copied (a value-receiver method on a struct holding the
// mutex would trip vet's copylocks). Format redacts the ring for every fmt verb, by
// pointer or by value. Format rather than String and GoString: those cover only the
// verbs fmt routes to them, and any other verb (%t, %c, %e, %f, %O, %U) is a bad verb
// for the inner pointer, which fmt then dereferences and prints in full.
type KeyRing struct {
	st *ringState
}

// ringState is a KeyRing's secret, pool and cached id.
type ringState struct {
	secret string
	db     *sql.DB

	mu sync.Mutex
	id int16
	ok bool
}

// NewKeyRing returns a ring over secret that resolves its key id through db.
func NewKeyRing(db *sql.DB, secret string) *KeyRing {
	return &KeyRing{st: &ringState{secret: secret, db: db}}
}

// Format implements fmt.Formatter. fmt hands every verb to a Formatter ahead of any
// other method, so no verb can reach the state behind the pointer. The write error is
// dropped on purpose: fmt.State has no way to report one back to the caller.
func (KeyRing) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redactedRing)
}

// state returns the ring's state, or nil for a nil or zero KeyRing.
func (k *KeyRing) state() *ringState {
	if k == nil {
		return nil
	}
	return k.st
}

// Key derives the key for label l from the ring's secret. It never touches the
// database. A nil or zero ring derives nothing and wraps ErrKeyUnavailable.
func (k *KeyRing) Key(l Label) ([]byte, error) {
	st := k.state()
	if st == nil {
		return nil, fmt.Errorf("derive key: %w: no ring", ErrKeyUnavailable)
	}
	return DeriveKey(st.secret, l)
}

// CurrentKeyID returns the registry id of the ring's secret, registering the secret
// on first use. It looks the secret up first and inserts only when it is absent, so
// resolving an already-registered secret consumes no identity value; two rings that
// race to register the same secret converge on one row. A resolved id is cached for
// the ring's lifetime; a failure is returned and never cached, so the next call
// tries again. A registry read or write that fails wraps ErrKeyUnavailable together
// with its cause, so a caller refuses keyed work. The driver error stays reachable
// through errors.Is and errors.As but is withheld from the text (withheld), so no
// error this or any helper built on it returns prints a driver message.
func (k *KeyRing) CurrentKeyID(ctx context.Context) (int16, error) {
	st := k.state()
	if st == nil {
		return 0, fmt.Errorf("resolve key id: %w: no ring", ErrKeyUnavailable)
	}

	st.mu.Lock()
	if st.ok {
		id := st.id
		st.mu.Unlock()
		return id, nil
	}
	st.mu.Unlock()

	// Resolve without holding the mutex across database I/O. Concurrent resolves on
	// one ring are idempotent (same check digest, same row), so the duplicate work
	// is harmless and no caller waits on another caller's context.
	id, err := st.resolve(ctx)
	if err != nil {
		return 0, err
	}

	st.mu.Lock()
	st.id, st.ok = id, true
	st.mu.Unlock()
	return id, nil
}

// resolve looks the ring's check digest up, registering it when absent.
func (s *ringState) resolve(ctx context.Context) (int16, error) {
	check, err := DeriveKey(s.secret, LabelKeyCheck)
	if err != nil {
		return 0, fmt.Errorf("resolve key id: %w", err)
	}
	if s.db == nil {
		return 0, fmt.Errorf("resolve key id: %w: no database", ErrKeyUnavailable)
	}

	id, found, err := s.lookup(ctx, check)
	if err != nil {
		return 0, err
	}
	if found {
		return id, nil
	}

	if _, err := s.db.ExecContext(ctx, insertKeySQL, check); err != nil {
		return 0, fmt.Errorf("register key id: %w: %w", ErrKeyUnavailable, withhold(err))
	}
	// DO NOTHING returns no row, and a racing registration may have won, so read the
	// row back whichever ring wrote it.
	id, found, err = s.lookup(ctx, check)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("resolve key id: %w: registration not visible", ErrKeyUnavailable)
	}
	return id, nil
}

// lookup reads the id registered for check; found is false when none is.
func (s *ringState) lookup(ctx context.Context, check []byte) (int16, bool, error) {
	var id int16
	err := s.db.QueryRowContext(ctx, selectKeyIDSQL, check).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look up key id: %w: %w", ErrKeyUnavailable, withhold(err))
	}
	return id, true, nil
}
