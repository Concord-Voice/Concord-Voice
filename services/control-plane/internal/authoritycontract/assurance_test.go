package authoritycontract

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func full() Evidence {
	return Evidence{Factors: []string{FactorPassword, FactorEmail}, PasswordLogin: true, RecoveryWindowClear: true}
}

func TestDeriveAssurance(t *testing.T) {
	A := func(s string) Assurance { return Assurance(s) }
	cases := []struct {
		name string
		in   AssuranceInput
		want Assurance
	}{
		{"enroll_new", AssuranceInput{Op: Op(OpEnrollNew)}, A(AssuranceInitial)},
		{"enroll_existing PoP", AssuranceInput{Op: Op(OpEnrollExisting), ValidPoP: true, PinLive: true, RootLive: true},
			A(AssuranceContinuity)},
		{"enroll_existing PoP with the pin revoked, or its liveness unset, is not continuity", AssuranceInput{
			Op: Op(OpEnrollExisting), ValidPoP: true, RootLive: true, Ev: full()}, A(AssuranceUnverified)},
		{"enroll_existing no PoP, R_full", AssuranceInput{Op: Op(OpEnrollExisting), PinLive: true, RootLive: true, Ev: full()},
			A(AssuranceRecovery)},
		{"enroll_existing no PoP, only R_email", AssuranceInput{Op: Op(OpEnrollExisting), PinLive: true, RootLive: true,
			Ev: Evidence{Factors: []string{FactorEmail}}}, A(AssuranceUnverified)},
		{"RB2-4: row 3 with {recovery-key, email, totp} is unverified", AssuranceInput{Op: Op(OpEnrollExisting), Row: 3,
			PinLive: true, RootLive: true,
			Ev: Evidence{Factors: []string{FactorEmail, FactorRecoveryKey, FactorTOTP}, MFAEnrolled: true}}, A(AssuranceUnverified)},
		{"replace_root R_rk on row 11 is recovery", AssuranceInput{Op: Op(OpReplaceRoot), Row: 11, HasRoot: true,
			PinLive: true, RootLive: true, Segment: A(AssuranceInitial),
			Ev: Evidence{Factors: []string{FactorPassword, FactorRecoveryKey}, PasswordLogin: true}}, A(AssuranceRecovery)},
		{"replace_root with the root revoked, or its liveness unset", AssuranceInput{Op: Op(OpReplaceRoot), Row: 11,
			HasRoot: true, PinLive: true, Segment: A(AssuranceInitial), Ev: full()}, A(AssuranceUnverified)},
		{"replace_root with the pin revoked, or its liveness unset", AssuranceInput{Op: Op(OpReplaceRoot), Row: 11,
			HasRoot: true, RootLive: true, Segment: A(AssuranceInitial), Ev: full()}, A(AssuranceUnverified)},
		{"replace_root operator_assisted", AssuranceInput{Op: Op(OpReplaceRoot), Row: 11, HasRoot: true, PinLive: true,
			RootLive: true, Segment: A(AssuranceInitial),
			Ev: Evidence{Factors: []string{FactorPassword, FactorEmail}, PasswordLogin: true, RecoveryWindowClear: true, OperatorAssisted: true}},
			A(AssuranceUnverified)},
		{"D-182: replace from an unverified segment stays unverified", AssuranceInput{Op: Op(OpReplaceRoot), Row: 11, HasRoot: true,
			PinLive: true, RootLive: true, Segment: A(AssuranceUnverified), Ev: full()}, A(AssuranceUnverified)},
		{"D-142: rotate from unverified stays unverified", AssuranceInput{Op: Op(OpRotateRoot), HasRoot: true,
			Segment: A(AssuranceUnverified)}, A(AssuranceUnverified)},
		{"rotate from initial is continuity", AssuranceInput{Op: Op(OpRotateRoot), HasRoot: true, Segment: A(AssuranceInitial)},
			A(AssuranceContinuity)},
		{"rotate from recovery stays recovery", AssuranceInput{Op: Op(OpRotateRoot), HasRoot: true, Segment: A(AssuranceRecovery)},
			A(AssuranceRecovery)},
		{"Δ13: key bound in initial gives recovery", AssuranceInput{Op: Op(OpRecover), HasRoot: true,
			Segment: A(AssuranceUnverified), BindingSegment: A(AssuranceInitial)}, A(AssuranceRecovery)},
		{"Δ13: key bound in unverified gives unverified", AssuranceInput{Op: Op(OpRecover), HasRoot: true,
			Segment: A(AssuranceInitial), BindingSegment: A(AssuranceUnverified)}, A(AssuranceUnverified)},
		{"recover under force_unverified", AssuranceInput{Op: Op(OpRecover), ForceUnverified: true,
			BindingSegment: A(AssuranceInitial)}, A(AssuranceUnverified)},
		{"force_unverified forces enroll_new", AssuranceInput{Op: Op(OpEnrollNew), ForceUnverified: true}, A(AssuranceUnverified)},
		{"force_unverified keeps \"\"", AssuranceInput{Op: Op(OpSuspend), ForceUnverified: true}, ""},
		{"private ops are \"\"", AssuranceInput{Op: Op(OpBindRecovery)}, ""},
		{"cancel is \"\"", AssuranceInput{Op: Op(OpCancelPending)}, ""},
	}
	for _, c := range cases {
		require.Equal(t, c.want, DeriveAssurance(c.in), c.name)
	}
}

