package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/redemption"
)

// fakeIssuer is a test double for codeIssuer. It records the spec it was called
// with and returns a canned result, so runIssue's orchestration can be exercised
// without a real DB handle.
type fakeIssuer struct {
	gotSpec redemption.IssueSpec
	codes   []redemption.IssuedCode
	err     error
}

func (f *fakeIssuer) Issue(_ context.Context, spec redemption.IssueSpec) ([]redemption.IssuedCode, error) {
	f.gotSpec = spec
	if f.err != nil {
		return nil, f.err
	}
	return f.codes, nil
}

func sampleCodes(n int) []redemption.IssuedCode {
	codes := make([]redemption.IssuedCode, n)
	for i := range codes {
		codes[i] = redemption.IssuedCode{ID: uuid.New(), Plaintext: "KS-AAAA-BBBB-CCCC"}
	}
	return codes
}

// TestRunIssue_HappyPathStdout: the orchestration builds the spec, mints codes,
// writes the plaintexts to stdout, and prints the once-only banner to stderr.
func TestRunIssue_HappyPathStdout(t *testing.T) {
	fi := &fakeIssuer{codes: sampleCodes(3)}
	var out, errOut bytes.Buffer

	opts := issueOptions{
		GrantKind:  redemption.GrantPremiumSubscription,
		Months:     12,
		Count:      3,
		Prefix:     "KS",
		SingleUse:  true,
		MaxRedeems: "1",
		BatchID:    "ks-2026",
	}
	err := runIssue(context.Background(), opts, fi, &out, &errOut)
	require.NoError(t, err)

	// Three plaintext lines on stdout.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	assert.Len(t, lines, 3)
	for _, l := range lines {
		assert.Equal(t, "KS-AAAA-BBBB-CCCC", l)
	}

	// Banner on stderr, never on stdout.
	assert.Contains(t, errOut.String(), "Generated 3 code(s)")
	assert.Contains(t, errOut.String(), "shown ONCE")

	// The spec the issuer received reflects the options (including months params
	// and the CLI context).
	assert.Equal(t, redemption.GrantPremiumSubscription, fi.gotSpec.GrantKind)
	assert.Equal(t, 3, fi.gotSpec.Count)
	assert.Equal(t, "KS", fi.gotSpec.Prefix)
	assert.Equal(t, "ks-2026", fi.gotSpec.BatchID)
	assert.Equal(t, redemption.IssuerContextCLI, fi.gotSpec.Context)
	require.NotNil(t, fi.gotSpec.GrantParams)
	assert.Equal(t, 12, fi.gotSpec.GrantParams["months"])
	require.NotNil(t, fi.gotSpec.MaxRedeems)
	assert.Equal(t, 1, *fi.gotSpec.MaxRedeems)
}

// TestRunIssue_HappyPathCSV: with a CSV path, the codes are written to the file
// (header + rows) and stdout stays empty; the banner reports the file.
func TestRunIssue_HappyPathCSV(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "codes.csv")
	fi := &fakeIssuer{codes: sampleCodes(2)}
	var out, errOut bytes.Buffer

	opts := issueOptions{
		GrantKind:  "feature:custom_themes",
		Count:      2,
		SingleUse:  true,
		MaxRedeems: "1",
		BatchID:    "csv-batch",
		CSVPath:    csvPath,
	}
	err := runIssue(context.Background(), opts, fi, &out, &errOut)
	require.NoError(t, err)

	// Stdout is empty (CSV path); banner + file report on stderr.
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "Wrote 2 code(s) to "+csvPath)
	assert.Contains(t, errOut.String(), "Generated 2 code(s)")

	contents, readErr := os.ReadFile(csvPath) //nolint:gosec // test-controlled temp path
	require.NoError(t, readErr)
	csv := string(contents)
	assert.Contains(t, csv, "code,batch_id,grant_kind") // header
	assert.Contains(t, csv, "KS-AAAA-BBBB-CCCC,csv-batch,feature:custom_themes")
	// Header + 2 rows = 3 non-empty lines.
	rows := strings.Split(strings.TrimSpace(csv), "\n")
	assert.Len(t, rows, 3)
}

// TestRunIssue_IssuerError: an issuer failure is wrapped and surfaced (no panic,
// no stdout), so main()'s log.Fatal path is exercised by a returned error.
func TestRunIssue_IssuerError(t *testing.T) {
	fi := &fakeIssuer{err: errors.New("boom")}
	var out, errOut bytes.Buffer

	err := runIssue(context.Background(), issueOptions{
		GrantKind: redemption.GrantPremiumSubscription, Count: 1, MaxRedeems: "1",
	}, fi, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "code generation failed")
	assert.Contains(t, err.Error(), "boom")
	assert.Empty(t, out.String(), "no codes printed on issuer failure")
}

