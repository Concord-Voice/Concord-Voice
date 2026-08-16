package presence

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// ServerMemberLoader reads one server's member set AT MOST ONCE and replays
// that single outcome to every sender in the same Rich Presence capture (#2681).
//
// Lifetime is exactly one capture. It is never stored on a longer-lived struct
// and never reused across captures: membership between two mutations is
// precisely the state a capture must re-read, so a loader that outlived a
// capture would let it compute an audience from PRE-mutation membership — the
// reconstruction error #2445 exists to prevent.
//
// It is NOT part of the #2445 two-phase seam. It lives entirely inside phase 1
// (PrepareCapture, pre-transaction) and is garbage when that returns. Do not
// mistake it for a cache that survives the transaction boundary.
//
// The load is LAZY. CaptureServerVoiceCandidatesWithMembers consults it only
// below its four short-circuits, so a capture whose senders all have presence
// disabled still issues zero member reads.
type ServerMemberLoader struct {
	db       DBTX
	serverID uuid.UUID

	// once is not defensive dressing: internal/presence is gated by the race
	// detector, and a plain bool here would falsely advertise that a future
	// concurrent PrepareCapture is safe. Callers are single-goroutine today.
	once    sync.Once
	members map[uuid.UUID]bool
	err     error
}

// NewServerMemberLoader builds a loader for one server. It performs no I/O.
func NewServerMemberLoader(db DBTX, serverID uuid.UUID) *ServerMemberLoader {
	return &ServerMemberLoader{db: db, serverID: serverID}
}

// boundTo reports whether this loader was built for serverID.
//
// A loader carries its OWN server binding and its own DBTX, and membersFor
// consults neither of the caller's. A caller that names one server while
// holding a loader for another would therefore resolve the WRONG server's
// membership and receive it with no error — a cross-server audience. Callers
// MUST assert the binding before using a loader; see captureServerMembers.
func (l *ServerMemberLoader) boundTo(serverID uuid.UUID) bool {
	return l.serverID == serverID
}

// membersFor returns the server's members with senderID removed, as a fresh
// copy. The underlying read happens once; a FAILED read is replayed to every
// later sender rather than retried once per sender.
//
// The read executes under the FIRST caller's ctx on the loader's OWN db, and
// both the value and the error are replayed to everyone after it. Every caller
// in a capture must therefore share the ctx and the DBTX the loader was built
// for; an in-transaction caller must build the loader over that same *sql.Tx,
// or the member read escapes the transaction's snapshot.
func (l *ServerMemberLoader) membersFor(
	ctx context.Context, senderID uuid.UUID,
) (map[uuid.UUID]bool, error) {
	l.once.Do(func() {
		l.members, l.err = allServerMembers(ctx, l.db, l.serverID)
	})
	if l.err != nil {
		return nil, l.err
	}
	return membersExcluding(l.members, senderID), nil
}
