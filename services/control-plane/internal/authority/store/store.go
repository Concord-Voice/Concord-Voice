package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// Hooks are the store's seams: InRecovery and Statfs are required, the rest
// are test-only.
type Hooks struct {
	DBNow      func(ctx context.Context) (uint64, error) // tests only; nil reads clock_timestamp()
	InRecovery func(ctx context.Context, conn *sql.Conn) (bool, error)
	Statfs     func() (uint64, error)
	Pause      func() // tests only; nil sleeps 1 ms while D-241's fence waits for db_now (Task 13)
}

// queryer is what *sql.Conn and *sql.Tx share.
type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// dbNow is db_now in ms: read once per transaction at its start (§3.2.4), and
// once before BEGIN for the pre-checks. The hook exists only for tests.
func (s *Store) dbNow(ctx context.Context, q queryer) (uint64, error) {
	if s.hooks.DBNow != nil {
		return s.hooks.DBNow(ctx)
	}
	return clockMs(ctx, q)
}

// clockMs is the database's clock_timestamp() in ms, read on q: dbNow's read,
// and reconcile-complete's, which has no hook (owner_restore.go).
func clockMs(ctx context.Context, q queryer) (uint64, error) {
	var ms int64
	if err := q.QueryRowContext(ctx, `SELECT floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint`).Scan(&ms); err != nil {
		return 0, err
	}
	if ms < 0 {
		return 0, errors.New("authority store: db_now precedes the epoch")
	}
	return uint64(ms), nil
}

// Config is what Open needs.
type Config struct {
	DB                       *sql.DB
	Signer                   signer.Signer
	Anchors                  []byte
	Probe                    ReplicationProbe
	Admission                *Admission
	Hooks                    Hooks
	Logger                   *slog.Logger
	TxDeadline, ReadDeadline time.Duration
}

// Store is the account authority's database and commit core.
type Store struct {
	db      *sql.DB
	sig     signer.Signer
	anchors ac.Anchors
	probe   ReplicationProbe
	adm     *Admission
	hooks   Hooks
	log     *slog.Logger
	txDL    time.Duration
	readDL  time.Duration

	trustMu       sync.Mutex
	trustKey      string
	trust         *ac.Trust
	trustAccepted map[string]bool // msg digests of the delegation rows the cached trust accepted (Task 9, B1-D37)

	// nonClient503 is the tick's "a full tick with a non-client 503" leg
	// (D-147, D-228): set on a 503 the client did not cause, read and
	// cleared by Tick.
	nonClient503 atomic.Bool

	asOfMu    sync.Mutex
	asOfLast  map[string]uint64 // D-241: the last as_of_ms signed per account (Task 13's reserveAsOf)
	asOfFloor uint64            // the newest as_of_ms pruned from asOfLast; nothing at or below it is reserved

	purgeBudget time.Duration // PurgeBudget; a field so a test can spend it (Task 14)
}

var _ Authority = (*Store)(nil)

// Open validates cfg. It performs no I/O: B2 calls CheckSchema before serving.
func Open(cfg Config) (*Store, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("authority store: nil database")
	case cfg.Signer == nil:
		return nil, errors.New("authority store: nil signer")
	case cfg.Hooks.Statfs == nil:
		return nil, errors.New("authority store: nil Statfs (the per-request disk floor fails closed)")
	}
	a, err := ac.DecodeAnchors(cfg.Anchors)
	if err != nil {
		return nil, errors.New("authority store: anchors do not decode")
	}
	s := &Store{db: cfg.DB, sig: cfg.Signer, anchors: a, probe: cfg.Probe, adm: cfg.Admission, hooks: cfg.Hooks,
		log: cfg.Logger, txDL: cfg.TxDeadline, readDL: cfg.ReadDeadline, purgeBudget: PurgeBudget}
	if s.probe == nil {
		s.probe = SQLProbe{}
	}
	if s.adm == nil {
		s.adm = NewAdmission()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.txDL == 0 {
		s.txDL = RequestDeadlineTx
	}
	if s.readDL == 0 {
		s.readDL = RequestDeadlineRead
	}
	return s, nil
}
