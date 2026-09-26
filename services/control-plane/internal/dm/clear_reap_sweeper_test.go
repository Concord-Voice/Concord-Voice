package dm

// Unit tests for the DM clear-reap discovery sweeper (#3462), driven against a
// fake ClearReapBatchRunner. No database: candidate discovery is stubbed through the
// package-level s.discover seam. Database-backed discovery coverage lives in
// clear_reap_sweeper_integration_test.go.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const clearReapTestConversationID = "11111111-1111-1111-1111-111111111111"

// fakeClearReapBatchRunner drives RunClearReapBatch from a caller-supplied plan
// function keyed by the overall call count (1-based), so a test can script an
// exact sequence of outcomes across candidates and batches.
type fakeClearReapBatchRunner struct {
	mu    sync.Mutex
	calls int
	plan  func(call int) (purge.ClearReapResult, error)
}

func (f *fakeClearReapBatchRunner) RunClearReapBatch(context.Context, purge.ClearReapPlan) (purge.ClearReapResult, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	return f.plan(call)
}

func (f *fakeClearReapBatchRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// oneCandidateThenNoneAfter is a discover stub selecting a single fixed
// candidate from an empty cursor and nothing thereafter.
func oneCandidateThenNoneAfter(id string) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, after string) ([]string, error) {
		if after == "" {
			return []string{id}, nil
		}
		return nil, nil
	}
}

func TestClearReapSweeperPeriodicPassHonorsVisitCap(t *testing.T) {
	// Past the cap the fake fails, so a regression that ignores the cap ends
	// here with an error instead of looping until the test binary's timeout.
	engine := &fakeClearReapBatchRunner{plan: func(call int) (purge.ClearReapResult, error) {
		if call > clearReapVisitCap {
			return purge.ClearReapResult{}, errors.New("engine called past the visit cap")
		}
		return purge.ClearReapResult{Outcome: purge.ClearReapReaped, DeletedCount: 1, More: true}, nil
	}}
	s := NewClearReapSweeper(nil, engine, nil)
	s.discover = oneCandidateThenNoneAfter(clearReapTestConversationID)

	result, err := s.RunPass(context.Background())

	require.NoError(t, err, "the visit cap must stop the loop before the engine is over-called")
	assert.Equal(t, clearReapVisitCap, engine.callCount(), "must stop at the visit cap for one candidate")
	assert.Equal(t, 1, result.Selected)
	assert.Equal(t, 1, result.Reaped)
	assert.Equal(t, clearReapVisitCap, result.Deleted)
	assert.Zero(t, result.Skipped)
	assert.Zero(t, result.Failed)
}

func TestClearReapSweeperPreflightDrainsWithoutVisitCap(t *testing.T) {
	engine := &fakeClearReapBatchRunner{plan: func(call int) (purge.ClearReapResult, error) {
		return purge.ClearReapResult{Outcome: purge.ClearReapReaped, DeletedCount: 1, More: call <= 120}, nil
	}}
	s := NewClearReapSweeper(nil, engine, nil)
	s.discover = oneCandidateThenNoneAfter(clearReapTestConversationID)

	err := s.RunPreflight(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 121, engine.callCount(), "preflight must drain past the periodic visit cap")
}

func TestClearReapSweeperClassifiesCandidateLocalErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deadlock", &pq.Error{Code: "40P01"}},
		{"lock_not_available", &pq.Error{Code: "55P03"}},
		{"serialization_failure", &pq.Error{Code: "40001"}},
		{"per-batch deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeClearReapBatchRunner{plan: func(int) (purge.ClearReapResult, error) {
				return purge.ClearReapResult{}, tc.err
			}}
			s := NewClearReapSweeper(nil, engine, nil)
			s.discover = oneCandidateThenNoneAfter(clearReapTestConversationID)

			result, err := s.RunPass(context.Background())

			require.Error(t, err, "a candidate-local error still fails the pass call")
			assert.Equal(t, 1, result.Failed)
			var passErr *clearReapPassError
			require.ErrorAs(t, err, &passErr)
			assert.False(t, passErr.fatal, "a candidate-local error must never be fatal")
		})
	}
}

