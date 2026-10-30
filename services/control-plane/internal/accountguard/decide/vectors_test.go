package decide

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vector harness (plan Task 5; the DoR AC7 shape: one JSON vector per row). Tasks 10-12
// reuse loadVectors, vector.Input.build, normalize and requireOutcomeEqual.
//
// File format (testdata/vectors.json, strictly decoded: an unknown key anywhere is an error):
//
//	{"vectors": [{
//	    "name": "t3/equal-class-opens-request",   unique
//	    "row": "t3",                              the T-table row label this vector covers
//	    "cite": "parent §6.2 T3; ...",            where the expected value comes from
//	    "input":  {action, proof, creds, primaries, reservations, reqs, recent_cancels,
//	               guessable_budget_clear, current_key_id, sso_login, device_bound},
//	    "want":   {row, verdict, source, effects, rotate_epoch, sets_password, continuation,
//	               perm_gen_bump, enrol_class, notices},
//	    "ignore": ["row", "notice.request_id", ...]   fields the plan leaves unspecified
//	}]}
//
// Times are relative to t0 ("-72h", "0", "1ns"), or "legacy" (1970-01-01Z) or "awaiting"
// (2999-01-01Z); an omitted time is the zero time. Credential refs are short strings
// "kind:id[:version]" (version defaults to 1); an omitted ref is the zero CredRef. Byte
// strings (fingerprints) are plain strings. Verdict and effect-kind names are local to this
// file, so the vectors do not pin the String text of those enums.
//
// Ignore tokens drop one unspecified part of both the expected and the actual outcome before
// comparing. Every token is listed in validateIgnore.

type vectorFile struct {
	Vectors []vector `json:"vectors"`
}

type vector struct {
	Name   string      `json:"name"`
	Row    string      `json:"row"`
	Cite   string      `json:"cite"`
	Input  vectorInput `json:"input"`
	Want   vectorWant  `json:"want"`
	Ignore []string    `json:"ignore"`
}

type vectorInput struct {
	Action               json.RawMessage `json:"action"`
	Proof                []string        `json:"proof"`
	Creds                []vCred         `json:"creds"`
	Primaries            []vPrimary      `json:"primaries"`
	Reservations         []vReservation  `json:"reservations"`
	Reqs                 []vReq          `json:"reqs"`
	RecentCancels        []vReq          `json:"recent_cancels"`
	GuessableBudgetClear bool            `json:"guessable_budget_clear"`
	CurrentKeyID         int16           `json:"current_key_id"`
	SSOLogin             bool            `json:"sso_login"`
	DeviceBound          []string        `json:"device_bound"`
}

type vCred struct {
	Ref        string `json:"ref"`
	Matures    string `json:"matures"`
	Contested  string `json:"contested"`
	Paper      bool   `json:"paper"`
	EnrolledAt string `json:"enrolled_at"`
	EnrolClass string `json:"enrol_class"`
}

type vPrimary struct {
	Ref             string `json:"ref"`
	BecamePrimaryAt string `json:"became_primary_at"`
	Matures         string `json:"matures"`
	Contested       string `json:"contested"`
	DisplacedAt     string `json:"displaced_at"`
}

type vReservation struct {
	AddressRef  string `json:"address_ref"`
	DisplacedAt string `json:"displaced_at"`
	FloorUntil  string `json:"floor_until"`
}

type vReq struct {
	ID                    string   `json:"id"`
	Kind                  string   `json:"kind"`
	Status                string   `json:"status"`
	OriginClass           string   `json:"origin_class"`
	Origins               []string `json:"origins"`
	Targets               []string `json:"targets"`
	OpenedAt              string   `json:"opened_at"`
	CompletesAt           string   `json:"completes_at"`
	HeldMs                int64    `json:"held_ms"`
	HeldSince             string   `json:"held_since"`
	DisputedAt            string   `json:"disputed_at"`
	DisputeGroup          string   `json:"dispute_group"`
	Fingerprint           string   `json:"fingerprint"`
	Coarse                string   `json:"coarse"`
	FingerprintKeyID      int16    `json:"fingerprint_key_id"`
	CancelReason          string   `json:"cancel_reason"`
	CancellerClass        string   `json:"canceller_class"`
	CancelledAt           string   `json:"cancelled_at"`
	AcceptedAddressNotice bool     `json:"accepted_address_notice"`
	MatureNoticesSettled  bool     `json:"mature_notices_settled"`
	ResetFactors          bool     `json:"reset_factors"`
}

type vCandidate struct {
	Keyed  string `json:"keyed"`
	Coarse string `json:"coarse"`
	KeyID  int16  `json:"key_id"`
}

type vectorWant struct {
	Row          string    `json:"row"`
	Verdict      string    `json:"verdict"`
	Source       string    `json:"source"`
	Effects      []vEffect `json:"effects"`
	RotateEpoch  bool      `json:"rotate_epoch"`
	SetsPassword bool      `json:"sets_password"`
	Continuation bool      `json:"continuation"`
	PermGenBump  bool      `json:"perm_gen_bump"`
	EnrolClass   string    `json:"enrol_class"`
	Notices      []vNotice `json:"notices"`
}

type vEffect struct {
	Kind           string `json:"kind"`
	RequestID      string `json:"request_id"`
	Cred           string `json:"cred"`
	Open           *vOpen `json:"open"`
	Reason         string `json:"reason"`
	CancellerClass string `json:"canceller_class"`
	Group          string `json:"group"`
	HeldMs         int64  `json:"held_ms"`
	HeldSince      string `json:"held_since"`
}

