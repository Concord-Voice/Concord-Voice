package attestation_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/attestation"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// TestLogRejected_NoRedis_SkipsCounter exercises the rdb==nil branch — no
// Redis ops, no counter logs.
func TestLogRejected_NoRedis_SkipsCounter(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewWithWriter(&buf)
	attestation.LogRejected(context.Background(), log, nil, attestation.ErrInvalid, "0.2.7", attestation.PlatformMacOS)

	out := buf.String()
	require.Contains(t, out, "attestation.rejected")
	require.NotContains(t, out, "attestation.counter_incr_failed")
	require.NotContains(t, out, "attestation.counter_expire_failed")
}

// TestLogRejected_HealthyRedis_NoFailureLog asserts the happy path emits no
// counter_incr_failed or counter_expire_failed lines (per finding #25 of
// #1264 review).
func TestLogRejected_HealthyRedis_NoFailureLog(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	var buf bytes.Buffer
	log := logger.NewWithWriter(&buf)
	attestation.LogRejected(context.Background(), log, rdb, attestation.ErrInvalid, "0.2.7", attestation.PlatformMacOS)

	out := buf.String()
	require.Contains(t, out, "attestation.rejected")
	require.NotContains(t, out, "attestation.counter_incr_failed")
	require.NotContains(t, out, "attestation.counter_expire_failed")
}

// TestLogRejected_RedisDown_LogsIncrFailure asserts the WARN-log added by
// finding #25 of #1264 review — Redis unreachable → Incr fails → WARN line
// names the counter key.
func TestLogRejected_RedisDown_LogsIncrFailure(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = rdb.Close() }()

	var buf bytes.Buffer
	log := logger.NewWithWriter(&buf)
	attestation.LogRejected(context.Background(), log, rdb, attestation.ErrInvalid, "0.2.7", attestation.PlatformMacOS)

	out := buf.String()
	require.Contains(t, out, "attestation.counter_incr_failed",
		"Incr error must surface as a WARN log")
	require.Contains(t, out, "key=attestation:rejected:ATTESTATION_INVALID")
}

// TestLogIssued_HealthyRedis_NoFailureLog asserts the issued-counter happy
// path emits no failure log.
func TestLogIssued_HealthyRedis_NoFailureLog(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	var buf bytes.Buffer
	log := logger.NewWithWriter(&buf)
	attestation.LogIssued(context.Background(), log, rdb, "0.2.7", "20260529", attestation.PlatformMacOS)

	out := buf.String()
	require.Contains(t, out, "attestation.issued")
	require.NotContains(t, out, "attestation.counter_incr_failed")
	require.NotContains(t, out, "attestation.counter_expire_failed")
}

// TestLogIssued_RedisDown_LogsIncrFailure mirrors the rejected test for the
// issued-counter axis.
func TestLogIssued_RedisDown_LogsIncrFailure(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = rdb.Close() }()

	var buf bytes.Buffer
	log := logger.NewWithWriter(&buf)
	attestation.LogIssued(context.Background(), log, rdb, "0.2.7", "20260529", attestation.PlatformMacOS)

	out := buf.String()
	require.Contains(t, out, "attestation.counter_incr_failed",
		"Incr error must surface as a WARN log on issued axis too")
	require.Contains(t, out, "key=attestation:issued:")
}
