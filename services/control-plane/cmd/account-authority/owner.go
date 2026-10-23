package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/server"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// The owner verbs (§3.4, B3-D3, B3-D9, B3-D12): restore-begin,
// reconcile-complete, clear-held, seal-legacy, end-drain, revoke-key and
// replication-gate. Each one parses its arguments strictly (a malformed form
// is usage, exit 2, before anything is read), loads the owner-CLI column,
// opens the owner store through openOwner, and runs its store call under
// ownerDeadline, finish's replication wait included (D-225: finish waits as
// long as its context).
// Results go to stdout as key=value lines (B3-D9); a failure is the one
// authority.exit line exitCode writes, at StepOwner, with ownerErr's reason.

// openOwner opens the owner store: server.OpenOwner, held in a variable so
// the integration tests can open it over their own pool. The shared test
// database is a TCP superuser URL, which Load's authority_owner-over-socket
// rule refuses, so the real OpenOwner cannot be reached from there.
var openOwner = server.OpenOwner

// digestHex is a SHA-384 digest's length in hex.
const digestHex = 96

// errNotReplicated is the exit text of a verb that committed locally and was
// not confirmed replicated, exactly as DoR:1743 words it.
var errNotReplicated = errors.New("committed locally, not replicated, re-run")

// ownerCall is one parsed owner verb.
type ownerCall struct {
	needSigner bool // clear-held without --dry-run alone (B3-D1)
	run        func(ctx context.Context, cfg server.Config, o *store.Owner, out io.Writer) error
}

// parseOwner parses an owner verb's arguments. false is usage, as is a verb
// that is not an owner verb.
func parseOwner(verb string, args []string) (ownerCall, bool) {
	switch verb {
	case "restore-begin":
		return ownerCall{run: restoreBegin}, len(args) == 0
	case "reconcile-complete":
		return ownerCall{run: reconcileComplete}, len(args) == 0
	case "clear-held":
		return parseClearHeld(args)
	case "seal-legacy":
		return parseSealLegacy(args)
	case "end-drain":
		return parseEndDrain(args)
	case "revoke-key":
		return parseRevokeKey(args)
	case "replication-gate":
		return parseReplicationGate(args)
	}
	return ownerCall{}, false
}

// parseClearHeld is clear-held [--dry-run] <account>...: --dry-run only first,
// then 1 to store.ClearHeldMax accounts, each a canonical lowercase UUID, no
// two alike. The store checks the list again; this keeps a malformed list a
// usage error that reads nothing.
func parseClearHeld(args []string) (ownerCall, bool) {
	dryRun := len(args) > 0 && args[0] == "--dry-run"
	if dryRun {
		args = args[1:]
	}
	if len(args) == 0 || len(args) > store.ClearHeldMax {
		return ownerCall{}, false
	}
	seen := make(map[string]bool, len(args))
	for _, a := range args {
		if id, err := uuid.Parse(a); err != nil || id.String() != a || seen[a] {
			return ownerCall{}, false
		}
		seen[a] = true
	}
	accounts := args
	return ownerCall{needSigner: !dryRun, run: func(ctx context.Context, _ server.Config, o *store.Owner, out io.Writer) error {
		var results []store.ClearResult
		var err error
		if dryRun {
			results, err = o.ClearHeldPlan(ctx, accounts)
		} else {
			results, err = o.ClearHeld(ctx, accounts)
		}
		return finishClear(out, results, err)
	}}, true
}

// finishClear prints one account=<uuid> outcome=<outcome> line per result,
// in input order, and then decides the exit: the store call's own error
// first (a replication failure is re-run whatever the accounts read), then
// any refused:<why> result, which exits 1 with reason=refused (B3-D11). The
// why is the store's error text, printed for the operator on stdout and never
// logged: the exit line counts the refusals and names no account.
func finishClear(out io.Writer, results []store.ClearResult, err error) error {
	refusedN := 0
	for _, r := range results {
		if _, werr := fmt.Fprintf(out, "account=%s outcome=%s\n", r.Account, r.Outcome); werr != nil && err == nil {
			err = werr
		}
		if strings.HasPrefix(r.Outcome, store.ClearRefused) {
			refusedN++
		}
	}
	if err != nil {
		return err
	}
	if refusedN > 0 {
		return server.RefuseAt(server.StepOwner, server.ReasonRefused,
			fmt.Errorf("clear-held refused %d of %d accounts", refusedN, len(results)))
	}
	return nil
}

// parseDigest is a 96-character hex SHA-384 digest.
func parseDigest(s string) ([]byte, bool) {
	if len(s) != digestHex {
		return nil, false
	}
	b, err := hex.DecodeString(s)
	return b, err == nil
}

// parseSealLegacy is seal-legacy --cp-digest <hex>.
func parseSealLegacy(args []string) (ownerCall, bool) {
	v, ok := oneFlag(args, "cp-digest")
	if !ok {
		return ownerCall{}, false
	}
	d, ok := parseDigest(v)
	return ownerCall{run: func(ctx context.Context, _ server.Config, o *store.Owner, out io.Writer) error {
		if err := o.SealLegacy(ctx, d); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "sealed=true")
		return err
	}}, ok
}

// parseEndDrain is end-drain <gen>:<serial>, two unsigned decimals.
func parseEndDrain(args []string) (ownerCall, bool) {
	if len(args) != 1 {
		return ownerCall{}, false
	}
	g, s, ok := strings.Cut(args[0], ":")
	if !ok {
		return ownerCall{}, false
	}
	gen, gerr := strconv.ParseUint(g, 10, 64) // no sign, no fraction, no overflow
	serial, serr := strconv.ParseUint(s, 10, 64)
	if gerr != nil || serr != nil {
		return ownerCall{}, false
	}
	return ownerCall{run: func(ctx context.Context, _ server.Config, o *store.Owner, out io.Writer) error {
		if err := o.EndDrain(ctx, gen, serial); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "drained=%d:%d\n", gen, serial)
		return err
	}}, true
}