type vOpen struct {
	Kind         string      `json:"kind"`
	Status       string      `json:"status"`
	Targets      []string    `json:"targets"`
	Origins      []string    `json:"origins"`
	OriginClass  string      `json:"origin_class"`
	ResetFactors bool        `json:"reset_factors"`
	OpenedAt     string      `json:"opened_at"`
	CompletesAt  string      `json:"completes_at"`
	DisputedAt   string      `json:"disputed_at"`
	CompletedAt  string      `json:"completed_at"`
	Candidate    *vCandidate `json:"candidate"`
}

type vNotice struct {
	Kind       string   `json:"kind"`
	RequestID  string   `json:"request_id"`
	Subject    string   `json:"subject"`
	ReservedTo []string `json:"reserved_to"`
}

// vectorVerdicts and vectorEffectKinds are the vector files' names for the two enums whose
// String text the tests deliberately do not pin.
var vectorVerdicts = map[string]Verdict{
	"refused": VerdictRefused, "applied": VerdictApplied, "opened": VerdictOpened,
	"disputed": VerdictDisputed, "held": VerdictHeld, "no_change": VerdictNoChange,
}

var vectorEffectKinds = map[string]EffectKind{
	"remove": EffectRemove, "contest": EffectContest, "open": EffectOpen, "cancel": EffectCancel,
	"dispute": EffectDispute, "return_to_pending": EffectReturnToPending, "complete": EffectComplete,
	"restore_primary": EffectRestorePrimary, "release_reservation": EffectReleaseReservation,
	"hold": EffectHold, "resync_mfa_flags": EffectResyncMFAFlags,
}

// vectorRowLabels are the labels a vector's "row" may carry.
var vectorRowLabels = map[string]bool{
	"t1": true, "t1b": true, "t2": true, "t2b": true, "t3": true, "t4": true, "t5": true, "t6": true,
	"t6c": true, "t7": true, "t8": true, "t9": true, "t10": true, "t11": true, "t12": true, "sweep": true,
}

// ---- conversion ----------------------------------------------------------------------

type conv struct{ err error }

func (c *conv) fail(format string, args ...any) {
	if c.err == nil {
		c.err = fmt.Errorf(format, args...)
	}
}

// convText parses an enum from its text; the empty string is the zero value.
func convText[T any, P unmarshalable[T]](c *conv, s string) T {
	var v T
	if s == "" {
		return v
	}
	if err := P(&v).UnmarshalText([]byte(s)); err != nil {
		c.fail("%q: %v", s, err)
	}
	return v
}

func (c *conv) ref(s string) CredRef {
	if s == "" {
		return CredRef{}
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[1] == "" {
		c.fail("bad credential ref %q: want kind:id[:version]", s)
		return CredRef{}
	}
	kind := convText[CredKind](c, parts[0])
	if kind == CredUnknown {
		c.fail("credential ref %q has no kind", s)
	}
	version := int64(1)
	if len(parts) == 3 {
		v, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			c.fail("credential ref %q: bad version: %v", s, err)
		}
		version = v
	}
	return CredRef{Kind: kind, ID: parts[1], Version: version}
}

func (c *conv) refs(ss []string) []CredRef {
	if len(ss) == 0 {
		return nil
	}
	out := make([]CredRef, 0, len(ss))
	for _, s := range ss {
		out = append(out, c.ref(s))
	}
	return out
}

func (c *conv) at(s string) time.Time {
	switch s {
	case "":
		return time.Time{}
	case "legacy":
		return legacyInstant
	case "awaiting":
		return awaitingInstant
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		c.fail("bad relative time %q: %v", s, err)
		return time.Time{}
	}
	return t0.Add(d)
}

func rawBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func (c *conv) candidate(v *vCandidate) Candidate {
	if v == nil {
		return Candidate{}
	}
	return Candidate{Keyed: rawBytes(v.Keyed), Coarse: rawBytes(v.Coarse), KeyID: v.KeyID}
}

func (v vReq) build(c *conv) Req {
	return Req{
		ID: v.ID, Kind: convText[ReqKind](c, v.Kind), Status: convText[ReqStatus](c, v.Status),
		OriginClass: convText[Class](c, v.OriginClass),
		Origins:     c.refs(v.Origins), Targets: c.refs(v.Targets),
		OpenedAt: c.at(v.OpenedAt), CompletesAt: c.at(v.CompletesAt),
		HeldMs: v.HeldMs, HeldSince: c.at(v.HeldSince),
		DisputedAt: c.at(v.DisputedAt), DisputeGroup: v.DisputeGroup,
		Fingerprint: rawBytes(v.Fingerprint), Coarse: rawBytes(v.Coarse), FingerprintKeyID: v.FingerprintKeyID,
		CancelReason: convText[CancelReason](c, v.CancelReason), CancellerClass: convText[Class](c, v.CancellerClass),
		CancelledAt:           c.at(v.CancelledAt),
		AcceptedAddressNotice: v.AcceptedAddressNotice, MatureNoticesSettled: v.MatureNoticesSettled,
		ResetFactors: v.ResetFactors,
	}
}

