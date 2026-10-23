package store

import ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"

// Kind is what an Outcome's body is.
type Kind uint8

// The Kinds.
const (
	KindCommitted Kind = iota + 1 // `committed` or `device-committed`
	KindRefused                   // `refused` (a recorded refusal)
	KindError                     // `error` (unrecorded)
	KindOK                        // a read's body (`head`, `history`, …)
)

// Outcome is a complete reply. Body is nil only on an empty 204-style read;
// a gated reply whose replication was not confirmed carries the `replication`
// error envelope instead of the signed bytes (step 12).
type Outcome struct {
	HTTP int
	Kind Kind
	Code string
	Body []byte
}

func errorOutcome(http int, code ac.ErrorCode) Outcome {
	return Outcome{HTTP: http, Kind: KindError, Code: string(code), Body: ac.ErrorEnvelope{Code: string(code)}.Encode()}
}

// OutcomeError is a refusal the server puts on the wire exactly as the store
// wrote it, status, ac code and envelope: an operator route's prologue
// refusal, 503 busy, not_leader, disk, store (a role read that failed) or
// replication (operator.go). The store returns it by value, possibly wrapped;
// a caller finds it with errors.As. Whatever tick leg the refusal warrants
// the store has already set, so the server counts nothing for it.
type OutcomeError struct{ Outcome Outcome }

func (e OutcomeError) Error() string { return "authority store: refused: " + e.Outcome.Code }
