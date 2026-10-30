// Package main verifies a protected Sonar caller proof before PR data is prepared.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	githubIssuer = "https://token.actions.githubusercontent.com"
	repository   = "Concord-Voice/Concord-Voice-Alpha"
	repositoryID = "1158152129"
	// A ruleset-loaded required workflow signs its ref as @main, not
	// @refs/heads/main (observed on the 2026-10-30 canary).
	workflowRef  = repository + "/.github/workflows/required-pr-build.yml@main"
	audienceBase = "concord-voice:protected-sonar:sha256:"
	maxProofSize = 64 << 10
	maxPayload   = 24 << 10
	maxTokenSize = 32 << 10
	maxOutput    = 32 << 10
	// GitHub backdates nbf five minutes before iat (observed 2026-10-30); allow
	// up to the lifetime bound so a modest change in that offset still verifies.
	maxNotBeforeSkew = 10 * time.Minute
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type proofFile struct {
	PayloadB64 string `json:"payload_b64"`
	OIDCJWT    string `json:"oidc_jwt"`
}

type payload struct {
	CallerRunID      int64                      `json:"caller_run_id"`
	CallerRunAttempt int64                      `json:"caller_run_attempt"`
	PRNumber         int64                      `json:"pr_number"`
	ExpectedHeadSHA  string                     `json:"expected_head_sha"`
	ExpectedBaseSHA  string                     `json:"expected_base_sha"`
	PRCIResult       string                     `json:"pr_ci_result"`
	Selection        json.RawMessage            `json:"selection"`
	Jobs             map[string]string          `json:"jobs"`
	Coverage         map[string]json.RawMessage `json:"coverage"`
}

type signedClaims struct {
	RepositoryID string `json:"repository_id"`
	Repository   string `json:"repository"`
	EventName    string `json:"event_name"`
	RunID        string `json:"run_id"`
	RunAttempt   string `json:"run_attempt"`
	WorkflowRef  string `json:"workflow_ref"`
	WorkflowSHA  string `json:"workflow_sha"`
	Subject      string `json:"sub"`
	NotBefore    int64  `json:"nbf"`
}

type verifiedClaims struct {
	RepositoryID string `json:"repository_id"`
	Repository   string `json:"repository"`
	EventName    string `json:"event_name"`
	RunID        string `json:"run_id"`
	RunAttempt   string `json:"run_attempt"`
	WorkflowRef  string `json:"workflow_ref"`
	WorkflowSHA  string `json:"workflow_sha"`
	Subject      string `json:"subject"`
}

type verifiedOutput struct {
	Payload json.RawMessage `json:"payload"`
	Claims  verifiedClaims  `json:"claims"`
}