// build returns a fresh Input on every call, so a caller can run Decide on one copy and
// compare it with another to detect mutation.
func (vi vectorInput) build() (Input, error) {
	c := &conv{}
	act, err := decodeAction(vi.Action)
	if err != nil {
		return Input{}, err
	}
	in := Input{
		Now: t0, Act: act, Proof: Proof{Used: c.refs(vi.Proof), DeviceBound: c.refs(vi.DeviceBound)},
		GuessableBudgetClear: vi.GuessableBudgetClear, CurrentKeyID: vi.CurrentKeyID, SSOLogin: vi.SSOLogin,
	}
	for _, v := range vi.Creds {
		in.Creds = append(in.Creds, Cred{
			Ref: c.ref(v.Ref), MaturesAt: c.at(v.Matures), ContestedAt: c.at(v.Contested),
			Paper: v.Paper, EnrolledAt: c.at(v.EnrolledAt),
			EnrolClass: convText[Class](c, v.EnrolClass),
		})
	}
	for _, v := range vi.Primaries {
		in.Primaries = append(in.Primaries, Primary{
			Ref: c.ref(v.Ref), BecamePrimaryAt: c.at(v.BecamePrimaryAt), MaturesAt: c.at(v.Matures),
			ContestedAt: c.at(v.Contested), DisplacedAt: c.at(v.DisplacedAt),
		})
	}
	for _, v := range vi.Reservations {
		in.Reservations = append(in.Reservations, Reservation{
			AddressRef: c.ref(v.AddressRef), DisplacedAt: c.at(v.DisplacedAt), FloorUntil: c.at(v.FloorUntil),
		})
	}
	for _, v := range vi.Reqs {
		in.Reqs = append(in.Reqs, v.build(c))
	}
	for _, v := range vi.RecentCancels {
		in.RecentCancels = append(in.RecentCancels, v.build(c))
	}
	return in, c.err
}

func (w vectorWant) outcome() (Outcome, error) {
	c := &conv{}
	o := Outcome{
		Row: convText[Row](c, w.Row), Source: Source(w.Source),
		RotateEpoch: w.RotateEpoch, SetsPassword: w.SetsPassword, Continuation: w.Continuation, PermGenBump: w.PermGenBump,
		EnrolClass: convText[Class](c, w.EnrolClass),
	}
	verdict, ok := vectorVerdicts[w.Verdict]
	if !ok {
		c.fail("unknown verdict %q", w.Verdict)
	}
	o.Verdict = verdict
	for _, e := range w.Effects {
		kind, ok := vectorEffectKinds[e.Kind]
		if !ok {
			c.fail("unknown effect kind %q", e.Kind)
		}
		eff := Effect{
			Kind: kind, RequestID: e.RequestID, Cred: c.ref(e.Cred),
			Reason: convText[CancelReason](c, e.Reason), CancellerClass: convText[Class](c, e.CancellerClass),
			Group: e.Group, HeldMs: e.HeldMs, HeldSince: c.at(e.HeldSince),
		}
		if e.Open != nil {
			eff.Open = Opening{
				Kind: convText[ReqKind](c, e.Open.Kind), Status: convText[ReqStatus](c, e.Open.Status),
				Targets: c.refs(e.Open.Targets), Origins: c.refs(e.Open.Origins),
				OriginClass: convText[Class](c, e.Open.OriginClass), ResetFactors: e.Open.ResetFactors,
				OpenedAt: c.at(e.Open.OpenedAt), CompletesAt: c.at(e.Open.CompletesAt),
				DisputedAt: c.at(e.Open.DisputedAt), CompletedAt: c.at(e.Open.CompletedAt),
				Candidate: c.candidate(e.Open.Candidate),
			}
		}
		o.Effects = append(o.Effects, eff)
	}
	for _, n := range w.Notices {
		o.Notices = append(o.Notices, Notice{
			Kind: convText[NoticeKind](c, n.Kind), RequestID: n.RequestID, Subject: c.ref(n.Subject),
			ReservedTo: c.refs(n.ReservedTo),
		})
	}
	return o, c.err
}

// ---- actions -------------------------------------------------------------------------

// actionFields lists, per action type, the keys other than "type" the action may carry.
var actionFields = map[string][]string{
	"sign_out_others": {}, "sign_out_everywhere": {}, "remove": {"target", "candidate"},
	"self_remove_backup": {"target"}, "enrol": {"ref"}, "contest": {},
	"open_break_glass": {"reset", "candidate"}, "cancel": {"ids"}, "complete": {"id"},
	"resolve": {"group", "chosen"}, "expire": {},
}

type actionJSON struct {
	Type      string      `json:"type"`
	Target    string      `json:"target"`
	Ref       string      `json:"ref"`
	Reset     bool        `json:"reset"`
	Candidate *vCandidate `json:"candidate"`
	IDs       []string    `json:"ids"`
	ID        string      `json:"id"`
	Group     string      `json:"group"`
	Chosen    string      `json:"chosen"`
}

