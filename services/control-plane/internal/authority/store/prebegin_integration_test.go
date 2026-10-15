package store

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// runPreBegin decodes body and runs steps 2–5 on a fresh connection.
func runPreBegin(t *testing.T, s *Store, f *authoritydb.Fixture, body []byte) (Outcome, bool) {
	t.Helper()
	rq, out, ok := decodeSubmit(body)
	if !ok {
		return out, false
	}
	conn, err := f.App.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	return s.preBegin(context.Background(), conn, rq)
}

func seal(t *testing.T, f *authoritydb.Fixture) {
	t.Helper()
	mustExec(t, f.Owner, `UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`, d48(4))
}

func TestPreBeginAdmitsAValidEnrollNewOnceSealed(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	a := r.NewAccount(t)
	b := a.EnrollNew(t)
	if out, ok := runPreBegin(t, s, f, b.Bytes); ok || out.Code != string(ac.ErrorNotSealed) {
		t.Fatalf("enroll_new before the seal = %+v, want 503 not_sealed", out)
	}
	seal(t, f)
	if out, ok := runPreBegin(t, s, f, b.Bytes); !ok {
		t.Fatalf("a valid enroll_new after the seal = %+v", out)
	}
}

// Review Focus 2.
func TestSubmitCrossFieldBindingRefused(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	seal(t, f)
	a := r.NewAccount(t)
	cases := map[string]struct {
		mut  func(*authoritytest.Parts)
		want ac.ErrorCode
	}{
		"cp-authz names another account":    {func(p *authoritytest.Parts) { p.CPAuthz.AccountID = authoritytest.NewRealm(t).NewAccount(t).ID }, ac.ErrorBadAuthz},
		"cp-authz names another request":    {func(p *authoritytest.Parts) { p.CPAuthz.RequestID = "44444444-4444-4444-8444-444444444444" }, ac.ErrorBadAuthz},
		"cp-evidence names another request": {func(p *authoritytest.Parts) { p.Evidence.RequestID = "44444444-4444-4444-8444-444444444444" }, ac.ErrorBadAuthz},
		"intent names another realm":        {func(p *authoritytest.Parts) { p.Intent.RealmID = "55555555-5555-4555-8555-555555555555" }, ac.ErrorMalformed},
		"actor restore":                     {func(p *authoritytest.Parts) { p.CPAuthz.Actor = ac.ActorRestore }, ac.ErrorMalformed},
		"CAK signature by another key":      {func(p *authoritytest.Parts) { p.SkipCAKSign = true; p.Submit.CPAuthzSig = make([]byte, 96) }, ac.ErrorBadAuthz},
	}
	for name, c := range cases {
		b := a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true,
			Factors: []string{ac.FactorRegistration}, Mutate: c.mut})
		out, ok := runPreBegin(t, s, f, b.Bytes)
		if ok || out.Kind != KindError || out.Code != string(c.want) {
			t.Fatalf("%s = %+v, want 400 %s and never a recorded refusal", name, out, c.want)
		}
	}
}

func TestPreBeginBadSignatures(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	seal(t, f)
	a := r.NewAccount(t)
	flip := func(b []byte) []byte {
		if len(b) == 0 {
			return b // Build's first Mutate call runs before signing
		}
		c := append([]byte(nil), b...)
		c[10] ^= 1
		return c
	}
	cases := map[string]func(*authoritytest.Parts){
		"sig_candidate":  func(p *authoritytest.Parts) { p.Submit.SigCandidate = flip(p.Submit.SigCandidate) },
		"ek_binding_sig": func(p *authoritytest.Parts) { p.Submit.EKBindingSig = flip(p.Submit.EKBindingSig) },
		"sig_new_device": func(p *authoritytest.Parts) { p.Submit.SigNewDevice = flip(p.Submit.SigNewDevice) },
	}
	for name, mut := range cases {
		b := a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true,
			Factors: []string{ac.FactorRegistration}, Mutate: mut})
		if out, ok := runPreBegin(t, s, f, b.Bytes); ok || out.Code != string(ac.ErrorBadSignature) {
			t.Fatalf("%s corrupted = %+v, want 400 bad_signature", name, out)
		}
	}
	op := a.Build(t, authoritytest.Spec{Op: ac.OpSuspend, Actor: ac.ActorOperator, Factors: []string{ac.FactorAdminWebAuthn}})
	if out, ok := runPreBegin(t, s, f, op.Bytes); ok || out.Code != string(ac.ErrorBadSignature) {
		t.Fatalf("an operator op with no sig_operator = %+v, want 400 bad_signature (LD-4)", out)
	}
}

