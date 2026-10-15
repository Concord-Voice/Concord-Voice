package store

import (
	"context"
	"errors"
)

// ErrNotImplemented is every B3 verb's answer in B1 (D-175, B1-D6).
var ErrNotImplemented = errors.New("authority store: not_implemented")

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

// Operator is the B3 surface (§3.4). PR-B3 implements it; in B1 every method
// returns ErrNotImplemented.
type Operator interface {
	InsertLegacyPopulation(ctx context.Context, body []byte) error
	ReconcileRequired(ctx context.Context, body []byte) error
	ReconcileImport(ctx context.Context, body []byte) (Outcome, error)
	ReconcileHold(ctx context.Context, body []byte) (Outcome, error)
	RestoreBegin(ctx context.Context) error
	ReconcileComplete(ctx context.Context) error
	ClearHeld(ctx context.Context, accountID string) error
	SealLegacy(ctx context.Context, snapshotDigest []byte) error
	EndDrain(ctx context.Context, gen, serial uint64) error
	RevokeKey(ctx context.Context, kind string, digest []byte) error
	ReplicationGate(ctx context.Context, on bool) error
}
