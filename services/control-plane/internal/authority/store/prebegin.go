package store

import (
	"bytes"
	"context"
	"database/sql"

	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
)

type cakMatch struct {
	Gen, Serial uint64
	Key         []byte
}

type request struct {
	Sub          ac.Submit
	Intent       ac.Intent
	IntentDigest []byte
	AZ           ac.CPAuthz
	Ev           ac.CPEvidence
	EKB          *ac.EKBinding
	DI           *ac.DeviceIntent
	D            []byte
	Bind         *ac.RecoveryBind
	H            []byte
	PoP          bool
	Class        opClass
	CAK          cakMatch
	LedgerDigest []byte           // D, or H(B) for cancel_recovery_bind; nil → IntentDigest
	Dev          *ac.DeviceSubmit // set on the device path
}

func (rq *request) ledgerKey() []byte {
	if rq.LedgerDigest != nil {
		return rq.LedgerDigest
	}
	return rq.IntentDigest
}

func malformed() (*request, Outcome, bool) { return nil, errorOutcome(400, ac.ErrorMalformed), false }

// decodeSubmit is step 2's decode (EN5 with the re-encode check, in A1) and
// the step-4 position checks that need no database.
func decodeSubmit(body []byte) (*request, Outcome, bool) {
	sub, err := ac.DecodeSubmit(body)
	if err != nil {
		return malformed()
	}
	rq := &request{Sub: sub}
	if rq.Intent, err = ac.DecodeIntent(sub.Intent); err != nil {
		return malformed()
	}
	if rq.AZ, err = ac.DecodeCPAuthz(sub.CPAuthz); err != nil {
		return malformed()
	}
	if rq.Ev, err = ac.DecodeCPEvidence(sub.CPEvidence); err != nil {
		return malformed()
	}
	if rq.AZ.Actor == ac.ActorRestore { // §2.9: only clear-held produces row 22
		return malformed()
	}
	rq.IntentDigest, rq.Class, rq.PoP = ac.Digest(sub.Intent), classOfOp(rq.Intent.Op), len(sub.LegacyPopSig) > 0
	if !decodeParts(rq) {
		return malformed()
	}
	return rq, Outcome{}, true
}

func decodeParts(rq *request) bool {
	sub, in := rq.Sub, rq.Intent
	if len(sub.EKBinding) > 0 {
		b, err := ac.DecodeEKBinding(sub.EKBinding)
		if err != nil {
			return false
		}
		if _, err := ac.ValidateEKSPKI(b.EKPublicKey); err != nil {
			return false
		}
		rq.EKB = &b
	}
	if (len(sub.DeviceIntent) > 0) != (len(in.DeviceIntentDigest) > 0) {
		return false
	}
	if len(sub.DeviceIntent) > 0 {
		di, err := ac.DecodeDeviceIntent(sub.DeviceIntent)
		if err != nil || !bytes.Equal(ac.Digest(sub.DeviceIntent), in.DeviceIntentDigest) ||
			di.RealmID != in.RealmID || di.AccountID != in.AccountID || di.RequestID != in.RequestID || di.Op != in.Op ||
			di.ExpectedSeq != in.ExpectedSeq || !bytes.Equal(di.ExpectedHeadDigest, in.ExpectedHeadDigest) {
			return false
		}
		rq.DI, rq.D = &di, ac.Digest(sub.DeviceIntent)
	}
	if in.Op == ac.OpRecover {
		b, err := ac.DecodeRecoveryBind(sub.RecoveryBind)
		if err != nil || !bytes.Equal(ac.Digest(sub.RecoveryBind), in.RecoveryBindDigest) || b.Kind != ac.BindKindBind {
			return false
		}
		rq.Bind, rq.H = &b, ac.Digest(sub.RecoveryBind)
	}
	return true
}

// preBegin is steps 2 (realm, then B1-D33's reconcile refusal), 2a, 2b, 3, 3a,
// 4 and 5 on the request's connection. proceed = false returns o: an error
// (unrecorded), or a stored outcome that the caller sends through the release gate.
func (s *Store) preBegin(ctx context.Context, conn *sql.Conn, rq *request) (o Outcome, proceed bool) {
	var realm string
	var sealed, reconciled bool
	if err := conn.QueryRowContext(ctx, `SELECT realm_id::text, legacy_sealed_at IS NOT NULL, reconciled FROM authority.meta WHERE id = 1`).
		Scan(&realm, &sealed, &reconciled); err != nil {
		return s.storeError(), false
	}
	if rq.Intent.RealmID != realm { // step 2
		return errorOutcome(400, ac.ErrorMalformed), false
	}
	if !reconciled { // B1-D33: before the replay lookup, so a replay is refused too
		return errorOutcome(503, ac.ErrorReconcile), false
	}
	// D-232: the hard floor refuses every request, a replay too, so it runs
	// before 2a. diskCheck repeats it on a miss; the bucket and the soft floor
	// stay miss-only (step 2a).
	if out, ok := s.hardFloorCheck(); !ok {
		return out, false
	}
	stored, hit, err := lookupStored(ctx, conn, rq.Intent.AccountID, rq.Intent.RequestID, rq.ledgerKey(), rq.Dev != nil) // 2a
	if err != nil {
		return s.storeError(), false
	}
	if hit {
		return stored, false
	}
	if !s.adm.takeCommitToken(rq.Class) { // 2b
		return errorOutcome(503, ac.ErrorBusy), false
	}
	if out, ok := s.diskCheck(rq.Class); !ok {
		return out, false
	}
	if out, ok := s.checkCAK(ctx, conn, rq); !ok { // 3, 3a
		return out, false
	}
	if (rq.Dev == nil && !verifyStateless(rq)) || (rq.Dev != nil && !verifyNewDevice(rq)) { // 4
		return errorOutcome(400, ac.ErrorBadSignature), false
	}
	if rq.Intent.Op == ac.OpEnrollNew && !sealed { // 5 (read uncached, D-171)
		return errorOutcome(503, ac.ErrorNotSealed), false
	}
	return Outcome{}, true
}