// A deadline that already rolled back the commit surfaces as a bare
// sql.ErrTxDone; clear_reap.go joins ctx.Err() so it stays classifiable as the
// per-batch deadline instead of falling through to the fatal default (#3463 review).
func TestIsCandidateLocalClearReapErrorRecognizesADeadlineThatRolledBackTheCommit(t *testing.T) {
	wrapped := fmt.Errorf("purge: commit clear reap batch: %w", errors.Join(context.DeadlineExceeded, sql.ErrTxDone))

	assert.True(t, isCandidateLocalClearReapError(context.Background(), wrapped))
}

func TestClearReapSweeperFatalErrorStopsPreflight(t *testing.T) {
	fatalErr := &pq.Error{Code: "23514"} // check_violation: not on the candidate-local list
	engine := &fakeClearReapBatchRunner{plan: func(int) (purge.ClearReapResult, error) {
		return purge.ClearReapResult{}, fatalErr
	}}
	s := NewClearReapSweeper(nil, engine, nil)
	s.discover = oneCandidateThenNoneAfter(clearReapTestConversationID)

	err := s.RunPreflight(context.Background())

	require.Error(t, err)
	var passErr *clearReapPassError
	require.ErrorAs(t, err, &passErr)
	assert.True(t, passErr.fatal, "an unrecognized SQLSTATE must abort preflight")
}

func TestClearReapSweeperCountsOutcomes(t *testing.T) {
	outcomes := []purge.ClearReapResult{
		{Outcome: purge.ClearReapGone},
		{Outcome: purge.ClearReapNotEligible},
		{Outcome: purge.ClearReapReaped, DeletedCount: 0},
		{Outcome: purge.ClearReapReaped, DeletedCount: 3},
	}
	engine := &fakeClearReapBatchRunner{plan: func(call int) (purge.ClearReapResult, error) {
		return outcomes[call-1], nil
	}}
	ids := []string{
		"11111111-0000-0000-0000-000000000001",
		"11111111-0000-0000-0000-000000000002",
		"11111111-0000-0000-0000-000000000003",
		"11111111-0000-0000-0000-000000000004",
	}
	s := NewClearReapSweeper(nil, engine, nil)
	s.discover = func(_ context.Context, after string) ([]string, error) {
		if after == "" {
			return ids, nil
		}
		return nil, nil
	}

	result, err := s.RunPass(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 4, result.Selected)
	assert.Equal(t, 3, result.Skipped, "Gone, NotEligible and a 0-row Reaped are each Skipped")
	assert.Equal(t, 1, result.Reaped)
	assert.Equal(t, 3, result.Deleted)
	assert.Zero(t, result.Failed)
}

// TestNextClearReapCursor pins the periodic worker's cursor decision table
// (#3463 review): a full page advances, a short or empty page (with no
// discovery failure) resets to the beginning, and a failed discovery keeps the
// old cursor so nothing is skipped.
func TestNextClearReapCursor(t *testing.T) {
	someErr := errors.New("discovery failed")
	candidateLocal := &clearReapPassError{failed: 1, fatal: false, err: someErr}

	for _, tc := range []struct {
		name         string
		cursor, next string
		result       ClearReapSweepResult
		err          error
		want         string
	}{
		{
			name:   "full page advances to next",
			cursor: "a", next: "z",
			result: ClearReapSweepResult{Selected: clearReapCandidateLimit},
			want:   "z",
		},
		{
			name:   "short page resets",
			cursor: "a", next: "z",
			result: ClearReapSweepResult{Selected: clearReapCandidateLimit - 1},
			want:   "",
		},
		{
			name:   "empty page with no error resets",
			cursor: "a", next: "a",
			result: ClearReapSweepResult{Selected: 0},
			want:   "",
		},
		{
			name:   "empty page with a discovery error keeps the cursor",
			cursor: "a", next: "a",
			result: ClearReapSweepResult{Selected: 0},
			err:    someErr,
			want:   "a",
		},
		{
			name:   "short page with a candidate-local pass error still resets",
			cursor: "a", next: "z",
			result: ClearReapSweepResult{Selected: 3, Failed: 1},
			err:    candidateLocal,
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nextClearReapCursor(tc.cursor, tc.next, tc.result, tc.err))
		})
	}
}

