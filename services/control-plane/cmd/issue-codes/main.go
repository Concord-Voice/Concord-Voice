// Command issue-codes is the admin CLI issuer for redemption codes (#1303).
//
// Authorization model: this tool requires direct DATABASE_URL access — it is an
// operator-on-the-box utility, the same trust level as `migrate`. There is no
// in-tool auth because possession of the production DB connection string IS the
// privilege (an attacker with it can already write redemption_codes directly).
// The HTTP generation endpoint (gated by REDEMPTION_ADMIN_TOKEN) is the path for
// callers WITHOUT DB access. Every generation is audited in-DB via the same
// DBAuditSink the HTTP path uses (issuer_context='cli').
//
// Codes are printed to stdout ONCE — only their SHA-256 hash is persisted. Pipe
// to a file or --csv for the Kickstarter backer survey; they are unrecoverable
// afterward. See [internal]redemption-code-issuance.md.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	_ "github.com/joho/godotenv/autoload" // dotenv autoload for DATABASE_URL
	_ "github.com/lib/pq"                 // postgres driver

	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

// issueOptions groups the CLI-supplied issuance parameters into one value so the
// orchestration function (runIssue) takes a single config struct instead of a
// long positional argument list (fixes go:S107 — function-with-too-many-params).
type issueOptions struct {
	GrantKind  string
	Months     int
	Count      int
	Prefix     string
	SingleUse  bool
	MaxRedeems string
	Expires    string
	BatchID    string
	CSVPath    string
}

// codeIssuer is the issuance dependency runIssue needs — satisfied by
// *redemption.Issuer in production and by a fake in tests. Abstracting it here is
// what makes the orchestration logic unit-testable without a real DB handle.
type codeIssuer interface {
	Issue(ctx context.Context, spec redemption.IssueSpec) ([]redemption.IssuedCode, error)
}

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	opts := parseFlags(fs, os.Args[1:])

	if opts.GrantKind == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --grant-kind is required")
		fs.Usage()
		os.Exit(2)
	}

	db := openDB()
	defer func() { _ = db.Close() }()

	issuer := redemption.NewIssuer(db, redemption.NewCatalog(), redemption.NewDBAuditSink())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := runIssue(ctx, opts, issuer, os.Stdout, os.Stderr); err != nil {
		log.Fatalf("%v", err)
	}
}

// parseFlags reads the CLI flags from args into an issueOptions value. It takes
// an explicit FlagSet (rather than the package-global flag.CommandLine) so it is
// directly unit-testable with synthetic args.
func parseFlags(fs *flag.FlagSet, args []string) issueOptions {
	var (
		grantKind  = fs.String("grant-kind", "", "Grant catalog key (e.g. premium:subscription, feature:custom_themes, cosmetic:founder_badge)")
		months     = fs.Int("months", 0, "Months for premium:subscription grants (sets grant_params {\"months\":N}); ignored for other kinds")
		count      = fs.Int("count", 1, "Number of codes to mint")
		prefix     = fs.String("prefix", "", "Non-secret support prefix (e.g. KS, PROMO); ≤16 chars")
		singleUse  = fs.Bool("single-use", true, "Mark codes single-use (one-off). Set --single-use=false for promo codes")
		maxRedeems = fs.String("max-redeems", "1", "Max redemptions per code: a positive integer, or 'unlimited' (promo). Unlimited requires --expires")
		expires    = fs.String("expires", "", "Hard expiry as RFC3339 (e.g. 2026-12-31T23:59:59Z). Required for unlimited promo codes")
		batchID    = fs.String("batch-id", "", "Shared campaign label across the batch (e.g. ks-2026-founder); ≤64 chars")
		csvPath    = fs.String("csv", "", "Write codes to this CSV path (columns: code,batch_id,grant_kind) instead of plain stdout")
	)
	// args come from a caller-built slice (os.Args[1:] in main); parse errors use
	// the FlagSet's configured error handling (ExitOnError in main, ContinueOnError
	// in tests).
	_ = fs.Parse(args)

	return issueOptions{
		GrantKind:  *grantKind,
		Months:     *months,
		Count:      *count,
		Prefix:     *prefix,
		SingleUse:  *singleUse,
		MaxRedeems: *maxRedeems,
		Expires:    *expires,
		BatchID:    *batchID,
		CSVPath:    *csvPath,
	}
}

