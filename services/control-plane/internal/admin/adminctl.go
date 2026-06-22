// Package admin implements platform-admin authentication for the Concord Voice
// Admin/Operations console (#1688): a separate admin identity isolated from
// end-user accounts, password (Argon2id) plus mandatory WebAuthn/FIDO2
// hardware-key authentication, opaque Redis-backed sessions, and an append-only
// audit log. See [internal]specs/2026-06-20-1688-admin-auth-design.md.
package admin

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/auth"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/database"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
)

// verbResetEnrollment is the break-glass recovery subcommand name (extracted to
// satisfy go:S1192 — it recurs as the dispatch case, the flag-set name, and the
// audit detail).
const verbResetEnrollment = "reset-enrollment"

// adminCtlDeps bundles the injected collaborators an adminctl verb operates on.
// Tests construct this against the isolated PG/Redis stack and an in-memory
// stdin/stdout, so the verbs run with no real TTY and no global state.
type adminCtlDeps struct {
	repo   *AdminRepo
	audit  *AuditLog
	enroll *EnrollmentStore
	// enrollBaseURL is the console origin the printed enrollment URL points at
	// (e.g. https://admin.example.org). Derived from the admin RP origins.
	enrollBaseURL string
	stdin         io.Reader
	stdout        io.Writer
}