// parseRevokeKey is revoke-key <cak|operator|cp_tls> <hex>.
func parseRevokeKey(args []string) (ownerCall, bool) {
	if len(args) != 2 {
		return ownerCall{}, false
	}
	kind := args[0]
	switch kind {
	case "cak", "operator", "cp_tls":
	default:
		return ownerCall{}, false
	}
	d, ok := parseDigest(args[1])
	return ownerCall{run: func(ctx context.Context, _ server.Config, o *store.Owner, out io.Writer) error {
		if err := o.RevokeKey(ctx, kind, d); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "revoked=%s\n", kind)
		return err
	}}, ok
}

// parseReplicationGate is replication-gate <on|off>, lowercase.
func parseReplicationGate(args []string) (ownerCall, bool) {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return ownerCall{}, false
	}
	on := args[0] == "on"
	return ownerCall{run: func(ctx context.Context, _ server.Config, o *store.Owner, out io.Writer) error {
		if err := o.ReplicationGate(ctx, on); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "replication_required=%t\n", on)
		return err
	}}, true
}

// restoreBegin reads AUTHORITY_TRUST_PATH itself, under S3's rule, and hands
// the bytes to RestoreBegin to install (DoR:1744 step 4), then prints the
// restore point it recorded.
func restoreBegin(ctx context.Context, cfg server.Config, o *store.Owner, out io.Writer) error {
	trust, err := server.ReadTrust(cfg)
	if err != nil {
		return err
	}
	pt, err := o.RestoreBegin(ctx, trust)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "restore_epoch=%d backup_max_commit_id=%d backup_max_issued_at_ms=%d restore_started_at=%s\n",
		pt.Epoch, pt.BackupMaxCommitID, pt.BackupMaxIssuedMs, pt.StartedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func reconcileComplete(ctx context.Context, _ server.Config, o *store.Owner, out io.Writer) error {
	c, err := o.ReconcileComplete(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "imported=%d held=%d\n", c.Imported, c.Held)
	return err
}

// ownerDeadline bounds one owner verb end to end. It is not the 8 s request
// deadline: a verb is a whole batch (a 64-account clear-held signs and floors
// each account in its own transaction, each under serve's 2 s lock, 5 s
// statement and 10 s idle bounds), and reconcile-complete locks every head.
// Nothing applies store.RequestDeadlineTx to an owner verb. 240 s sits under the
// clear-held window's default 300 s unit budget
// ([internal]authority-clear-held-window.sh), so the verb writes
// its own step=owner exit before systemd stops the unit.
// ponytail: one bound for every verb; per-verb bounds if B3-D13's realm-scale
// measurement shows reconcile-complete needs more.
var ownerDeadline = 240 * time.Second

// runOwner runs a parsed owner verb: the owner-CLI column, the owner store,
// then the store call bounded by ownerDeadline, its error mapped by ownerErr.
// A loader or OpenOwner refusal keeps its own step, as serve's do.
func runOwner(ctx context.Context, c ownerCall, out io.Writer, log *slog.Logger) error {
	cfg, err := server.Load(server.VerbOwner, os.Environ())
	if err != nil {
		return err
	}
	o, closeFn, err := openOwner(cfg, c.needSigner, log)
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(ctx, ownerDeadline)
	defer cancel()
	return ownerErr(c.run(ctx, cfg, o, out))
}

// ownerErr maps an owner verb's error to its exit (B3-D12), always at
// StepOwner: ServeLock held, or its session lost mid-verb (ErrServeLockLost),
// is lock, ahead of the replication a verb cut short by that loss also
// reports; a standby is not_leader; a commit not confirmed replicated is
// replication, with DoR:1743's text in place of the cause; the verb's own
// refusal, and a trust file restore-begin cannot use (ErrTrustUnusable,
// ErrRSKReused, ErrNoMeta, the Task 8 hand-off), is refused; a realm
// mismatch, and a halted installed trust (T4), is trust; a lock wait that
// ran out (55P03, the 2 s lock_timeout every owner transaction sets) is lock,
// since a re-run converges once the other transaction ends; anything else is
// config. An error that is already an exit (finishClear's, ReadTrust's)
// passes through.
func ownerErr(err error) error {
	var ee *server.ExitError
	var halt *ac.HaltError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee):
		return err
	case errors.Is(err, store.ErrServeLockHeld), errors.Is(err, store.ErrServeLockLost):
		return server.RefuseAt(server.StepOwner, server.ReasonLock, err)
	case errors.Is(err, store.ErrRealmMismatch), errors.As(err, &halt): // as migrate maps a mismatch; a T4 halt is the trust's
		return server.RefuseAt(server.StepOwner, server.ReasonTrust, err)
	case errors.Is(err, store.ErrNotLeader):
		return server.RefuseAt(server.StepOwner, server.ReasonNotLeader, err)
	case errors.Is(err, store.ErrNotReplicated):
		return server.RefuseAt(server.StepOwner, server.ReasonReplication, errNotReplicated)
	case errors.Is(err, store.ErrRefused), errors.Is(err, store.ErrTrustUnusable), errors.Is(err, store.ErrRSKReused),
		errors.Is(err, store.ErrNoMeta):
		return server.RefuseAt(server.StepOwner, server.ReasonRefused, err)
	}
	if qe, ok := as[*pq.Error](err); ok && qe.Code == "55P03" { // lock_not_available
		return server.RefuseAt(server.StepOwner, server.ReasonLock, err)
	}
	return server.RefuseAt(server.StepOwner, server.ReasonConfig, err)
}
