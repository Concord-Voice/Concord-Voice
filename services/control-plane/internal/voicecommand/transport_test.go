package voicecommand

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/stmthook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

var errTransportInterrupted = errors.New("test transport interrupted")

// The fake substitutes only the broker. Receipts, transactions, commit loss and
// concurrent callers all use PostgreSQL through the production entrypoint.
type bootstrapProvisioner struct {
	db                                         *sql.DB
	mu                                         sync.Mutex
	generation                                 uuid.UUID
	identity                                   TransportIdentity
	phase                                      string
	initializes, verifies, releases            int
	initializeFault, releaseFault, verifyFault error
	afterInitialize                            func()
	allowConcurrentReceiptLock                 bool
	releasedReceipts                           []TransportReceipt
}

var _ TransportProvisioner = (*bootstrapProvisioner)(nil)

func fixtureTransportIdentity() TransportIdentity {
	return TransportIdentity{
		StreamCreatedAt:      time.Date(2026, 10, 13, 12, 30, 1, 123456789, time.UTC),
		OwnerStoreCreatedAt:  time.Date(2026, 10, 13, 12, 30, 1, 234567891, time.UTC),
		InitializingRevision: 1,
	}
}

func sameTransportIdentity(a, b TransportIdentity) bool {
	return a.StreamCreatedAt.Equal(b.StreamCreatedAt) &&
		a.OwnerStoreCreatedAt.Equal(b.OwnerStoreCreatedAt) &&
		a.InitializingRevision == b.InitializingRevision
}

func (p *bootstrapProvisioner) Initialize(ctx context.Context, generation uuid.UUID) (TransportIdentity, error) {
	// A broker round trip while this caller holds the receipt row is unsafe.
	// Separate concurrent callers may overlap a short receipt commit.
	query := `SELECT generation FROM voice_command_transport WHERE id = TRUE FOR UPDATE NOWAIT`
	if p.allowConcurrentReceiptLock {
		query = `SELECT generation FROM voice_command_transport WHERE id = TRUE`
	}
	var stored uuid.UUID
	if err := p.db.QueryRowContext(ctx, query).Scan(&stored); err != nil {
		return TransportIdentity{}, fmt.Errorf("broker work found a locked receipt: %w", err)
	}
	if stored != generation {
		return TransportIdentity{}, errors.New("broker received a different receipt generation")
	}
	p.mu.Lock()
	p.initializes++
	if p.generation != uuid.Nil && p.generation != generation {
		p.mu.Unlock()
		return TransportIdentity{}, errors.New("broker generation reset")
	}
	p.generation = generation
	if p.phase == "" {
		p.phase = "initializing"
	}
	if p.identity.StreamCreatedAt.IsZero() {
		p.identity = fixtureTransportIdentity()
	}
	// Capture the network response before the barrier: a delayed caller may
	// return stale evidence after a different provisioner commits the receipt.
	identity := p.identity
	fault, after := p.initializeFault, p.afterInitialize
	p.initializeFault, p.afterInitialize = nil, nil
	p.mu.Unlock()
	if after != nil {
		after()
	}
	return identity, fault
}

func (p *bootstrapProvisioner) Verify(ctx context.Context, receipt TransportReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifies++
	if p.verifyFault != nil {
		return p.verifyFault
	}
	if p.generation != receipt.Generation || p.phase == "" || !sameTransportIdentity(p.identity, receipt.Identity) {
		return errors.New("transport continuity lost")
	}
	return nil
}

func (p *bootstrapProvisioner) ReleaseInitialOwner(ctx context.Context, receipt TransportReceipt) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releases++
	p.releasedReceipts = append(p.releasedReceipts, receipt)
	if p.generation != receipt.Generation || !sameTransportIdentity(p.identity, receipt.Identity) {
		return errors.New("owner release used stale resource identity or initializing revision")
	}
	var provisioned sql.NullTime
	var streamCreated, ownerCreated sql.NullString
	var revision sql.NullInt64
	if err := p.db.QueryRowContext(ctx,
		`SELECT provisioned_at, stream_created_at, owner_store_created_at, initializing_revision
         FROM voice_command_transport WHERE id = TRUE AND generation = $1`, receipt.Generation,
	).Scan(&provisioned, &streamCreated, &ownerCreated, &revision); err != nil {
		return err
	}
	if !provisioned.Valid || !streamCreated.Valid || !ownerCreated.Valid || !revision.Valid {
		return errors.New("owner was released before its full receipt committed")
	}
	if streamCreated.String != receipt.Identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano) ||
		ownerCreated.String != receipt.Identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano) ||
		revision.Int64 <= 0 || uint64(revision.Int64) != receipt.Identity.InitializingRevision {
		return errors.New("owner was released with identity different from its committed receipt")
	}
	if p.releaseFault != nil {
		fault := p.releaseFault
		p.releaseFault = nil
		return fault
	}
	if p.phase == "initializing" {
		p.phase = "released"
	}
	return nil
}

