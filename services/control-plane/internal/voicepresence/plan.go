// Package voicepresence reconciles active Server Voice Rich Presence with
// RBAC/SBAC visibility mutations (#2445). It is the ONLY package importing both
// internal/rbac and internal/presence: internal/rbac declares the narrow
// PresenceRecheck interface at the consumer, and this package implements it.
package voicepresence

import (
	"sort"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/google/uuid"
)

// SenderCapture is one active Server Voice sender's pre-mutation state. It is
// filled in two phases: PrepareCapture (pre-transaction) sets SenderID,
// ChannelID, Scope, and Candidates; CaptureVisibility (in-transaction) sets
// OldAudience = Candidates ∩ txVisible(ChannelID).
type SenderCapture struct {
	SenderID    uuid.UUID
	ChannelID   string
	Scope       presence.Scope
	Candidates  map[uuid.UUID]bool
	OldAudience map[uuid.UUID]bool
}

// Plan is the opaque capture handed back to internal/rbac. Senders are ordered
// by UUID so a category cascade dispatches deterministically. ServerID and
// OnlyUserID are carried from phase 1 so phase 2 needs no extra parameters.
type Plan struct {
	ServerID   string
	OnlyUserID *string
	Senders    []SenderCapture
}

// HasWork reports whether any sender carries a non-empty CAPTURED AUDIENCE —
// i.e. whether phase 2 found anyone who could see the sender before the write.
// Phase-1 candidates alone are not work. A capture over a channel with no
// active senders, or over a sender whose base presence is off, is the benign
// terminal.
func (p *Plan) HasWork() bool {
	if p == nil {
		return false
	}
	for _, sender := range p.Senders {
		if len(sender.OldAudience) > 0 {
			return true
		}
	}
	return false
}

// hasCandidates reports whether phase 1 found anything for phase 2 to filter.
// When it is false, CaptureVisibility issues ZERO visibility queries.
func (p *Plan) hasCandidates() bool {
	if p == nil {
		return false
	}
	for _, sender := range p.Senders {
		if len(sender.Candidates) > 0 {
			return true
		}
	}
	return false
}

// CapturedAudience is the union of every sender's captured audience — the exact
// viewer set Abandon disconnects fail-closed.
func (p *Plan) CapturedAudience() map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool)
	if p == nil {
		return out
	}
	for _, sender := range p.Senders {
		for viewerID, included := range sender.OldAudience {
			if included {
				out[viewerID] = true
			}
		}
	}
	return out
}

type planBuilder struct {
	serverID   string
	onlyUserID *string
	bySender   map[uuid.UUID]*SenderCapture
}

func newPlanBuilder(serverID string, onlyUserID *string) *planBuilder {
	return &planBuilder{
		serverID:   serverID,
		onlyUserID: onlyUserID,
		bySender:   make(map[uuid.UUID]*SenderCapture),
	}
}

// add records one sender's phase-1 CANDIDATE set for one channel. A sender
// occupies at most one voice channel (buildServerVoice rejects an ambiguous
// multi-row sender), so the union across channels is defensive: it guarantees a
// cascade issues exactly ONE RefreshServerVoiceRecheck per sender and never
// drops a candidate. OldAudience stays empty until CaptureVisibility runs.
func (b *planBuilder) add(
	senderID uuid.UUID,
	channelID string,
	scope presence.Scope,
	candidates map[uuid.UUID]bool,
) {
	existing, seen := b.bySender[senderID]
	if !seen {
		existing = &SenderCapture{
			SenderID:    senderID,
			ChannelID:   channelID,
			Scope:       scope,
			Candidates:  make(map[uuid.UUID]bool, len(candidates)),
			OldAudience: make(map[uuid.UUID]bool),
		}
		b.bySender[senderID] = existing
	}
	for candidateID, included := range candidates {
		if included {
			existing.Candidates[candidateID] = true
		}
	}
}

func (b *planBuilder) build() *Plan {
	senders := make([]SenderCapture, 0, len(b.bySender))
	for _, capture := range b.bySender {
		senders = append(senders, *capture)
	}
	sort.Slice(senders, func(i, j int) bool {
		return senders[i].SenderID.String() < senders[j].SenderID.String()
	})
	return &Plan{ServerID: b.serverID, OnlyUserID: b.onlyUserID, Senders: senders}
}
