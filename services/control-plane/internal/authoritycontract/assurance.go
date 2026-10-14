package authoritycontract

import "slices"

// The derivation's invariants (§2.8, D-142, D-143, D-182, LD-35, D-254),
// which the authority and the CP monitor both rely on:
//
//   - The result is "" or one of §2.8's four labels, never anything else.
//   - It is non-empty exactly for the ops that propose a root (enroll_new,
//     enroll_existing, replace_root, recover) and for rotate_root, except on
//     rows 4 and 12, whose only outcome is D and whose column is ""; every
//     other op, published or private, is "". On a root-proposing op that
//     installs its root, "" would read as "leaves assurance unchanged",
//     which inherits the current segment's label: the laundering LD-35
//     forbids. A D outcome installs nothing, so "" there launders nothing.
//   - force_unverified leaves only "" or unverified.
//   - replace_root is never stronger than the current segment (D-182), and
//     neither is enroll_existing without PoP on an account with a root.
//     recover is never stronger than min(recovery, binding segment), and the
//     current segment does not cap it (LD-35, D-254).
//   - A segment the caller does not know ("" where the op reads one, or a
//     value outside the four labels) is unverified (§2.8 monitor rule).
//
// What breaks them: passing an unknown segment straight to capAt, which
// hands the limit back as the result, so "" or an unknown string would leave
// the function; and skipping the cap on replace_root when HasRoot is unset,
// which no reachable from-state allows (row 11 is Ac, Su, Sor).

// AssuranceInput carries what §2.8's DeriveAssurance reads. Segment is the
// current segment's assurance; BindingSegment is the binding segment's, for
// recover (LD-35, D-254). HasRoot is read by enroll_existing only: an
// account with no root has no segment, so the cap does not apply there.
// replace_root and rotate_root always have a root (rows 5 and 11), so they
// always read Segment. Wherever a segment is read, "" or a value outside
// §2.8's labels is read as unverified.
//
// PinLive and RootLive are the from-state's legacy_pin_revoked = 0 and
// root_revoked = 0, true where the account has no pin or no root. Both refuse
// at their zero values, as Evidence.LegacyPinLive does: an input that never
// fills them derives neither continuity nor recovery. Row must be set too;
// Row 0 names no row, so rows 4 and 12 are not read as D-only.
type AssuranceInput struct {
	Op              Op
	ValidPoP        bool
	PinLive         bool
	RootLive        bool
	ForceUnverified bool
	HasRoot         bool
	Ev              Evidence
	Row             int
	Segment         Assurance
	BindingSegment  Assurance
}

// rank orders initial = continuity > recovery > unverified; "" ranks 0, and
// so does any value outside §2.8's vocabulary.
func rank(a Assurance) int {
	switch a {
	case Assurance(AssuranceInitial), Assurance(AssuranceContinuity):
		return 3
	case Assurance(AssuranceRecovery):
		return 2
	case Assurance(AssuranceUnverified):
		return 1
	}
	return 0
}

// NoStronger is the monitor rule (§2.8): a is no stronger than b. A value
// outside §2.8's vocabulary ("" and the four labels) is never no stronger
// than anything: rank gives it 0, so without this check an unknown result
// label would pass against every derivation.
//
// "" must stay no stronger than every label: D outcomes carry "" (they record
// no assurance), so tightening this would make the monitor refuse a D result.
// The monitor relies on the §2.9 client column to refuse "" on a P outcome;
// do not tighten NoStronger to do that job.
func NoStronger(a, b Assurance) bool {
	if !slices.Contains(assuranceValues, string(a)) || !slices.Contains(assuranceValues, string(b)) {
		return false
	}
	return rank(a) <= rank(b)
}

// capAt returns a, or limit when a is stronger than limit.
func capAt(a, limit Assurance) Assurance {
	if rank(a) > rank(limit) {
		return limit
	}
	return a
}

// segmentRule is rotate_root's: continuity if the segment opened as initial
// or continuity, otherwise the segment's own value.
func segmentRule(seg Assurance) Assurance {
	if rank(seg) == 3 {
		return Assurance(AssuranceContinuity)
	}
	return seg
}

// knownSegment reads a segment the caller passed: one of §2.8's four labels
// stands, and anything else ("" included) is a segment the caller does not
// know, which §2.8's monitor rule reads as unverified.
func knownSegment(seg Assurance) Assurance {
	if slices.Contains(segmentAssurances, string(seg)) {
		return seg
	}
	return Assurance(AssuranceUnverified)
}

// DeriveAssurance is §2.8's pure derivation (D-148), shared by the
// authority and the CP monitor. It derives the label of a P (or active)
// outcome. Rows 4 and 12 have only a D outcome, whose Allowed-assurance
// column is "" (DoR:612, :620, :685), so for (enroll_existing, row 4) and
// (replace_root, row 12) it returns "". It cannot see the outcome of rows 2, 3
// and 11, which may be P or D: a caller uses "" for EVERY D outcome, those
// rows' included, and never this function's label there.
func DeriveAssurance(in AssuranceInput) Assurance {
	if dOnlyRow(in) {
		return ""
	}
	a := deriveRaw(in)
	if in.ForceUnverified && a != "" {
		return Assurance(AssuranceUnverified)
	}
	return a
}

// dOnlyRow is a row of in.Op whose only outcome is D(x) with the competitor's
// candidate burned (rows 4 and 12, toDispBurn). A row number that is not a
// row of in.Op falls through to the op's derivation.
func dOnlyRow(in AssuranceInput) bool {
	r, ok := RowByN(in.Row)
	return ok && r.Op == string(in.Op) && r.to == toDispBurn
}

func deriveRaw(in AssuranceInput) Assurance {
	switch in.Op {
	case Op(OpEnrollNew):
		return Assurance(AssuranceInitial)
	case Op(OpEnrollExisting):
		if in.ValidPoP && in.PinLive {
			return Assurance(AssuranceContinuity)
		}
		return recoveryOrUnverified(in)
	case Op(OpReplaceRoot):
		return recoveryOrUnverified(in)
	case Op(OpRecover):
		return capAt(Assurance(AssuranceRecovery), knownSegment(in.BindingSegment))
	case Op(OpRotateRoot):
		return segmentRule(knownSegment(in.Segment))
	}
	return ""
}

// recoveryOrUnverified is the third §2.8 row: enroll_existing without PoP,
// or replace_root. R_rk serves replace_root on rows 11 and 12 only (G-32,
// RB2-4), so a row-3 enroll_existing needs R_full.
func recoveryOrUnverified(in AssuranceInput) Assurance {
	rRk := in.Op == Op(OpReplaceRoot) && in.Ev.rRk(in.Row)
	floorMet := in.Ev.rFull() || rRk
	a := Assurance(AssuranceRecovery)
	if !in.RootLive || !in.PinLive || in.Ev.OperatorAssisted || in.ForceUnverified || !floorMet {
		a = Assurance(AssuranceUnverified)
	}
	if in.HasRoot || in.Op == Op(OpReplaceRoot) {
		a = capAt(a, knownSegment(in.Segment))
	}
	return a
}
