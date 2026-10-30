// Package loginfamily provides SQL authority for logical login sessions.
// Transaction capabilities never imply that their caller has committed.
package loginfamily

import (
	"database/sql"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AccessIdentity carries already-verified token claims for SQL authority checks.
type AccessIdentity struct {
	UserID          uuid.UUID
	SID             uuid.UUID
	CredentialEpoch string
	AccessExpiresAt time.Time
}

// Binding is a user/family/epoch snapshot and grants no transaction authority.
type Binding struct {
	UserID          uuid.UUID
	FamilyID        uuid.UUID
	CredentialEpoch string
}

// AccessGrant is an expiry-checked read snapshot, not a productive final guard.
type AccessGrant struct {
	UserID          uuid.UUID
	FamilyID        uuid.UUID
	SID             uuid.UUID
	CredentialEpoch string
	AccessExpiresAt time.Time
}

// AuthScope describes the captured revocation frontier, never a future wildcard.
type AuthScope string

// Accepted auth revocation scopes.
const (
	ScopeSession AuthScope = "session"
	ScopeUser    AuthScope = "user"
)

// AuthReason records the original cause of an auth revocation operation.
type AuthReason string

// Accepted original auth revocation reasons.
const (
	ReasonLogout         AuthReason = "logout"
	ReasonExplicitRevoke AuthReason = "explicit_revoke"
	ReasonDisplacement   AuthReason = "displacement"
	ReasonTheft          AuthReason = "theft"
	ReasonDisablement    AuthReason = "disablement"
	ReasonErasure        AuthReason = "erasure"
)

// AuthCause pairs the closed scope and reason accepted for auth revocation.
type AuthCause struct {
	Scope  AuthScope
	Reason AuthReason
}

// DeliveryOwnerKind distinguishes auth operations from existing epoch parents.
type DeliveryOwnerKind string

// Supported durable delivery owner kinds.
const (
	OwnerAuth  DeliveryOwnerKind = "auth"
	OwnerEpoch DeliveryOwnerKind = "epoch"
)

// DeliveryOwner locates retained delivery work without asserting commit or remote success.
type DeliveryOwner struct {
	Kind            DeliveryOwnerKind
	UserID          uuid.UUID
	AuthOperationID uuid.UUID
	EpochGeneration uuid.UUID
}

// RevocationResult reports uncommitted transitions, changed refresh rows, and original owners.
type RevocationResult struct {
	NewlyTerminalFamilies int64
	RevokedRefreshRows    int64
	Owners                []DeliveryOwner
}

// EpochOwner locates an existing epoch parent whose complete tuple must be revalidated in SQL.
type EpochOwner struct {
	Generation                uuid.UUID
	UserID                    uuid.UUID
	SupersededCredentialEpoch string
	CredentialEpoch           string
}

// GCResult reports collected evidence counts and whether eligible work remains incomplete.
type GCResult struct {
	DeletedSIDs               int64
	DeletedFamilies           int64
	DeletedOperations         int64
	DeletedConnectionClosures int64
	DeletedRoomClosures       int64
	Incomplete                bool
}

// UserLockMode selects the strongest users-row lock required by the complete transaction.
type UserLockMode uint8

// Supported users-row locks, ordered by increasing strength.
const (
	UserShare UserLockMode = iota + 1
	UserNoKeyUpdate
	UserUpdate
)

// FamilyLockMode selects a shared guard or a family mutation lock.
type FamilyLockMode uint8

// Supported family-row locks, ordered by increasing strength.
const (
	FamilyShare FamilyLockMode = iota + 1
	FamilyNoKeyUpdate
)

// State is owned by these capabilities, never by a process-wide transaction map.
// The PostgreSQL marker binds the private lock history to this transaction.
// Callers must not roll back savepoints or reset that marker.
type authorityState struct {
	mu           sync.Mutex
	tx           *sql.Tx
	marker       uuid.UUID
	users        []uuid.UUID
	epochs       map[uuid.UUID]string
	mode         UserLockMode
	usersToken   uuid.UUID
	targetsReady bool
}

// WriterBarrier carries private same-transaction rollout-lock provenance.
type WriterBarrier struct{ state *authorityState }

// UserAuthoritySet represents one complete sorted set of locked users.
type UserAuthoritySet struct{ state *authorityState }

// UserAuthority is a checked view of a member of the locked user set.
type UserAuthority struct {
	state  *authorityState
	userID uuid.UUID
}

// LiveFamily carries private transaction-bound authority for a live family.
type LiveFamily struct {
	user    UserAuthority
	binding Binding
	mode    FamilyLockMode
	token   uuid.UUID
}

// Targets retains a complete transaction-bound family capture in SQL storage.
type Targets struct {
	user  UserAuthority
	token uuid.UUID
	epoch *string
}

// Binding returns only a snapshot; the zero capability returns the zero binding.
func (f LiveFamily) Binding() Binding { return f.binding }
