package stepup

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newBudgetRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestBudget_AdmitsUpToTheLimitThen429(t *testing.T) {
	rdb, _ := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()

	for i := 0; i < BudgetLimit; i++ {
		require.Nil(t, b.Consume(ctx, "u1"), "attempt %d must be admitted", i+1)
	}
	e := b.Consume(ctx, "u1")
	require.NotNil(t, e)
	require.Equal(t, http.StatusTooManyRequests, e.Status)
	require.Equal(t, ErrMsgTooManyAttempts, e.Body["error"])
	require.Nil(t, e.Cause, "an exhausted budget is an outcome, not a fault")
	require.Nil(t, b.Consume(ctx, "u2"), "the budget is per user")
}

// An unevaluable budget DENIES, and says so honestly: 503 with its own body,
// never the 429 that blames the user.
func TestBudget_UnwiredOrFailingRedisIs503(t *testing.T) {
	ctx := context.Background()

	e := NewBudget(nil, "stepup:test:").Consume(ctx, "u1")
	require.NotNil(t, e)
	require.Equal(t, http.StatusServiceUnavailable, e.Status)
	require.Equal(t, ErrMsgBudgetUnavailable, e.Body["error"])
	require.ErrorIs(t, e, errBudgetUnwired)

	rdb, mr := newBudgetRedis(t)
	mr.Close()
	e = NewBudget(rdb, "stepup:test:").Consume(ctx, "u1")
	require.NotNil(t, e)
	require.Equal(t, http.StatusServiceUnavailable, e.Status)
	require.Equal(t, ErrMsgBudgetUnavailable, e.Body["error"])
	require.NotNil(t, e.Cause, "the transport error is kept for the caller to log")
}

func TestBudget_ClearResetsAndReportsFailure(t *testing.T) {
	rdb, mr := newBudgetRedis(t)
	b := NewBudget(rdb, "stepup:test:")
	ctx := context.Background()
	for i := 0; i < BudgetLimit; i++ {
		require.Nil(t, b.Consume(ctx, "u1"))
	}
	require.NoError(t, b.Clear(ctx, "u1"))
	require.False(t, mr.Exists(b.Key("u1")))
	require.Nil(t, b.Consume(ctx, "u1"), "a cleared budget admits again")

	require.NoError(t, NewBudget(nil, "stepup:test:").Clear(ctx, "u1"), "an unwired budget has nothing to clear")

	mr.Close()
	err := b.Clear(ctx, "u1")
	require.Error(t, err)
	require.False(t, errors.Is(err, errBudgetUnwired))
}

func TestBudget_KeyIsPrefixPlusUser(t *testing.T) {
	require.Equal(t, "stepup:mfa_settings:abc", NewBudget(nil, "stepup:mfa_settings:").Key("abc"))
}
