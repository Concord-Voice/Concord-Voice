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