// decodeAction decodes an action keyed on "type". An unknown type, a key the type does not
// take, or any unknown key is an error.
func decodeAction(raw json.RawMessage) (Action, error) {
	if len(raw) == 0 {
		return nil, errors.New("action is missing")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("action is not an object: %w", err)
	}
	typeRaw, ok := keys["type"]
	if !ok {
		return nil, errors.New(`action has no "type"`)
	}
	var typ string
	if err := json.Unmarshal(typeRaw, &typ); err != nil {
		return nil, fmt.Errorf(`action "type" is not a string: %w`, err)
	}
	allowed, ok := actionFields[typ]
	if !ok {
		return nil, fmt.Errorf("unknown action type %q", typ)
	}
	for k := range keys {
		if k == "type" {
			continue
		}
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return nil, fmt.Errorf("action %q does not take key %q", typ, k)
		}
	}
	var a actionJSON
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("action %q: %w", typ, err)
	}
	c := &conv{}
	var act Action
	switch typ {
	case "sign_out_others":
		act = SignOutOthers{}
	case "sign_out_everywhere":
		act = SignOutEverywhere{}
	case "remove":
		act = Remove{Target: c.ref(a.Target), Candidate: c.candidate(a.Candidate)}
	case "self_remove_backup":
		act = SelfRemoveBackup{Target: c.ref(a.Target)}
	case "enrol":
		act = Enrol{Ref: c.ref(a.Ref)}
	case "contest":
		act = Contest{}
	case "open_break_glass":
		act = OpenBreakGlass{Reset: a.Reset, Candidate: c.candidate(a.Candidate)}
	case "cancel":
		act = Cancel{IDs: a.IDs}
	case "complete":
		act = Complete{ID: a.ID}
	case "resolve":
		act = Resolve{Group: a.Group, Chosen: a.Chosen}
	default:
		act = Expire{}
	}
	return act, c.err
}

// ---- loading -------------------------------------------------------------------------

// validateIgnore accepts the closed set of ignore tokens.
func validateIgnore(tok string) error {
	name, arg, hasArg := strings.Cut(tok, ":")
	if !hasArg {
		switch name {
		case "row", "rotate_epoch", "notice.request_id", "notice.reserved_to", "notice.change_subject",
			"open.targets", "open.completes_at":
			return nil
		}
		return fmt.Errorf("unknown ignore token %q", tok)
	}
	c := &conv{}
	switch name {
	case "notice.kind":
		if convText[NoticeKind](c, arg) == NoticeUnknown {
			return fmt.Errorf("ignore token %q names no notice kind", tok)
		}
		return c.err
	case "effect.request_id", "effect.kind":
		if _, ok := vectorEffectKinds[arg]; !ok {
			return fmt.Errorf("ignore token %q names no effect kind", tok)
		}
		return nil
	case "effect.dispute":
		if arg == "" {
			return fmt.Errorf("ignore token %q needs a request id", tok)
		}
		return nil
	}
	return fmt.Errorf("unknown ignore token %q", tok)
}

// parseVectors strictly decodes a vectors file: an unknown key or trailing data is an error.
func parseVectors(data []byte) ([]vector, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f vectorFile
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the vectors object")
	}
	if len(f.Vectors) == 0 {
		return nil, errors.New("no vectors")
	}
	seen := map[string]bool{}
	for _, v := range f.Vectors {
		switch {
		case v.Name == "":
			return nil, errors.New("a vector has no name")
		case seen[v.Name]:
			return nil, fmt.Errorf("duplicate vector name %q", v.Name)
		case strings.TrimSpace(v.Cite) == "":
			return nil, fmt.Errorf("vector %q cites no spec row", v.Name)
		case !vectorRowLabels[v.Row]:
			return nil, fmt.Errorf("vector %q has unknown row label %q", v.Name, v.Row)
		}
		seen[v.Name] = true
		for _, tok := range v.Ignore {
			if err := validateIgnore(tok); err != nil {
				return nil, fmt.Errorf("vector %q: %w", v.Name, err)
			}
		}
	}
	return f.Vectors, nil
}

// loadVectors reads testdata/vectors.json (relative to the package directory; CI runs -trimpath).
func loadVectors(t *testing.T) []vector {
	t.Helper()
	data, err := os.ReadFile("testdata/vectors.json")
	require.NoError(t, err)
	vs, err := parseVectors(data)
	require.NoError(t, err)
	return vs
}

// ---- comparison ----------------------------------------------------------------------

func utc(tm time.Time) time.Time {
	if tm.IsZero() {
		return time.Time{}
	}
	return tm.UTC().Round(0)
}

func credRefLess(a, b CredRef) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	return a.Version < b.Version
}

// nilIfEmptyRefs returns a copy in the same order, nil when empty. The order of an emitted
// []CredRef is pinned (architect ruling 6), so comparisons must not sort it.
func nilIfEmptyRefs(in []CredRef) []CredRef {
	if len(in) == 0 {
		return nil
	}
	return append([]CredRef(nil), in...)
}

// refsStrictlyIncreasing reports whether refs are in the canonical order (Kind numeric, ID
// byte-wise, Version) with no duplicate.
func refsStrictlyIncreasing(refs []CredRef) bool {
	for i := 1; i < len(refs); i++ {
		if !credRefLess(refs[i-1], refs[i]) {
			return false
		}
	}
	return true
}

func nilIfEmptyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

// effectLess is the canonical Effects order (plan D18, ruling 6): effect kind, request ID, then
// the credential by Kind, ID (byte-wise) and Version.
func effectLess(a, b Effect) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.RequestID != b.RequestID {
		return a.RequestID < b.RequestID
	}
	return credRefLess(a.Cred, b.Cred)
}

// noticeLess is the canonical Notices order (plan D18, ruling 6): notice kind, request ID, then
// the subject by Kind, ID (byte-wise) and Version.
func noticeLess(a, b Notice) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.RequestID != b.RequestID {
		return a.RequestID < b.RequestID
	}
	return credRefLess(a.Subject, b.Subject)
}