// TestDeriveAssuranceUnknownSegmentIsUnverified pins §2.8's monitor rule
// inside the derivation: a segment the caller does not know ("" where the op
// reads one, or a value outside §2.8's four labels) is unverified. It is
// never "", which on a root-proposing op would read as "leaves assurance
// unchanged" and survive force_unverified, and never the unknown value
// itself, which capAt would otherwise hand straight back.
func TestDeriveAssuranceUnknownSegmentIsUnverified(t *testing.T) {
	A := func(s string) Assurance { return Assurance(s) }
	for _, seg := range []Assurance{"", A("bogus")} {
		cases := []struct {
			name string
			in   AssuranceInput
		}{
			{"recover, unknown binding segment", AssuranceInput{Op: Op(OpRecover), HasRoot: true,
				Segment: A(AssuranceInitial), BindingSegment: seg}},
			{"recover, unknown binding segment, force_unverified", AssuranceInput{Op: Op(OpRecover),
				ForceUnverified: true, BindingSegment: seg}},
			{"replace_root, unknown current segment", AssuranceInput{Op: Op(OpReplaceRoot), Row: 11, HasRoot: true,
				PinLive: true, RootLive: true, Segment: seg, Ev: full()}},
			{"replace_root is capped even without HasRoot", AssuranceInput{Op: Op(OpReplaceRoot), Row: 11,
				PinLive: true, RootLive: true, Segment: seg, Ev: full()}},
			{"enroll_existing on an account with a root, unknown segment", AssuranceInput{Op: Op(OpEnrollExisting),
				HasRoot: true, PinLive: true, RootLive: true, Segment: seg, Ev: full()}},
			{"rotate_root, unknown segment", AssuranceInput{Op: Op(OpRotateRoot), HasRoot: true, Segment: seg}},
		}
		for _, c := range cases {
			require.Equal(t, A(AssuranceUnverified), DeriveAssurance(c.in), "%s (segment %q)", c.name, seg)
		}
	}
	require.Equal(t, A(AssuranceUnverified), DeriveAssurance(AssuranceInput{Op: Op(OpReplaceRoot), Row: 11,
		PinLive: true, RootLive: true, Segment: A(AssuranceUnverified), Ev: full()}), "D-182 holds without HasRoot")
}

