// Command stripe-soak is the operator tooling for the test-mode billing soak
// (epic #2033, spec §16). It has one subcommand:
//
//	stripe-soak attach-clock --customer cus_…
//
// attach-clock creates a Stripe test clock with the Customer attached (spec
// F14), so a subscription the tester then buys through Checkout lives on the
// clock and renewals can be driven from the Dashboard. Stripe cannot detach a
// Customer from a clock, and deleting the clock deletes the Customer, so the
// tool refuses any Customer the local dev stack's Checkout did not create.
// It never writes to the database: the mapping stays Checkout's (R35).
//
// Authorization model: an operator-run tool, run from services/control-plane
// on a dev machine and never built into an image. The key is the dev stack's
// own STRIPE_SECRET_KEY, read from the environment or services/control-plane/
// .env like the control plane reads it. Only a test-mode key is accepted. The
// key needs Test clocks write, a test-mode-only permission (runbook § Soak).
// The tool never prints the key.
//
// Exit status: 0 on success; 1 when a Stripe call fails or the Customer is
// refused; 2 on a usage or key error, before any request.
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

	"github.com/joho/godotenv"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/billing"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

const (
	keyEnv = "STRIPE_SECRET_KEY"

	// The metadata Checkout writes on every Customer it creates
	// (internal/billing/checkout_customer.go).
	metadataUserID = "concord_user_id"
	metadataEnv    = "concord_env"

	// soakEnvironment is the only concord_env the soak runs under (spec §16).
	soakEnvironment = "development"

	// clockLead puts the default frozen_time a little ahead of this machine's
	// clock: Stripe refuses a frozen_time in its own past for an existing
	// Customer, and the two clocks are never exactly equal.
	clockLead = time.Minute

	runTimeout = time.Minute
)

// stripeAPIBaseURL is empty outside tests, which keeps stripe-go's default
// host. A package variable rather than a flag, so no input can send the key
// elsewhere.
var stripeAPIBaseURL = ""

// now is replaced by tests.
var now = time.Now

func main() {
	_ = godotenv.Load() // the dev stack's .env, as the control plane loads it; a set variable wins
	os.Exit(runMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "attach-clock" {
		_, _ = fmt.Fprintln(stderr, "usage: stripe-soak attach-clock --customer cus_… [--frozen-time RFC3339]")
		return 2
	}
	fs := flag.NewFlagSet("attach-clock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	customer := fs.String("customer", "", "the Checkout-created test Customer to attach (required)")
	frozen := fs.String("frozen-time", "", "the clock's start time, RFC 3339, not in the past (default: one minute from now)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || !strings.HasPrefix(*customer, "cus_") {
		_, _ = fmt.Fprintln(stderr, "attach-clock: --customer cus_… is required and takes no other argument")
		return 2
	}
	frozenAt := now().Add(clockLead)
	if *frozen != "" {
		t, err := time.Parse(time.RFC3339, *frozen)
		if err != nil || t.Before(now()) {
			_, _ = fmt.Fprintln(stderr, "attach-clock: --frozen-time must be RFC 3339 and not in the past")
			return 2
		}
		frozenAt = t
	}

	key := strings.TrimSpace(os.Getenv(keyEnv))
	mode, _, err := config.ParseStripeKeyMode(key)
	if err != nil || mode != config.StripeKeyModeTest {
		_, _ = fmt.Fprintf(stderr, "attach-clock: %s must be a test-mode key (rk_test_ or sk_test_)\n", keyEnv)
		return 2
	}

	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	client := billing.NewClient(key, stripeAPIBaseURL)
	clock, err := attachClock(ctx, client, *customer, frozenAt)
	if err != nil {
		reportFailure(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "attached %s to test clock %s (frozen at %s)\n",
		*customer, clock.ID, time.Unix(clock.FrozenTime, 0).UTC().Format(time.RFC3339))
	_, _ = fmt.Fprintln(stdout, "next: reopen Checkout as the same user; advance the clock in the Dashboard (Billing → Test clocks)")
	return 0
}

// errRefused marks a Customer the tool will not attach.
var errRefused = errors.New("refused")

// attachClock checks that cus is a live, clock-free Customer made by a dev
// stack's Checkout, then creates a clock with it attached.
func attachClock(ctx context.Context, client *stripe.Client, cus string, frozenAt time.Time) (*stripe.TestHelpersTestClock, error) {
	c, err := client.V1Customers.Retrieve(ctx, cus, nil)
	if err != nil {
		return nil, fmt.Errorf("retrieve customer: %w", err)
	}
	switch {
	case c.Deleted:
		return nil, fmt.Errorf("%w: %s is deleted", errRefused, cus)
	case c.TestClock != nil:
		return nil, fmt.Errorf("%w: %s is already on test clock %s", errRefused, cus, c.TestClock.ID)
	case c.Metadata[metadataUserID] == "":
		return nil, fmt.Errorf("%w: %s has no %s, so Checkout did not create it", errRefused, cus, metadataUserID)
	case c.Metadata[metadataEnv] != soakEnvironment:
		return nil, fmt.Errorf("%w: %s was created with %s=%q, not %q", errRefused, cus, metadataEnv, c.Metadata[metadataEnv], soakEnvironment)
	}
	clock, err := client.V1TestHelpersTestClocks.Create(ctx, &stripe.TestHelpersTestClockCreateParams{
		Customer:   stripe.String(cus),
		FrozenTime: stripe.Int64(frozenAt.Unix()),
		Name:       stripe.String("soak " + cus),
	})
	if err != nil {
		return nil, fmt.Errorf("create test clock: %w", err)
	}
	return clock, nil
}

// reportFailure writes err to stderr. Stripe's text for a rejected key quotes
// the key's prefix and last four characters, so a 401/403 gets a fixed line.
func reportFailure(stderr io.Writer, err error) {
	var se *stripe.Error
	if errors.As(err, &se) && (se.HTTPStatusCode == http.StatusUnauthorized || se.HTTPStatusCode == http.StatusForbidden) {
		_, _ = fmt.Fprintf(stderr, "attach-clock: Stripe rejected %s (HTTP %d); it needs Customers read and Test clocks write\n", keyEnv, se.HTTPStatusCode)
		return
	}
	_, _ = fmt.Fprintf(stderr, "attach-clock: %v\n", err)
}