// TestRunIssue_InvalidOptions: a bad --max-redeems is caught by buildSpec BEFORE
// the issuer is reached (the fake records no call), and the error is wrapped.
func TestRunIssue_InvalidOptions(t *testing.T) {
	fi := &fakeIssuer{codes: sampleCodes(1)}
	var out, errOut bytes.Buffer

	err := runIssue(context.Background(), issueOptions{
		GrantKind: redemption.GrantPremiumSubscription, Count: 1, MaxRedeems: "not-a-number",
	}, fi, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid options")
	// The issuer was never called: its recorded spec is the zero value.
	assert.Empty(t, fi.gotSpec.GrantKind, "buildSpec must fail before Issue is called")
	assert.Empty(t, out.String())
}

// TestRunIssue_CSVCreateError: an unwritable CSV path surfaces a wrapped
// "failed to write codes" error after a successful mint (the codes exist, the
// export fails).
func TestRunIssue_CSVCreateError(t *testing.T) {
	fi := &fakeIssuer{codes: sampleCodes(1)}
	var out, errOut bytes.Buffer

	// A path whose parent directory does not exist → os.Create fails.
	badPath := filepath.Join(t.TempDir(), "no-such-dir", "codes.csv")
	err := runIssue(context.Background(), issueOptions{
		GrantKind: "feature:custom_themes", Count: 1, MaxRedeems: "1", CSVPath: badPath,
	}, fi, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to write codes")
}

// TestBuildSpec covers the option→spec conversion branches: months params,
// expiry parsing (valid + invalid), and the unlimited (empty max-redeems) case.
func TestBuildSpec(t *testing.T) {
	t.Run("premium with months and expiry", func(t *testing.T) {
		spec, err := buildSpec(issueOptions{
			GrantKind:  redemption.GrantPremiumSubscription,
			Months:     6,
			Count:      4,
			Prefix:     "PROMO",
			SingleUse:  false,
			MaxRedeems: "10",
			Expires:    "2026-12-31T23:59:59Z",
			BatchID:    "b1",
		})
		require.NoError(t, err)
		assert.Equal(t, redemption.GrantPremiumSubscription, spec.GrantKind)
		assert.Equal(t, 4, spec.Count)
		assert.Equal(t, "PROMO", spec.Prefix)
		assert.False(t, spec.SingleUse)
		require.NotNil(t, spec.MaxRedeems)
		assert.Equal(t, 10, *spec.MaxRedeems)
		require.NotNil(t, spec.ExpiresAt)
		assert.Equal(t, 2026, spec.ExpiresAt.Year())
		require.NotNil(t, spec.GrantParams)
		assert.Equal(t, 6, spec.GrantParams["months"])
		assert.Equal(t, redemption.IssuerContextCLI, spec.Context)
	})

	t.Run("unlimited (empty max-redeems) → nil MaxRedeems", func(t *testing.T) {
		spec, err := buildSpec(issueOptions{
			GrantKind: "feature:custom_themes", Count: 1, MaxRedeems: "unlimited",
			Expires: "2026-12-31T23:59:59Z",
		})
		require.NoError(t, err)
		assert.Nil(t, spec.MaxRedeems, "unlimited → nil pointer")
		assert.Nil(t, spec.GrantParams, "no months → nil params")
	})

	t.Run("invalid max-redeems rejected", func(t *testing.T) {
		_, err := buildSpec(issueOptions{GrantKind: "x", Count: 1, MaxRedeems: "zero"})
		assert.Error(t, err)
	})

	t.Run("invalid expiry rejected", func(t *testing.T) {
		_, err := buildSpec(issueOptions{
			GrantKind: "x", Count: 1, MaxRedeems: "1", Expires: "not-a-date",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "want RFC3339")
	})
}

// TestEmit_Stdout writes plaintext lines to the out writer and nothing to errOut.
func TestEmit_Stdout(t *testing.T) {
	var out, errOut bytes.Buffer
	codes := []redemption.IssuedCode{
		{Plaintext: "AAA"}, {Plaintext: "BBB"},
	}
	err := emit(&out, &errOut, codes, "", "batch", "kind")
	require.NoError(t, err)
	assert.Equal(t, "AAA\nBBB\n", out.String())
	assert.Empty(t, errOut.String())
}

// TestEmit_CSV writes a CSV file and reports it on errOut (not out).
func TestEmit_CSV(t *testing.T) {
	var out, errOut bytes.Buffer
	csvPath := filepath.Join(t.TempDir(), "out.csv")
	codes := []redemption.IssuedCode{{Plaintext: "ZZZ"}}

	err := emit(&out, &errOut, codes, csvPath, "the-batch", "premium:subscription")
	require.NoError(t, err)
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "Wrote 1 code(s) to "+csvPath)

	contents, readErr := os.ReadFile(csvPath) //nolint:gosec // test-controlled temp path
	require.NoError(t, readErr)
	assert.Contains(t, string(contents), "ZZZ,the-batch,premium:subscription")
}

// TestEmit_CSVCreateError returns a wrapped error when the path is unwritable.
func TestEmit_CSVCreateError(t *testing.T) {
	var out, errOut bytes.Buffer
	badPath := filepath.Join(t.TempDir(), "missing-dir", "out.csv")
	err := emit(&out, &errOut, sampleCodes(1), badPath, "b", "k")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create csv")
}

// TestParseFlags exercises the flag→options mapping with a synthetic FlagSet and
// argument slice. This pins that every flag lands on the matching option field.
func TestParseFlags(t *testing.T) {
	fs := flag.NewFlagSet("issue-codes", flag.ContinueOnError)
	opts := parseFlags(fs, []string{
		"--grant-kind=premium:subscription",
		"--months=3",
		"--count=7",
		"--prefix=KS",
		"--single-use=false",
		"--max-redeems=unlimited",
		"--expires=2026-01-01T00:00:00Z",
		"--batch-id=bx",
		"--csv=/tmp/x.csv",
	})
	assert.Equal(t, "premium:subscription", opts.GrantKind)
	assert.Equal(t, 3, opts.Months)
	assert.Equal(t, 7, opts.Count)
	assert.Equal(t, "KS", opts.Prefix)
	assert.False(t, opts.SingleUse)
	assert.Equal(t, "unlimited", opts.MaxRedeems)
	assert.Equal(t, "2026-01-01T00:00:00Z", opts.Expires)
	assert.Equal(t, "bx", opts.BatchID)
	assert.Equal(t, "/tmp/x.csv", opts.CSVPath)
}

// TestParseFlags_Defaults pins the zero-arg defaults (single-use true, count 1,
// max-redeems "1").
func TestParseFlags_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("issue-codes", flag.ContinueOnError)
	opts := parseFlags(fs, nil)
	assert.Empty(t, opts.GrantKind)
	assert.Equal(t, 1, opts.Count)
	assert.True(t, opts.SingleUse)
	assert.Equal(t, "1", opts.MaxRedeems)
}

// devDBPassword matches the docker-compose dev default. Assembled separately from
// the URL to satisfy static credential analysis (S6698/S2068), mirroring
// testhelpers.go.
var devDBPassword = "concord_dev_password" //nolint:gosec // test-only, matches docker-compose dev default // pragma: allowlist secret

// devDatabaseURL mirrors testhelpers' default-URL resolution (env override →
// docker-compose dev default) so connectDB can be exercised against the running
// dev PostgreSQL without importing the test-only helper.
func devDatabaseURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://concord:" + devDBPassword + "@localhost:5432/concord?sslmode=disable"
}