func (s *Store) storeError() Outcome {
	s.nonClient503.Store(true)
	return errorOutcome(503, ac.ErrorStore)
}

// checkCAK is step 3 (T8 part of T9, the evidence binding, the actor, the
// cross-field binding: B1-D16) and step 3a (T12 without revoked_keys).
func (s *Store) checkCAK(ctx context.Context, conn *sql.Conn, rq *request) (Outcome, bool) {
	az, in := rq.AZ, rq.Intent
	if az.RealmID != in.RealmID || az.AccountID != in.AccountID || az.RequestID != in.RequestID ||
		!bytes.Equal(az.IntentDigest, rq.IntentDigest) || !bytes.Equal(az.EvidenceDigest, ac.Digest(rq.Sub.CPEvidence)) ||
		rq.Ev.RequestID != az.RequestID || (az.Actor != ac.ActorUser && az.Actor != ac.ActorOperator) {
		return errorOutcome(400, ac.ErrorBadAuthz), false
	}
	tr, accepted, err := s.trustFor(ctx, conn)
	if err != nil {
		return s.signerOrStore(err), false
	}
	now, err := s.dbNow(ctx, conn)
	if err != nil {
		return s.storeError(), false
	}
	cur, err := s.signingDelegation(ctx, conn, tr.CurrentGen(), now, accepted)
	if err != nil {
		return s.signerOrStore(err), false
	}
	gen, serial, ok := tr.CAKAuthority(rq.Sub.CPAuthz, rq.Sub.CPAuthzSig, cur.Gen, cur.Serial, az.AssertedAtMs)
	if !ok {
		return errorOutcome(400, ac.ErrorBadAuthz), false
	}
	signedUnder, err := loadDelegation(ctx, conn, gen, serial)
	if err != nil {
		return s.storeError(), false
	}
	for _, k := range signedUnder.D.CAKPublicKeys {
		if ac.VerifyP384(k, rq.Sub.CPAuthz, rq.Sub.CPAuthzSig) {
			rq.CAK = cakMatch{Gen: gen, Serial: serial, Key: k}
		}
	}
	if rq.CAK.Key == nil {
		return errorOutcome(400, ac.ErrorBadAuthz), false
	}
	ref := ac.DelegationRef{Gen: cur.Gen, Serial: cur.Serial}
	if az.Actor == ac.ActorOperator && !tr.OperatorAuthority(rq.IntentDigest, rq.Sub.SigOperator, ref,
		az.AssertedAtMs, ref, func([]byte) bool { return false }) {
		return errorOutcome(400, ac.ErrorBadSignature), false
	}
	return Outcome{}, true
}

func (s *Store) signerError() Outcome {
	s.nonClient503.Store(true)
	return errorOutcome(503, ac.ErrorSigner)
}

// verifyStateless is step 4 for every signature that does not depend on the
// head (B1-D17): a present signature that fails is 400 bad_signature; a
// missing required one is left to row selection (recorded wrong_state).
func verifyStateless(rq *request) bool {
	sub, in := rq.Sub, rq.Intent
	if len(sub.SigCandidate) > 0 && (len(in.CandidateRoot) == 0 ||
		!ac.VerifyP384(in.CandidateRoot, ac.Sig{Role: ac.RoleCandidate, Digest: rq.IntentDigest}.Encode(), sub.SigCandidate)) {
		return false
	}
	if len(sub.EKBindingSig) > 0 && len(in.CandidateRoot) > 0 && !ac.VerifyP384(in.CandidateRoot, sub.EKBinding, sub.EKBindingSig) {
		return false
	}
	if rq.PoP {
		pop := ac.LegacyEKPoP{RealmID: in.RealmID, AccountID: in.AccountID, IntentDigest: rq.IntentDigest,
			LegacyEKDigest: in.LegacyEKDigest}.Encode()
		if !ac.VerifyLegacyPSS(sub.LegacySPKI, pop, sub.LegacyPopSig) {
			return false
		}
	}
	if rq.DI != nil && len(rq.DI.NewDeviceDSK) > 0 &&
		!ac.VerifyP384(rq.DI.NewDeviceDSK, ac.Sig{Role: ac.RoleNewDevice, Digest: rq.D}.Encode(), sub.SigNewDevice) {
		return false
	}
	if rq.Bind != nil && !ac.VerifyP384(rq.Bind.RecoveryKey, ac.Sig{Role: ac.RoleRecoveryKey, Digest: rq.IntentDigest}.Encode(), sub.SigRecovery) {
		return false
	}
	return true
}
