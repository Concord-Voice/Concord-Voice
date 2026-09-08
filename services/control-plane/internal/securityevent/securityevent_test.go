package securityevent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

type fakeClock struct{ now time.Time }

func newFakeClock(now time.Time) *fakeClock  { return &fakeClock{now: now} }
func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func testEvent(evidence string) Event {
	return Event{EventType: EventAuthentication, Outcome: OutcomeDenied, Severity: SeverityMedium, ReasonCode: ReasonInvalidCredentials, AuthMethod: AuthPassword, RouteTemplate: RouteAuthLogin, EvidenceRef: evidence}
}

func TestWriterEmitsClosedEvent(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	w := newWriter(filepath.Join(dir, "events.jsonl"), ServiceControlPlane, nil, clock.Now, func() string { return "11111111-1111-4111-8111-111111111111" })
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	clock.Advance(time.Second)
	require.NoError(t, w.Close())
	require.Equal(t, map[string]any{
		"schema_version": "security-event.v1", "event_id": "11111111-1111-4111-8111-111111111111",
		"occurred_at": "2026-08-31T12:00:00Z", "service": "control-plane", "event_type": "authentication",
		"outcome": "denied", "severity": "medium", "reason_code": "invalid_credentials",
		"auth_method": "password", "route_template": "POST /api/v1/auth/login",
		"count": float64(1), "correlation_ref": "corr_0123456789abcdef",
	}, readOneJSONLine(t, filepath.Join(dir, "events.jsonl")))
}

func TestWriterRejectsUnknownEnumAndEvidence(t *testing.T) {
	w := newWriter(filepath.Join(t.TempDir(), "events.jsonl"), ServiceControlPlane, nil, time.Now, newUUIDv4)
	w.Emit(context.Background(), Event{EventType: EventType("unknown")})
	w.Emit(context.Background(), Event{EventType: EventAuthentication, Outcome: OutcomeDenied, Severity: SeverityMedium, ReasonCode: ReasonInvalidCredentials, EvidenceRef: "user-42"})
	require.Equal(t, uint64(2), w.Health().ValidationFailures)
}

func TestWriterRefusesActiveFileAt20MiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.NoError(t, os.Truncate(path, maxFileBytes))
	w := newWriter(path, ServiceControlPlane, nil, time.Now, newUUIDv4)
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.Error(t, w.Close())
	require.Equal(t, uint64(1), w.Health().SizeRefusals)
	require.Equal(t, HealthDegraded, w.Health().Status)
}

func TestWriterRecoversFromSizeRefusalAfterSuccessfulWrite(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "events.jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.NoError(t, os.Truncate(path, maxFileBytes))
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	w.Emit(ctx, testEvent(""))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(1)))
	require.NoError(t, os.Truncate(path, 0))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(2)))
	require.NoError(t, w.Close())
	require.Equal(t, HealthHealthy, w.Health().Status)
	require.Equal(t, uint64(1), w.Health().SizeRefusals)
}

func TestWriterLogsSinkDegradationOnceAndRestoration(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "events.jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.NoError(t, os.Truncate(path, maxFileBytes))
	var logs bytes.Buffer
	w := newWriter(path, ServiceControlPlane, logger.NewWithWriter(&logs), clock.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	w.Emit(ctx, testEvent(""))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(1)))
	require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("Security event sink refused oversized record")))

	require.NoError(t, os.Truncate(path, 0))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(2)))
	require.NoError(t, w.Close())
	require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("Security event sink restored")))
}

func TestWriterLogsWriteFailureOnce(t *testing.T) {
	var logs bytes.Buffer
	path := filepath.Join(t.TempDir(), "missing", "events.jsonl")
	w := newWriter(path, ServiceControlPlane, logger.NewWithWriter(&logs), time.Now, newUUIDv4)
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.Error(t, w.Close())
	require.Equal(t, 1, bytes.Count(logs.Bytes(), []byte("Security event sink write failed")))
}

func TestWriterEnforcesExact2KiBLineBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, time.Now, newUUIDv4)
	w.mu.Lock()
	require.Equal(t, appendWritten, w.writeLineLocked(bytes.Repeat([]byte{'a'}, maxLineBytes-1)))
	require.Equal(t, appendDiscard, w.writeLineLocked(bytes.Repeat([]byte{'a'}, maxLineBytes)))
	w.mu.Unlock()
	// #nosec G304 -- path is created by this test with t.TempDir.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, contents, maxLineBytes)
	require.Equal(t, uint64(1), w.Health().SizeRefusals)
}

func TestWriterCoalescesDuplicatesWithBoundedCount(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	for range maxEventCount + 10 {
		w.Emit(ctx, testEvent(""))
	}
	require.NoError(t, w.Close())
	line := readOneJSONLine(t, path)
	require.Equal(t, float64(maxEventCount), line["count"])
	require.Equal(t, uint64(maxEventCount+9), w.Health().Coalesced)
}

