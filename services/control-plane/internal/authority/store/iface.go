package store

import (
	"context"
	"errors"
)

// The operator routes' own refusals (B3-D6). server.operatorErr answers
// ErrSealed and ErrReconciled 409 with no body (B3-D2), and ErrBadAuthz 400
// bad_authz.
var (
	// ErrSealed: the legacy population is sealed, so it takes no more rows.
	ErrSealed = errors.New("authority store: the legacy population is sealed")
	// ErrReconciled: the realm is reconciled, so it takes no import or hold.
	ErrReconciled = errors.New("authority store: the realm is reconciled")
	// ErrBadAuthz: a body that does not decode, or is not authorised.
	ErrBadAuthz = errors.New("authority store: bad_authz")
)

// Authority is the store interface PR-B2's handlers call (§3.3).
type Authority interface {
	Submit(ctx context.Context, body []byte) Outcome       // POST /v1/transactions
	SubmitDevice(ctx context.Context, body []byte) Outcome // POST /v1/device-transactions
	Head(ctx context.Context, accountID string) Outcome
	History(ctx context.Context, accountID string, afterSeq uint64) Outcome
	Devices(ctx context.Context, accountID string, afterDseq uint64) Outcome
	Statuses(ctx context.Context, body []byte) Outcome // POST /v1/status
	PendingHeads(ctx context.Context, after string) Outcome
	LegacyPopulation(ctx context.Context, after string) Outcome
	Tick(ctx context.Context, in TickInput) error
	Purge(ctx context.Context) (PurgeReport, error)
	Operator
}

// Operator is the four operator routes serve answers (§3.3, B3-D3). The
// seven owner verbs are not on it: they run in the owner CLI, as
// authority_owner, on *Owner (owner.go), and are never served over HTTP.
type Operator interface {
	InsertLegacyPopulation(ctx context.Context, body []byte) error
	ReconcileRequired(ctx context.Context, body []byte) error
	ReconcileImport(ctx context.Context, body []byte) (Outcome, error)
	ReconcileHold(ctx context.Context, body []byte) (Outcome, error)
}