func normalizeEffect(e Effect) Effect {
	e.HeldSince = utc(e.HeldSince)
	e.Open.Targets = nilIfEmptyRefs(e.Open.Targets)
	e.Open.Origins = nilIfEmptyRefs(e.Open.Origins)
	e.Open.OpenedAt = utc(e.Open.OpenedAt)
	e.Open.CompletesAt = utc(e.Open.CompletesAt)
	e.Open.DisputedAt = utc(e.Open.DisputedAt)
	e.Open.CompletedAt = utc(e.Open.CompletedAt)
	e.Open.Candidate.Keyed = nilIfEmptyBytes(e.Open.Candidate.Keyed)
	e.Open.Candidate.Coarse = nilIfEmptyBytes(e.Open.Candidate.Coarse)
	return e
}

// normalize makes two outcomes comparable: times are UTC, empty slices are nil, and the
// effect and notice lists are sorted. Ref lists are NOT sorted. It returns a copy.
// The order of the effect and notice lists is checked separately
// (TestVectors_OutputIsCanonicallyOrdered); the order of every []CredRef is pinned here by
// comparing it as emitted (architect ruling 6).
func normalize(o Outcome) Outcome {
	out := o
	out.Effects = nil
	for _, e := range o.Effects {
		out.Effects = append(out.Effects, normalizeEffect(e))
	}
	sort.SliceStable(out.Effects, func(i, j int) bool {
		if effectLess(out.Effects[i], out.Effects[j]) != effectLess(out.Effects[j], out.Effects[i]) {
			return effectLess(out.Effects[i], out.Effects[j])
		}
		return fmt.Sprintf("%+v", out.Effects[i]) < fmt.Sprintf("%+v", out.Effects[j])
	})
	out.Notices = nil
	for _, n := range o.Notices {
		n.ReservedTo = nilIfEmptyRefs(n.ReservedTo)
		out.Notices = append(out.Notices, n)
	}
	sort.SliceStable(out.Notices, func(i, j int) bool {
		if noticeLess(out.Notices[i], out.Notices[j]) != noticeLess(out.Notices[j], out.Notices[i]) {
			return noticeLess(out.Notices[i], out.Notices[j])
		}
		return fmt.Sprintf("%+v", out.Notices[i]) < fmt.Sprintf("%+v", out.Notices[j])
	})
	return out
}

func isChangeNotice(k NoticeKind) bool {
	switch k {
	case NoticeChangeRequested, NoticeChangeCancelled, NoticeChangeCompleted, NoticeChangeDisputed:
		return true
	}
	return false
}

// applyIgnore removes the parts of an outcome that a vector's ignore tokens name. It returns a
// copy and leaves its argument untouched.
func applyIgnore(o Outcome, ignore []string) Outcome {
	out := o
	out.Effects = append([]Effect(nil), o.Effects...)
	out.Notices = append([]Notice(nil), o.Notices...)
	for _, tok := range ignore {
		name, arg, _ := strings.Cut(tok, ":")
		switch name {
		case "row":
			out.Row = RowNone
		case "rotate_epoch":
			out.RotateEpoch = false
		case "notice.request_id":
			for i := range out.Notices {
				out.Notices[i].RequestID = ""
			}
		case "notice.reserved_to":
			for i := range out.Notices {
				out.Notices[i].ReservedTo = nil
			}
		case "notice.change_subject":
			for i := range out.Notices {
				if isChangeNotice(out.Notices[i].Kind) {
					out.Notices[i].Subject = CredRef{}
				}
			}
		case "notice.kind":
			var c conv
			drop := convText[NoticeKind](&c, arg)
			kept := out.Notices[:0:0]
			for _, n := range out.Notices {
				if n.Kind != drop {
					kept = append(kept, n)
				}
			}
			out.Notices = kept
		case "open.targets":
			for i := range out.Effects {
				out.Effects[i].Open.Targets = nil
			}
		case "open.completes_at":
			for i := range out.Effects {
				out.Effects[i].Open.CompletesAt = time.Time{}
			}
		case "effect.request_id":
			for i := range out.Effects {
				if out.Effects[i].Kind == vectorEffectKinds[arg] {
					out.Effects[i].RequestID = ""
				}
			}
		case "effect.kind":
			kept := out.Effects[:0:0]
			for _, e := range out.Effects {
				if e.Kind != vectorEffectKinds[arg] {
					kept = append(kept, e)
				}
			}
			out.Effects = kept
		case "effect.dispute":
			kept := out.Effects[:0:0]
			for _, e := range out.Effects {
				if e.Kind != EffectDispute || e.RequestID != arg {
					kept = append(kept, e)
				}
			}
			out.Effects = kept
		}
	}
	return out
}

// requireOutcomeEqual compares an expected and an actual outcome after dropping the ignored parts.
func requireOutcomeEqual(t *testing.T, want, got Outcome, ignore []string) {
	t.Helper()
	require.Equal(t, normalize(applyIgnore(want, ignore)), normalize(applyIgnore(got, ignore)))
}

// ---- the vector tests -----------------------------------------------------------------

// decideVector builds the vector's input afresh and runs Decide on it.
func decideVector(t *testing.T, v vector) (Input, Outcome) {
	t.Helper()
	in, err := v.Input.build()
	require.NoError(t, err)
	return in, Decide(in)
}

func TestVectors(t *testing.T) {
	vs := loadVectors(t)
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			want, err := v.Want.outcome()
			require.NoError(t, err)
			require.Contains(t, []Source{SourceUserSession, SourceBreakGlass}, want.Source,
				"a vector expects only user_session or break_glass (spec §7)")
			_, got := decideVector(t, v)
			requireOutcomeEqual(t, want, got, v.Ignore)
		})
	}
	t.Logf("%d vectors", len(vs))
}

