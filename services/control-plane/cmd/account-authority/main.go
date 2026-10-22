// Command account-authority is the account-root authority (#3587): serve,
// migrate and keygen. It only dispatches (D-119); the work is in
// internal/authority/server and internal/authority/store.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	gm "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/server"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const usage = "usage: account-authority serve | migrate --realm <uuid> | keygen --out <path>"

func main() {
	ctx, stop := signalContext(context.Background(), signal.NotifyContext, syscall.SIGTERM, os.Interrupt)
	code := runMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// signalContext is notify (signal.NotifyContext) for sigs, with the default
// handler restored as soon as the first signal arrives. NotifyContext keeps
// swallowing the signals until its stop runs, so without this a second SIGINT
// or SIGTERM would be absorbed too, and a step that cannot see ctx
// (migrate's MigrateEmbedded takes none) could not be interrupted at all.
// With it the first signal asks politely and the second ends the process.
func signalContext(parent context.Context,
	notify func(context.Context, ...os.Signal) (context.Context, context.CancelFunc), sigs ...os.Signal,
) (context.Context, context.CancelFunc) {
	ctx, stop := notify(parent, sigs...)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// runMain dispatches a verb and returns the exit code: 0 done, 1 refused or
// failed, 2 usage (B2-D8).
func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	log := logger.NewJSON(stdout).Logger
	verb, rest := "", []string(nil)
	if len(args) > 0 {
		verb, rest = args[0], args[1:]
	}
	var err error
	switch verb {
	case "serve":
		if len(rest) != 0 {
			return usageExit(stderr)
		}
		err = serve(ctx, log)
	case "migrate":
		realm, ok := oneFlag(rest, "realm")
		if !ok {
			return usageExit(stderr)
		}
		err = migrate(ctx, realm)
	case "keygen":
		out, ok := oneFlag(rest, "out")
		if !ok {
			return usageExit(stderr)
		}
		err = keygen(out, stdout)
	default:
		return usageExit(stderr)
	}
	return exitCode(log, err)
}

func usageExit(stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, usage)
	return 2
}

// oneFlag parses exactly one required --name value and no positional
// argument. A flag given twice is refused: last-wins would let
// `keygen --out a --out b` write b while the operator watches a. The flag
// package's own messages are discarded, so the only output of a usage error
// is runMain's one line.
func oneFlag(args []string, name string) (string, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var value string
	seen := false
	fs.Func(name, "", func(s string) error {
		if seen {
			return errors.New("repeated")
		}
		seen, value = true, s
		return nil
	})
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return "", false
	}
	return value, value != ""
}

// exitCode logs a failure as one authority.exit line, the only line a run
// writes on the way out: Run and the verbs return their error and never log
// it. A loader error is config: it names the variable, never its value
// (§3.6). The line carries op like every other, and no outcome, because an
// exit is not a store outcome (the Task 7 ruling). The reason is in reason
// alone: an ExitError's error is its cause's text, so it never carries the
// "config: " that ExitError.Error prefixes (Code-M3).
func exitCode(log *slog.Logger, err error) int {
	if err == nil {
		return 0
	}
	reason, step, cause := server.ReasonConfig, "", err
	var ee *server.ExitError
	if errors.As(err, &ee) {
		reason, step, cause = ee.Reason, ee.Step, ee.Err
	}
	args := []any{"op", "authority.exit", "reason", reason}
	if step != "" {
		args = append(args, "step", step) // a closed set, server.Step*
	}
	log.Info("authority.exit", append(args, "error", exitText(cause))...)
	return 1
}

// multiError is golang-migrate's MultiError. The type is deprecated, and
// v4.20.1's own code joins with errors.Join instead (which exitText renders
// as a join), but it is still exported with no Unwrap, so an older runner or
// a driver can return one and the filter must know it.
type multiError = gm.MultiError //nolint:staticcheck // deprecated, but a caller can still return it

// noText is what a reduction reads when the error it reduces has no cause.
const noText = "failed"