// TestDeriveAssuranceInvariants sweeps every op, segment value, binding
// segment value, evidence shape and flag, and checks what the authority and
// the monitor rely on: the result is in §2.8's vocabulary; it is non-empty
// exactly for the root-proposing ops and rotate_root, except on the D-only
// rows 4 and 12 of their own op (""); force_unverified leaves
// only "" or unverified; recover never exceeds min(recovery, binding
// segment); replace_root never exceeds the current segment; rotate_root is
// the segment rule. An unknown segment is read as unverified throughout.
func TestDeriveAssuranceInvariants(t *testing.T) {
	A := func(s string) Assurance { return Assurance(s) }
	known := func(a Assurance) Assurance {
		if slices.Contains(segmentAssurances, string(a)) {
			return a
		}
		return A(AssuranceUnverified)
	}
	ops := append(slices.Clone(publishedOps), OpAddDevice, OpBindRecovery, OpCancelRecoveryBind, "bogus")
	labelled := map[string]bool{OpEnrollNew: true, OpEnrollExisting: true, OpReplaceRoot: true, OpRecover: true,
		OpRotateRoot: true}
	segs := []Assurance{"", A("bogus"), A(AssuranceInitial), A(AssuranceContinuity), A(AssuranceRecovery),
		A(AssuranceUnverified)}
	rk := Evidence{Factors: []string{FactorPassword, FactorRecoveryKey}, PasswordLogin: true}
	evs := []Evidence{{}, full(), rk}
	n := 0
	for _, op := range ops {
		for _, seg := range segs {
			for _, bseg := range segs {
				for _, ev := range evs {
					for mask := range 32 {
						for _, row := range []int{3, 4, 11, 12} {
							in := AssuranceInput{Op: Op(op), Ev: ev, Row: row, Segment: seg, BindingSegment: bseg,
								ValidPoP: mask&1 != 0, PinLive: mask&2 == 0, RootLive: mask&4 == 0,
								ForceUnverified: mask&8 != 0, HasRoot: mask&16 != 0}
							got := DeriveAssurance(in)
							n++
							dOnly := (op == OpEnrollExisting && row == 4) || (op == OpReplaceRoot && row == 12)
							if !slices.Contains(assuranceValues, string(got)) || (labelled[op] && !dOnly) != (got != "") {
								t.Fatalf("%+v: got %q", in, got)
							}
							if in.ForceUnverified && got != "" && got != A(AssuranceUnverified) {
								t.Fatalf("%+v: force_unverified left %q", in, got)
							}
							switch op {
							case OpRecover:
								if !NoStronger(got, capAt(A(AssuranceRecovery), known(bseg))) {
									t.Fatalf("%+v: recover exceeds its binding cap: %q", in, got)
								}
							case OpReplaceRoot:
								if !NoStronger(got, known(seg)) {
									t.Fatalf("%+v: replace_root exceeds the segment cap: %q", in, got)
								}
							case OpRotateRoot:
								if !in.ForceUnverified && got != segmentRule(known(seg)) {
									t.Fatalf("%+v: rotate_root is not the segment rule: %q", in, got)
								}
							}
						}
					}
				}
			}
		}
	}
	require.Equal(t, len(ops)*len(segs)*len(segs)*len(evs)*32*4, n)
}

func TestNoStronger(t *testing.T) {
	A := func(s string) Assurance { return Assurance(s) }
	require.True(t, NoStronger(A(AssuranceContinuity), A(AssuranceInitial)), "initial = continuity")
	require.True(t, NoStronger(A(AssuranceInitial), A(AssuranceContinuity)))
	require.True(t, NoStronger(A(AssuranceUnverified), A(AssuranceRecovery)))
	require.False(t, NoStronger(A(AssuranceRecovery), A(AssuranceUnverified)), "the monitor blocks an upgrade")
	require.True(t, NoStronger("", ""))
	require.False(t, NoStronger(A(AssuranceRecovery), ""))
}

// TestNoStrongerRefusesUnknownLabels: a label outside §2.8's vocabulary
// ranks with "" in rank(), so without the vocabulary check the monitor rule
// would accept "bogus" as no stronger than any derivation.
func TestNoStrongerRefusesUnknownLabels(t *testing.T) {
	A := func(s string) Assurance { return Assurance(s) }
	for _, v := range assuranceValues {
		require.False(t, NoStronger(A("bogus"), A(v)), "result %q against %q", "bogus", v)
		require.False(t, NoStronger(A(v), A("bogus")), "result %q against %q", v, "bogus")
	}
	require.False(t, NoStronger(A("bogus"), A("bogus")))
}