// TestVectorsCoverEveryRow (plan Task 9): at least one vector per T-table row, plus the named extras.
func TestVectorsCoverEveryRow(t *testing.T) {
	vs := loadVectors(t)
	rows := map[string]int{}
	names := map[string]bool{}
	for _, v := range vs {
		rows[v.Row]++
		names[v.Name] = true
	}
	for _, row := range []string{"t1", "t1b", "t2", "t2b", "t3", "t4", "t5", "t6", "t6c", "t7", "t8", "t9", "t10", "t11", "t12"} {
		assert.Positive(t, rows[row], "no vector covers row %s", row)
	}
	extras := []string{
		"t11/break-glass-restores-reserved-primary",
		"sweep/reservation-released",
		"sweep/reservation-held-before-floor",
		"sweep/reservation-held-while-primary-immature",
		"sweep/reservation-held-by-open-break-glass",
		"t1b/contests-phisher-immature-factor",
		"t1b/contests-immature-anchor",
		"t1b/never-contests-address-or-password",
		"t1b/does-not-contest-higher-enrolment",
	}
	for _, name := range extras {
		assert.True(t, names[name], "missing named vector %s", name)
	}
}

// TestVectors_EveryActingRowHasAnAcceptingVector guards that no row's vectors are refusals
// only: each acting row needs at least one non-refused expectation.
func TestVectors_EveryActingRowHasAnAcceptingVector(t *testing.T) {
	accepting := map[string]bool{}
	for _, v := range loadVectors(t) {
		if v.Want.Verdict != "refused" {
			accepting[v.Row] = true
		}
	}
	for _, row := range []string{"t1", "t1b", "t2", "t2b", "t3", "t4", "t5", "t6", "t6c", "t7", "t8", "t10", "t11", "t12", "sweep"} {
		assert.True(t, accepting[row], "row %s has no accepting vector", row)
	}
}

// TestVectors_OutputIsCanonicallyOrdered (plan D18): Effects and Notices come out sorted by
// (kind, request ID, credential kind, credential ID) and are nil when empty.
func TestVectors_OutputIsCanonicallyOrdered(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			_, got := decideVector(t, v)
			if len(got.Effects) == 0 {
				assert.Nil(t, got.Effects, "empty Effects must be nil")
			}
			if len(got.Notices) == 0 {
				assert.Nil(t, got.Notices, "empty Notices must be nil")
			}
			assert.True(t, sort.SliceIsSorted(got.Effects, func(i, j int) bool { return effectLess(got.Effects[i], got.Effects[j]) }),
				"Effects not canonically sorted: %+v", got.Effects)
			assert.True(t, sort.SliceIsSorted(got.Notices, func(i, j int) bool { return noticeLess(got.Notices[i], got.Notices[j]) }),
				"Notices not canonically sorted: %+v", got.Notices)
		})
	}
}

// TestVectors_EmittedRefListsAreStrictlyOrdered (architect ruling 6): every emitted []CredRef --
// Origins, Targets and ReservedTo -- is in canonical order (Kind numeric, ID byte-wise, Version)
// and carries no duplicate. PR3 hashes origin_fingerprint over Origins as emitted.
func TestVectors_EmittedRefListsAreStrictlyOrdered(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			_, got := decideVector(t, v)
			for _, e := range got.Effects {
				assert.True(t, refsStrictlyIncreasing(e.Open.Origins), "Origins %+v", e.Open.Origins)
				assert.True(t, refsStrictlyIncreasing(e.Open.Targets), "Targets %+v", e.Open.Targets)
			}
			for _, n := range got.Notices {
				assert.True(t, refsStrictlyIncreasing(n.ReservedTo), "ReservedTo %+v", n.ReservedTo)
			}
		})
	}
}

// TestVectors_ExpectedRefListsAreWrittenInCanonicalOrder guards the data: because the comparison
// does not sort ref lists, a vector listing them out of order would pin the wrong order.
func TestVectors_ExpectedRefListsAreWrittenInCanonicalOrder(t *testing.T) {
	for _, v := range loadVectors(t) {
		want, err := v.Want.outcome()
		require.NoError(t, err, v.Name)
		for _, e := range want.Effects {
			assert.True(t, refsStrictlyIncreasing(e.Open.Origins), "%s: Origins %+v", v.Name, e.Open.Origins)
			assert.True(t, refsStrictlyIncreasing(e.Open.Targets), "%s: Targets %+v", v.Name, e.Open.Targets)
		}
		for _, n := range want.Notices {
			assert.True(t, refsStrictlyIncreasing(n.ReservedTo), "%s: ReservedTo %+v", v.Name, n.ReservedTo)
		}
	}
}

// TestVectors_NonRefusedOutcomesNameARow (ruling 8): a non-refused outcome never carries RowNone.
func TestVectors_NonRefusedOutcomesNameARow(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			_, got := decideVector(t, v)
			if got.Verdict != VerdictRefused {
				assert.NotEqual(t, RowNone, got.Row)
			}
		})
	}
}

// TestVectors_NoIgnoreTokensRemain records that the architect's rulings decided every field the
// plan left open: the vector file ignores nothing. A future vector may use an ignore token only
// for a field a later ruling leaves open, and must then relax this test with a citation.
func TestVectors_NoIgnoreTokensRemain(t *testing.T) {
	for _, v := range loadVectors(t) {
		assert.Empty(t, v.Ignore, v.Name)
	}
}