func TestWriterFlushesIdleBucketWhenTimerFires(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	timers := &manualTimerFactory{}
	w.afterFunc = timers.AfterFunc
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	_, err := os.Stat(path)
	require.Error(t, err)
	timers.Fire(t)
	require.Len(t, readJSONLines(t, path), 1)
	require.NoError(t, w.Close())
}

func TestWriterRecordsBudgetDropsLocally(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	for i := 0; i < maxUniqueEvents; i++ {
		w.Emit(ctx, testEvent(testUUID(i)))
	}
	w.Emit(ctx, testEvent(testUUID(maxUniqueEvents)))
	require.Equal(t, maxUniqueEvents, w.Health().PendingUnique)
	require.NoError(t, w.Close())
	lines := readJSONLines(t, path)
	require.Len(t, lines, maxUniqueEvents)
	for _, line := range lines {
		require.NotEqual(t, string(EventPipelineHealth), line["event_type"])
	}
	require.Equal(t, uint64(1), w.Health().Dropped)
}

func TestWriterRejectsCallerPipelineEvents(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	w := newWriter(filepath.Join(t.TempDir(), "events.jsonl"), ServiceControlPlane, nil, clock.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	for i := 0; i < maxUniqueEvents-1; i++ {
		w.Emit(ctx, testEvent(testUUID(i)))
	}
	for i := 0; i < 5; i++ {
		w.Emit(ctx, Event{EventType: EventPipelineHealth, Outcome: OutcomeDegraded, Severity: SeverityMedium, ReasonCode: ReasonCode(testReason(i))})
	}
	require.Equal(t, maxUniqueEvents-1, w.Health().PendingUnique)
	require.Equal(t, uint64(5), w.Health().ValidationFailures)
}

func TestWriterRejectsCallerPipelineEventsWithoutWritingThem(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	for i := 0; i < maxUniqueEvents-2; i++ {
		w.Emit(ctx, testEvent(testUUID(i)))
	}
	for _, reason := range writerOwnedReasons() {
		w.Emit(ctx, Event{EventType: EventPipelineHealth, Outcome: OutcomeDegraded, Severity: SeverityMedium, ReasonCode: reason})
	}
	w.Emit(ctx, testEvent(testUUID(maxUniqueEvents)))
	require.Equal(t, uint64(len(writerOwnedReasons())), w.Health().ValidationFailures)
	require.Zero(t, w.Health().Dropped)
	require.Equal(t, maxUniqueEvents-1, w.Health().PendingUnique)
	require.NoError(t, w.Close())
	lines := readJSONLines(t, path)
	for _, reason := range writerOwnedReasons() {
		require.Zero(t, countReason(lines, string(reason)))
	}
}

func TestWriterRejectsWriterOwnedReasonsOnNonPipelineEvents(t *testing.T) {
	w := newWriter(filepath.Join(t.TempDir(), "events.jsonl"), ServiceControlPlane, nil, time.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	for _, reason := range writerOwnedReasons() {
		w.Emit(ctx, Event{EventType: EventAuthentication, Outcome: OutcomeDenied, Severity: SeverityMedium, ReasonCode: reason})
	}
	require.Equal(t, uint64(len(writerOwnedReasons())), w.Health().ValidationFailures)
	require.Zero(t, w.Health().PendingUnique)
	require.NoError(t, w.Close())
}

func TestWriterRecoversAfterSinkBecomesWritable(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	w.Emit(ctx, testEvent(""))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(1))) // flush fails while parent is absent
	require.NoError(t, os.Mkdir(filepath.Dir(path), 0o750))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(2))) // a successful ordinary append clears local degradation
	require.NoError(t, w.Close())
	lines := readJSONLines(t, path)
	for _, line := range lines {
		require.NotEqual(t, string(EventPipelineHealth), line["event_type"])
	}
	require.GreaterOrEqual(t, w.Health().WriteFailures, uint64(1))
}

func TestWriterRearmsRetryTimerAfterQuietSinkFailure(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	timers := &manualTimerFactory{}
	w.afterFunc = timers.AfterFunc
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	first := timers.timer
	timers.Fire(t)
	require.NotSame(t, first, timers.timer, "a transient sink failure must rearm one bounded retry timer")
	require.Equal(t, 1, w.Health().PendingUnique)

	require.NoError(t, os.Mkdir(filepath.Dir(path), 0o750))
	timers.Fire(t)
	require.Zero(t, w.Health().PendingUnique)
	require.NoError(t, w.Close())
	require.Len(t, readJSONLines(t, path), 1)
}