// exitText is err's text for the exit line (B2-D32), and the one barrier
// that keeps a path, an address, SQL or a row value out of it (CWE-532). An
// error that carries a value in a typed slot is rebuilt from the slot's
// operation and its cause alone, so the text around it, which may have been
// wrapped in at any depth, goes too. The types are found with errors.As:
//   - a joined error (errors.Join, a fmt.Errorf with two %w) is rendered
//     element by element, "; " apart, so a typed sibling cannot swallow the
//     others;
//   - golang-migrate's database.Error and MultiError have no Unwrap, and
//     their text embeds the query, pgErr.Detail (row values) and the cause's
//     address. A database.Error keeps its cause alone, "database: " then the
//     cause reduced the same way; a MultiError is a join;
//   - a network error names addresses (the database's socket path or host
//     and port, a listener's, a resolver's), so one keeps its operation and
//     its system error, "network dial: connection refused", or
//     "network read: i/o timeout" for a connect_timeout that expired;
//   - a path error names the path, so one keeps its operation and its
//     cause, "open: permission denied" (C1); a link error names two;
//   - a pq.Error keeps its Message: "pq: " then the Message. Detail, Hint,
//     Where and the schema, table, column and constraint fields name data
//     and objects, and the position and SQLSTATE add nothing a log needs.
//
// A nil error, and a typed nil pointer in any of those slots, reads "failed".
// So does a reduction that panics, which errors.As does when it walks a typed
// nil *fs.PathError or *net.OpError: this is the last thing a failing run
// writes, and a stack trace would carry what the line must not. The panic
// value is dropped. Anything else is the error's own text, which the loaders
// keep free of values by naming the variable, never what it held.
func exitText(err error) (text string) {
	defer func() {
		if recover() != nil {
			text = noText
		}
	}()
	if err == nil {
		return noText
	}
	if j, ok := as[interface{ Unwrap() []error }](err); ok {
		return joinText(j.Unwrap())
	}
	if de, ok := as[database.Error](err); ok {
		return databaseText(&de)
	}
	if dp, ok := as[*database.Error](err); ok {
		return databaseText(dp)
	}
	if me, ok := as[multiError](err); ok {
		return joinText(me.Errs)
	}
	if mp, ok := as[*multiError](err); ok {
		return multiText(mp)
	}
	if oe, ok := as[*net.OpError](err); ok {
		return networkText(oe)
	}
	if pe, ok := as[*fs.PathError](err); ok {
		return pathText(pe)
	}
	if le, ok := as[*os.LinkError](err); ok {
		return linkText(le)
	}
	if qe, ok := as[*pq.Error](err); ok {
		return pqText(qe)
	}
	return err.Error()
}

// as is errors.As for the target type T, which must be an error type or an
// interface.
func as[T any](err error) (T, bool) {
	var t T
	return t, errors.As(err, &t)
}

func joinText(errs []error) string {
	if len(errs) == 0 {
		return noText
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = exitText(e)
	}
	return strings.Join(parts, "; ")
}

func multiText(m *multiError) string {
	if m == nil {
		return noText
	}
	return joinText(m.Errs)
}

func databaseText(de *database.Error) string {
	if de == nil {
		return noText
	}
	return "database: " + exitText(de.OrigErr)
}

// networkPrefix opens every reduced *net.OpError on the exit line.
const networkPrefix = "network "

func networkText(oe *net.OpError) string {
	if oe == nil {
		return noText
	}
	var errno syscall.Errno
	switch {
	case errors.As(oe.Err, &errno):
		return networkPrefix + oe.Op + ": " + errno.Error()
	case errors.Is(oe.Err, os.ErrDeadlineExceeded):
		return networkPrefix + oe.Op + ": i/o timeout"
	}
	return networkPrefix + oe.Op + " failed"
}

func pathText(pe *fs.PathError) string {
	if pe == nil {
		return noText
	}
	return pe.Op + ": " + exitText(pe.Err)
}

func linkText(le *os.LinkError) string {
	if le == nil {
		return noText
	}
	return le.Op + ": " + exitText(le.Err)
}

func pqText(qe *pq.Error) string {
	if qe == nil {
		return noText
	}
	return "pq: " + qe.Message
}

func serve(ctx context.Context, log *slog.Logger) error {
	cfg, err := server.Load(server.VerbServe, os.Environ())
	if err != nil {
		return err
	}
	deps, closeFn, err := server.Open(cfg, log)
	if err != nil {
		return err
	}
	defer closeFn()
	return server.Run(ctx, deps)
}