// TestVectors_DecideDoesNotMutateItsInput: Decide must not write through any slice or struct
// of the snapshot it is given.
func TestVectors_DecideDoesNotMutateItsInput(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			used, _ := decideVector(t, v)
			fresh, err := v.Input.build()
			require.NoError(t, err)
			require.Equal(t, fresh, used, "Decide changed its input")
		})
	}
}

// TestVectors_DecideIsDeterministic: two runs over identical snapshots give identical outcomes
// (no map iteration order, no package state).
func TestVectors_DecideIsDeterministic(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			_, first := decideVector(t, v)
			for i := 0; i < 3; i++ {
				_, again := decideVector(t, v)
				require.Equal(t, first, again)
			}
		})
	}
}

// reorder returns a copy of xs reversed (mode 0) or rotated left by one (mode 1).
func reorder[T any](xs []T, mode int) []T {
	if xs == nil {
		return nil
	}
	out := make([]T, len(xs))
	for i := range xs {
		switch {
		case mode == 0:
			out[len(xs)-1-i] = xs[i]
		case len(xs) > 0:
			out[i] = xs[(i+1)%len(xs)]
		}
	}
	return out
}

func reorderReqs(reqs []Req, mode int) []Req {
	out := reorder(reqs, mode)
	for i := range out {
		out[i].Origins = reorder(out[i].Origins, mode)
		out[i].Targets = reorder(out[i].Targets, mode)
	}
	return out
}

// reorderInput reorders every slice the snapshot carries, including the Cancel action's IDs.
func reorderInput(in Input, mode int) Input {
	out := in
	out.Proof = Proof{Used: reorder(in.Proof.Used, mode), DeviceBound: reorder(in.Proof.DeviceBound, mode)}
	out.Creds = reorder(in.Creds, mode)
	out.Primaries = reorder(in.Primaries, mode)
	out.Reservations = reorder(in.Reservations, mode)
	out.Reqs = reorderReqs(in.Reqs, mode)
	out.RecentCancels = reorderReqs(in.RecentCancels, mode)
	if c, ok := in.Act.(Cancel); ok {
		out.Act = Cancel{IDs: reorder(c.IDs, mode)}
	}
	return out
}

// TestVectors_OutputIsIndependentOfInputSliceOrder (plan D18): reversing or rotating any input
// slice leaves the outcome byte-identical.
func TestVectors_OutputIsIndependentOfInputSliceOrder(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			in, err := v.Input.build()
			require.NoError(t, err)
			base := Decide(in)
			for mode := 0; mode < 2; mode++ {
				permuted, err := v.Input.build()
				require.NoError(t, err)
				require.Equal(t, base, Decide(reorderInput(permuted, mode)), "mode %d", mode)
			}
		})
	}
}

// TestVectors_SourceNeverUnattributedOrOperator (plan Task 5, D11).
func TestVectors_SourceNeverUnattributedOrOperator(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			_, got := decideVector(t, v)
			assert.Contains(t, []Source{SourceUserSession, SourceBreakGlass}, got.Source)
		})
	}
}

// TestVectors_RefusedExpectationsCarryNothing: a refused expectation lists no effects, notices
// or flags (an editing slip in the data would otherwise pin a nonsensical refusal).
func TestVectors_RefusedExpectationsCarryNothing(t *testing.T) {
	for _, v := range loadVectors(t) {
		if v.Want.Verdict != "refused" {
			continue
		}
		w := v.Want
		assert.Empty(t, w.Effects, v.Name)
		assert.Empty(t, w.Notices, v.Name)
		assert.False(t, w.RotateEpoch || w.SetsPassword || w.Continuation || w.PermGenBump, v.Name)
	}
}

// ---- harness self-tests ---------------------------------------------------------------

