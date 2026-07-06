//nolint:revive // "api" is the established package name shared with router.go; renaming is out of scope for this PR.
package api

import (
	"crypto/sha256"
	"io"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/feedback"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"golang.org/x/crypto/hkdf"
)

// buildFeedbackHandler constructs the #158 feedback handler. When the
// feedback PAT / repo are set in cfg, it wires the GitHub REST client; when
// either is empty (dev / self-hosted) the handler runs the log-only stub
// (see internal/feedback/handlers.go). Production guard at config.go
// fatal-exits before we ever reach the stub path in `ENVIRONMENT=production`.
//
// Mirrors the `buildPrivacyHandler` / `buildOAuthHandler` / `build*Handler`
// pattern — extracted so NewRouter's cognitive complexity stays under the
// SonarCloud threshold.
func buildFeedbackHandler(cfg *config.Config, log *logger.Logger) *feedback.Handler {
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
	return feedback.NewHandler(log, github, corrKey)
}
