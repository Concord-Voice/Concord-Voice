// Package permgen spells the permission-cache generation keys (#3453), so the
// cache that writes them (internal/rbac) and the step-up grace store that
// reads them (internal/stepup, #3454) cannot disagree about a key. rbac
// imports stepup, so stepup cannot import rbac; this leaf is the one place
// both can reach. See rbac.PermissionCache for what a generation means and
// who bumps it.
//
// It imports only the standard library, and nothing may make it import more:
// it sits below both of its consumers. It also owns how a generation is made
// and how long it lives (New, TTL), so a generation the grace store seeds is
// indistinguishable from one the cache seeds.
package permgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	userPrefix   = "permgen:u:"
	serverPrefix = "permgen:s:"

	// TTL bounds how long an idle subject's generation lives. Every writer
	// that seeds or renews a generation uses it: the permission cache, and the
	// step-up grace store when it seeds a generation the cache never has.
	TTL = 24 * time.Hour

	// generationBytes is 64 bits, encoded as 16 hex characters. Only two
	// consecutive generations of one subject need to differ, so wider values
	// would spend memory on every perm:* value for nothing (#3453 spec L3).
	generationBytes = 8
)

// New returns a fresh random generation. A writer seeds an absent generation
// with it under SET NX, so it never overwrites one another writer created.
func New() (string, error) {
	var b [generationBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate permission generation: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// UserKey is the user generation key, permgen:u:{userID}.
func UserKey(userID string) string { return userPrefix + CanonicalID(userID) }

// ServerKey is the server generation key, permgen:s:{serverID}.
func ServerKey(serverID string) string { return serverPrefix + CanonicalID(serverID) }

// CanonicalID is the form an id takes inside every permission-cache key. A
// uuid, in any spelling PostgreSQL resolves to the same row (either case,
// braces, hyphens anywhere or nowhere), becomes its canonical lowercase
// 8-4-4-4-12 form; anything else is used as given.
//
// Keys must follow the database's equality, not the request's spelling. Before
// this, the key was the RAW path parameter, so a member who read through
// /servers/<UPPERCASE-ID>/... got an entry tagged with a server generation that
// a canonical bump never reaches, and after an enforcement flip kept reading
// and using the dangerous bits through that spelling until the TTL (#3453
// red-team; the canonical-only invalidation patterns had the same hole for
// role and override changes). Folding a string PostgreSQL would reject merges
// nothing a database read ever accepted, so it can never join two rows.
func CanonicalID(s string) string {
	var hex [32]byte
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
			c += 'a' - 'A'
		case c == '-', c == '{', c == '}':
			continue
		default:
			return s
		}
		if n == len(hex) {
			return s
		}
		hex[n] = c
		n++
	}
	if n != len(hex) {
		return s
	}
	h := string(hex[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
