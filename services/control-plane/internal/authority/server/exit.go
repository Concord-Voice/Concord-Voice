package server

// The exit reasons (B2-D8, B3-D12), exported for cmd/account-authority's
// verbs.
const (
	ReasonConfig = "config"
	ReasonTrust  = "trust"
	ReasonLock   = "lock"
	// ReasonNotLeader: an owner verb reached a standby (B3-D7).
	ReasonNotLeader = "not_leader"
	// ReasonReplication: an owner verb committed locally, not replicated
	// (D-225); a re-run converges (B3-D8).
	ReasonReplication = "replication"
	// ReasonRefused: an owner verb's own refusal, such as a digest mismatch,
	// an unknown key or a wrong state.
	ReasonRefused = "refused"
)

// The steps an ExitError names: a closed set, logged as
// authority.exit's step beside the reason, so an operator reads which
// startup step refused without parsing the error text.
const (
	StepDeps        = "deps"
	StepDumpable    = "dumpable"
	StepCredentials = "credentials"
	StepInputs      = "inputs"
	StepDatabase    = "database"
	StepListen      = "listen"
	StepRole        = "role"
	StepLock        = "lock"
	StepSchema      = "schema"
	StepStore       = "store"
	StepLeader      = "leader"
	StepTrust       = "trust"
	StepKeys        = "keys"
	StepCert        = "cert"
	StepPromotion   = "promotion"
	StepServe       = "serve"
	StepRealm       = "realm"
	StepMigrate     = "migrate"
	StepMeta        = "meta"
	StepOwner       = "owner" // an owner verb's own step (B3-D12)
)

// ExitError is a refusal to start, or a fatal stop: exit 1, logged as
// authority.exit with Reason (B2-D8) and, when set, Step.
type ExitError struct {
	Reason string
	Step   string
	Err    error
}

// Error reads as the reason alone when there is no cause, so formatting an
// exit never panics.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

func refuse(reason string, err error) error { return &ExitError{Reason: reason, Err: err} }

// RefuseAt is refuse naming the step that refused. cmd/account-authority's
// verbs build their refusals with it, so the reasons and steps are this
// package's alone.
func RefuseAt(step, reason string, err error) error {
	return &ExitError{Reason: reason, Step: step, Err: err}
}