// RunAdminCtl is the entrypoint for the `control-plane admin <verb>` subcommand
// (bootstrap / reset-enrollment). It is invoked out-of-band via `docker exec` on
// the server for first-admin provisioning and break-glass recovery — never via
// the GitHub deploy pipeline (the trust anchor is host shell access, not CI).
//
// It is a thin wrapper: it parses the verb, loads config, opens the DB + Redis,
// builds adminCtlDeps, and delegates to the testable runAdminCtl. Returns a
// process exit code.
func RunAdminCtl(args []string) int {
	if len(args) == 0 {
		outf(os.Stderr, "usage: control-plane admin <bootstrap|reset-enrollment> [flags]\n")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		outf(os.Stderr, "admin: load config: %v\n", err)
		return 1
	}

	db, err := database.New(cfg.DatabaseURL)
	if err != nil {
		outf(os.Stderr, "admin: open database: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	rdb, err := database.NewRedisClient(cfg.RedisURL)
	if err != nil {
		outf(os.Stderr, "admin: open redis: %v\n", err)
		return 1
	}
	defer func() { _ = rdb.Close() }()

	deps := adminCtlDeps{
		repo:          NewAdminRepo(db),
		audit:         NewAuditLog(db),
		enroll:        NewEnrollmentStore(rdb),
		enrollBaseURL: enrollBaseURL(cfg),
		stdin:         os.Stdin,
		stdout:        os.Stdout,
	}

	return runAdminCtl(context.Background(), deps, args)
}

// enrollBaseURL picks the console origin for the printed enrollment URL: the
// first configured admin RP origin, else a localhost fallback for dev.
func enrollBaseURL(cfg *config.Config) string {
	if len(cfg.AdminWebAuthnRPOrigins) > 0 {
		return strings.TrimRight(cfg.AdminWebAuthnRPOrigins[0], "/")
	}
	return "https://localhost:8443"
}

// runAdminCtl dispatches a parsed verb against injected deps. Split from
// RunAdminCtl so tests drive it with an isolated stack + in-memory stdin/stdout.
func runAdminCtl(ctx context.Context, deps adminCtlDeps, args []string) int {
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "bootstrap":
		if err := runBootstrap(ctx, deps, rest); err != nil {
			outf(deps.stdout, "admin bootstrap failed: %v\n", err)
			return 1
		}
		return 0
	case verbResetEnrollment:
		if err := runResetEnrollment(ctx, deps, rest); err != nil {
			outf(deps.stdout, "admin reset-enrollment failed: %v\n", err)
			return 1
		}
		return 0
	default:
		outf(deps.stdout, "admin: unknown verb %q (want bootstrap|reset-enrollment)\n", verb)
		return 2
	}
}

// outf writes to an operator-facing stdout, discarding the non-actionable write
// error (a CLI cannot recover from a failed terminal write; the operation has
// already succeeded by the time we print). Centralising the discard keeps the
// errcheck-satisfying `_` out of every print site.
func outf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// runBootstrap creates a pending admin (password set, no key yet) and mints a
// one-time enrollment token. It reads the password from deps.stdin (a TTY in
// production, an in-memory reader in tests), validates strength, hashes it via
// Argon2id, inserts the row, mints the token, prints the enrollment URL + token
// to STDOUT (never the logger — secrets must not reach any log sink), and audits
// EventBootstrap.
func runBootstrap(ctx context.Context, deps adminCtlDeps, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(deps.stdout)
	username := fs.String("username", "", "operator handle for the new admin (required)")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin without an interactive prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("--username is required")
	}

	password, err := readPassword(deps.stdin, deps.stdout, *passwordStdin)
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	if err := auth.ValidatePasswordStrength(password); err != nil {
		return fmt.Errorf("password too weak: %w", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	adminUser, err := deps.repo.CreatePending(ctx, *username, hash)
	if err != nil {
		return fmt.Errorf("create pending admin: %w", err)
	}

	token, err := deps.enroll.MintEnrollmentToken(ctx, adminUser.ID)
	if err != nil {
		return fmt.Errorf("mint enrollment token: %w", err)
	}

	// Audit BEFORE printing so the row exists even if stdout is redirected away.
	// The audit row carries the outcome + handle, NEVER the password or token.
	if auditErr := deps.audit.Write(ctx, AuditEvent{
		AdminID:   &adminUser.ID,
		Actor:     *username,
		EventType: EventBootstrap,
		Result:    AuditSuccess,
		Detail:    map[string]any{"verb": "bootstrap"},
	}); auditErr != nil {
		return fmt.Errorf("audit bootstrap: %w", auditErr)
	}

	printEnrollment(deps.stdout, *username, deps.enrollBaseURL, token)
	return nil
}

// runResetEnrollment is the break-glass recovery verb: it disables an existing
// admin's hardware keys (so a lost/compromised key cannot authenticate) and
// mints a fresh enrollment token so the operator can register a new key. It does
// NOT touch the password. Audits EventCredentialRevoked + EventBootstrap.
func runResetEnrollment(ctx context.Context, deps adminCtlDeps, args []string) error {
	fs := flag.NewFlagSet(verbResetEnrollment, flag.ContinueOnError)
	fs.SetOutput(deps.stdout)
	username := fs.String("username", "", "operator handle of the admin to reset (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("--username is required")
	}

	adminUser, err := deps.repo.GetByUsername(ctx, *username)
	if err != nil {
		return fmt.Errorf("look up admin: %w", err)
	}

	// Disable every existing credential so the old key set can no longer be used.
	creds, err := deps.repo.ListCredentials(ctx, adminUser.ID)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}
	if len(creds) > 0 {
		if delErr := deps.repo.DeleteCredentials(ctx, adminUser.ID); delErr != nil {
			return fmt.Errorf("revoke credentials: %w", delErr)
		}
		if auditErr := deps.audit.Write(ctx, AuditEvent{
			AdminID:   &adminUser.ID,
			Actor:     *username,
			EventType: EventCredentialRevoked,
			Result:    AuditSuccess,
			Detail:    map[string]any{"verb": verbResetEnrollment, "revoked": len(creds)},
		}); auditErr != nil {
			return fmt.Errorf("audit credential revoke: %w", auditErr)
		}
	}

	// Re-arm enrollment: a reset admin returns to pending until a new key lands.
	if err := deps.repo.SetStatus(ctx, adminUser.ID, StatusPending); err != nil {
		return fmt.Errorf("reset status to pending: %w", err)
	}

	token, err := deps.enroll.MintEnrollmentToken(ctx, adminUser.ID)
	if err != nil {
		return fmt.Errorf("mint enrollment token: %w", err)
	}

	if auditErr := deps.audit.Write(ctx, AuditEvent{
		AdminID:   &adminUser.ID,
		Actor:     *username,
		EventType: EventBootstrap,
		Result:    AuditSuccess,
		Detail:    map[string]any{"verb": verbResetEnrollment},
	}); auditErr != nil {
		return fmt.Errorf("audit reset-enrollment: %w", auditErr)
	}

	printEnrollment(deps.stdout, *username, deps.enrollBaseURL, token)
	return nil
}

// readPassword reads a single line as the password. With promptSuppressed
// (--password-stdin) it reads raw from stdin for scripted use; otherwise it
// prints an interactive prompt first. The trailing newline is trimmed. The
// password is NEVER written to any log sink.
func readPassword(stdin io.Reader, stdout io.Writer, promptSuppressed bool) (string, error) {
	if !promptSuppressed {
		outf(stdout, "Enter a strong password for the new admin: ")
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// printEnrollment writes the one-time enrollment URL + token to stdout (the
// operator's terminal). This is the ONLY place the token surfaces; it is never
// logged. The token is carried as a query parameter the enrollment page reads.
func printEnrollment(stdout io.Writer, username, baseURL, token string) {
	url := fmt.Sprintf("%s/admin/enroll?username=%s&token=%s", baseURL, username, token)
	outf(stdout, "Admin created. Complete enrollment within 1 hour at the URL below.\n")
	outf(stdout, "Copy this from your terminal — it is NOT written to any log:\n")
	outf(stdout, "  Username:   %s\n", username)
	outf(stdout, "  Enroll URL: %s\n", url)
	outf(stdout, "  Token:      %s\n", token)
	outf(stdout, "Register a BACKUP hardware key immediately to avoid lockout.\n")
}
