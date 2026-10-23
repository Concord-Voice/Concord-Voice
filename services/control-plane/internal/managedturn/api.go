// Package managedturn owns the managed TURN shared-key runtime store (#3931).
//
// The writer keeps coturn's dynamic SQLite turn_secret set in host RAM and
// publishes the control plane's active issuer key only after fresh
// authenticated allocations succeed. See
// [internal]plans/2026-10-18-3931-turn-runtime.md § Task 2.
//
// # Invariants
//
// The coordinator maintains four artefacts: the turn_secret rows coturn reads
// (RAM), the active-key file the control plane reads (RAM), the durable
// non-secret state file, and the operator's key source. It keeps these
// invariants:
//
//  1. Publication follows proof. The active file is only ever replaced with a
//     key that is (a) present in the database under the exact realm, written
//     by this process in the same transition, and (b) proven by fresh
//     authenticated allocations over UDP, TCP and TLS. Every refusal or
//     failure before the active-file rename leaves the active file and the
//     state file byte-for-byte unchanged.
//  2. Recorded and published keys are never deleted. Retirement is deferred
//     until #2639 bounds credential and allocation lifetime, so the recorded
//     set holds at most MaxKeys keys. Exactly two paths delete rows, and only
//     rows that are neither in state.key_ids nor in the active file: Bootstrap
//     removes the row it just inserted when that key's probe fails, and
//     DiscardUnrecorded removes the leftovers of an interrupted bootstrap or
//     add-candidate. This is safe because no credential was ever minted from
//     such a row: the issuer only mints from the active file, and the probe's
//     own credential is in-process and expires within MaxProbeTTL.
//  3. The database must agree with the state. A transition refuses
//     (ErrStoreMismatch) unless the rows equal the recorded key set, allowing
//     only the key the same operation is (re)adding. An empty database under a
//     stale state is therefore never treated as ready: only Recover, with the
//     exact recorded set, may refill it.
//  4. Generation advances exactly when the active file is (re)published
//     (Bootstrap, Promote, Rollback, Recover). AddCandidate rewrites the state
//     under the same generation. Each state write is a compare-and-swap on the
//     generation the transition loaded.
//  5. One transition at a time. Every transition holds an exclusive flock on
//     the state directory from its first read to its last write, so a second
//     writer can never publish between another's probe and state commit.
//
// Ordering inside a transition is insert, probe, publish, then state. A crash
// can therefore leave an unrecorded, unpublished row, which DiscardUnrecorded
// clears, or a published key ahead of the state. In the second case every
// mutating operation refuses with ErrActiveMismatch, and Status reports both
// IDs, until the operator replays the interrupted switch: Promote or Rollback
// of the key the active file holds proves it afresh and commits the state
// without re-publishing.
//
// # Phases
//
// "active" means every key in the set has been proven (it is, or once was,
// the published key). "candidate" means the set holds one key that has never
// been proven. Rollback is only permitted in the active phase, to a key other
// than the published one.
//
// # Operator generation guard
//
// Coordinator.ExpectGeneration is the operator's compare-and-swap guard: the
// generation the operator last observed (0 when no state exists). When it is
// set, every mutating transition refuses with ErrStaleGeneration unless the
// state loaded under the lock still holds that generation, so an operator
// acting on a stale status cannot overwrite a newer transition.
package managedturn

import (
	"context"
	"errors"
	"time"
)

// Bounds shared by the store, the stdin reader and the issuer file.
const (
	MinKeyLen    = 43  // shortest accepted key, in bytes
	MaxKeyLen    = 127 // turn_secret.value is varchar(127)
	MaxKeys      = 2   // active plus one candidate; retirement waits for #2639
	MaxProbeTTL  = 60 * time.Second
	ProbeUserTag = "turn-probe"
)

// ActiveFileName is the file WriteActiveFile publishes inside its directory.
const ActiveFileName = "turn-key"

// Sentinel errors. Callers log only these, never key material.
var (
	ErrNotImplemented   = errors.New("managedturn: not implemented")
	ErrInvalidKey       = errors.New("managedturn: invalid key")
	ErrInputTooLarge    = errors.New("managedturn: input exceeds bound")
	ErrKeySetFull       = errors.New("managedturn: key set full")
	ErrUnknownKey       = errors.New("managedturn: key not in set")
	ErrKeySetMismatch   = errors.New("managedturn: recovered key set does not match state")
	ErrStateExists      = errors.New("managedturn: state already initialised")
	ErrStaleGeneration  = errors.New("managedturn: stale generation")
	ErrProbeFailed      = errors.New("managedturn: authenticated allocation probe failed")
	ErrUnsafePermission = errors.New("managedturn: unsafe path ownership or mode")

	ErrNoState           = errors.New("managedturn: no state; bootstrap first")
	ErrInvalidState      = errors.New("managedturn: invalid state record")
	ErrStoreMismatch     = errors.New("managedturn: database key set does not match state; run recover for missing rows or discard-unrecorded for extra ones")
	ErrInvalidTransition = errors.New("managedturn: transition not allowed from the current state")
	ErrInvalidConfig     = errors.New("managedturn: invalid configuration")
	ErrActiveMismatch    = errors.New("managedturn: active file key does not match state; replay the interrupted promote or rollback")
	ErrUnprovenKeyLeft   = errors.New("managedturn: unproven key left in the database; run discard-unrecorded")
)

// Prober proves fresh authenticated allocations with a candidate key.
type Prober interface {
	Probe(ctx context.Context, key Key) error
}
