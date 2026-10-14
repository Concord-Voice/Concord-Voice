package voicecommand

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

// TransportIdentity binds bootstrap to immutable broker creation times and the
// exact initializing revision, even when a recreated KV store resets revisions.
type TransportIdentity struct {
	StreamCreatedAt      time.Time
	OwnerStoreCreatedAt  time.Time
	InitializingRevision uint64
}

// TransportReceipt is confirmed SQL evidence, never a delayed initializer's local result.
type TransportReceipt struct {
	Generation uuid.UUID
	Identity   TransportIdentity
}

// Valid reports whether the complete identity can be persisted without rounding.
func (identity TransportIdentity) Valid() bool {
	return !identity.StreamCreatedAt.IsZero() && !identity.OwnerStoreCreatedAt.IsZero() &&
		identity.InitializingRevision > 0 && identity.InitializingRevision <= math.MaxInt64
}

// TransportProvisioner keeps broker operations outside receipt transactions.
// Verify and ReleaseInitialOwner use only the committed receipt's evidence.
type TransportProvisioner interface {
	Initialize(context.Context, uuid.UUID) (TransportIdentity, error)
	Verify(context.Context, TransportReceipt) error
	ReleaseInitialOwner(context.Context, TransportReceipt) error
}

type storedTransportReceipt struct {
	receipt     TransportReceipt
	provisioned bool
}

// LoadReceipt returns only confirmed, complete provisioning evidence. Recovery
// tools share this parsing boundary instead of interpreting partial SQL state.
func LoadReceipt(ctx context.Context, db *sql.DB) (TransportReceipt, error) {
	if db == nil {
		return TransportReceipt{}, errors.New("voice command receipt database unavailable")
	}
	stored, err := readTransportReceipt(ctx, db)
	if err != nil {
		return TransportReceipt{}, err
	}
	if !stored.provisioned {
		return TransportReceipt{}, errors.New("voice command transport has not been provisioned")
	}
	return stored.receipt, nil
}

func readTransportReceipt(ctx context.Context, db *sql.DB) (storedTransportReceipt, error) {
	var stored storedTransportReceipt
	var provisioned sql.NullTime
	var streamCreated, ownerCreated sql.NullString
	var revision sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT generation, provisioned_at,
		stream_created_at, owner_store_created_at, initializing_revision
		FROM voice_command_transport WHERE id = TRUE`,
	).Scan(&stored.receipt.Generation, &provisioned, &streamCreated, &ownerCreated, &revision)
	if err != nil {
		return stored, fmt.Errorf("read voice command transport receipt: %w", err)
	}
	if stored.receipt.Generation == uuid.Nil {
		return stored, errors.New("voice command transport receipt has no generation")
	}
	stored.provisioned = provisioned.Valid
	if !stored.provisioned {
		if streamCreated.Valid || ownerCreated.Valid || revision.Valid {
			return stored, errors.New("unprovisioned voice command receipt carries partial identity")
		}
		return stored, nil
	}
	if !streamCreated.Valid || !ownerCreated.Valid || !revision.Valid || revision.Int64 <= 0 {
		return stored, errors.New("provisioned voice command receipt has incomplete identity")
	}
	identity := TransportIdentity{InitializingRevision: uint64(revision.Int64)}
	identity.StreamCreatedAt, err = canonicalBrokerTime(streamCreated.String)
	if err != nil {
		return stored, err
	}
	identity.OwnerStoreCreatedAt, err = canonicalBrokerTime(ownerCreated.String)
	if err != nil {
		return stored, err
	}
	stored.receipt.Identity = identity
	return stored, nil
}

func canonicalBrokerTime(value string) (time.Time, error) {
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || timestamp.IsZero() || timestamp.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, errors.New("voice command receipt has noncanonical broker creation time")
	}
	return timestamp.UTC(), nil
}

// BootstrapTransport cannot bless recreated broker state. Every release uses a
// freshly read committed receipt, discarding any stale Initialize result.
func BootstrapTransport(ctx context.Context, db *sql.DB, provisioner TransportProvisioner) error {
	if db == nil || provisioner == nil {
		return errors.New("voice command transport bootstrap dependencies unavailable")
	}
	stored, err := readTransportReceipt(ctx, db)
	if err != nil {
		return err
	}
	if stored.provisioned {
		return verifyAndRelease(ctx, provisioner, stored.receipt)
	}
	identity, initializeErr := provisioner.Initialize(ctx, stored.receipt.Generation)
	if initializeErr != nil {
		// A competing bootstrap may have committed and released while this caller
		// was paused. Only its committed receipt can resolve the resulting conflict.
		current, readErr := readTransportReceipt(ctx, db)
		if readErr != nil || !current.provisioned || current.receipt.Generation != stored.receipt.Generation {
			return errors.Join(fmt.Errorf("initialize voice command transport: %w", initializeErr), readErr)
		}
		return verifyAndRelease(ctx, provisioner, current.receipt)
	}
	if !identity.Valid() {
		return errors.New("voice command initialization returned incomplete broker identity")
	}
	commitErr := commitTransportReceipt(ctx, db, TransportReceipt{Generation: stored.receipt.Generation, Identity: identity})
	// A fresh pool read is required after both clean and ambiguous commits. The
	// winner may have committed another broker incarnation while this caller paused.
	current, readErr := readTransportReceipt(ctx, db)
	if readErr != nil {
		return errors.Join(commitErr, readErr)
	}
	if !current.provisioned || current.receipt.Generation != stored.receipt.Generation {
		return errors.Join(errors.New("voice command receipt commit unconfirmed"), commitErr)
	}
	return verifyAndRelease(ctx, provisioner, current.receipt)
}

func verifyAndRelease(ctx context.Context, provisioner TransportProvisioner, receipt TransportReceipt) error {
	if err := provisioner.Verify(ctx, receipt); err != nil {
		return fmt.Errorf("verify voice command transport continuity: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("voice command bootstrap canceled before owner release: %w", err)
	}
	if err := provisioner.ReleaseInitialOwner(ctx, receipt); err != nil {
		return fmt.Errorf("release initial voice command owner: %w", err)
	}
	return nil
}

func commitTransportReceipt(ctx context.Context, db *sql.DB, receipt TransportReceipt) (returnErr error) {
	revision := receipt.Identity.InitializingRevision
	if revision == 0 || revision > math.MaxInt64 {
		return errors.New("voice command initializing revision exceeds PostgreSQL BIGINT")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin voice command receipt commit: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, fmt.Errorf("rollback voice command receipt: %w", err))
		}
	}()
	result, err := tx.ExecContext(ctx, `UPDATE voice_command_transport
		SET provisioned_at = clock_timestamp(), stream_created_at = $2,
		owner_store_created_at = $3, initializing_revision = $4
		WHERE id = TRUE AND generation = $1 AND provisioned_at IS NULL`,
		receipt.Generation, receipt.Identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
		receipt.Identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano),
		int64(revision))
	if err != nil {
		return fmt.Errorf("record provisioned voice command transport: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect voice command receipt update: %w", err)
	}
	if changed != 0 && changed != 1 {
		return errors.New("voice command transport receipt singleton violated")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit voice command receipt: %w", err)
	}
	return nil
}
