package server

import (
	"crypto/ecdsa"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// dialTimeout bounds each database connection's establishment, the socket
// connect and the startup exchange together (N1). A URL's connect_timeout
// can shorten it and never lengthen it (M1). A context deadline cannot
// bound it: lib/pq v1.12.3 hands its context to the socket connect alone
// (conn.go, dial) and runs the startup
// exchange with no deadline unless ConnectTimeout is set, and database/sql
// calls Connect synchronously, so a database that accepts and never answers
// would hold serve's start, and any request that redials, for ever. It is a
// read's deadline (RequestDeadlineRead): a local socket and an authentication
// take milliseconds. A var so a test can shorten it.
var dialTimeout = 4 * time.Second

// The trust inputs' read caps are the codec's own (ac.CapAnchors and
// ac.CapTrust): a file over them cannot decode, so the cap refuses nothing
// the codec would accept.
const (
	anchorsMax = ac.CapAnchors
	trustMax   = ac.CapTrust
)

var errUncheckedDB = errors.New("the database Config was not checked by Load")

// The pool's bounds (Sec-M3). poolMaxOpen is every connection serve can hold
// at once, so it never waits on a pool it is itself filling:
//   - preHandshakeBudget pin checks, each one revoked_keys read, since the
//     listener holds the pre-handshake slot through the check (E1);
//   - authenticatedBudget connections, each serving one request at a time:
//     checkPeer's read, then the route's one connection or transaction;
//   - poolLockSessions, the serve lock's session, held for the whole run;
//   - poolLoopSessions, the tick (Tick, then PurgeDue), the role poll (Leader,
//     then a promotion's schema and trust reads) and the one purge leg.
//
// No path holds a connection while it waits for a second: each of these takes
// one, and releases it before the next, so the cap cannot deadlock. It is
// 32 + 16 + 1 + 3 = 52, the figure #3593's max_connections must leave for
// authority_app (DoR §3.3). Idle connections are kept for the authenticated
// budget, the steady load; a burst of pin checks closes what it opened past
// that. migrate's owner pool opens through OpenDB too, and holds a few.
const (
	poolLockSessions = 1
	poolLoopSessions = 3
	poolMaxOpen      = preHandshakeBudget + authenticatedBudget + poolLockSessions + poolLoopSessions
	poolMaxIdle      = authenticatedBudget
)

// undumpable is makeUndumpable, called by Open before it reads a key. A var
// so a test can make it fail.
var undumpable = makeUndumpable

// Open turns serve's loaded Config into Deps: it makes the process
// non-dumpable before any key is read, reads the two
// credentials (F2) and the two trust inputs (S3), opens the database pool
// and binds the listener. closeFn releases the pool and the socket. A
// refusal names the variable, never its value (§3.6); a process that cannot
// be made non-dumpable is refused as config. On a non-Linux OS the
// credential read refuses, so Open is reason=config there (B2-D16).
func Open(cfg Config, log *slog.Logger) (d Deps, closeFn func(), err error) {
	if err := undumpable(); err != nil {
		return Deps{}, nil, RefuseAt(StepDumpable, ReasonConfig, err)
	}
	sig, tlsKey, err := loadKeys(cfg.CredentialsDir)
	if err != nil {
		return Deps{}, nil, RefuseAt(StepCredentials, ReasonConfig, err)
	}
	anchors, trust, err := readInputs(cfg)
	if err != nil {
		return Deps{}, nil, RefuseAt(StepInputs, ReasonTrust, err)
	}
	db, err := OpenDB(cfg)
	if err != nil {
		return Deps{}, nil, RefuseAt(StepDatabase, ReasonConfig, err)
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr.String())
	if err != nil {
		_ = db.Close()
		return Deps{}, nil, RefuseAt(StepListen, ReasonConfig, bindError(err))
	}
	dir := cfg.DiskWatchPath
	d = Deps{DB: db, RSK: sig, TLSKey: tlsKey, Anchors: anchors, TrustFile: trust, Listen: ln,
		Statfs: func() (uint64, uint64, error) { return statfs(dir) }, Clock: (&clockWatch{read: clockSynced, log: log}).synced,
		Notify: notifier(os.Getenv("NOTIFY_SOCKET"), log), Log: log}
	return d, func() { _ = ln.Close(); _ = db.Close() }, nil
}

// OpenDB is the authority's one database dial (N1): sql.OpenDB over
// pq.NewConnectorConfig of c.DB, the Config checkDBURL returned, never the
// URL in c.DatabaseURL. A second parse of the URL re-reads the environment
// and drops the passfile sentinel, and lib/pq then reads $HOME/.pgpass on
// every connection. A Config Load did not check is refused, by the marker
// setDB alone sets rather than by DB's fields, which a pq.Config built from
// any URL can fake. Each connection is bounded by the
// shorter of the URL's connect_timeout and dialTimeout, and the pool by
// poolMaxOpen and poolMaxIdle. It dials nothing itself; the first use does.
// migrate's owner pool opens through it too.
func OpenDB(c Config) (*sql.DB, error) {
	if !c.checked {
		return nil, errUncheckedDB
	}
	pc := c.DB
	if pc.ConnectTimeout <= 0 || pc.ConnectTimeout > dialTimeout {
		pc.ConnectTimeout = dialTimeout // the URL may shorten the bound, never lift it (M1)
	}
	conn, err := pq.NewConnectorConfig(pc)
	if err != nil {
		return nil, errors.New("lib/pq refuses the checked Config")
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(poolMaxOpen)
	db.SetMaxIdleConns(poolMaxIdle)
	return db, nil
}

// bindError keeps a failed bind's cause and drops the address, which a
// net.OpError's text carries: a refusal names the variable, never its value
// (B2-D32). The cause reads like "bind: address already in use".
func bindError(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
	}
	return fmt.Errorf("AUTHORITY_LISTEN_ADDR does not bind: %w", err)
}

// loadKeys reads the RSK and the TLS key (F2, B2-D11).
func loadKeys(dir string) (signer.Signer, *ecdsa.PrivateKey, error) {
	sig, err := loadSigner(dir)
	if err != nil {
		return nil, nil, err
	}
	tlsKey, err := loadKey(dir, credTLS)
	if err != nil {
		return nil, nil, err
	}
	return sig, tlsKey, nil
}

// loadSigner reads the RSK alone (F2) as the signer. serve reads it through
// loadKeys; clear-held, the one owner verb that signs, reads it here and
// never reads the TLS key, which an owner verb has no use for (B3-D1).
func loadSigner(dir string) (signer.Signer, error) {
	rsk, err := loadKey(dir, credRSK)
	if err != nil {
		return nil, err
	}
	sig, err := signer.NewP384(rsk)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", credRSK, err)
	}
	return sig, nil
}

