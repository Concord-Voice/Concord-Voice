//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"crypto/sha256"
	"io"
	"strings"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/feedback"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"golang.org/x/crypto/hkdf"
)

// buildFeedbackHandler constructs the #158 feedback handler. The reserved
// self-host disabled repository selects a handler that refuses reports before
// reading them. Other configurations wire the GitHub REST client when both
// credentials are set, or retain the development stub when either is empty.
// Config's production credential guard remains mandatory.
//
// Mirrors the `buildPrivacyHandler` / `buildOAuthHandler` / `build*Handler`
// pattern — extracted so NewRouter's cognitive complexity stays under the
// SonarCloud threshold.
func buildFeedbackHandler(cfg *config.Config, log *logger.Logger) *feedback.Handler {
	if config.IsSelfHostedInstance(cfg.InstanceType) &&
		strings.EqualFold(strings.TrimSpace(cfg.GitHubFeedback.Repo), "selfhost/disabled") {
		return feedback.NewDisabledHandler()
	}
	var github feedback.GitHubIssueCreator
	if cfg.GitHubFeedback.Token != "" && cfg.GitHubFeedback.Repo != "" {
		github = feedback.NewClient(cfg.GitHubFeedback.Token, cfg.GitHubFeedback.Repo)
	}
	// Derive a dedicated correlation key from JWTSecret via HKDF (never reuse
	// the raw signing key) so the reporter-token purpose is cryptographically
	// separated from JWT signing — mirrors auditIPKey in oauth_wiring.go. No
	// new env/deploy surface.
	corrKey := make([]byte, 32)
	if _, err := io.ReadFull(
		hkdf.New(sha256.New, []byte(cfg.JWTSecret), nil, []byte("concord/feedback-correlation/v1")),
		corrKey,
	); err != nil {
		log.Fatal("Failed to derive feedback correlation key", "error", err)
	}
	return feedback.NewHandler(log, github, corrKey, cfg.PublicMediaBaseURL)
}