func TestWriterRearmsRetryTimerAfterBucketRolloverFailure(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "missing", "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	timers := &manualTimerFactory{}
	w.afterFunc = timers.AfterFunc
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	w.Emit(ctx, testEvent(""))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(1)))
	require.NotNil(t, timers.timer, "bucket rollover failure must retain a retry timer")
	require.Equal(t, 2, w.Health().PendingUnique)
}

func TestWriterRecoversAfterFailureWithinOneFlush(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, clock.Now, newUUIDv4)
	failFirst := true
	w.beforeOpen = func() bool {
		if !failFirst {
			return false
		}
		failFirst = false
		return true
	}
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	w.Emit(ctx, testEvent(testUUID(1)))
	clock.Advance(time.Second)
	w.Emit(ctx, testEvent(testUUID(2)))
	require.NoError(t, w.Close())
	lines := readJSONLines(t, path)
	require.Len(t, lines, 2)
	require.Equal(t, testUUID(1), lines[0]["evidence_ref"], "the retained event must write before later events")
	require.Equal(t, testUUID(2), lines[1]["evidence_ref"])
	require.Equal(t, "2026-08-31T12:00:00Z", lines[0]["occurred_at"], "retry must preserve the retained event's original bucket")
	require.Equal(t, "2026-08-31T12:00:01Z", lines[1]["occurred_at"])
	for _, line := range lines {
		require.NotEqual(t, string(EventPipelineHealth), line["event_type"])
	}
	require.Equal(t, HealthHealthy, w.Health().Status)
}

func TestWriterRepairsPartialWriteOnSameFileBeforeRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, time.Now, func() string { return "11111111-1111-4111-8111-111111111111" })
	realWrite := w.fileWrite
	realTruncate := w.fileTruncate
	writeCalls := 0
	w.fileWrite = func(file *os.File, payload []byte) (int, error) {
		writeCalls++
		if writeCalls != 1 {
			return realWrite(file, payload)
		}
		written, err := realWrite(file, payload[:len(payload)/2])
		require.NoError(t, err)
		return written, errors.New("injected partial write")
	}
	truncateCalls := 0
	var repairFile *os.File
	w.fileTruncate = func(file *os.File, size int64) error {
		truncateCalls++
		if truncateCalls <= 2 {
			if repairFile != nil {
				require.Same(t, repairFile, file, "failed repair must retain the same open inode")
			}
			repairFile = file
			return errors.New("injected truncate failure")
		}
		require.Same(t, repairFile, file, "repair must target the still-open inode")
		return realTruncate(file, size)
	}

	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.Error(t, w.Close())
	// #nosec G304 -- path is created by this test with t.TempDir.
	torn, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, torn)
	require.NotContains(t, string(torn), "\n")
	require.Equal(t, 1, w.Health().PendingUnique)

	require.Error(t, w.Close())
	// #nosec G304 -- path is created by this test with t.TempDir.
	stillTorn, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, torn, stillTorn)
	require.Equal(t, 1, writeCalls, "no append may pass an unrepaired prefix")

	require.NoError(t, w.Close())
	require.Len(t, readJSONLines(t, path), 1)
	require.Equal(t, 2, writeCalls)
	require.Equal(t, 3, truncateCalls)
	require.Equal(t, HealthHealthy, w.Health().Status)
}

func TestWriterCommitsFullWriteReportedWithErrorWithoutRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, time.Now, func() string { return "11111111-1111-4111-8111-111111111111" })
	failOpen := true
	w.beforeOpen = func() bool {
		if !failOpen {
			return false
		}
		failOpen = false
		return true
	}
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.Error(t, w.Close())

	realWrite := w.fileWrite
	w.fileWrite = func(file *os.File, payload []byte) (int, error) {
		written, err := realWrite(file, payload)
		require.NoError(t, err)
		return written, errors.New("injected post-write error")
	}
	require.Error(t, w.Close())
	require.Len(t, readJSONLines(t, path), 1)
	require.Equal(t, 0, w.Health().PendingUnique)
	require.Equal(t, HealthDegraded, w.Health().Status)
	require.Equal(t, uint64(2), w.Health().WriteFailures)

	require.Error(t, w.Close())
	require.Len(t, readJSONLines(t, path), 1, "a committed record must never be retried")
}

func TestWriterCommitsSuccessfulWriteWhenCloseReportsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, time.Now, func() string { return "11111111-1111-4111-8111-111111111111" })
	realClose := w.fileClose
	closeCalls := 0
	w.fileClose = func(file *os.File) error {
		closeCalls++
		err := realClose(file)
		require.NoError(t, err)
		if closeCalls == 1 {
			return errors.New("injected close error")
		}
		return nil
	}

	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.Error(t, w.Close())
	require.Len(t, readJSONLines(t, path), 1)
	require.Equal(t, 0, w.Health().PendingUnique)
	require.Equal(t, HealthDegraded, w.Health().Status)
	require.Equal(t, uint64(1), w.Health().WriteFailures)

	require.Error(t, w.Close())
	require.Len(t, readJSONLines(t, path), 1, "an ambiguously closed committed record must never be retried")
}