func loadKey(dir, name string) (*ecdsa.PrivateKey, error) {
	der, err := readCredential(dir, name)
	if err != nil {
		return nil, err
	}
	defer clear(der) // the parsed key holds its own copy; no second copy of the DER stays in the heap (CWE-226)
	k, err := parseKey(der)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", name, err)
	}
	return k, nil
}

// readInputs reads the anchors and the trust file under S3's rule.
func readInputs(cfg Config) (anchors, trust []byte, err error) {
	if anchors, err = readTrustFile("AUTHORITY_ANCHORS_PATH", cfg.AnchorsPath, anchorsMax); err != nil {
		return nil, nil, err
	}
	if trust, err = readTrustFile("AUTHORITY_TRUST_PATH", cfg.TrustPath, trustMax); err != nil {
		return nil, nil, err
	}
	return anchors, trust, nil
}

// readTrustFile reads path, variable v's value, under the credentials'
// integrity rule (S3): on an empty database nothing else stands between
// serve and substituted anchors, which would let a trust file of their
// choosing install as the realm's. The file is opened by openJudged, the
// credentials' opener, so the parent passes dirRule as opened and the file
// is judged as opened; and the read stops at limit. The files are public, so
// only writing is refused: trustFileRule. A refusal names v, never the path.
func readTrustFile(v, path string, limit int) ([]byte, error) {
	euid, err := geteuid()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", v, err)
	}
	f, fi, err := openJudged(filepath.Dir(path), filepath.Base(path), euid)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", v, err)
	}
	defer func() { _ = f.Close() }()
	if err := trustFileRule(fi.Mode(), ownerOf(fi), euid); err != nil {
		return nil, fmt.Errorf("%s is %w", v, err)
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", v, pathless(err))
	}
	if len(b) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", v, limit)
	}
	return b, nil
}

// trustFileRule is S3's file half: a regular file owned by root or euid that
// neither group nor other can write. Under a POSIX ACL the group bits are the
// mask, which caps every named entry, so a clear group write bit means no
// named user or group can write either.
func trustFileRule(mode fs.FileMode, uid, euid uint32) error {
	switch {
	case !mode.IsRegular():
		return errors.New("not a regular file")
	case uid != 0 && uid != euid:
		return errors.New("not owned by root or the euid")
	case mode.Perm()&0o022 != 0:
		return errors.New("writable by group or other")
	}
	return nil
}
