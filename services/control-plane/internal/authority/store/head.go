package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/policy"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

// headRow is one authority.account_heads row. Nullable integers are *uint64;
// nullable timestamps are milliseconds with 0 for NULL.
type headRow struct {
	AccountID                                       string
	State, Prior                                    string
	Seq, RG                                         uint64
	Entry                                           []byte
	IssuedAtMs                                      uint64
	SuspOp, SuspUser                                bool
	Root                                            []byte
	RootRevoked                                     bool
	EK                                              []byte
	EKGen                                           uint64
	PendRoot                                        []byte
	PendErase                                       bool
	PendEK                                          []byte
	PendEKGen                                       uint64 // 0 for NULL
	PendEpoch                                       []byte
	ActBase, ActG, ActUnavailBase, ActFloor         *uint64
	Pin                                             []byte
	PinRevoked                                      bool
	LastCancelAtMs, LastEraseCancelAtMs             uint64
	Held, ForceUnverified                           bool
	Segment                                         string
	SegmentRoot                                     []byte
	ErasedAtMs                                      uint64
	Dseq                                            uint64
	DeviceHead, DeviceSet, DeviceSetDigest          []byte
	RecoveryBindAssurance, ReplacementBindAssurance string
	ReplUnavailBase, ReplG, ReplFloor               *uint64
	RecoverGuardUntilMs                             *uint64
	PendingRecover                                  bool
	RealmID                                         string

	burnOnActivate bool // not a column; see activate
}

func u64p(v uint64) *uint64 { return &v }

func (h *headRow) susp() []string {
	s := []string{}
	if h.SuspOp {
		s = append(s, ac.ActorOperator)
	}
	if h.SuspUser {
		s = append(s, ac.ActorUser)
	}
	return s
}

