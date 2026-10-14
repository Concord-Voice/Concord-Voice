package authoritycontract

import "slices"

// FloorName is a §2.8 floor. Floor (floor.go) evaluates them.
type FloorName string

// The §2.8 floors (DoR:557–567).
const (
	FloorS         FloorName = "S"
	FloorH         FloorName = "H"
	FloorRFull     FloorName = "R_full"
	FloorREmail    FloorName = "R_email"
	FloorRRk       FloorName = "R_rk"
	FloorREG       FloorName = "REG"
	FloorEraseList FloorName = "ERASE_LIST"
	FloorNone      FloorName = "NONE"
	FloorOP        FloorName = "OP"
)

// guard is a From-column predicate a code alone does not express.
type guard uint8

const (
	gNone               guard = iota
	gUserNotInSet             // row 7
	gUserInSetUnrevoked       // row 9
	gOpInSetUnrevoked         // row 10
	gPriorAbsent              // row 4
	gPriorActiveOrSusp        // row 12
	gRootUnrevoked            // row 13: a root exists and root_revoked = 0
	gLivePinPriorAbsent       // row 14
	gPriorNotAbsent           // row 16
	gAbsentPrior              // row 17: Ap, or P/D with prior absent
)

// sigReq is the Required-signatures column as flags.
type sigReq uint16

const (
	sigCandidate       sigReq = 1 << iota // sig_candidate under intent #12
	sigEKByCandidate                      // ek_binding signed by the candidate
	sigEKByCurrent                        // ek_binding signed by the head's active root
	sigPoP                                // legacy_pop_sig and legacy_spki
	sigCurrent                            // sig_current_root under the head's active root
	sigOperator                           // sig_operator under T12
	sigRecovery                           // sig_recovery under B.recovery_key (recover)
	sigBindProof                          // recovery_bind and recovery_bind_sig (§2.22)
	sigPendingRecovery                    // sig_recovery under chain-head pending_recovery_key (row 21)
	sigHeldStatus                         // held_status (row 22)
)

// toRule is the To column.
type toRule uint8

const (
	toEnrollActive toRule = iota + 1 // row 1
	toPendOrDisp                     // rows 2, 3, 11: P(from) or D(from), set and root_revoked carried
	toDispBurn                       // rows 4, 12: D(x), the competitor's candidate burned
	toRotateRoot                     // row 5
	toRotateEK                       // row 6
	toSuspendAdd                     // rows 7, 8: set ∪ {actor}
	toUnsuspend                      // rows 9, 10: set \ {actor}; Ac if empty
	toRestore                        // rows 13, 21, 22: exactly x
	toAp                             // row 14
	toSor                            // rows 15, 16: Sor, set ∪ {operator}
	toApr                            // row 17
	toPendErase                      // row 18: Pe(x)
	toErased                         // row 19: E, the from-state root kept
	toPr                             // row 20: Pr(x)
)

// assureRule is the Allowed-assurance column.
type assureRule uint8

const (
	aInitial assureRule = iota + 1
	aRow2               // P: continuity; D: ""
	aRow3               // Ap: recovery or unverified; Apr: unverified; D: ""
	aSegment            // row 5: §2.8's segment rule
	aEmpty              // ""
	aRow11              // Sor: unverified; else recovery or unverified ≤ the current segment; D: ""
	aRow20              // ≤ min(recovery, the binding segment)
)

// anbRule is the anb column; R2 enforces it, this records it.
type anbRule uint8

const (
	anbZero anbRule = iota + 1
	anbIfPending
	anbAlways
)

// HeldRule is the authority-only held column.
type HeldRule uint8

// The held column's values (DoR:684).
const (
	HeldRefuse    HeldRule = iota // recorded `held`
	HeldExempt                    // rows 16, 17, 19
	HeldOnly                      // row 22: admitted only while held
	HeldCommitsPe                 // row 18: commits a Pe that cannot activate while held
)