func migrate(ctx context.Context, realm string) error {
	if id, err := uuid.Parse(realm); err != nil || id.String() != realm {
		return server.RefuseAt(server.StepRealm, server.ReasonConfig, errors.New("--realm must be a canonical lowercase UUID"))
	}
	cfg, err := server.Load(server.VerbMigrate, os.Environ())
	if err != nil {
		return err
	}
	return migrateWith(ctx, cfg, realm)
}

// migrateWith is migrate on a loaded Config. It dials through server.OpenDB
// alone (N1): the Config Load checked, never lib/pq's own parse of its URL,
// which reads $HOME/.pgpass and dials with no bound. A Config Load did not
// check is refused there, as config.
func migrateWith(ctx context.Context, cfg server.Config, realm string) error {
	db, err := server.OpenDB(cfg)
	if err != nil {
		return server.RefuseAt(server.StepDatabase, server.ReasonConfig, err)
	}
	defer func() { _ = db.Close() }()
	return migrateDB(ctx, db, realm)
}

// migrateDB is migrate on an open owner pool (B2-D2): the serve lock (D-171),
// the embedded migrations, then meta's first write. The lock and the meta
// write each run under their own deadline. The migrations do not:
// store.MigrateEmbedded takes no context (the shared runner's signature), and
// nothing else bounds it. ConnectTimeout bounds only each connection's
// establishment, and golang-migrate's own advisory lock and any DDL lock wait
// are unbounded. The operator's way out is a signal: the first SIGINT or
// SIGTERM cancels ctx, which this step cannot see, and the second reaches the
// default handler and ends the process (signalContext).
func migrateDB(ctx context.Context, db *sql.DB, realm string) error {
	var lock *store.ServeLock
	err := server.Within(ctx, func(c context.Context) (err error) {
		lock, err = store.AcquireServeLock(c, db) // the lock's session outlives c: only the acquiring query uses it
		return err
	})
	if errors.Is(err, store.ErrServeLockHeld) {
		return server.RefuseAt(server.StepLock, server.ReasonLock, err)
	}
	if err != nil {
		return server.RefuseAt(server.StepLock, server.ReasonConfig, err)
	}
	defer lock.Release()
	if err := store.MigrateEmbedded(db); err != nil {
		return server.RefuseAt(server.StepMigrate, server.ReasonConfig, err)
	}
	err = server.Within(ctx, func(c context.Context) error { return store.EnsureMeta(c, db, realm) })
	if errors.Is(err, store.ErrRealmMismatch) {
		return server.RefuseAt(server.StepMeta, server.ReasonTrust, err)
	}
	if err != nil {
		return server.RefuseAt(server.StepMeta, server.ReasonConfig, err)
	}
	return nil
}

// keygen is B2-D11: a P-384 PKCS#8 DER key at out, created with
// O_EXCL|O_NOFOLLOW and mode 0400 whatever the umask, removed again if the
// write or the printing fails. It prints the point and the TLS pin, never
// the key.
func keygen(out string, stdout io.Writer) error {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	defer clear(der) // written or not, the DER is not left in the heap (CWE-226)
	pt, err := k.PublicKey.ECDH()
	if err != nil {
		return err
	}
	pin, err := ac.TLSPin(pt.Bytes())
	if err != nil {
		return err
	}
	if err := writeKey(out, der); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "point=%x\npin=%x\n", pt.Bytes(), pin); err != nil {
		// The ceremony needs the point and the pin, and O_EXCL refuses a
		// retry over this file: remove it so keygen can run again.
		return removeKey(out, err)
	}
	return nil
}

// removeKey removes the key keygen just made, because the ceremony cannot use
// it. A removal that fails is joined to cause, not dropped: the key is still
// there, a retry will be refused by O_EXCL, and the operator must be told.
func removeKey(out string, cause error) error {
	if rerr := os.Remove(out); rerr != nil {
		return errors.Join(cause, rerr)
	}
	return cause
}

func writeKey(out string, der []byte) error {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o400) //nolint:gosec // operator-supplied --out path; O_NOFOLLOW covers only the last component, and O_EXCL already refuses a symlink there
	if err != nil {
		return err
	}
	err = f.Chmod(0o400) // OpenFile's mode passes through the umask
	if err == nil {
		_, err = f.Write(der)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return removeKey(out, err)
	}
	return nil
}