func b01(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// published is the client-visible head (A2's pub). ANB is R2's
// asserted + Δ + g = activation_base_ms + activation_g_ms on a pending head.
// Op distinguishes Pr (recover) from P (a candidate) and Pe (an erase).
func (h *headRow) published() ac.PublishedHead {
	p := ac.PublishedHead{Seq: h.Seq, Entry: h.Entry, State: h.State, Prior: h.Prior, Susp: h.susp(), Root: h.Root,
		RootRevoked: b01(h.RootRevoked), EK: h.EK, PendRoot: h.PendRoot, PendEK: h.PendEK, Pin: h.Pin,
		PinRevoked: b01(h.PinRevoked), RG: h.RG, Issued: h.IssuedAtMs, EKGen: h.EKGen, PendEKGen: h.PendEKGen}
	if h.State == ac.StatePending && h.ActBase != nil && h.ActG != nil {
		p.ANB = *h.ActBase + *h.ActG
	}
	switch {
	case h.PendingRecover:
		p.Op = ac.OpRecover
	case h.PendErase:
		p.Op = ac.OpErase
	case h.State == ac.StatePending:
		p.Op = ac.OpReplaceRoot
	}
	return p
}

func (h *headRow) policyHead(inPopulation bool) policy.Head {
	return policy.Head{Pub: h.published(), Held: h.Held, ForceUnverified: h.ForceUnverified,
		LastCancelAtMs: h.LastCancelAtMs, LastEraseCancelAtMs: h.LastEraseCancelAtMs, PendingEpochDigest: h.PendEpoch,
		Segment: ac.Assurance(h.Segment), RecoveryBindAssurance: ac.Assurance(h.RecoveryBindAssurance),
		RecoveryKeyBound: h.RecoveryBindAssurance != "", InPopulation: inPopulation}
}

const headColumns = `account_id::text, state, seq, recovery_gen, head_entry_digest, COALESCE(head_issued_at_ms, 0),
	COALESCE(prior_state, ''), susp_operator, susp_user, active_root, root_revoked, ek_binding_digest, ek_generation,
	pending_root, pending_erase, pending_ek_binding_digest, COALESCE(pending_ek_generation, 0), pending_epoch_digest,
	activation_base_ms, activation_g_ms, activation_unavail_base_ms, activation_floor_ms, legacy_ek_digest,
	legacy_pin_revoked, last_cancel_at, last_erase_cancel_at, held, force_unverified, COALESCE(segment_assurance, ''),
	segment_root, erased_at, dseq, device_head_digest, device_set, device_set_digest,
	COALESCE(recovery_bind_assurance, ''), COALESCE(replacement_bind_assurance, ''), replacement_unavail_base_ms,
	replacement_g_ms, replacement_floor_ms, recover_guard_until_ms, pending_recover,
	(SELECT realm_id::text FROM authority.meta WHERE id = 1)`

// The head queries are constants, so every call passes a query fixed at compile
// time and every value is a parameter (go:S2077).
const (
	selectHead         = `SELECT ` + headColumns + ` FROM authority.account_heads WHERE account_id = $1`
	selectPendingHeads = `SELECT ` + headColumns + ` FROM authority.account_heads
		WHERE state IN ('pending', 'disputed') AND account_id::text > $1 ORDER BY account_id LIMIT $2`
)

// selectHeadForUpdate is one literal rather than a concatenation of headColumns.
// SonarCloud flagged the concatenated form at its call in lockHead (go:S2077),
// though not the two concatenated constants that only reads.go uses.
// TestSelectHeadForUpdateIsSelectHeadLocked pins it to selectHead + " FOR UPDATE".
const selectHeadForUpdate = `SELECT account_id::text, state, seq, recovery_gen, head_entry_digest, COALESCE(head_issued_at_ms, 0),
	COALESCE(prior_state, ''), susp_operator, susp_user, active_root, root_revoked, ek_binding_digest, ek_generation,
	pending_root, pending_erase, pending_ek_binding_digest, COALESCE(pending_ek_generation, 0), pending_epoch_digest,
	activation_base_ms, activation_g_ms, activation_unavail_base_ms, activation_floor_ms, legacy_ek_digest,
	legacy_pin_revoked, last_cancel_at, last_erase_cancel_at, held, force_unverified, COALESCE(segment_assurance, ''),
	segment_root, erased_at, dseq, device_head_digest, device_set, device_set_digest,
	COALESCE(recovery_bind_assurance, ''), COALESCE(replacement_bind_assurance, ''), replacement_unavail_base_ms,
	replacement_g_ms, replacement_floor_ms, recover_guard_until_ms, pending_recover,
	(SELECT realm_id::text FROM authority.meta WHERE id = 1) FROM authority.account_heads WHERE account_id = $1 FOR UPDATE`

// u64 reads a BIGINT the store wrote from db_now or a counter. Neither is
// ever negative; a negative value (corruption) reads as 0, as NULL does.
func u64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// i64 writes a ms value or a counter. Both stay far below 2^63 (ms values
// derive from db_now); a larger value saturates.
func i64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func msOfNull(t sql.NullTime) uint64 {
	if !t.Valid {
		return 0
	}
	return u64(t.Time.UnixMilli())
}

func ptrOf(n sql.NullInt64) *uint64 {
	if !n.Valid {
		return nil
	}
	v := u64(n.Int64)
	return &v
}

func scanHead(row interface{ Scan(...any) error }) (*headRow, error) {
	var h headRow
	var ab, ag, au, af, ru, rg, rf, guard sql.NullInt64
	var lc, lec, ea sql.NullTime
	err := row.Scan(&h.AccountID, &h.State, &h.Seq, &h.RG, &h.Entry, &h.IssuedAtMs, &h.Prior, &h.SuspOp, &h.SuspUser,
		&h.Root, &h.RootRevoked, &h.EK, &h.EKGen, &h.PendRoot, &h.PendErase, &h.PendEK, &h.PendEKGen, &h.PendEpoch,
		&ab, &ag, &au, &af, &h.Pin, &h.PinRevoked, &lc, &lec, &h.Held, &h.ForceUnverified, &h.Segment, &h.SegmentRoot,
		&ea, &h.Dseq, &h.DeviceHead, &h.DeviceSet, &h.DeviceSetDigest, &h.RecoveryBindAssurance,
		&h.ReplacementBindAssurance, &ru, &rg, &rf, &guard, &h.PendingRecover, &h.RealmID)
	if err != nil {
		return nil, err
	}
	h.ActBase, h.ActG, h.ActUnavailBase, h.ActFloor = ptrOf(ab), ptrOf(ag), ptrOf(au), ptrOf(af)
	h.ReplUnavailBase, h.ReplG, h.ReplFloor, h.RecoverGuardUntilMs = ptrOf(ru), ptrOf(rg), ptrOf(rf), ptrOf(guard)
	h.LastCancelAtMs, h.LastEraseCancelAtMs, h.ErasedAtMs = msOfNull(lc), msOfNull(lec), msOfNull(ea)
	return &h, nil
}

// lockHead is L1 (§3.2.4 step 1): INSERT … ON CONFLICT DO NOTHING, then
// SELECT … FOR UPDATE. A genesis head of a population account takes the pin.
func lockHead(ctx context.Context, tx *sql.Tx, account string) (*headRow, bool, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO authority.account_heads (account_id, state, seq, recovery_gen,
		susp_operator, susp_user, root_revoked, legacy_pin_revoked)
		VALUES ($1, 'absent', 0, 0, false, false, false, false) ON CONFLICT (account_id) DO NOTHING`, account); err != nil {
		return nil, false, err
	}
	h, err := scanHead(tx.QueryRowContext(ctx, selectHeadForUpdate, account))
	if err != nil {
		return nil, false, err
	}
	var pop []byte
	err = tx.QueryRowContext(ctx, `SELECT legacy_ek_digest FROM authority.legacy_population WHERE account_id = $1`, account).Scan(&pop)
	if err != nil && err != sql.ErrNoRows {
		return nil, false, err
	}
	inPop := err == nil
	if inPop && h.Seq == 0 && len(h.Pin) == 0 {
		h.Pin = pop
	}
	return h, inPop, nil
}

func nullU(p *uint64) any {
	if p == nil {
		return nil
	}
	return i64(*p)
}

func nullMs(ms uint64) any {
	if ms == 0 {
		return nil
	}
	return time.UnixMilli(i64(ms)).UTC()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullGen(g uint64, set bool) any {
	if !set {
		return nil
	}
	return i64(g)
}

// saveHead writes every mutable column (step 11). The caller holds L1.
func saveHead(ctx context.Context, tx *sql.Tx, h *headRow) error {
	_, err := tx.ExecContext(ctx, `UPDATE authority.account_heads SET state = $2, seq = $3, recovery_gen = $4,
		head_entry_digest = $5, head_issued_at_ms = $6, prior_state = $7, susp_operator = $8, susp_user = $9,
		active_root = $10, root_revoked = $11, ek_binding_digest = $12, ek_generation = $13, pending_root = $14,
		pending_erase = $15, pending_ek_binding_digest = $16, pending_ek_generation = $17, pending_epoch_digest = $18,
		activation_base_ms = $19, activation_g_ms = $20, activation_unavail_base_ms = $21, activation_floor_ms = $22,
		legacy_ek_digest = $23, legacy_pin_revoked = $24, last_cancel_at = $25, last_erase_cancel_at = $26,
		force_unverified = $27, segment_assurance = $28, segment_root = $29, erased_at = $30, dseq = $31,
		device_head_digest = $32, device_set = $33, device_set_digest = $34, recovery_bind_assurance = $35,
		replacement_bind_assurance = $36, replacement_unavail_base_ms = $37, replacement_g_ms = $38,
		replacement_floor_ms = $39, recover_guard_until_ms = $40, pending_recover = $41
		WHERE account_id = $1`,
		h.AccountID, h.State, i64(h.Seq), i64(h.RG), h.Entry, nullU(nilIfZero(h.IssuedAtMs)), nullStr(h.Prior),
		h.SuspOp, h.SuspUser, h.Root, h.RootRevoked, h.EK, i64(h.EKGen), h.PendRoot, h.PendErase, h.PendEK,
		nullGen(h.PendEKGen, h.State == ac.StatePending && len(h.PendRoot) > 0), h.PendEpoch,
		nullU(h.ActBase), nullU(h.ActG), nullU(h.ActUnavailBase), nullU(h.ActFloor), h.Pin, h.PinRevoked,
		nullMs(h.LastCancelAtMs), nullMs(h.LastEraseCancelAtMs), h.ForceUnverified, nullStr(h.Segment), h.SegmentRoot,
		nullMs(h.ErasedAtMs), i64(h.Dseq), h.DeviceHead, h.DeviceSet, h.DeviceSetDigest,
		nullStr(h.RecoveryBindAssurance), nullStr(h.ReplacementBindAssurance), nullU(h.ReplUnavailBase),
		nullU(h.ReplG), nullU(h.ReplFloor), nullU(h.RecoverGuardUntilMs), h.PendingRecover)
	return err
}

func nilIfZero(v uint64) *uint64 {
	if v == 0 {
		return nil
	}
	return &v
}

var errBadSet = errors.New("authority store: device set does not decode")

// burnOnActivate is set by lockHead from pending_recover: a Pr activation burns
// the bound recovery key (DR8).
func (h *headRow) activate(visible uint64, pendingAssurance string) {
	pub := h.published().Activated()
	h.burnOnActivate = h.PendingRecover
	newRoot := len(h.PendRoot) > 0
	h.State, h.Prior = pub.State, ""
	h.SuspOp, h.SuspUser = has(pub.Susp, ac.ActorOperator), has(pub.Susp, ac.ActorUser)
	h.Root, h.RootRevoked, h.EK, h.EKGen = pub.Root, pub.RootRevoked == 1, pub.EK, pub.EKGen
	h.PendRoot, h.PendEK, h.PendEKGen, h.PendEpoch, h.PendErase, h.PendingRecover = nil, nil, 0, nil, false, false
	h.ActBase, h.ActG, h.ActUnavailBase, h.ActFloor = nil, nil, nil, nil
	if newRoot {
		h.Segment, h.SegmentRoot, h.ForceUnverified = pendingAssurance, h.Root, false // D-188
		if h.burnOnActivate {
			h.RecoveryBindAssurance = ""
		}
	} else {
		h.ErasedAtMs = visible // Pe(x) → E, root kept
	}
}

func (h *headRow) moveSlotToBound(ds ac.DeviceSet) {
	if len(ds.ReplacementKey) > 0 {
		h.RecoveryBindAssurance = h.ReplacementBindAssurance
	} else {
		h.RecoveryBindAssurance = "" // an unbind completes to no key (D-273)
	}
}

func (h *headRow) clearSlotColumns() {
	h.ReplacementBindAssurance, h.ReplUnavailBase, h.ReplG, h.ReplFloor = "", nil, nil, nil
}

func (h *headRow) setDeviceSet(set []byte) error {
	d, err := ac.DeviceSetDigest(set)
	if err != nil {
		return err
	}
	h.DeviceSet, h.DeviceSetDigest = set, d
	return nil
}

func has(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
