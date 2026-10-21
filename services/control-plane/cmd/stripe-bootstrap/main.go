// Command stripe-bootstrap provisions Concord's Stripe catalog (epic #2033,
// spec §6): products Supersonic and Wing, the four prices, the founders-50
// coupon and the Customer Portal configuration marked concord_portal=v1. It
// is idempotent. A second run creates nothing and only re-asserts the Portal
// configuration, and an existing object whose immutable field differs from
// the catalog stops the run before its first write.
//
// Authorization model: an operator-run tool, run from a checkout and never
// built into an image. The catalog-write key comes from STRIPE_CATALOG_KEY
// and nowhere else. The tool never reads STRIPE_SECRET_KEY: that is the
// deployed runtime key, which by design holds no catalog write permission
// (spec §11). It never prints the key.
//
// Typical flow (see [internal]stripe-billing.md):
//
//	stripe-bootstrap --mode test --dry-run   # report planned writes; write nothing
//	stripe-bootstrap --mode test             # provision test mode
//
// --mode is required and must equal the key's own mode, so a live key never
// runs by accident. Live runs need Mark's explicit approval.
//
// Exit status: 0 on success; 1 when a Stripe call fails or an existing object
// conflicts with the catalog; 2 on a usage or key error, before any request.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/billing"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// catalogKeyEnv is the only variable the key is read from.
const catalogKeyEnv = "STRIPE_CATALOG_KEY"

// runTimeout bounds a whole run, which makes about a dozen API calls.
const runTimeout = 5 * time.Minute

// stripeAPIBaseURL is empty in production, which keeps stripe-go's default
// (https://api.stripe.com). Tests point it at an httptest server. It is a
// package variable rather than a flag or env var, so no runtime input can
// send a live key to another host.
var stripeAPIBaseURL = ""

// bootstrapOptions groups the CLI flags.
type bootstrapOptions struct {
	Mode   string
	DryRun bool
}

func main() {
	os.Exit(runMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// runMain parses flags, checks the key against --mode, runs the bootstrap and
// prints its report. It returns the exit code instead of calling os.Exit, so
// tests can drive it. Errors go to stderr and never include the key.
func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stripe-bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts, err := parseFlags(fs, args)
	if err != nil {
		return 2 // parseFlags already wrote the specifics to stderr
	}
	key := strings.TrimSpace(os.Getenv(catalogKeyEnv))
	if key == "" {
		_, _ = fmt.Fprintf(stderr, "%s is not set\n", catalogKeyEnv)
		return 2
	}
	keyMode, _, err := config.ParseStripeKeyMode(key)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", catalogKeyEnv, err)
		return 2
	}
	if string(keyMode) != opts.Mode {
		_, _ = fmt.Fprintf(stderr, "--mode %s does not match the %s-mode key in %s\n", opts.Mode, keyMode, catalogKeyEnv)
		return 2
	}

	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	client := billing.NewClient(key, stripeAPIBaseURL)
	report, err := billing.Bootstrap(runCtx, client, billing.BootstrapOptions{DryRun: opts.DryRun})
	printErr := printReport(stdout, opts.Mode, report)
	if err != nil {
		reportFailure(stderr, err)
		return 1
	}
	if printErr != nil {
		// The catalog may have changed, so a run whose report was lost must not
		// pass as a success. A rerun reports every object without writing again.
		_, _ = fmt.Fprintf(stderr, "stripe-bootstrap: writing the report failed: %v; rerun to print every object\n", printErr)
		return 1
	}
	return 0
}

// reportFailure writes a failed run's error to stderr. Stripe's own text for a
// rejected key (HTTP 401 or 403) quotes the key with its prefix and last four
// characters, and the error's string form carries that text verbatim, so a
// rejected key gets a fixed line instead and nothing from Stripe's message.
// The line names the failed operation when it is the key-free prefix the
// billing package wrapped around Stripe's error, so the operator can tell
// which permission the key lacks.
func reportFailure(stderr io.Writer, err error) {
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) &&
		(stripeErr.HTTPStatusCode == http.StatusUnauthorized || stripeErr.HTTPStatusCode == http.StatusForbidden) {
		during := ""
		if op, ok := strings.CutSuffix(err.Error(), ": "+stripeErr.Error()); ok {
			during = " during " + op
		}
		_, _ = fmt.Fprintf(stderr, "stripe-bootstrap: Stripe rejected the key in %s (HTTP %d)%s; check that it is current and has the permissions in [internal]stripe-billing.md\n",
			catalogKeyEnv, stripeErr.HTTPStatusCode, during)
		return
	}
	_, _ = fmt.Fprintf(stderr, "stripe-bootstrap: %v\n", err)
	if errors.Is(err, billing.ErrCatalogConflict) {
		_, _ = fmt.Fprintln(stderr, "stripe-bootstrap: no write was made; resolve the conflict by hand ([internal]stripe-billing.md)")
	}
}

// errUsage reports a rejected command line. parseFlags has already written
// the specifics to fs.Output(), so the error carries no text of its own.
var errUsage = errors.New("usage error")

// parseFlags reads the flags. It rejects a missing or unknown --mode and any
// positional argument, writing the specifics to fs.Output().
func parseFlags(fs *flag.FlagSet, args []string) (bootstrapOptions, error) {
	mode := fs.String("mode", "", `Stripe mode the key must belong to: "test" or "live" (required)`)
	dryRun := fs.Bool("dry-run", false, "Read and check only; report the creates and updates a real run would make")
	if err := fs.Parse(args); err != nil {
		return bootstrapOptions{}, err // ContinueOnError already printed it to fs.Output()
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(fs.Output(), "unexpected argument(s): %s\n", strings.Join(fs.Args(), " "))
		return bootstrapOptions{}, errUsage
	}
	switch config.StripeKeyMode(*mode) {
	case config.StripeKeyModeTest, config.StripeKeyModeLive:
		return bootstrapOptions{Mode: *mode, DryRun: *dryRun}, nil
	}
	_, _ = fmt.Fprintf(fs.Output(), "--mode must be %q or %q\n", config.StripeKeyModeTest, config.StripeKeyModeLive)
	return bootstrapOptions{}, errUsage
}

// printReport writes one line per catalog object and a summary line, and
// returns the first write error.
func printReport(w io.Writer, mode string, report billing.BootstrapReport) error {
	verbs := map[billing.Action]string{
		billing.ActionUnchanged: "unchanged",
		billing.ActionCreate:    "created",
		billing.ActionUpdate:    "updated",
	}
	if report.DryRun {
		verbs[billing.ActionCreate] = "would-create"
		verbs[billing.ActionUpdate] = "would-update"
	}
	for _, s := range report.Steps {
		if _, err := fmt.Fprintf(w, "%-12s %-20s %-18s %s\n", verbs[s.Action], s.Kind, s.Key, s.ID); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "mode=%s dry_run=%t create=%d update=%d unchanged=%d\n", mode, report.DryRun,
		report.Count(billing.ActionCreate), report.Count(billing.ActionUpdate), report.Count(billing.ActionUnchanged))
	return err
}
