package authoritycontract

import "slices"

// BindTier is §2.22's tier table (LD-41, LD-43, LD-47, LD-48, LD-50; D-264,
// D-275, D-280): the shortest bind_wait the evidence earns, or "" for none.
// Pure, Go only; floor H is applied separately (Floor).
func BindTier(factors []string, mfaEnrolled bool) BindWait {
	in := func(n string) bool { return slices.Contains(factors, n) }
	login := in(FactorPassword) || in(FactorSSO)
	realMFA := in(FactorTOTP) || in(FactorWebAuthn) || in(FactorPasskey)
	count := 0
	for _, c := range []bool{login, in(FactorEmail), in(FactorEmailBackup), realMFA} {
		if c {
			count++
		}
	}
	if count >= 3 && (!mfaEnrolled || realMFA) {
		return BindWait(BindWaitNow)
	}
	withBackup := count
	if !realMFA && in(FactorBackupCode) {
		withBackup++
	}
	switch {
	case withBackup >= 2:
		return BindWait(BindWaitDelta)
	case withBackup == 1:
		return BindWait(BindWaitReplace)
	}
	return ""
}

func waitRank(w BindWait) int {
	switch w {
	case BindWait(BindWaitNow):
		return 1
	case BindWait(BindWaitDelta):
		return 2
	case BindWait(BindWaitReplace):
		return 3
	}
	return 0
}

// BindWaitAdmitted is the authority-only DR9 (a) rule: the asked wait is no
// shorter than the tier (now < delta < replace); no tier admits nothing.
func BindWaitAdmitted(asked, tier BindWait) bool {
	a, t := waitRank(asked), waitRank(tier)
	return a > 0 && t > 0 && a >= t
}
