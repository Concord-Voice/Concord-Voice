// Command mfa-rekey re-encrypts stored TOTP seeds to the active
// MFA_ENCRYPTION_KEY_VERSION (#2307).
//
// Authorization model: this tool requires direct DATABASE_URL access — it is an
// operator-on-the-box utility, the same trust level as cmd/issue-codes and
// cmd/migrate. Possession of the production DB connection string IS the
// privilege. It additionally needs the MFA_ENCRYPTION_KEY* env values (the
// keyring) to re-seal rows; it never prints key material.
//
// Typical rotation flow (see [internal]mfa-key-rotation.md):
//
//	mfa-rekey --dry-run     # count rows pending re-encryption
//	mfa-rekey               # re-seal them under the active version
//
// Exit status: 0 on convergence; 1 if any row could not be decrypted (those
// rows are reported on stderr and left untouched); 2 on usage errors.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	_ "github.com/joho/godotenv/autoload" // dotenv autoload for DATABASE_URL + MFA_* vars
	_ "github.com/lib/pq"                 // postgres driver

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfa"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// rekeyOptions groups the CLI flags (mirrors cmd/issue-codes' issueOptions).
type rekeyOptions struct {
	DryRun    bool
	BatchSize int
}

func main() {
	os.Exit(runMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// runMain is the testable orchestration behind main: parse flags, build the
// keyring, connect, run the backfill, report. It returns the process exit code
// (0 converged, 1 some rows failed, 2 setup/usage error) instead of calling
// os.Exit, so an integration test can drive it against a real DB. Setup errors
// go to stderr — never key material.
func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mfa-rekey", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts, err := parseFlags(fs, args)
	if err != nil {
		// parseFlags already wrote the specifics to stderr (fs.Output). Bail
		// BEFORE connecting so a typo (e.g. --batch-size nope, an unknown flag,
		// or a stray positional arg) can never proceed to a live rekey.
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "failed to load configuration: %v\n", err)
		return 2
	}
	ring, err := mfa.ParseKeyring(cfg.MFAEncryptionKey, cfg.MFAEncryptionKeyVersion, cfg.MFAEncryptionKeysRetired)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "invalid MFA encryption keyring: %v\n", err)
		return 2
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "open database: %v\n", err)
		return 2
	}
	defer func() { _ = db.Close() }()

	// Bound the startup health check so a stalled database (accepting the TCP
	// connection but never responding) cannot hang the tool indefinitely. The
	// 30-minute runCtx below governs the backfill itself.
	pingCtx, pingCancel := context.WithTimeout(ctx, 15*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		_, _ = fmt.Fprintf(stderr, "ping database: %v\n", err)
		return 2
	}

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	res, err := mfa.Rekey(runCtx, db, ring, opts.BatchSize, opts.DryRun)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "rekey: %v\n", err)
		return 1
	}
	return report(res, opts.DryRun, ring.ActiveVersion(), stdout, stderr)
}

// parseFlags reads CLI flags from args (explicit FlagSet for testability,
// mirroring cmd/issue-codes). It returns an error on an unparseable flag or any
// unexpected positional argument, so the caller can refuse to run against
// production data. On error, the specifics are already written to fs.Output().
func parseFlags(fs *flag.FlagSet, args []string) (rekeyOptions, error) {
	dryRun := fs.Bool("dry-run", false, "Report the pending row count; mutate nothing")
	batchSize := fs.Int("batch-size", 100, "Rows fetched per batch")
	if err := fs.Parse(args); err != nil {
		return rekeyOptions{}, err // ContinueOnError already printed it to fs.Output()
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(fs.Output(), "unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		return rekeyOptions{}, fmt.Errorf("unexpected argument(s): %s", strings.Join(fs.Args(), " "))
	}
	return rekeyOptions{DryRun: *dryRun, BatchSize: *batchSize}, nil
}

// report prints the result summary and returns the process exit code.
// Failed rows go to stderr with the user UUID and sealed version (operator
// remediation data — never key material).
func report(res mfa.RekeyResult, dryRun bool, activeVersion int, stdout, stderr io.Writer) int {
	if dryRun {
		_, _ = fmt.Fprintf(stdout, "dry-run: %d row(s) pending re-encryption to version %d\n", res.Scanned, activeVersion)
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "rekeyed %d, skipped %d (concurrent rewrite), failed %d\n", res.Rekeyed, res.Skipped, len(res.Failed))
	for _, f := range res.Failed {
		_, _ = fmt.Fprintf(stderr, "FAILED user=%s sealed_version=%d: %v\n", f.UserID, f.SealedVersion, f.Err)
	}
	if len(res.Failed) > 0 {
		return 1
	}
	return 0
}