func (p *bootstrapProvisioner) state() (uuid.UUID, string, int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.generation, p.phase, p.initializes, p.verifies, p.releases
}

func bootstrapDatabase(t *testing.T) (*sql.DB, uuid.UUID) {
	t.Helper()
	db, _ := testdb.SetupTestDB(t)
	// Reset only this test's isolated singleton, without modifying its schema.
	_, err := db.Exec(`DELETE FROM voice_command_transport`)
	require.NoError(t, err)
	generation := uuid.New()
	_, err = db.Exec(`INSERT INTO voice_command_transport (id, generation) VALUES (TRUE, $1)`, generation)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := db.Exec(`DELETE FROM voice_command_transport`)
		assert.NoError(t, cleanupErr)
	})
	return db, generation
}

func requireReceipt(t *testing.T, db *sql.DB, generation uuid.UUID, provisioned bool) TransportReceipt {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM voice_command_transport`).Scan(&count))
	require.Equal(t, 1, count, "concurrent bootstrap must retain exactly one receipt")
	var got uuid.UUID
	var timestamp sql.NullTime
	var streamCreated, ownerCreated sql.NullString
	var revision sql.NullInt64
	require.NoError(t, db.QueryRow(
		`SELECT generation, provisioned_at, stream_created_at, owner_store_created_at, initializing_revision
         FROM voice_command_transport WHERE id = TRUE`,
	).Scan(&got, &timestamp, &streamCreated, &ownerCreated, &revision))
	assert.Equal(t, generation, got, "a restart must never replace the transport generation")
	assert.Equal(t, provisioned, timestamp.Valid)
	assert.Equal(t, provisioned, streamCreated.Valid)
	assert.Equal(t, provisioned, ownerCreated.Valid)
	assert.Equal(t, provisioned, revision.Valid, "all receipt identity fields commit atomically")
	receipt := TransportReceipt{Generation: got}
	if provisioned {
		var err error
		receipt.Identity.StreamCreatedAt, err = time.Parse(time.RFC3339Nano, streamCreated.String)
		require.NoError(t, err)
		receipt.Identity.OwnerStoreCreatedAt, err = time.Parse(time.RFC3339Nano, ownerCreated.String)
		require.NoError(t, err)
		initializingRevision := revision.Int64
		if initializingRevision <= 0 {
			t.Fatal("a provisioned receipt must have a positive initializing revision")
			return receipt
		}
		receipt.Identity.InitializingRevision = uint64(initializingRevision)
		assert.Equal(t, streamCreated.String, receipt.Identity.StreamCreatedAt.UTC().Format(time.RFC3339Nano))
		assert.Equal(t, ownerCreated.String, receipt.Identity.OwnerStoreCreatedAt.UTC().Format(time.RFC3339Nano))
	}
	return receipt
}

func TestBootstrapTransportInterruptedAndConcurrent(t *testing.T) {
	t.Run("interrupted initialization resumes the same generation", func(t *testing.T) {
		db, generation := bootstrapDatabase(t)
		p := &bootstrapProvisioner{db: db, initializeFault: errTransportInterrupted}
		require.ErrorIs(t, BootstrapTransport(context.Background(), db, p), errTransportInterrupted)
		requireReceipt(t, db, generation, false)
		got, phase, _, _, releases := p.state()
		assert.Equal(t, generation, got)
		assert.Equal(t, "initializing", phase)
		assert.Zero(t, releases, "initializing ownership cannot be admitted")

		require.NoError(t, BootstrapTransport(context.Background(), db, p))
		receipt := requireReceipt(t, db, generation, true)
		assert.Equal(t, fixtureTransportIdentity(), receipt.Identity, "the receipt preserves exact broker nanoseconds and original revision")
		got, phase, initializes, _, releases := p.state()
		assert.Equal(t, generation, got)
		assert.Equal(t, "released", phase)
		assert.Equal(t, 2, initializes)
		assert.Equal(t, 1, releases)
	})

	t.Run("receipt commit followed by interrupted release resumes without initializing", func(t *testing.T) {
		db, generation := bootstrapDatabase(t)
		p := &bootstrapProvisioner{db: db, releaseFault: errTransportInterrupted}
		require.ErrorIs(t, BootstrapTransport(context.Background(), db, p), errTransportInterrupted)
		requireReceipt(t, db, generation, true)
		_, phase, _, _, _ := p.state()
		assert.Equal(t, "initializing", phase)
		require.NoError(t, BootstrapTransport(context.Background(), db, p))
		got, phase, initializes, verifies, _ := p.state()
		assert.Equal(t, generation, got)
		assert.Equal(t, "released", phase)
		assert.Equal(t, 1, initializes, "a provisioned receipt forbids Initialize")
		assert.Positive(t, verifies)
	})

	t.Run("concurrent callers converge on one generation and committed receipt", func(t *testing.T) {
		db, generation := bootstrapDatabase(t)
		p := &bootstrapProvisioner{db: db, allowConcurrentReceiptLock: true}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		const callers = 8
		results := make(chan error, callers)
		start := make(chan struct{})
		for range callers {
			go func() {
				<-start
				results <- BootstrapTransport(ctx, db, p)
			}()
		}
		close(start)
		for range callers {
			assert.NoError(t, <-results)
		}
		requireReceipt(t, db, generation, true)
		got, phase, _, _, _ := p.state()
		assert.Equal(t, generation, got)
		assert.Equal(t, "released", phase)
	})
}

func TestBootstrapTransportCommitAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		committed, readFails bool
	}{
		{name: "commit succeeded but its acknowledgement was lost", committed: true},
		{name: "commit was rolled back before its acknowledgement was lost"},
		{name: "receipt cannot be confirmed after a committed acknowledgement loss", committed: true, readFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, generation := bootstrapDatabase(t)
			hook, wrapped := stmthook.Open(t)
			p := &bootstrapProvisioner{db: db}
			p.afterInitialize = func() {
				var readFault error
				if tc.readFails {
					readFault = stmthook.ErrInjected
				}
				hook.Arm([]string{"UPDATE voice_command_transport", "voice_command_transport"}, nil, readFault)
				hook.ArmCommit(tc.committed, errTransportInterrupted)
			}
			err := BootstrapTransport(context.Background(), wrapped, p)
			if tc.committed && !tc.readFails {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			seen, interleavedErr := hook.Report()
			require.NoError(t, interleavedErr)
			require.Equal(t, 2, seen, "an ambiguous COMMIT requires a fresh receipt read")
			requireReceipt(t, db, generation, tc.committed)
			_, phase, initializes, _, releases := p.state()
			assert.Equal(t, 1, initializes)
			if tc.committed && !tc.readFails {
				assert.Equal(t, "released", phase)
				assert.Equal(t, 1, releases)
			} else {
				assert.Equal(t, "initializing", phase)
				assert.Zero(t, releases, "an unconfirmed receipt must retain initializing ownership")
				require.NoError(t, BootstrapTransport(context.Background(), db, p))
				requireReceipt(t, db, generation, true)
			}
		})
	}
}

func TestBootstrapTransportCancellationRetainsInitializingOwner(t *testing.T) {
	for _, cancelAfterCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel after commit=%t", cancelAfterCommit), func(t *testing.T) {
			db, generation := bootstrapDatabase(t)
			hook, wrapped := stmthook.Open(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &bootstrapProvisioner{db: db}
			p.afterInitialize = func() {
				if !cancelAfterCommit {
					cancel()
					return
				}
				hook.Arm([]string{"UPDATE voice_command_transport", "voice_command_transport"}, nil, nil)
				hook.Observe(func(context.Context) { cancel() })
				hook.ArmCommit(true, errTransportInterrupted)
			}
			require.Error(t, BootstrapTransport(ctx, wrapped, p))
			requireReceipt(t, db, generation, cancelAfterCommit)
			_, phase, _, _, releases := p.state()
			assert.Equal(t, "initializing", phase)
			assert.Zero(t, releases)
			require.NoError(t, BootstrapTransport(context.Background(), db, p))
			requireReceipt(t, db, generation, true)
		})
	}
}

func TestBootstrapTransportContinuityLoss(t *testing.T) {
	db, generation := bootstrapDatabase(t)
	p := &bootstrapProvisioner{db: db}
	require.NoError(t, BootstrapTransport(context.Background(), db, p))
	p.mu.Lock()
	p.phase = ""
	p.verifyFault = errors.New("owner key or stream was lost")
	p.mu.Unlock()
	require.Error(t, BootstrapTransport(context.Background(), db, p))
	requireReceipt(t, db, generation, true)
	got, phase, initializes, verifies, releases := p.state()
	assert.Equal(t, generation, got)
	assert.Empty(t, phase, "normal bootstrap cannot recreate lost ownership")
	assert.Equal(t, 1, initializes)
	assert.Positive(t, verifies)
	assert.Equal(t, 1, releases, "a failed Verify cannot release an owner")
}

func TestBootstrapTransportSuccessfulCommitRequiresReceiptReread(t *testing.T) {
	db, generation := bootstrapDatabase(t)
	hook, wrapped := stmthook.Open(t)
	p := &bootstrapProvisioner{db: db}
	p.afterInitialize = func() {
		hook.Arm([]string{"UPDATE voice_command_transport", "voice_command_transport"}, nil, stmthook.ErrInjected)
	}
	require.Error(t, BootstrapTransport(context.Background(), wrapped, p))
	receipt := requireReceipt(t, db, generation, true)
	assert.Equal(t, fixtureTransportIdentity(), receipt.Identity)
	seen, interleavedErr := hook.Report()
	require.NoError(t, interleavedErr)
	assert.Equal(t, 2, seen, "even an acknowledged COMMIT must reread its persisted identity")
	_, phase, _, _, releases := p.state()
	assert.Equal(t, "initializing", phase)
	assert.Zero(t, releases, "a failed post-commit identity read cannot release the owner")
	require.NoError(t, BootstrapTransport(context.Background(), db, p))
}

func TestBootstrapTransportStaleInitializerUsesWinningReceipt(t *testing.T) {
	db, generation := bootstrapDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	captured := make(chan struct{})
	resume := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	p := &bootstrapProvisioner{db: db, allowConcurrentReceiptLock: true}
	p.afterInitialize = func() { close(captured); <-resume }
	result := make(chan error, 1)
	go func() { result <- BootstrapTransport(ctx, db, p) }()
	select {
	case <-captured:
	case err := <-result:
		t.Fatalf("first bootstrap did not reach the controlled broker response: %v", err)
	case <-ctx.Done():
		t.Fatal("first bootstrap did not reach the broker barrier")
	}
	winning := TransportIdentity{
		StreamCreatedAt:      time.Date(2026, 10, 13, 12, 31, 2, 345678912, time.UTC),
		OwnerStoreCreatedAt:  time.Date(2026, 10, 13, 12, 31, 2, 456789123, time.UTC),
		InitializingRevision: 7,
	}
	p.mu.Lock()
	p.identity = winning
	p.mu.Unlock()
	winnerErr := BootstrapTransport(ctx, db, p)
	if winnerErr == nil {
		// Media acquired the confirmed winner before the stale response returned.
		p.mu.Lock()
		p.phase = "held"
		p.mu.Unlock()
	}
	close(resume)
	staleErr := <-result
	require.NoError(t, winnerErr)
	require.NoError(t, staleErr, "a zero-row update must use the winning committed receipt")
	receipt := requireReceipt(t, db, generation, true)
	assert.Equal(t, winning, receipt.Identity, "late evidence must not overwrite an immutable receipt")
	_, phase, initializes, verifies, _ := p.state()
	assert.Equal(t, "held", phase, "a delayed provisioner cannot reset live ownership")
	assert.Equal(t, 2, initializes)
	assert.Positive(t, verifies, "the winning resource identity must be verified before late release")
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, released := range p.releasedReceipts {
		assert.Equal(t, generation, released.Generation)
		assert.Equal(t, winning, released.Identity, "release consumes the SQL receipt, never the stale Initialize response")
	}
}

func TestLoadReceipt(t *testing.T) {
	t.Run("confirmed identity retains exact broker nanoseconds and revision", func(t *testing.T) {
		db, generation := bootstrapDatabase(t)
		p := &bootstrapProvisioner{db: db}
		require.NoError(t, BootstrapTransport(context.Background(), db, p))
		loaded, err := LoadReceipt(context.Background(), db)
		require.NoError(t, err)
		assert.Equal(t, generation, loaded.Generation)
		assert.Equal(t, fixtureTransportIdentity(), loaded.Identity)
	})
	t.Run("unprovisioned singleton is not a recovery receipt", func(t *testing.T) {
		db, generation := bootstrapDatabase(t)
		_, err := LoadReceipt(context.Background(), db)
		require.Error(t, err)
		requireReceipt(t, db, generation, false)
	})
	t.Run("missing singleton is not first-time provisioning", func(t *testing.T) {
		db, _ := bootstrapDatabase(t)
		_, err := db.Exec(`DELETE FROM voice_command_transport`)
		require.NoError(t, err)
		_, err = LoadReceipt(context.Background(), db)
		require.Error(t, err)
	})
	t.Run("canceled receipt read fails closed", func(t *testing.T) {
		db, _ := bootstrapDatabase(t)
		p := &bootstrapProvisioner{db: db}
		require.NoError(t, BootstrapTransport(context.Background(), db, p))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := LoadReceipt(ctx, db)
		require.Error(t, err)
	})
}
