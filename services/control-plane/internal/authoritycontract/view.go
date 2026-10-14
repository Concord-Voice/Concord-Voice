package authoritycontract

import (
	"bytes"
	"slices"
	"strings"
)

// viewOf builds a DeviceView from a device-set (a malformed set yields the
// empty view: the caller has already refused it). admitted holds
// `admitted_by` records; one the codec refuses (checkAdmittedRecord) also
// yields the empty view, which holds no device, no admitter and no recovery
// key, rather than a view that silently skips or misreads the record. The
// slot is unresolved when it shows pending at an as_of at or after
// replacement_nb_ms: own devices hold no upper bound on its completion (DR9
// (c), G-41), so it may complete at any later instant (V3-01). A view with no
// as_of (DeviceHead.View) reports none.
func viewOf(set []byte, dseq, asOf uint64, admitted []string) DeviceView {
	v := DeviceView{Dseq: dseq, AsOfMs: asOf, admitted: map[string]string{}}
	for _, r := range admitted {
		if checkAdmittedRecord(r) != nil {
			return DeviceView{Dseq: dseq, AsOfMs: asOf, admitted: map[string]string{}}
		}
		id, by, _ := strings.Cut(r, ".")
		v.admitted[id] = by
	}
	s, err := parseSet(set)
	if err != nil {
		return v
	}
	v.Active, v.Pending = s.act, s.pend
	if len(s.RecoveryKey) == 97 {
		k := [97]byte(s.RecoveryKey)
		v.Recovery.Bound = &k
	}
	v.Recovery.BindDigest = s.RecoveryBindDigest
	if len(s.ReplacementKey) == 97 {
		k := [97]byte(s.ReplacementKey)
		v.Recovery.Replacement = &k
	}
	v.Recovery.ReplacementBindDigest, v.Recovery.ReplacementNbMs = s.ReplacementBindDigest, s.ReplacementNbMs
	v.Recovery.Unresolved = asOf != 0 && len(s.ReplacementBindDigest) > 0 && asOf >= s.ReplacementNbMs
	return v
}

// View is the persisted DeviceHead's ledger: device-head #7, before Normalize's
// derived activation, with no as_of (AsOfMs 0). h must come from
// ParseDeviceHead or VerifyDeviceLedger; a hand-built head whose set, last
// device result or admitted_by records the codec refuses yields the empty view
// (viewOf).
func (h DeviceHead) View() DeviceView {
	var dseq uint64
	if len(h.LastDeviceResult) > 0 {
		dr, err := DecodeDeviceResult(h.LastDeviceResult)
		if err != nil {
			return DeviceView{admitted: map[string]string{}}
		}
		dseq = dr.Dseq
	}
	return viewOf(h.DeviceSet, dseq, 0, h.AdmittedBy)
}

// HasActiveDSK is the CP query's step 4: digest is SHA-384 of a 97-byte dsk.
func (v DeviceView) HasActiveDSK(digest []byte) bool {
	return slices.ContainsFunc(v.Active, func(e DeviceEntry) bool { return bytes.Equal(Digest(e.DSK[:]), digest) })
}

// AdmittedBy is RT4-3: the admitting actor of id, and with transitive its
// admitter's admitter, until a device with no recorded admitter. A cycle ends
// the walk rather than looping.
func (v DeviceView) AdmittedBy(id string, transitive bool) []string {
	var out []string
	for seen := map[string]bool{id: true}; ; {
		by := v.admitted[id]
		if by == "" || seen[by] {
			return out
		}
		out = append(out, by)
		if !transitive {
			return out
		}
		seen[by], id = true, by
	}
}

// BoundRecovery is RelayRecoverSign's check (D-257): the bound recovery key and
// its bind digest, effective at the view's as_of.
//
// It ignores Recovery.Unresolved, which is correct at as_of: the device-status
// showed the slot pending then, so it had not completed and the bound key was
// the one shown (RT6-9). It is not a promise about any later instant. An
// unresolved slot may complete at any time after as_of with no upper bound (DR9
// (c), G-41), replacing the bound key (a refresh) or unbinding it (an unbind),
// and the authority judges a recover against the key bound at its own db_now
// (DR9 (e)). A caller relaying later than as_of must treat the key of an
// Unresolved view as possibly superseded: verify a fresh device-status first,
// or expect the authority's recovery_key_unbound.
func (v DeviceView) BoundRecovery() (key [97]byte, bindDigest []byte, ok bool) {
	if v.Recovery.Bound == nil {
		return key, nil, false
	}
	return *v.Recovery.Bound, v.Recovery.BindDigest, true
}
