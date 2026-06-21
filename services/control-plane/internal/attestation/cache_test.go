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

// fakeReader implements attestation.Reader for tests.
type fakeReader struct {
	binaries []attestation.ReleaseBinary
	spas     []attestation.ReleaseSPA
}

func (f *fakeReader) ListActiveBinaries(_ context.Context) ([]attestation.ReleaseBinary, error) {
	return f.binaries, nil
}

func (f *fakeReader) ListActiveSPAs(_ context.Context) ([]attestation.ReleaseSPA, error) {
	return f.spas, nil
}

func TestCache_HydrateFromRepo(t *testing.T) {
	ctx := context.Background()
	repo := &fakeReader{
		binaries: []attestation.ReleaseBinary{{
			Version:  "0.2.7",
			Platform: attestation.PlatformMacOS,
			CertHash: "abc123",
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))

	rb, ok := c.LookupBinary("0.2.7", attestation.PlatformMacOS)
	require.True(t, ok)
	require.Equal(t, "abc123", rb.CertHash)
}

func TestCache_LookupBinary_Missing_ReturnsFalse(t *testing.T) {
	log := logger.New("development")
	c := attestation.NewCache(&fakeReader{}, nil, nil, log)
	_, ok := c.LookupBinary("0.9.0", attestation.PlatformMacOS)
	require.False(t, ok)
}

func TestCache_LookupBinary_RevokedReturnsFalse(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	repo := &fakeReader{
		binaries: []attestation.ReleaseBinary{{
			Version:   "0.2.5",
			Platform:  attestation.PlatformMacOS,
			CertHash:  "xyz",
			RevokedAt: &now,
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))
	_, ok := c.LookupBinary("0.2.5", attestation.PlatformMacOS)
	require.False(t, ok)
}

func TestCache_LookupBinary_WrongPlatformReturnsFalse(t *testing.T) {
	ctx := context.Background()
	repo := &fakeReader{
		binaries: []attestation.ReleaseBinary{{
			Version:  "0.2.7",
			Platform: attestation.PlatformMacOS,
			CertHash: "abc123",
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))
	_, ok := c.LookupBinary("0.2.7", attestation.PlatformWindows)
	require.False(t, ok)
}

func TestCache_LookupSPA_Found(t *testing.T) {
	ctx := context.Background()
	repo := &fakeReader{
		spas: []attestation.ReleaseSPA{{
			SpaVersion: "spa-abc1234",
			HTMLHash:   "hash1",
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))

	rs, ok := c.LookupSPA("spa-abc1234")
	require.True(t, ok)
	require.Equal(t, "hash1", rs.HTMLHash)
}

func TestCache_LookupSPA_RevokedReturnsFalse(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	repo := &fakeReader{
		spas: []attestation.ReleaseSPA{{
			SpaVersion: "spa-old0001",
			HTMLHash:   "hash2",
			RevokedAt:  &now,
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))
	_, ok := c.LookupSPA("spa-old0001")
	require.False(t, ok)
}

func TestCache_LookupSPA_Missing_ReturnsFalse(t *testing.T) {
	log := logger.New("development")
	c := attestation.NewCache(&fakeReader{}, nil, nil, log)
	_, ok := c.LookupSPA("spa-nonexistent")
	require.False(t, ok)
}

// TestCache_LookupBinary_ReturnsCopy verifies that mutating the returned struct
// does NOT affect the in-memory cache (finding #23 of #1264 review). The cache
// returns by value, so a caller that overwrites a field can no longer corrupt
// state observable to other lookups.
func TestCache_LookupBinary_ReturnsCopy(t *testing.T) {
	ctx := context.Background()
	repo := &fakeReader{
		binaries: []attestation.ReleaseBinary{{
			Version:  "0.2.7",
			Platform: attestation.PlatformMacOS,
			CertHash: "original-hash",
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))

	rb1, ok := c.LookupBinary("0.2.7", attestation.PlatformMacOS)
	require.True(t, ok)
	rb1.CertHash = "tampered-by-consumer"

	rb2, ok := c.LookupBinary("0.2.7", attestation.PlatformMacOS)
	require.True(t, ok)
	require.Equal(t, "original-hash", rb2.CertHash,
		"second lookup must not see the first caller's mutation")
}

// TestCache_LookupSPA_ReturnsCopy is the SPA-axis sibling of the binary test.
func TestCache_LookupSPA_ReturnsCopy(t *testing.T) {
	ctx := context.Background()
	repo := &fakeReader{
		spas: []attestation.ReleaseSPA{{
			SpaVersion: "spa-abc1234",
			HTMLHash:   "original-hash",
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))

	rs1, ok := c.LookupSPA("spa-abc1234")
	require.True(t, ok)
	rs1.HTMLHash = "tampered-by-consumer"

	rs2, ok := c.LookupSPA("spa-abc1234")
	require.True(t, ok)
	require.Equal(t, "original-hash", rs2.HTMLHash,
		"second lookup must not see the first caller's mutation")
}

func TestCache_IsRevoked_NilRedis_ReturnsFalse(t *testing.T) {
	ctx := context.Background()
	log := logger.New("development")
	c := attestation.NewCache(&fakeReader{}, nil, nil, log)
	// With nil rdb, IsRevoked should fail-open (false) since we can't check Redis.
	require.False(t, c.IsRevoked(ctx, "0.2.7"))
}

func TestCache_Hydrate_ReplacesExistingEntries(t *testing.T) {
	ctx := context.Background()
	repo := &fakeReader{
		binaries: []attestation.ReleaseBinary{{
			Version:  "0.2.6",
			Platform: attestation.PlatformMacOS,
			CertHash: "first",
		}},
	}
	log := logger.New("development")
	c := attestation.NewCache(repo, nil, nil, log)
	require.NoError(t, c.Hydrate(ctx))
	_, ok := c.LookupBinary("0.2.6", attestation.PlatformMacOS)
	require.True(t, ok)

	// Replace with new data — old entry should be gone.
	repo.binaries = []attestation.ReleaseBinary{{
		Version:  "0.2.8",
		Platform: attestation.PlatformMacOS,
		CertHash: "second",
	}}
	require.NoError(t, c.Hydrate(ctx))
	_, oldOK := c.LookupBinary("0.2.6", attestation.PlatformMacOS)
	require.False(t, oldOK)
	_, newOK := c.LookupBinary("0.2.8", attestation.PlatformMacOS)
	require.True(t, newOK)
}

// TestCache_Start_NilNATS_NoSubscriptions covers the nc==nil branch of Start:
// no NATS subscriptions are created, return value is (nil, nil), and the
// poll-fallback goroutine is still spawned. The goroutine exits when ctx is
// cancelled — the test uses a cancellable context so the goroutine doesn't
// leak past the test boundary.
func TestCache_Start_NilNATS_NoSubscriptions(t *testing.T) {
	log := logger.New("development")
	c := attestation.NewCache(&fakeReader{}, nil, nil, log)

	ctx, cancel := context.WithCancel(context.Background())
	subs, err := c.Start(ctx)
	require.NoError(t, err)
	require.Nil(t, subs, "no NATS client → no subscriptions returned")

	// Cancel to terminate the poll-fallback goroutine cleanly. The goroutine
	// uses ctx.Done() to exit; a brief sleep gives the scheduler time to run
	// the select-case before the test function returns. The 50ms budget is
	// long enough on any commodity hardware but short enough to keep the
	// test deterministic.
	cancel()
	time.Sleep(50 * time.Millisecond)
}

// TestCache_IsRevoked_RedisError_FailClosed covers the fail-closed branch:
// when rdb.SIsMember errors, IsRevoked returns true so callers treat the
// version as revoked. Triggered by pointing the client at a non-existent
// Redis server (DialTimeout exhausted → connection refused error). MaxRetries
// is set to 0 so the test fails fast rather than waiting for the default
// 3-retry backoff.
//
// Also asserts the WARN log added by finding #16 of the #1264 review:
// silent fail-closed makes "actually revoked" indistinguishable from "Redis
// is down" in the operator-facing log stream. The structured WARN line
// carries event=attestation.is_revoked_redis_error so operators can
// distinguish the two failure modes.
func TestCache_IsRevoked_RedisError_FailClosed(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 50 * time.Millisecond,
		MaxRetries:  -1, // disable retries; -1 means no retries (vs 0 which is "use default")
	})
	defer func() { _ = rdb.Close() }()

	var buf bytes.Buffer
	log := logger.NewWithWriter(&buf)
	c := attestation.NewCache(&fakeReader{}, nil, rdb, log)

	// fail-closed: error means treat as revoked
	require.True(t, c.IsRevoked(context.Background(), "0.2.7"),
		"IsRevoked on Redis error must fail-closed (return true)")

	output := buf.String()
	require.Contains(t, output, "attestation.is_revoked_redis_error",
		"Redis error in IsRevoked must emit a structured WARN log")
	require.Contains(t, output, "level=WARN",
		"the emitted log must be at WARN level")
	require.Contains(t, output, "version=0.2.7",
		"the WARN log must include the version that triggered the check")
}

// TestCache_IsRevoked_Found_NoLog asserts the happy path emits no
// IsRevoked-error log. Negative assertion paired with the fail-closed test
// above: the absence of the warn signal indicates Redis is healthy.
func TestCache_IsRevoked_Found_NoLog(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	require.NoError(t, rdb.SAdd(ctx, "attestation:revoked_versions", "0.2.5").Err())

	var buf bytes.Buffer
	log := logger.NewWithWriter(&buf)
	c := attestation.NewCache(&fakeReader{}, nil, rdb, log)

	require.True(t, c.IsRevoked(ctx, "0.2.5"))
	require.False(t, c.IsRevoked(ctx, "0.2.7"))

	require.NotContains(t, buf.String(), "is_revoked_redis_error",
		"happy-path IsRevoked must not emit the Redis-error WARN log")
}

// fakeReaderError implements attestation.Reader and returns an error from
// ListActiveBinaries to exercise the Hydrate error path.
type fakeReaderError struct{}

func (f *fakeReaderError) ListActiveBinaries(_ context.Context) ([]attestation.ReleaseBinary, error) {
	return nil, errFakeReader
}

func (f *fakeReaderError) ListActiveSPAs(_ context.Context) ([]attestation.ReleaseSPA, error) {
	return nil, errFakeReader
}

var errFakeReader = errFakeReaderSentinel{msg: "fake repo error"}

type errFakeReaderSentinel struct{ msg string }

func (e errFakeReaderSentinel) Error() string { return e.msg }

// TestCache_Hydrate_BinariesError covers the ListActiveBinaries error branch
// in Hydrate. The error must propagate up so the wiring helper can decide
// fatal vs warn.
func TestCache_Hydrate_BinariesError(t *testing.T) {
	log := logger.New("development")
	c := attestation.NewCache(&fakeReaderError{}, nil, nil, log)
	err := c.Hydrate(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fake repo error")
}

// fakeReaderSpaError returns success from ListActiveBinaries but error from
// ListActiveSPAs, so we exercise the second error branch in Hydrate.
type fakeReaderSpaError struct{}

func (f *fakeReaderSpaError) ListActiveBinaries(_ context.Context) ([]attestation.ReleaseBinary, error) {
	return nil, nil
}

func (f *fakeReaderSpaError) ListActiveSPAs(_ context.Context) ([]attestation.ReleaseSPA, error) {
	return nil, errFakeReader
}

// TestCache_Hydrate_SPAsError covers the ListActiveSPAs error branch in
// Hydrate (second step after binaries succeed).
func TestCache_Hydrate_SPAsError(t *testing.T) {
	log := logger.New("development")
	c := attestation.NewCache(&fakeReaderSpaError{}, nil, nil, log)
	err := c.Hydrate(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fake repo error")
}