// TestClearReapSweeperWorkerResetsCursorAfterAShortPage falsifies the pre-fix
// behaviour: a short (non-full) first page used to require a second, empty
// pass before the cursor reset, which is exactly the extra tick that let a
// visit-capped candidate go unresumed for a whole cycle. The second discovery
// call must already show after=="".
func TestClearReapSweeperWorkerResetsCursorAfterAShortPage(t *testing.T) {
	engine := &fakeClearReapBatchRunner{plan: func(int) (purge.ClearReapResult, error) {
		return purge.ClearReapResult{Outcome: purge.ClearReapReaped}, nil
	}}
	var mu sync.Mutex
	var afterValues []string
	calls := 0
	s := NewClearReapSweeper(nil, engine, nil)
	s.discover = func(_ context.Context, after string) ([]string, error) {
		mu.Lock()
		afterValues = append(afterValues, after)
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return []string{clearReapTestConversationID}, nil // one candidate: a short page
		}
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunWorker(ctx, time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(afterValues) >= 2 && afterValues[0] == "" && afterValues[1] == ""
	}, time.Second, time.Millisecond, "a short page must reset the cursor on the very next discovery call")

	cancel()
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

// TestClearReapSweeperResumesACappedCandidateOnTheNextTick drives runPassAfter
// and nextClearReapCursor directly, tick by tick, with no timers: a candidate
// that still has work when a periodic visit hits its cap must be resumed on
// the very next tick, and fully drained rather than losing work at the cap.
func TestClearReapSweeperResumesACappedCandidateOnTheNextTick(t *testing.T) {
	const totalCalls = 120 // > one visit cap (50), < three
	engine := &fakeClearReapBatchRunner{plan: func(call int) (purge.ClearReapResult, error) {
		return purge.ClearReapResult{Outcome: purge.ClearReapReaped, DeletedCount: 1000, More: call < totalCalls}, nil
	}}
	s := NewClearReapSweeper(nil, engine, nil)
	s.discover = func(context.Context, string) ([]string, error) {
		if engine.callCount() >= totalCalls {
			return nil, nil
		}
		return []string{clearReapTestConversationID}, nil
	}

	var cursor string
	tick := func() ClearReapSweepResult {
		result, next, err := s.runPassAfter(context.Background(), cursor, clearReapVisitCap)
		cursor = nextClearReapCursor(cursor, next, result, err)
		return result
	}

	r1 := tick()
	assert.Equal(t, clearReapVisitCap, engine.callCount(), "tick 1 stops at the visit cap")
	assert.Equal(t, 1, r1.Capped)
	assert.Equal(t, "", cursor, "a short page (one candidate) resets immediately")

	r2 := tick()
	assert.Equal(t, 2*clearReapVisitCap, engine.callCount(), "tick 2 resumes the same candidate for another full cap")
	assert.Equal(t, 1, r2.Capped)

	r3 := tick()
	assert.Equal(t, totalCalls, engine.callCount(), "tick 3 drains the remainder without hitting the cap")
	assert.Equal(t, 0, r3.Capped)

	r4 := tick()
	assert.Zero(t, r4.Selected, "the candidate is fully drained and no longer discovered")
}

// TestClearReapSweeperFullPageAdvancesCursorToTheLastID pins the other half of
// the cursor decision: a full page (== clearReapCandidateLimit) advances past
// itself rather than resetting, using the last discovered id.
func TestClearReapSweeperFullPageAdvancesCursorToTheLastID(t *testing.T) {
	ids := make([]string, clearReapCandidateLimit)
	for i := range ids {
		ids[i] = fmt.Sprintf("11111111-0000-0000-0000-%012d", i)
	}
	engine := &fakeClearReapBatchRunner{plan: func(int) (purge.ClearReapResult, error) {
		return purge.ClearReapResult{Outcome: purge.ClearReapGone}, nil
	}}
	s := NewClearReapSweeper(nil, engine, nil)
	calls := 0
	s.discover = func(context.Context, string) ([]string, error) {
		calls++
		if calls == 1 {
			return ids, nil
		}
		return nil, nil
	}

	result, next, err := s.runPassAfter(context.Background(), "", clearReapVisitCap)
	cursor := nextClearReapCursor("", next, result, err)

	require.NoError(t, err)
	assert.Equal(t, clearReapCandidateLimit, result.Selected)
	assert.Zero(t, result.Capped)
	assert.Equal(t, ids[len(ids)-1], cursor)
}