// ForceUnverifiedRule is the authority-only force_unverified column.
type ForceUnverifiedRule uint8

// The force_unverified column's values (DoR:685).
const (
	FUKeep   ForceUnverifiedRule = iota // "" stays ""; nothing to force
	FURefuse                            // wrong_state
	FUForce                             // a non-empty assurance becomes unverified
)

// NCancelRule is the authority-only N_cancel column.
type NCancelRule uint8

// The N_cancel column's values (DoR:682).
const (
	NCancelNone       NCancelRule = iota
	NCancelDisputed               // rows 2, 3, 11 within N_cancel of last_cancel_at → D
	NCancelWrongState             // row 18 within N_cancel of last_erase_cancel_at → wrong_state
)

// PopulationRule is the authority-only population column.
type PopulationRule uint8

// The population column's values (DoR:680).
const (
	PopAny   PopulationRule = iota
	PopIn                   // rows 2–4
	PopNotIn                // row 1 (`in_population`)
)

// Row is one §2.9 row. N is 1–22; the private ops have N = 0 and only their
// authority-only columns, because they are not rows of the published table.
// Every Row a caller receives is a copy: changing it never changes the table.
type Row struct {
	N       int
	Op      string
	Actor   string
	From    []FromCode
	guard   guard
	sigs    sigReq
	optSigs sigReq
	to      toRule
	DeltaRG uint64
	assure  assureRule
	anb     anbRule

	Floors            []FloorName // any one suffices
	PoPFloors         []FloorName // row 4 with a PoP: replaces Floors
	Held              HeldRule
	ForceUnverified   ForceUnverifiedRule
	NCancel           NCancelRule
	Population        PopulationRule
	SetsLastCancel    bool // rows 13, 14; last_erase_cancel_at too when they cancel a Pe (DoR:690)
	ClearsCancelTimes bool // rows 15–17 (D-145, D-186)
}

// clone copies the row's slices, so a caller cannot write through to the table.
func (r Row) clone() Row {
	r.From, r.Floors, r.PoPFloors = slices.Clone(r.From), slices.Clone(r.Floors), slices.Clone(r.PoPFloors)
	return r
}

var (
	anyButPePrE = []FromCode{FromA0, FromAp, FromApr, FromAc, FromSu, FromSo, FromSor, FromP, FromD}
	anyButPrE   = []FromCode{FromA0, FromAp, FromApr, FromAc, FromSu, FromSo, FromSor, FromP, FromD, FromPe}
	anyButE     = []FromCode{FromAc, FromSu, FromSo, FromSor, FromP, FromD, FromPe, FromPr}
)

// floors lists a row's floors; any one suffices.
func floors(f ...FloorName) []FloorName { return f }

