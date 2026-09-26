package mfa

// Reproduction for the WebAuthn inline-token consume error path: a Redis fault
// on the GETDEL wraps the raw driver error via fmt.Errorf("...: %w", err), and
// a go-redis hook that annotates its error with the failing command's
// arguments (as one legitimately can) puts the live, spendable token — the
// key's own suffix — into the error handlers.go's callers then log. The
// property under test: the error consumeWebAuthnInlineToken (via VerifyCode)
// returns on a Redis fault must never contain the submitted token, while still
// being a non-nil error (fail closed).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// iteFaultHook fails GETDEL (and GET+DEL, in case of an implementation split)
// for keys under the inline-token prefix, annotating the returned error with
// the command's own arguments — modeling a go-redis client hook that logs or
// wraps a command's argument list, which necessarily includes the key.
type iteFaultHook struct{}

func (iteFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (iteFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		name := cmd.Name()
		if name == "getdel" || name == "get" || name == "del" {
			if args := cmd.Args(); len(args) >= 2 {
				if k, ok := args[1].(string); ok && strings.HasPrefix(k, "mfa_inline_purpose_token:") {
					err := fmt.Errorf("redis: %v", cmd.Args())
					cmd.SetErr(err)
					return err
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (iteFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestInlineToken_ConsumeErrorDoesNotCarryTheToken: a Redis fault on consuming
// a WebAuthn inline token must fail closed without leaking the live token
// through the returned (and subsequently logged) error.
func TestInlineToken_ConsumeErrorDoesNotCarryTheToken(t *testing.T) {
	db := iuNewTestDB(t)
	kr := iuKeyring(t)
	clean := iuNewTestRedis(t)
	hooked := redis.NewClient(&redis.Options{Addr: clean.Options().Addr})
	t.Cleanup(func() { _ = hooked.Close() })
	hooked.AddHook(iteFaultHook{})

	userID := iuCreateUser(t, db, "ite-password-1")
	h := iuHandler(db, hooked, kr)

	const canary = "this-is-a-leak-canary-not-a-token" // > 20 chars: required to reach the consume path

	verified, err := h.VerifyCode(context.Background(), userID, stepup.PurposeBackupEmailSet, canary)

	assert.False(t, verified, "a Redis fault on token consumption must never verify")
	require.Error(t, err, "a Redis fault on token consumption must fail closed, not silently pass")
	assert.NotContains(t, err.Error(), canary,
		"the error returned from a faulted inline-token consume must not carry the submitted token")
}
