package accountguard

import "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"

// ---------------------------------------------------------------------------
// The Runner shape these tests were written against (Task 10, #3680 PR3). The producer
// implements to it; every name below is a contract between runner.go and runner_test.go.
//
//	type Runner struct {
//	    ...
//	    policy       func(decide.Input) decide.Outcome // New: the legacy stub; tests: decide.Decide
//	    reg          registry                          // New: productionRegistry()
//	    commitPlain  func(*sql.Tx) error               // nil means tx.Commit(); the plain strategy's commit
//	    beforeCommit func()                            // nil means none; called immediately before every
//	                                                   // commit attempt, plain or forced
//	}
//
// New(d) leaves commitPlain and beforeCommit nil. A test sets them on the value
// newTestRunner returns, before calling Run.
//
// commitPlain is how a test injects an ambiguous commit: it commits the real transaction,
// then returns an error that is not a proven rollback. beforeCommit is how a test observes
// state at the instant before commit (it must run inside the transaction, so the fence marker
// is still held there) or races a writer in.
// ---------------------------------------------------------------------------

// newTestRunner is a Runner whose policy is decide.Decide and whose registry is reg.
func newTestRunner(d Deps, reg registry) *Runner {
	return newTestRunnerWithPolicy(d, reg, decide.Decide)
}

// newTestRunnerWithPolicy is a Runner with a fake policy: how a test reaches an outcome decide
// never emits (a password write without a rotation, a flag that changes between passes).
func newTestRunnerWithPolicy(d Deps, reg registry, policy func(decide.Input) decide.Outcome) *Runner {
	r := New(d)
	r.policy = policy
	r.reg = reg
	return r
}