// table is DoR §2.9, rows 1–22: the client-checkable columns (DoR:609–632)
// and the authority-only columns (DoR:678–690), one entry per row, in order.
var table = []Row{
	{N: 1, Op: OpEnrollNew, Actor: ActorUser, From: []FromCode{FromA0}, sigs: sigCandidate | sigEKByCandidate,
		to: toEnrollActive, assure: aInitial, anb: anbZero, Floors: floors(FloorREG), ForceUnverified: FURefuse, Population: PopNotIn},
	{N: 2, Op: OpEnrollExisting, Actor: ActorUser, From: []FromCode{FromAp}, sigs: sigCandidate | sigEKByCandidate | sigPoP,
		to: toPendOrDisp, DeltaRG: 1, assure: aRow2, anb: anbIfPending, Floors: floors(FloorS), ForceUnverified: FURefuse,
		NCancel: NCancelDisputed, Population: PopIn},
	{N: 3, Op: OpEnrollExisting, Actor: ActorUser, From: []FromCode{FromAp, FromApr}, sigs: sigCandidate | sigEKByCandidate,
		to: toPendOrDisp, DeltaRG: 1, assure: aRow3, anb: anbIfPending, Floors: floors(FloorRFull, FloorREmail),
		ForceUnverified: FUForce, NCancel: NCancelDisputed, Population: PopIn},
	{N: 4, Op: OpEnrollExisting, Actor: ActorUser, From: []FromCode{FromP, FromD}, guard: gPriorAbsent,
		sigs: sigCandidate | sigEKByCandidate, optSigs: sigPoP, to: toDispBurn, DeltaRG: 1, assure: aEmpty, anb: anbZero,
		Floors: floors(FloorRFull, FloorREmail), PoPFloors: floors(FloorS), Population: PopIn},
	{N: 5, Op: OpRotateRoot, Actor: ActorUser, From: []FromCode{FromAc}, sigs: sigCandidate | sigEKByCandidate | sigCurrent,
		to: toRotateRoot, assure: aSegment, anb: anbZero, Floors: floors(FloorS), ForceUnverified: FURefuse},
	{N: 6, Op: OpRotateEK, Actor: ActorUser, From: []FromCode{FromAc}, sigs: sigCurrent | sigEKByCurrent,
		to: toRotateEK, assure: aEmpty, anb: anbZero, Floors: floors(FloorS), ForceUnverified: FURefuse},
	{N: 7, Op: OpSuspend, Actor: ActorUser, From: []FromCode{FromAc, FromSo, FromSor}, guard: gUserNotInSet,
		to: toSuspendAdd, assure: aEmpty, anb: anbZero, Floors: floors(FloorH)},
	{N: 8, Op: OpSuspend, Actor: ActorOperator, From: []FromCode{FromAc, FromSu}, sigs: sigOperator,
		to: toSuspendAdd, assure: aEmpty, anb: anbZero, Floors: floors(FloorOP)},
	{N: 9, Op: OpUnsuspend, Actor: ActorUser, From: []FromCode{FromSu, FromSo}, guard: gUserInSetUnrevoked, sigs: sigCurrent,
		to: toUnsuspend, assure: aEmpty, anb: anbZero, Floors: floors(FloorH), ForceUnverified: FURefuse},
	{N: 10, Op: OpUnsuspend, Actor: ActorOperator, From: []FromCode{FromSo}, guard: gOpInSetUnrevoked, sigs: sigOperator,
		to: toUnsuspend, assure: aEmpty, anb: anbZero, Floors: floors(FloorOP)},
	{N: 11, Op: OpReplaceRoot, Actor: ActorUser, From: []FromCode{FromAc, FromSu, FromSor}, sigs: sigCandidate | sigEKByCandidate,
		to: toPendOrDisp, DeltaRG: 1, assure: aRow11, anb: anbIfPending, Floors: floors(FloorRFull, FloorREmail, FloorRRk),
		ForceUnverified: FUForce, NCancel: NCancelDisputed},
	{N: 12, Op: OpReplaceRoot, Actor: ActorUser, From: []FromCode{FromP, FromD}, guard: gPriorActiveOrSusp,
		sigs: sigCandidate | sigEKByCandidate, to: toDispBurn, DeltaRG: 1, assure: aEmpty, anb: anbZero,
		Floors: floors(FloorRFull, FloorREmail, FloorRRk), ForceUnverified: FUForce},
	{N: 13, Op: OpCancelPending, Actor: ActorUser, From: []FromCode{FromP, FromD, FromPe}, guard: gRootUnrevoked, sigs: sigCurrent,
		to: toRestore, DeltaRG: 1, assure: aEmpty, anb: anbZero, Floors: floors(FloorNone), SetsLastCancel: true},
	{N: 14, Op: OpCancelPending, Actor: ActorUser, From: []FromCode{FromP, FromD, FromPe}, guard: gLivePinPriorAbsent,
		sigs: sigPoP, to: toAp, DeltaRG: 1, assure: aEmpty, anb: anbZero, Floors: floors(FloorNone), SetsLastCancel: true},
	{N: 15, Op: OpRevokeRoot, Actor: ActorOperator, From: []FromCode{FromAc, FromSu, FromSo}, sigs: sigOperator,
		to: toSor, DeltaRG: 1, assure: aEmpty, anb: anbZero, Floors: floors(FloorOP), ClearsCancelTimes: true},
	{N: 16, Op: OpRevokeRoot, Actor: ActorOperator, From: []FromCode{FromP, FromD}, guard: gPriorNotAbsent, sigs: sigOperator,
		to: toSor, DeltaRG: 1, assure: aEmpty, anb: anbZero, Floors: floors(FloorOP), Held: HeldExempt, ClearsCancelTimes: true},
	{N: 17, Op: OpRevokeRoot, Actor: ActorOperator, From: []FromCode{FromAp, FromP, FromD}, guard: gAbsentPrior, sigs: sigOperator,
		to: toApr, DeltaRG: 1, assure: aEmpty, anb: anbZero, Floors: floors(FloorOP), Held: HeldExempt, ClearsCancelTimes: true},
	{N: 18, Op: OpErase, Actor: ActorUser, From: anyButPePrE, to: toPendErase, DeltaRG: 1, assure: aEmpty, anb: anbAlways,
		Floors: floors(FloorH, FloorEraseList), Held: HeldCommitsPe, NCancel: NCancelWrongState},
	{N: 19, Op: OpErase, Actor: ActorOperator, From: anyButPrE, sigs: sigOperator, to: toErased, assure: aEmpty, anb: anbZero,
		Floors: floors(FloorOP), Held: HeldExempt},
	{N: 20, Op: OpRecover, Actor: ActorUser, From: anyButE, sigs: sigCandidate | sigEKByCandidate | sigRecovery | sigBindProof,
		to: toPr, DeltaRG: 1, assure: aRow20, anb: anbAlways, Floors: floors(FloorNone), ForceUnverified: FUForce},
	{N: 21, Op: OpCancelPending, Actor: ActorUser, From: []FromCode{FromPr}, sigs: sigPendingRecovery,
		to: toRestore, DeltaRG: 1, assure: aEmpty, anb: anbZero, Floors: floors(FloorNone)},
	{N: 22, Op: OpCancelPending, Actor: ActorRestore, From: []FromCode{FromP, FromD, FromPr}, sigs: sigHeldStatus,
		to: toRestore, DeltaRG: 1, assure: aEmpty, anb: anbZero, Floors: floors(FloorNone), Held: HeldOnly},
}