func TestHarness_ParseVectorsIsStrict(t *testing.T) {
	good := `{"vectors":[{"name":"a","row":"t1","cite":"c","input":{"action":{"type":"expire"}},"want":{"verdict":"refused","source":"user_session"}}]}`
	_, err := parseVectors([]byte(good))
	require.NoError(t, err)

	bad := map[string]string{
		"unknown top-level key": `{"vectors":[],"extra":1}`,
		"unknown vector key":    `{"vectors":[{"name":"a","row":"t1","cite":"c","oops":1,"input":{"action":{"type":"expire"}},"want":{}}]}`,
		"unknown input key":     `{"vectors":[{"name":"a","row":"t1","cite":"c","input":{"action":{"type":"expire"},"oops":1},"want":{}}]}`,
		"unknown factor key":    `{"vectors":[{"name":"a","row":"t1","cite":"c","input":{"action":{"type":"expire"},"creds":[{"ref":"totp:x","oops":1}]},"want":{}}]}`,
		"unknown want key":      `{"vectors":[{"name":"a","row":"t1","cite":"c","input":{"action":{"type":"expire"}},"want":{"oops":1}}]}`,
		"unknown effect key":    `{"vectors":[{"name":"a","row":"t1","cite":"c","input":{"action":{"type":"expire"}},"want":{"effects":[{"kind":"remove","oops":1}]}}]}`,
		"trailing data":         good + `{}`,
		"no vectors":            `{"vectors":[]}`,
		"duplicate name":        `{"vectors":[{"name":"a","row":"t1","cite":"c","input":{},"want":{}},{"name":"a","row":"t1","cite":"c","input":{},"want":{}}]}`,
		"missing cite":          `{"vectors":[{"name":"a","row":"t1","input":{},"want":{}}]}`,
		"unknown row label":     `{"vectors":[{"name":"a","row":"t99","cite":"c","input":{},"want":{}}]}`,
		"unknown ignore entry":  `{"vectors":[{"name":"a","row":"t1","cite":"c","ignore":["everything"],"input":{},"want":{}}]}`,
		"unknown notice kind":   `{"vectors":[{"name":"a","row":"t1","cite":"c","ignore":["notice.kind:bogus"],"input":{},"want":{}}]}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := parseVectors([]byte(doc))
			assert.Error(t, err)
		})
	}
}

func TestHarness_DecodeActionIsStrict(t *testing.T) {
	for _, doc := range []string{
		`{"type":"remove","target":"sso:x","candidate":{"keyed":"a","coarse":"b","key_id":1}}`,
		`{"type":"sign_out_others"}`, `{"type":"sign_out_everywhere"}`, `{"type":"self_remove_backup","target":"backup_address:x"}`,
		`{"type":"enrol","ref":"totp:x"}`, `{"type":"contest"}`, `{"type":"open_break_glass","reset":true}`,
		`{"type":"cancel","ids":["a","b"]}`, `{"type":"complete","id":"a"}`, `{"type":"resolve","group":"g","chosen":"a"}`,
		`{"type":"expire"}`,
	} {
		act, err := decodeAction(json.RawMessage(doc))
		require.NoError(t, err, doc)
		require.NotNil(t, act, doc)
	}
	for name, doc := range map[string]string{
		"unknown type":        `{"type":"bogus"}`,
		"missing type":        `{"target":"sso:x"}`,
		"key from another":    `{"type":"expire","target":"sso:x"}`,
		"unknown key":         `{"type":"remove","target":"sso:x","oops":1}`,
		"bad ref":             `{"type":"remove","target":"nope"}`,
		"bad ref kind":        `{"type":"remove","target":"bogus:x"}`,
		"not an object":       `[1]`,
		"unknown cand. key":   `{"type":"remove","target":"sso:x","candidate":{"oops":1}}`,
		"non-string type":     `{"type":1}`,
		"cancel ids wrong":    `{"type":"cancel","ids":"a"}`,
		"unparseable version": `{"type":"enrol","ref":"totp:x:v"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeAction(json.RawMessage(doc))
			assert.Error(t, err)
		})
	}
	_, err := decodeAction(nil)
	assert.Error(t, err, "a missing action")
}

func TestHarness_RelativeTimesAndRefs(t *testing.T) {
	c := &conv{}
	assert.Equal(t, t0, c.at("0"))
	assert.Equal(t, t0.Add(-72*time.Hour), c.at("-72h"))
	assert.Equal(t, t0.Add(time.Nanosecond), c.at("1ns"))
	assert.True(t, c.at("").IsZero())
	assert.Equal(t, legacyInstant, c.at("legacy"))
	assert.Equal(t, awaitingInstant, c.at("awaiting"))
	require.NoError(t, c.err)

	assert.Equal(t, CredRef{Kind: CredTOTP, ID: "x", Version: 1}, c.ref("totp:x"))
	assert.Equal(t, CredRef{Kind: CredPassword, ID: "pw", Version: 2}, c.ref("password:pw:2"))
	assert.Equal(t, CredRef{}, c.ref(""))
	require.NoError(t, c.err)

	bad := &conv{}
	bad.at("tomorrow")
	assert.Error(t, bad.err)
}

// TestHarness_NormalizeAndIgnore proves the comparison is neither blind nor brittle: it
// ignores order and nil-versus-empty, honours an ignore token, and still sees a real difference.
func TestHarness_NormalizeAndIgnore(t *testing.T) {
	a := Outcome{Row: RowT2, Verdict: VerdictApplied, Source: SourceUserSession, Effects: []Effect{
		{Kind: EffectRemove, Cred: fxSSO}, {Kind: EffectResyncMFAFlags},
	}, Notices: []Notice{{Kind: NoticeCredentialRemoved, Subject: fxSSO, RequestID: "r1"}}}
	b := a
	b.Effects = []Effect{{Kind: EffectResyncMFAFlags}, {Kind: EffectRemove, Cred: fxSSO}}
	requireOutcomeEqual(t, a, b, nil)

	empty := Outcome{Effects: []Effect{}, Notices: []Notice{}}
	requireOutcomeEqual(t, Outcome{}, empty, nil)

	c := a
	c.Notices = []Notice{{Kind: NoticeCredentialRemoved, Subject: fxSSO, RequestID: "other"}}
	assert.NotEqual(t, normalize(applyIgnore(a, nil)), normalize(applyIgnore(c, nil)))
	requireOutcomeEqual(t, a, c, []string{"notice.request_id"})

	d := a
	d.Effects = []Effect{{Kind: EffectRemove, Cred: fxTOTP}, {Kind: EffectResyncMFAFlags}}
	assert.NotEqual(t, normalize(applyIgnore(a, nil)), normalize(applyIgnore(d, []string{"notice.request_id"})))

	// applyIgnore must not write through to its argument.
	before := Outcome{Notices: []Notice{{Kind: NoticeChangeRequested, RequestID: "r", Subject: fxSSO}}}
	snapshot := Outcome{Notices: []Notice{{Kind: NoticeChangeRequested, RequestID: "r", Subject: fxSSO}}}
	_ = applyIgnore(before, []string{"notice.request_id", "notice.change_subject"})
	assert.Equal(t, snapshot, before)
}