func TestPopSignatureIsVerifiedBeforeBegin(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	a := r.NewAccount(t)
	lk := authoritytest.NewLegacyKey(t)
	a.Legacy = &lk
	b := a.Build(t, authoritytest.Spec{Op: ac.OpEnrollExisting, NewRoot: true, NewEK: true, PoP: true,
		Factors: []string{ac.FactorPassword}, PasswordLogin: true,
		Mutate: func(p *authoritytest.Parts) {
			if len(p.Submit.LegacyPopSig) > 0 {
				p.Submit.LegacyPopSig[5] ^= 1
			}
		}})
	if out, ok := runPreBegin(t, s, f, b.Bytes); ok || out.Code != string(ac.ErrorBadSignature) {
		t.Fatalf("a corrupted PoP = %+v, want 400 bad_signature", out)
	}
}

// D-184: the replay pre-check runs before the CAK check, so a committed op
// converges even after its CAK is no longer accepted.
func TestReplayPreCheckRunsBeforeTheCAKCheck(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	seal(t, f)
	a := r.NewAccount(t)
	b := a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true,
		Factors: []string{ac.FactorRegistration},
		Mutate:  func(p *authoritytest.Parts) { p.SkipCAKSign = true; p.Submit.CPAuthzSig = make([]byte, 96) }})
	sub, _ := ac.DecodeSubmit(b.Bytes)
	mustExec(t, f.App, `INSERT INTO authority.request_ledger (account_id, request_id, intent_digest, code, cp_evidence)
		VALUES ($1, $2, $3, 'wrong_state', $4)`, a.ID, b.Parts.Intent.RequestID, ac.Digest(sub.Intent), sub.CPEvidence)
	out, ok := runPreBegin(t, s, f, b.Bytes)
	if ok || out.Kind != KindRefused || out.Code != string(ac.RefusalWrongState) {
		t.Fatalf("a stored refusal under a bad CAK = %+v, want the stored refusal, not 400 bad_authz", out)
	}
	changed := a.Build(t, authoritytest.Spec{Op: ac.OpEnrollNew, NewRoot: true, NewEK: true, RequestID: b.Parts.Intent.RequestID,
		Factors: []string{ac.FactorRegistration}})
	if out, ok := runPreBegin(t, s, f, changed.Bytes); ok || out.HTTP != 409 || out.Code != string(ac.ErrorRequestConflict) {
		t.Fatalf("the same request_id with another intent = %+v, want 409 request_conflict", out)
	}
}

// Review round 2: a delegation row the trust ignored (here a higher serial with
// a bad second signature, listing the same RSK) is never the signing delegation.
func TestSigningDelegationSkipsARowTheTrustIgnored(t *testing.T) {
	f := authoritydb.Setup(t)
	r := authoritytest.NewRealm(t)
	s := newTestStore(t, f, r, &fakeProbe{}, Hooks{})
	good := r.Current
	bad := r.NextDelegation(t)
	bad.SigB = append([]byte(nil), bad.SigB...)
	bad.SigB[0] ^= 0xff // fails dual control: AddSigned returns ErrIgnored
	r.Install(t, f.App, bad)
	ctx := context.Background()
	tr, accepted, err := s.trustFor(ctx, f.App)
	if err != nil {
		t.Fatal(err)
	}
	now, err := s.dbNow(ctx, f.App)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.signingDelegation(ctx, f.App, tr.CurrentGen(), now, accepted)
	if err != nil || d.Serial != good.D.Serial {
		t.Fatalf("signing delegation = %+v, %v; want serial %d, never the ignored serial %d", d, err, good.D.Serial, bad.D.Serial)
	}
}