// TestClearReapSweeperFatalErrorReportsFirstCandidateNotLater pins runPassAfter's
// firstFatal selection (#3463 review): when several candidates fail in one
// pass, the reported error — and therefore the logged SQLSTATE — must be the
// FIRST fatal one, never a later candidate-local error that happens to fail too.
func TestClearReapSweeperFatalErrorReportsFirstCandidateNotLater(t *testing.T) {
	var buf bytes.Buffer
	engine := &fakeClearReapBatchRunner{plan: func(call int) (purge.ClearReapResult, error) {
		if call == 1 {
			return purge.ClearReapResult{}, &pq.Error{Code: "23514", Message: "should never reach the log"} // fatal: not candidate-local
		}
		return purge.ClearReapResult{}, &pq.Error{Code: "40001", Message: "should never reach the log either"} // candidate-local
	}}
	ids := []string{
		"11111111-0000-0000-0000-000000000001",
		"11111111-0000-0000-0000-000000000002",
	}
	s := NewClearReapSweeper(nil, engine, logger.NewWithWriter(&buf))
	s.discover = func(_ context.Context, after string) ([]string, error) {
		if after == "" {
			return ids, nil
		}
		return nil, nil
	}

	err := s.RunPreflight(context.Background())

	require.Error(t, err)
	var passErr *clearReapPassError
	require.ErrorAs(t, err, &passErr)
	assert.True(t, passErr.fatal)
	var pqErr *pq.Error
	require.ErrorAs(t, err, &pqErr)
	assert.Equal(t, "23514", string(pqErr.Code), "the first fatal candidate's SQLSTATE must be reported, not a later candidate-local one")

	logs := buf.String()
	assert.Contains(t, logs, "sqlstate=23514")
	assert.NotContains(t, logs, "sqlstate=40001")
	assert.Contains(t, logs, "fatal=true")
}

// TestClearReapSweeperNonPQFatalErrorLogsErrorClass pins logResult's fallback
// for a fatal error that carries no SQLSTATE at all.
func TestClearReapSweeperNonPQFatalErrorLogsErrorClass(t *testing.T) {
	var buf bytes.Buffer
	engine := &fakeClearReapBatchRunner{plan: func(int) (purge.ClearReapResult, error) {
		return purge.ClearReapResult{}, sql.ErrConnDone
	}}
	s := NewClearReapSweeper(nil, engine, logger.NewWithWriter(&buf))
	s.discover = oneCandidateThenNoneAfter(clearReapTestConversationID)

	err := s.RunPreflight(context.Background())

	require.Error(t, err)
	var passErr *clearReapPassError
	require.ErrorAs(t, err, &passErr)
	assert.True(t, passErr.fatal, "sql.ErrConnDone is not on the candidate-local list")

	logs := buf.String()
	assert.Contains(t, logs, "error_class=bad_conn")
	assert.Contains(t, logs, "fatal=true")
	assert.NotContains(t, logs, clearReapTestConversationID, "no candidate ID may reach the log (I6)")
}

func TestClearReapSweeperLogsCountsOnly(t *testing.T) {
	var buf bytes.Buffer
	engine := &fakeClearReapBatchRunner{plan: func(int) (purge.ClearReapResult, error) {
		return purge.ClearReapResult{}, &pq.Error{Code: "40001", Message: "should never reach the log"}
	}}
	s := NewClearReapSweeper(nil, engine, logger.NewWithWriter(&buf))
	s.discover = oneCandidateThenNoneAfter(clearReapTestConversationID)

	// 40001 is candidate-local, so preflight tolerates it and still drains to
	// completion (D4); the failed pass is what this test needs logged.
	require.NoError(t, s.RunPreflight(context.Background()))
	logs := buf.String()
	assert.Contains(t, logs, "sqlstate=40001")
	assert.NotContains(t, logs, "should never reach the log")
	uuidLike := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-`)
	assert.False(t, uuidLike.MatchString(logs), "log output must contain no UUID-shaped string:\n%s", logs)
}

func TestClearReapSweeperRunPassRequiresDatabaseWhenUsingTheDefaultDiscoverer(t *testing.T) {
	s := NewClearReapSweeper(nil, &fakeClearReapBatchRunner{}, nil)

	_, err := s.RunPass(context.Background())

	require.ErrorContains(t, err, "requires database")
}

func TestClearReapSweeperWorkerUsesDefaultIntervalWhenUnset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	NewClearReapSweeper(nil, &fakeClearReapBatchRunner{}, nil).RunWorker(ctx, 0)
}
