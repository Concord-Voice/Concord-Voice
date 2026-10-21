package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/billing"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// None of these tests may call t.Parallel: they set env vars and the
// package-level stripeAPIBaseURL.

const fakeTestKey = "sk_test_fake"

const duplicateProductsBody = `{"object":"list","url":"/v1/products","has_more":false,"data":[` +
	`{"id":"prod_A","object":"product","active":true,"metadata":{"concord_product":"supersonic"}},` +
	`{"id":"prod_B","object":"product","active":true,"metadata":{"concord_product":"supersonic"}}]}`

// stripeStub answers the bootstrap's four reads as an empty Stripe account
// and refuses every write with a 500. fail turns every request into a 500,
// rejectStatus turns every request into a key rejection (401 or 403) whose
// message is rejectMessage, and productsBody overrides the product list.
type stripeStub struct {
	t             *testing.T
	mu            sync.Mutex
	fail          bool
	rejectStatus  int
	rejectMessage string
	productsBody  string
	requests      int
	writes        int
	authHeaders   []string
}

func (s *stripeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	s.authHeaders = append(s.authHeaders, r.Header.Get("Authorization"))
	status, body := http.StatusOK, `{"object":"list","url":"/v1/stub","has_more":false,"data":[]}`
	switch {
	case s.rejectStatus != 0:
		status, body = s.rejectStatus, fmt.Sprintf(`{"error":{"type":"invalid_request_error","message":%q}}`, s.rejectMessage)
	case s.fail:
		status, body = http.StatusInternalServerError, `{"error":{"type":"api_error","message":"stub outage"}}`
	case r.Method != http.MethodGet:
		s.writes++
		status, body = http.StatusInternalServerError, `{"error":{"type":"api_error","message":"stub refuses writes"}}`
	case r.URL.Path == "/v1/products" && s.productsBody != "":
		body = s.productsBody
	case strings.HasPrefix(r.URL.Path, "/v1/coupons/"):
		status, body = http.StatusNotFound, `{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such coupon"}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Stripe-Should-Retry", "false")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		s.t.Errorf("stub write: %v", err)
	}
}

func (s *stripeStub) counts() (requests, writes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.writes
}

// useStub serves stub and points runMain at it for the rest of the test.
func useStub(t *testing.T, stub *stripeStub) {
	t.Helper()
	stub.t = t
	srv := httptest.NewServer(stub)
	prev := stripeAPIBaseURL
	stripeAPIBaseURL = srv.URL
	t.Cleanup(func() {
		stripeAPIBaseURL = prev
		srv.Close()
	})
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	require.NotEmpty(t, stripeAPIBaseURL, "runCLI must follow useStub: an empty base URL sends requests to api.stripe.com")
	var out, errOut bytes.Buffer
	code = runMain(t.Context(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestParseFlags_Valid(t *testing.T) {
	cases := map[string]struct {
		args []string
		want bootstrapOptions
	}{
		"test":         {[]string{"--mode", "test"}, bootstrapOptions{Mode: "test"}},
		"live dry run": {[]string{"--mode=live", "--dry-run"}, bootstrapOptions{Mode: "live", DryRun: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fs := flag.NewFlagSet("stripe-bootstrap", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			got, err := parseFlags(fs, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRunMain_UsageErrorsExitTwo(t *testing.T) {
	cases := map[string]struct {
		args       []string
		wantStderr string
	}{
		"mode missing":        {nil, "--mode must be"},
		"mode unknown":        {[]string{"--mode", "staging"}, "--mode must be"},
		"mode upper case":     {[]string{"--mode", "TEST"}, "--mode must be"},
		"unknown flag":        {[]string{"--mode", "test", "--force"}, "flag provided but not defined: -force"},
		"positional argument": {[]string{"--mode", "test", "extra"}, "unexpected argument(s): extra"},
		"bad dry-run value":   {[]string{"--mode", "test", "--dry-run=maybe"}, "invalid boolean value"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(catalogKeyEnv, fakeTestKey)
			stub := &stripeStub{}
			useStub(t, stub)

			code, stdout, stderr := runCLI(t, tc.args...)

			assert.Equal(t, 2, code)
			assert.Contains(t, stderr, tc.wantStderr)
			assert.Empty(t, stdout)
			requests, _ := stub.counts()
			assert.Zero(t, requests, "a usage error must stop before any Stripe request")
		})
	}
}

// TestRunMain_KeyErrorsExitTwo covers the key rules: STRIPE_CATALOG_KEY only
// (never STRIPE_SECRET_KEY), no publishable key, and --mode equal to the
// key's mode. None of them may print the key or reach Stripe.
func TestRunMain_KeyErrorsExitTwo(t *testing.T) {
	cases := map[string]struct {
		catalogKey string
		secretKey  string
		mode       string
		wantStderr string
	}{
		"catalog key unset, runtime key set": {"", "sk_test_runtime", "test", "STRIPE_CATALOG_KEY is not set"},
		"publishable key":                    {"pk_test_fake", "", "test", "STRIPE_CATALOG_KEY: " + config.ErrInvalidStripeKey.Error()},
		"test key with --mode live":          {fakeTestKey, "", "live", "--mode live does not match the test-mode key"},
		"live key with --mode test":          {"rk_live_fake", "", "test", "--mode test does not match the live-mode key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(catalogKeyEnv, tc.catalogKey)
			t.Setenv("STRIPE_SECRET_KEY", tc.secretKey)
			stub := &stripeStub{}
			useStub(t, stub)

			code, stdout, stderr := runCLI(t, "--mode", tc.mode)

			assert.Equal(t, 2, code)
			assert.Contains(t, stderr, tc.wantStderr)
			for _, key := range []string{tc.catalogKey, tc.secretKey} {
				if key != "" {
					assert.NotContains(t, stdout+stderr, key, "a key must never be printed")
				}
			}
			requests, _ := stub.counts()
			assert.Zero(t, requests, "a key error must stop before any Stripe request")
		})
	}
}

func TestRunMain_DryRunAgainstStub(t *testing.T) {
	// A dotenv writer's trailing CR/LF is trimmed before the key is used.
	t.Setenv(catalogKeyEnv, fakeTestKey+"\r\n")
	stub := &stripeStub{}
	useStub(t, stub)

	code, stdout, stderr := runCLI(t, "--mode", "test", "--dry-run")

	require.Equal(t, 0, code, "stderr: %s", stderr)
	requests, writes := stub.counts()
	assert.Equal(t, 5, requests, "products, active prices, archived prices, coupon and portal configurations are read once each")
	assert.Zero(t, writes, "a dry run must not write")
	stub.mu.Lock()
	assert.Equal(t, []string{"Bearer sk_test_fake", "Bearer sk_test_fake", "Bearer sk_test_fake", "Bearer sk_test_fake", "Bearer sk_test_fake"}, stub.authHeaders)
	stub.mu.Unlock()

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.Len(t, lines, 9)
	assert.Equal(t, []string{"would-create", "product", "supersonic"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"would-create", "price", "wing_yearly"}, strings.Fields(lines[5]))
	assert.Equal(t, []string{"would-create", "coupon", "founders-50"}, strings.Fields(lines[6]))
	assert.Equal(t, []string{"would-create", "portal_configuration", "v1"}, strings.Fields(lines[7]))
	assert.Equal(t, "mode=test dry_run=true create=8 update=0 unchanged=0", lines[8])
	assert.Empty(t, stderr)
	assert.NotContains(t, stdout+stderr, fakeTestKey)
}

// failOnceWriter fails only its failAt-th write (counting from 0) and accepts
// the rest, so a later successful write cannot hide an earlier lost line.
type failOnceWriter struct {
	failAt, n int
}

func (w *failOnceWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n-1 == w.failAt {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

func TestRunMain_ReportWriteFailureExitsOne(t *testing.T) {
	t.Setenv(catalogKeyEnv, fakeTestKey)
	useStub(t, &stripeStub{})
	for _, failAt := range []int{0, 8} { // the first object line; the summary line
		t.Run(fmt.Sprintf("write %d fails", failAt), func(t *testing.T) {
			out := &failOnceWriter{failAt: failAt}
			var errOut bytes.Buffer

			code := runMain(t.Context(), []string{"--mode", "test", "--dry-run"}, out, &errOut)

			assert.Equal(t, 1, code, "a lost report must not exit 0")
			assert.Equal(t, "stripe-bootstrap: writing the report failed: short write; rerun to print every object\n", errOut.String())
		})
	}
}

func TestRunMain_StripeFailureExitsOne(t *testing.T) {
	t.Setenv(catalogKeyEnv, fakeTestKey)
	useStub(t, &stripeStub{fail: true})

	code, stdout, stderr := runCLI(t, "--mode", "test")

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "stripe-bootstrap: billing: bootstrap: list products: ")
	assert.NotContains(t, stderr, "no write was made")
	assert.Equal(t, "mode=test dry_run=false create=0 update=0 unchanged=0\n", stdout)
	assert.NotContains(t, stdout+stderr, fakeTestKey)
}

// TestRunMain_RejectedKeyPrintsFixedLine covers Stripe's 401 and 403 answers to
// the catalog key. Stripe's message quotes the key with its prefix and last four
// characters, and stripe-go's error string carries it verbatim, so none of that
// text may reach stdout or stderr: only the fixed line, which names the variable
// and the HTTP status.
func TestRunMain_RejectedKeyPrintsFixedLine(t *testing.T) {
	cases := map[string]struct {
		status  int
		message string
	}{
		"401 invalid key":        {http.StatusUnauthorized, "Invalid API Key provided: sk_test_****fake"},
		"403 missing permission": {http.StatusForbidden, "The provided key 'sk_test_****fake' does not have the required permissions for this endpoint"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(catalogKeyEnv, fakeTestKey)
			stub := &stripeStub{rejectStatus: tc.status, rejectMessage: tc.message}
			useStub(t, stub)

			code, stdout, stderr := runCLI(t, "--mode", "test")

			assert.Equal(t, 1, code)
			assert.Contains(t, stderr, fmt.Sprintf(
				"stripe-bootstrap: Stripe rejected the key in STRIPE_CATALOG_KEY (HTTP %d) during billing: bootstrap: list products; check that it is current and has the permissions in [internal]stripe-billing.md\n", tc.status))
			assert.Equal(t, "mode=test dry_run=false create=0 update=0 unchanged=0\n", stdout)
			for _, leak := range []string{tc.message, "****fake", fakeTestKey} {
				assert.NotContains(t, stdout+stderr, leak, "a rejected key must never echo Stripe's message or the key")
			}
			requests, _ := stub.counts()
			assert.Equal(t, 1, requests, "a rejected key stops the run at its first request")
		})
	}
}

func TestRunMain_CatalogConflictExitsOne(t *testing.T) {
	t.Setenv(catalogKeyEnv, fakeTestKey)
	stub := &stripeStub{productsBody: duplicateProductsBody}
	useStub(t, stub)

	code, _, stderr := runCLI(t, "--mode", "test")

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "products prod_A and prod_B both carry metadata concord_product=supersonic")
	assert.Contains(t, stderr, "no write was made")
	_, writes := stub.counts()
	assert.Zero(t, writes)
}

// TestPrintReport_VerbPerAction pins the verb printed for every Action in a
// real run and in a dry run. The stub tests above reach only would-create, so
// without this a swapped created/updated/unchanged verb would stay green.
func TestPrintReport_VerbPerAction(t *testing.T) {
	steps := []billing.BootstrapStep{
		{Kind: billing.KindProduct, Key: "supersonic", ID: "prod_S", Action: billing.ActionUnchanged},
		{Kind: billing.KindPrice, Key: "wing_yearly", ID: "price_W", Action: billing.ActionCreate},
		{Kind: billing.KindPortalConfiguration, Key: "v1", ID: "bpc_1", Action: billing.ActionUpdate},
	}
	cases := map[string]struct {
		dryRun bool
		verbs  []string
	}{
		"real run": {false, []string{"unchanged", "created", "updated"}},
		"dry run":  {true, []string{"unchanged", "would-create", "would-update"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, printReport(&out, "live", billing.BootstrapReport{DryRun: tc.dryRun, Steps: steps}))

			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			require.Len(t, lines, len(steps)+1)
			for i, s := range steps {
				assert.Equal(t, []string{tc.verbs[i], s.Kind, s.Key, s.ID}, strings.Fields(lines[i]))
			}
			assert.Equal(t, fmt.Sprintf("mode=live dry_run=%t create=1 update=1 unchanged=1", tc.dryRun), lines[len(steps)])
		})
	}
}

// TestReportFailure_RejectedKeyNamesTheOperationNotStripesText covers the
// wrapped form a create call returns: billing: bootstrap: create product
// <plan>: <stripe error>. The fixed line names the operation, so the operator
// can tell which permission the key lacks, and carries nothing of Stripe's
// message, which quotes the key's prefix and last four characters.
func TestReportFailure_RejectedKeyNamesTheOperationNotStripesText(t *testing.T) {
	const stripeMessage = "Invalid API Key provided: rk_test_****1234"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("HTTP %d at the tail of the chain", status), func(t *testing.T) {
			stripeErr := &stripe.Error{HTTPStatusCode: status, Msg: stripeMessage}
			wrapped := fmt.Errorf("billing: bootstrap: create product %s: %w", "supersonic", stripeErr)
			require.Contains(t, wrapped.Error(), "rk_test_****1234", "control: the raw error text does carry the key")

			var stderr bytes.Buffer
			reportFailure(&stderr, wrapped)

			assert.Equal(t, fmt.Sprintf(
				"stripe-bootstrap: Stripe rejected the key in STRIPE_CATALOG_KEY (HTTP %d) during billing: bootstrap: create product supersonic; check that it is current and has the permissions in [internal]stripe-billing.md\n", status),
				stderr.String())
			for _, leak := range []string{stripeMessage, "rk_test_", "****1234", "Invalid API Key"} {
				assert.NotContains(t, stderr.String(), leak)
			}
		})
		t.Run(fmt.Sprintf("HTTP %d not at the tail of the chain", status), func(t *testing.T) {
			stripeErr := &stripe.Error{HTTPStatusCode: status, Msg: stripeMessage}
			wrapped := fmt.Errorf("billing: bootstrap: create product %s: %w: while retrying", "supersonic", stripeErr)

			var stderr bytes.Buffer
			reportFailure(&stderr, wrapped)

			assert.Equal(t, fmt.Sprintf(
				"stripe-bootstrap: Stripe rejected the key in STRIPE_CATALOG_KEY (HTTP %d); check that it is current and has the permissions in [internal]stripe-billing.md\n", status),
				stderr.String(), "no during clause when the operation prefix cannot be split off")
			assert.NotContains(t, stderr.String(), "rk_test_")
			assert.NotContains(t, stderr.String(), "Invalid API Key")
		})
	}
}
