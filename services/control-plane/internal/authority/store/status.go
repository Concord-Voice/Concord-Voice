package store

import (
	"errors"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

func digestOr(p []byte) []byte {
	if len(p) == 0 {
		return nil
	}
	return ac.KeyDigest(p)
}

// statusOf is R5's Derive(H, s) over the normalized head h (n is Normalize's
// output on it). A held raw pending head stays pending with anb = ANB_NEVER;
// an unheld one is pending until `visible` (B1-D23).
func statusOf(realm string, h *headRow, n normOut, now uint64, sig *sigDelegation) ac.Status {
	st := ac.Status{RealmID: realm, AccountID: h.AccountID, Seq: h.Seq, HeadEntryDigest: h.Entry, State: h.State,
		PriorState: h.Prior, SuspendedBy: h.susp(), ActiveRootDigest: digestOr(h.Root), RootRevoked: b01(h.RootRevoked),
		EKBindingDigest: h.EK, PendingRootDigest: digestOr(h.PendRoot), PendingEKBindingDigest: h.PendEK,
		LegacyEKDigest: h.Pin, LegacyPinRevoked: b01(h.PinRevoked), Held: b01(h.Held), AsOfMs: now,
		ValidUntilMs: now + VMs, DelegationGen: sig.Gen, DelegationSerial: sig.Serial}
	if h.State == ac.StatePending {
		st.ActivationNotBeforeMs = n.Visible
		if h.Held {
			st.ActivationNotBeforeMs = ac.ANBNever
		}
	}
	return st
}

func deviceStatusOf(realm string, h *headRow, now uint64, sig *sigDelegation) ac.DeviceStatus {
	return ac.DeviceStatus{RealmID: realm, AccountID: h.AccountID, ChainSeq: h.Seq, ChainHeadDigest: h.Entry, Dseq: h.Dseq,
		DeviceHeadDigest: h.DeviceHead, DeviceSetDigest: h.DeviceSetDigest, AsOfMs: now, ValidUntilMs: now + VMs,
		DelegationGen: sig.Gen, DelegationSerial: sig.Serial}
}

// signedOf signs msg with this node's RSK into a `signed` envelope (sig_b "").
func (s *Store) signedOf(typ string, msg []byte) ([]byte, error) {
	if msg == nil {
		return nil, errBadSet
	}
	sig, err := signer.SignAndVerify(s.sig, msg)
	if err != nil {
		return nil, errors.Join(errSigner, err) // a failed sign is 503 signer (D-45); errBadSet stays store
	}
	return ac.Signed{Type: typ, Msg: msg, SigA: sig}.Encode(), nil
}

// asOfRetries bounds D-241's wait for db_now to pass an account's last as_of_ms.
// About 50 ms of 1-ms pauses: past it, the database clock is behind (503 clock).
const asOfRetries = 50

// asOfKeepMs bounds the fence's memory: Tick prunes entries older than this.
// Only a database clock stepped back further than this meets the floor (503
// clock), as D-36 refuses a write under a clock behind the head.
const asOfKeepMs = 10 * 60 * 1000

// reserveAsOf is D-241 (DoR:478): at most one status per (account, as_of_ms).
// It records nowMs for every account when nowMs is past the floor and past
// each one's last signed as_of_ms, and records nothing when any is not. In
// memory, on the leader.
func (s *Store) reserveAsOf(accounts []string, nowMs uint64) bool {
	s.asOfMu.Lock()
	defer s.asOfMu.Unlock()
	if nowMs <= s.asOfFloor {
		return false
	}
	for _, a := range accounts {
		if nowMs <= s.asOfLast[a] {
			return false
		}
	}
	if s.asOfLast == nil {
		s.asOfLast = map[string]uint64{}
	}
	for _, a := range accounts {
		s.asOfLast[a] = nowMs
	}
	return true
}

// pruneAsOf drops the entries more than asOfKeepMs behind nowMs, so the map
// holds about ten minutes of reads, including IDs with no account row (review
// round 3). The floor rises to the newest pruned as_of_ms, so a pruned entry
// can never let a second status share its coordinate. Tick calls it.
func (s *Store) pruneAsOf(nowMs uint64) {
	if nowMs <= asOfKeepMs {
		return
	}
	cut := nowMs - asOfKeepMs
	s.asOfMu.Lock()
	defer s.asOfMu.Unlock()
	for a, t := range s.asOfLast {
		if t <= cut {
			delete(s.asOfLast, a)
			s.asOfFloor = max(s.asOfFloor, t)
		}
	}
}

// pause waits for the database clock to move on: 1 ms, or the test hook.
func (s *Store) pause() {
	if s.hooks.Pause != nil {
		s.hooks.Pause()
		return
	}
	time.Sleep(time.Millisecond)
}
