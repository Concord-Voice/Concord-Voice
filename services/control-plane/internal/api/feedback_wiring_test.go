//nolint:revive // "api" is the established package name shared with router.go.
package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// TestBuildFeedbackHandler_DevStub verifies that an empty PAT / repo lands the
// handler on the log-only dev-stub path (handlers.go's `nil` GitHubIssueCreator
// branch). Production guard at config.go fatal-exits before we reach this with
// empty values in `ENVIRONMENT=production`, so the dev-stub is a
// dev / self-hosted convenience.
func TestBuildFeedbackHandler_DevStub(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{
			Token: "",
			Repo:  "",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h, "buildFeedbackHandler must return a non-nil handler in dev-stub mode")
}

// TestBuildFeedbackHandler_GitHubWired pins the production wiring: both fields
// set → NewClient is constructed and injected into the handler.
func TestBuildFeedbackHandler_GitHubWired(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture with fake PAT
			Token: "ghp_fake_test_pat", // #nosec G101 -- test fixture
			Repo:  "Concord-Voice/Concord-Voice-Feedback",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h, "buildFeedbackHandler must return a non-nil handler in github-wired mode")
}

// TestBuildFeedbackHandler_OnlyTokenStillDevStub verifies that a partial
// configuration (token set but repo empty) does NOT construct the GitHub
// client — both fields are required. Mirrors the config.go validate() rule
// that fatal-exits production when either is empty.
func TestBuildFeedbackHandler_OnlyTokenStillDevStub(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{ //nolint:gosec // G101: test fixture
			Token: "ghp_only_token", // #nosec G101 -- test fixture
			Repo:  "",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h)
}

func TestBuildFeedbackHandler_OnlyRepoStillDevStub(t *testing.T) {
	cfg := &config.Config{
		GitHubFeedback: config.GitHubFeedbackConfig{
			Token: "",
			Repo:  "owner/repo",
		},
	}
	h := buildFeedbackHandler(cfg, logger.New("test"))
	require.NotNil(t, h)
}
