package authoritycontract

// The authority's commit path (#3587 PR-B1) builds every published result
// from the same row interpreter clients check results with, so authority
// acceptance ⊆ client acceptance holds by construction (DoR §2.9 invariants,
// AC9). These are thin exports over A2's unexported interpreter: they add no
// rule and read no clock.

// PublishedHead is the client-visible published state (A2's pub).
type PublishedHead struct {
	Seq         uint64
	Entry       []byte
	State       string
	Prior       string
	Susp        []string
	Root        []byte
	RootRevoked uint64
	EK          []byte
	PendRoot    []byte
	PendEK      []byte
	Pin         []byte
	PinRevoked  uint64
	RG          uint64
	ANB         uint64
	Assurance   string
	Issued      uint64
	Op          string
	EKGen       uint64
	PendEKGen   uint64
}

func (h PublishedHead) pub() pub      { return pub(h) }
func publishedOf(p pub) PublishedHead { return PublishedHead(p) }

// GenesisPublishedHead is the head of an account with no event.
func GenesisPublishedHead() PublishedHead { return publishedOf(genesisPub()) }

// HeadOfResult is the published head after a result (A2 pubOfResult).
func HeadOfResult(r Result, resultBytes []byte, ekGen, pendEKGen uint64) PublishedHead {
	return publishedOf(pubOfResult(r, resultBytes, ekGen, pendEKGen))
}

// FromCode is §2.9's from-state code of h.
func (h PublishedHead) FromCode() FromCode { return classify(h.pub()) }

// Activated is §2.9's derived activation of a pending head (no event, no seq bump).
func (h PublishedHead) Activated() PublishedHead { return publishedOf(activate(h.pub())) }

// SelectRow finds the unique §2.9 row whose From, guard and signature pattern
// admit b against h. A bundle whose intent or cp-authz does not decode is
// malformed; no admitting row is transition_invalid.
func SelectRow(h PublishedHead, b Bundle) (Row, Reason) {
	in, err := DecodeIntent(b.Intent)
	if err != nil {
		return Row{}, ReasonMalformed
	}
	az, err := DecodeCPAuthz(b.CPAuthz)
	if err != nil {
		return Row{}, ReasonMalformed
	}
	return selectRow(h.pub(), in.Op, az.Actor, presenceOf(b))
}

// Post is the post-state a row's To column requires.
type Post struct {
	State, Prior     string
	Susp             []string
	Root, EK         []byte
	RootRevoked      uint64
	PendRoot, PendEK []byte
	PinRevoked       uint64
}

// ExpectedPost is the To column of row against h for intent in. outcomeP
// selects the P (not D) outcome of rows 2, 3 and 11.
func ExpectedPost(row Row, h PublishedHead, in Intent, outcomeP bool) Post {
	w := expected(row, h.pub(), rowCtx{in: in, outcomeP: outcomeP})
	prior := ""
	if w.state == StatePending || w.state == StateDisputed {
		prior = w.prior
	}
	return Post{State: w.state, Prior: prior, Susp: w.susp, Root: w.root, EK: w.ek, RootRevoked: w.rootRevoked,
		PendRoot: w.pendRoot, PendEK: w.pendEK, PinRevoked: w.pinRevoked}
}

// CheckPost is the client-checkable check of res against row and h (post-state,
// Δrg, assurance). The authority runs it on every result before signing.
func CheckPost(row Row, h PublishedHead, in Intent, res Result, outcomeP bool, segment, bindingSegment Assurance) Reason {
	return checkPost(row, h.pub(), rowCtx{in: in, res: res, outcomeP: outcomeP, segment: segment, bindingSegment: bindingSegment})
}