// privateRows holds the private ops' authority-only columns (DoR:678, :685).
var privateRows = map[string]Row{
	OpAddDevice:          {Op: OpAddDevice, Actor: ActorUser, Floors: floors(FloorS), ForceUnverified: FURefuse},
	OpBindRecovery:       {Op: OpBindRecovery, Actor: ActorUser, Floors: floors(FloorH), ForceUnverified: FURefuse},
	OpCancelRecoveryBind: {Op: OpCancelRecoveryBind, Actor: ActorUser, Floors: floors(FloorNone)},
}

// Rows returns the published table, rows 1–22 in order.
func Rows() []Row {
	out := make([]Row, len(table))
	for i, r := range table {
		out[i] = r.clone()
	}
	return out
}

// RowByN returns row n.
func RowByN(n int) (Row, bool) {
	if n < 1 || n > len(table) {
		return Row{}, false
	}
	return table[n-1].clone(), true
}

// PrivateRow returns a private op's authority-only columns (LD-32, LD-34).
func PrivateRow(op string) (Row, bool) {
	r, ok := privateRows[op]
	return r.clone(), ok
}

// rowsFor is every published row of one (op, actor), in table order.
func rowsFor(op, actor string) []Row {
	var out []Row
	for _, r := range table {
		if r.Op == op && r.Actor == actor {
			out = append(out, r.clone())
		}
	}
	return out
}