func TestWriterTrimsProcessedOrderPrefixOnEveryRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, time.Now, func() string { return "11111111-1111-4111-8111-111111111111" })
	corr := "corr_0123456789abcdef"
	occurredAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	w.mu.Lock()
	w.enqueueLocked(testEvent(testUUID(0)), corr, occurredAt)
	w.beforeOpen = func() bool { return true }
	require.Error(t, w.flushLocked())
	require.Len(t, w.pendingOrder, 1)
	for index := 1; index <= 2*maxUniqueEvents; index++ {
		w.enqueueLocked(testEvent(testUUID(index)), corr, occurredAt)
		openCalls := 0
		w.beforeOpen = func() bool {
			openCalls++
			return openCalls == 2
		}
		require.Error(t, w.flushLocked())
		require.Len(t, w.pending, 1)
		require.Len(t, w.pendingOrder, 1, "processed prefixes must not accumulate across retries")
	}
	w.beforeOpen = nil
	require.NoError(t, w.flushLocked())
	require.Empty(t, w.pendingOrder)
	w.mu.Unlock()

	require.Len(t, readJSONLines(t, path), 2*maxUniqueEvents+1)
}

func TestWriterReopensAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, time.Now, newUUIDv4)
	w.bucket = time.Now().UTC().Truncate(time.Second)
	w.beforeAppend = func() {
		require.NoError(t, os.Rename(path, path+".1"))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.NoError(t, w.Close())
	require.Equal(t, uint64(1), w.Health().StaleFileRetries)
	require.Len(t, readJSONLines(t, path), 1)
}

func TestWriterCloseIsIdempotent(t *testing.T) {
	w := newWriter(filepath.Join(t.TempDir(), "events.jsonl"), ServiceControlPlane, nil, time.Now, newUUIDv4)
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.NoError(t, w.Close())
	require.NoError(t, w.Close())
}

func TestWriterCloseIgnoresRacingTimerCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w := newWriter(path, ServiceControlPlane, nil, time.Now, newUUIDv4)
	timers := &manualTimerFactory{}
	w.afterFunc = timers.AfterFunc
	w.Emit(withCorrelation(context.Background(), "corr_0123456789abcdef"), testEvent(""))
	require.NoError(t, w.Close())
	timers.timer.callback() // models a timer that fired immediately before Stop.
	require.Len(t, readJSONLines(t, path), 1)
}

func TestWriterConcurrentEmitIsRaceSafe(t *testing.T) {
	w := newWriter(filepath.Join(t.TempDir(), "events.jsonl"), ServiceControlPlane, nil, time.Now, newUUIDv4)
	ctx := withCorrelation(context.Background(), "corr_0123456789abcdef")
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() { defer group.Done(); w.Emit(ctx, testEvent("")) }()
	}
	group.Wait()
	require.NoError(t, w.Close())
}

func readOneJSONLine(t *testing.T, path string) map[string]any {
	t.Helper()
	lines := readJSONLines(t, path)
	require.Len(t, lines, 1)
	return lines[0]
}

func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	// #nosec G304 -- path is created by this test with t.TempDir.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := make([]map[string]any, 0, bytes.Count(contents, []byte{'\n'}))
	for _, line := range bytes.Split(bytes.TrimSpace(contents), []byte{'\n'}) {
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(line, &decoded))
		lines = append(lines, decoded)
	}
	return lines
}

func countReason(lines []map[string]any, reason string) int {
	count := 0
	for _, line := range lines {
		if line["reason_code"] == reason {
			count++
		}
	}
	return count
}

type manualTimer struct {
	callback func()
	stopped  bool
}

func (t *manualTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

type manualTimerFactory struct{ timer *manualTimer }

func (f *manualTimerFactory) AfterFunc(_ time.Duration, callback func()) timer {
	f.timer = &manualTimer{callback: callback}
	return f.timer
}

func (f *manualTimerFactory) Fire(t *testing.T) {
	t.Helper()
	require.NotNil(t, f.timer)
	require.False(t, f.timer.stopped)
	f.timer.callback()
}

func testUUID(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func testReason(i int) string {
	reasons := []ReasonCode{ReasonDiskWatermark, ReasonDependencyUnavailable, ReasonDependencyRecovered, ReasonAuditWriteFailed, ReasonAttestationCacheDegraded}
	return string(reasons[i%len(reasons)])
}

func writerOwnedReasons() []ReasonCode {
	return []ReasonCode{ReasonWriterValidationDrop, ReasonWriterBudgetDrop, ReasonWriterWriteFailed, ReasonWriterRecovered, ReasonWriterSizeRefused}
}