func main() {
	var proofPath, outPath string
	flag.StringVar(&proofPath, "proof", "", "path to caller proof")
	flag.StringVar(&outPath, "out", "", "path for verified claims")
	flag.Parse()
	if proofPath == "" || outPath == "" || flag.NArg() != 0 {
		if _, err := fmt.Fprintln(os.Stderr, "usage: verify-sonar-caller-proof --proof PROOF --out VERIFIED"); err != nil {
			os.Exit(1)
		}
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(ctx, proofPath, outPath, githubIssuer); err != nil {
		// Errors are fixed classes: neither a JWT nor a response body reaches stderr.
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, proofPath, outPath, issuer string) error {
	// #nosec G304 -- --proof is an intentional CLI path; its bytes are bounded and authenticated.
	file, err := os.Open(proofPath)
	if err != nil {
		return errors.New("cannot open proof")
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxProofSize+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > maxProofSize {
		return errors.New("invalid proof size")
	}
	verified, err := verifyProof(ctx, raw, issuer, time.Now)
	if err != nil {
		return err
	}
	if err := writeVerified(outPath, verified); err != nil {
		return errors.New("cannot write verified claims")
	}
	return nil
}

// The authority invariant: only a GitHub-signed token from this repository's
// protected workflow can authenticate the exact canonical payload bytes.
func verifyProof(ctx context.Context, raw []byte, issuer string, now func() time.Time) ([]byte, error) {
	var envelope proofFile
	if err := strictJSON(raw, &envelope); err != nil || envelope.PayloadB64 == "" || envelope.OIDCJWT == "" || len(envelope.OIDCJWT) > maxTokenSize {
		return nil, errors.New("invalid proof envelope")
	}
	payloadBytes, err := base64.StdEncoding.Strict().DecodeString(envelope.PayloadB64)
	if err != nil || len(payloadBytes) == 0 || len(payloadBytes) > maxPayload || base64.StdEncoding.EncodeToString(payloadBytes) != envelope.PayloadB64 {
		return nil, errors.New("invalid payload encoding")
	}
	digest := sha256.Sum256(payloadBytes)
	expectedAudience := audienceBase + hex.EncodeToString(digest[:])

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	token, err := provider.VerifierContext(ctx, &oidc.Config{
		ClientID:             expectedAudience,
		Now:                  now,
		SupportedSigningAlgs: []string{"RS256"},
	}).Verify(ctx, envelope.OIDCJWT)
	if err != nil || len(token.Audience) != 1 || token.Audience[0] != expectedAudience {
		return nil, errors.New("OIDC verification failed")
	}
	p, err := parsePayload(payloadBytes)
	if err != nil {
		return nil, err
	}
	var claims signedClaims
	if err := token.Claims(&claims); err != nil || !validClaims(token, claims, p, now()) {
		return nil, errors.New("OIDC claims rejected")
	}
	out, err := json.Marshal(verifiedOutput{
		Payload: payloadBytes,
		Claims: verifiedClaims{
			RepositoryID: claims.RepositoryID,
			Repository:   claims.Repository,
			EventName:    claims.EventName,
			RunID:        claims.RunID,
			RunAttempt:   claims.RunAttempt,
			WorkflowRef:  claims.WorkflowRef,
			WorkflowSHA:  claims.WorkflowSHA,
			Subject:      claims.Subject,
		},
	})
	if err != nil || len(out) > maxOutput {
		return nil, errors.New("verified output exceeds limit")
	}
	return append(out, '\n'), nil
}

func parsePayload(raw []byte) (payload, error) {
	var p payload
	if err := strictJSON(raw, &p); err != nil {
		return p, errors.New("invalid payload")
	}
	// Re-encoding rejects duplicates, extra keys, alternate number spellings,
	// and any noncanonical whitespace or key order before the digest is trusted.
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return p, errors.New("invalid payload")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, raw) {
		return p, errors.New("noncanonical payload")
	}
	if p.CallerRunID <= 0 || p.CallerRunAttempt <= 0 || p.PRNumber <= 0 ||
		!shaPattern.MatchString(p.ExpectedHeadSHA) || !shaPattern.MatchString(p.ExpectedBaseSHA) ||
		p.PRCIResult != "success" || len(p.Selection) == 0 || p.Selection[0] != '{' {
		return p, errors.New("invalid payload identifiers")
	}
	for _, name := range []string{"ioc", "build", "semgrep", "eslint", "govulncheck"} {
		if !validResult(p.Jobs[name]) {
			return p, errors.New("invalid job verdict")
		}
	}
	if p.Jobs["build"] != "success" || len(p.Jobs) != 5 || len(p.Coverage) != 4 {
		return p, errors.New("invalid report shape")
	}
	for _, name := range []string{"desktop", "control_plane", "media_plane", "admin"} {
		flag, ok := p.Coverage[name]
		if !ok || (!bytes.Equal(flag, []byte("true")) && !bytes.Equal(flag, []byte("false"))) {
			return p, errors.New("invalid coverage shape")
		}
	}
	return p, nil
}

func validResult(value string) bool {
	switch value {
	case "success", "failure", "cancelled", "skipped":
		return true
	default:
		return false
	}
}

func validClaims(token *oidc.IDToken, c signedClaims, p payload, now time.Time) bool {
	if c.RepositoryID != repositoryID || c.Repository != repository || c.EventName != "pull_request" ||
		c.RunID != strconv.FormatInt(p.CallerRunID, 10) ||
		c.RunAttempt != strconv.FormatInt(p.CallerRunAttempt, 10) ||
		c.WorkflowRef != workflowRef || !shaPattern.MatchString(c.WorkflowSHA) || c.Subject == "" {
		return false
	}
	if token.IssuedAt.IsZero() || token.Expiry.IsZero() || c.NotBefore <= 0 {
		return false
	}
	nbf := time.Unix(c.NotBefore, 0)
	return !token.IssuedAt.After(now.Add(30*time.Second)) &&
		!nbf.After(now) &&
		token.Expiry.After(now) &&
		token.Expiry.After(token.IssuedAt) &&
		token.Expiry.Sub(token.IssuedAt) <= 10*time.Minute &&
		!nbf.After(token.IssuedAt) &&
		!nbf.Before(token.IssuedAt.Add(-maxNotBeforeSkew))
}

func strictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func writeVerified(path string, data []byte) (retErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".verified-sonar-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