// TestConnectDB_Success opens + pings the dev database. This is the same
// connection path openDB takes (openDB just wraps it with config.Load +
// log.Fatal). Skips cleanly if the dev DB is unreachable.
func TestConnectDB_Success(t *testing.T) {
	db, err := connectDB(devDatabaseURL())
	if err != nil {
		t.Skipf("dev database unreachable, skipping: %v", err)
	}
	require.NotNil(t, db)
	t.Cleanup(func() { _ = db.Close() })
	assert.NoError(t, db.Ping())
}

// TestConnectDB_PingFailure: a syntactically valid DSN pointing at a dead port
// opens lazily but fails on Ping, surfacing the wrapped "failed to ping" error.
func TestConnectDB_PingFailure(t *testing.T) {
	// Port 1 is reserved and not listening; sql.Open is lazy so the error comes
	// from the eager Ping inside connectDB.
	db, err := connectDB("postgres://nobody:nobody@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to ping database")
	assert.Nil(t, db, "a failed connectDB returns a nil handle (it closes the lazy one)")
}

// TestConnectDB_OpenFailure: a malformed DSN fails at sql.Open (the parse stage)
// before any network I/O.
func TestConnectDB_OpenFailure(t *testing.T) {
	db, err := connectDB("postgres://%zz-invalid")
	require.Error(t, err)
	assert.Nil(t, db)
}