// runIssue is the testable issuance orchestration: build the spec, mint the codes
// via the supplied issuer, write them out (CSV or stdout), and print the once-only
// banner to errOut. main() is a thin shell around it (flag parse → openDB → call).
// Returns an error rather than calling log.Fatal so the logic is unit-testable.
func runIssue(ctx context.Context, opts issueOptions, issuer codeIssuer, out, errOut io.Writer) error {
	spec, err := buildSpec(opts)
	if err != nil {
		return fmt.Errorf("invalid options: %w", err)
	}

	codes, err := issuer.Issue(ctx, spec)
	if err != nil {
		return fmt.Errorf("code generation failed: %w", err)
	}

	if err := emit(out, errOut, codes, opts.CSVPath, opts.BatchID, opts.GrantKind); err != nil {
		return fmt.Errorf("failed to write codes: %w", err)
	}

	// Best-effort human-facing banner to stderr; a write error to the terminal is
	// non-actionable in a CLI, so the result is intentionally discarded.
	_, _ = fmt.Fprintf(errOut, "Generated %d code(s) for grant_kind=%q batch_id=%q. They are shown ONCE and only the SHA-256 hash is stored.\n",
		len(codes), opts.GrantKind, opts.BatchID)
	return nil
}

// buildSpec converts CLI options to an IssueSpec, parsing max-redeems and expiry.
func buildSpec(opts issueOptions) (redemption.IssueSpec, error) {
	maxR, err := redemption.ParseMaxRedeems(opts.MaxRedeems)
	if err != nil {
		return redemption.IssueSpec{}, err
	}

	var expiresAt *time.Time
	if opts.Expires != "" {
		t, perr := time.Parse(time.RFC3339, opts.Expires)
		if perr != nil {
			return redemption.IssueSpec{}, fmt.Errorf("invalid --expires %q (want RFC3339): %w", opts.Expires, perr)
		}
		expiresAt = &t
	}

	var params map[string]any
	if opts.Months > 0 {
		params = map[string]any{"months": opts.Months}
	}

	return redemption.IssueSpec{
		GrantKind:   opts.GrantKind,
		GrantParams: params,
		Count:       opts.Count,
		Prefix:      opts.Prefix,
		SingleUse:   opts.SingleUse,
		MaxRedeems:  maxR,
		ExpiresAt:   expiresAt,
		BatchID:     opts.BatchID,
		Context:     redemption.IssuerContextCLI,
		// CreatedBy left nil — CLI has no user identity; the audit records
		// issuer_context='cli'.
	}, nil
}

// openDB connects using the same config loader as the server (DATABASE_URL). It
// is the thin log.Fatal shell around the testable connectDB; the connection
// logic lives in connectDB so it can be exercised against the dev database.
func openDB() *sql.DB {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}
	db, err := connectDB(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return db
}

// connectDB opens and pings a postgres connection for databaseURL, returning an
// error instead of fatal-exiting so it is unit-testable.
func connectDB(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	return db, nil
}

// emit writes the codes either to a CSV file or as plain lines on out. The CSV
// path writes nothing to out (the banner on errOut reports the destination).
func emit(out, errOut io.Writer, codes []redemption.IssuedCode, csvPath, batchID, grantKind string) error {
	if csvPath != "" {
		f, err := os.Create(csvPath) //nolint:gosec // operator-supplied path, operator-on-the-box trust
		if err != nil {
			return fmt.Errorf("create csv %q: %w", csvPath, err)
		}
		defer func() { _ = f.Close() }()
		if err := redemption.WriteCSV(f, codes, batchID, grantKind); err != nil {
			return err
		}
		// Best-effort destination report to stderr (non-actionable on failure).
		_, _ = fmt.Fprintf(errOut, "Wrote %d code(s) to %s\n", len(codes), csvPath)
		return nil
	}
	for _, c := range codes {
		// The plaintext lines are the primary CLI output; a stdout write error is
		// non-actionable (the operator pipes/reads them), so it is discarded.
		_, _ = fmt.Fprintln(out, c.Plaintext)
	}
	return nil
}
