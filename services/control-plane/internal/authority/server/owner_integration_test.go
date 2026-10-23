package server

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/authoritytest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/signer"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authority/store"
	ac "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/authoritycontract"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// The owner verbs against a running serve (B3 Task 4, DoR:1762). They live in
// this package because serveRealm and its helpers do; the verbs themselves are
// store.Owner's, run over the owner pool exactly as the CLI runs them.

// DoR:1762: end-drain takes effect on the next request, with no restart.
// Serial 1 lists the realm's CAK; serial 2 supersedes it with a fresh CAK, so
// the old CAK is admitted only by T9's drain clause. serve starts once and
// is never restarted: it reads drain_grace_ms uncached, in the transaction.
func TestEndDrainTakesEffectWithoutRestart(t *testing.T) {
	slowLoops(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	d1 := rl.Current

	// D-223: a delegation may not reuse another's RSK, so serial 2 carries the
	// RSK and TLS key the node under test runs with.
	rsk2, err := signer.GenerateP384()
	if err != nil {
		t.Fatal(err)
	}
	tls2 := newKey(t)
	oldCAK, freshCAK := rl.CAK, newKey(t)
	d2 := rl.NextDelegation(t, authoritytest.WithRSKs(rsk2.PublicKey()), func(d *ac.Delegation) {
		d.CAKPublicKeys = [][]byte{authoritytest.Point(freshCAK)}
		d.AuthorityTLSKeys = [][]byte{authoritytest.Point(tls2)}
	})
	deps := baseDeps(f, rl, trustOf(t, d1, d2))
	deps.RSK, deps.TLSKey = rsk2, tls2
	buf := &syncBuffer{}
	deps.Log = slog.New(slog.NewJSONHandler(buf, nil))
	r := startRun(t, deps)
	r.ready(t) // the leader installs serial 2 before READY=1
	if got := serials(t, f.App); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("installed %v, want [1 2]", got)
	}
	if _, err := f.Owner.Exec(`UPDATE authority.meta SET legacy_sealed_at = clock_timestamp(), population_digest = $1`,
		make([]byte, 48)); err != nil { // enroll_new needs the seal
		t.Fatal(err)
	}
	s := &served{f: f, rl: rl, r: r, log: buf}
	c := cpClient(t, rl.CPTLS, tls2)
	if m := getSigner(t, c, r.addr); m.Role != "leader" || m.DelegationSerial != 2 {
		t.Fatalf("signer before the verb: %+v, want the leader on serial 2", m)
	}

	// Control: the superseded CAK is admitted inside its drain grace.
	rl.CAK = oldCAK
	a1 := rl.NewAccount(t)
	s.commit(t, c, a1, a1.EnrollNew(t).Bytes)

	o, err := store.OpenOwner(store.OwnerConfig{DB: f.Owner, Anchors: rl.AnchorBytes})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.EndDrain(ctx, 0, 1); err != nil {
		t.Fatalf("EndDrain(0, 1) = %v, want nil", err)
	}

	// The next submit under that CAK is bad_authz; serve was not restarted.
	code, body := call(t, c, r.addr, "POST", "/v1/transactions", rl.NewAccount(t).EnrollNew(t).Bytes)
	if code != http.StatusBadRequest || string(body) != string(envelope(ac.ErrorBadAuthz)) {
		t.Fatalf("a submit under the drained CAK: %d %q, want 400 bad_authz", code, body)
	}

	// The refusal is the drain's alone: serial 2's own CAK is still admitted.
	rl.CAK = freshCAK
	a3 := rl.NewAccount(t)
	s.commit(t, c, a3, a3.EnrollNew(t).Bytes)

	if n := r.notes.count("READY=1"); n != 1 {
		t.Fatalf("READY=1 sent %d times; serve must run through the verb untouched", n)
	}
	select {
	case err := <-r.done:
		t.Fatalf("Run returned during the test: %v", err)
	default:
	}
}

// DoR:1763: seal-legacy takes effect on the next request, with no restart.
// Before the seal enroll_new is refused 503 not_sealed (B1's preBegin); the
// verb seals the population the CP uploaded, and the very next enroll_new is
// committed by the same serve, which reads legacy_sealed_at per request.
func TestSealTakesEffectWithoutRestart(t *testing.T) {
	slowLoops(t)
	s := serveRealm(t, nil)
	c := cpClient(t, s.rl.CPTLS, s.rl.TLSKey)

	// The population: two rows the CP uploaded, written as the app role does.
	recs := []ac.PopulationRecord{
		{AccountID: "00000000-0000-4000-8000-000000000001", LegacyEKDigest: bytes.Repeat([]byte{1}, 48)},
		{AccountID: "00000000-0000-4000-8000-000000000002", LegacyEKDigest: bytes.Repeat([]byte{2}, 48)},
	}
	for _, r := range recs {
		if _, err := s.f.App.Exec(`INSERT INTO authority.legacy_population (account_id, legacy_ek_digest) VALUES ($1, $2)`,
			r.AccountID, r.LegacyEKDigest); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := ac.PopulationDigest(recs)
	if err != nil {
		t.Fatal(err)
	}

	// Control: unsealed, an enroll_new is refused for that reason alone.
	code, body := call(t, c, s.r.addr, "POST", "/v1/transactions", s.rl.NewAccount(t).EnrollNew(t).Bytes)
	if code != http.StatusServiceUnavailable || !bytes.Equal(body, envelope(ac.ErrorNotSealed)) {
		t.Fatalf("enroll_new before the seal: %d %q, want 503 not_sealed", code, body)
	}

	o, err := store.OpenOwner(store.OwnerConfig{DB: s.f.Owner, Anchors: s.rl.AnchorBytes})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.SealLegacy(ctx, digest); err != nil {
		t.Fatalf("SealLegacy = %v, want nil", err)
	}

	// The next enroll_new is admitted; serve was not restarted.
	a := s.rl.NewAccount(t)
	s.commit(t, c, a, a.EnrollNew(t).Bytes)

	if n := s.r.notes.count("READY=1"); n != 1 {
		t.Fatalf("READY=1 sent %d times; serve must run through the verb untouched", n)
	}
	select {
	case err := <-s.r.done:
		t.Fatalf("Run returned during the test: %v", err)
	default:
	}
}

// Task 11: OpenOwner hands the trust file it reads to the store, so revoke-key
// can revoke a CAK that only a not-yet-installed, anchor-signed delegation in
// that file lists. Dropping the file in OpenOwner makes the verb refuse it.
func TestOpenOwnerPassesTheTrustFile(t *testing.T) {
	dumpableOK(t)
	f := authoritydb.Setup(t)
	rl := authoritytest.NewRealm(t)
	f.SeedMeta(t, rl.ID)
	rl.Install(t, f.App, rl.Current)
	d1 := rl.Current // NextDelegation advances rl.Current
	fresh := newKey(t)
	d2 := rl.NextDelegation(t, func(d *ac.Delegation) { d.CAKPublicKeys = [][]byte{authoritytest.Point(fresh)} })

	env := ownerEnv(t)
	if err := os.WriteFile(env["AUTHORITY_ANCHORS_PATH"], rl.AnchorBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env["AUTHORITY_TRUST_PATH"], trustOf(t, d1, d2), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ownerLoad(t, env)
	hook := openOwnerDB
	t.Cleanup(func() { openOwnerDB = hook })
	openOwnerDB = func(Config) (*sql.DB, error) {
		return authoritydb.OpenAs(t, authoritydb.URL(t, authoritydb.EnvURL, authoritydb.DBName), authoritydb.RoleOwner), nil
	}
	o, closeFn, err := OpenOwner(cfg, false, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.RevokeKey(ctx, "cak", ac.KeyDigest(authoritytest.Point(fresh))); err != nil {
		t.Fatalf("RevokeKey(a CAK only the trust file lists) = %v, want nil", err)
	}
	var n int
	if err := f.App.QueryRow(`SELECT count(*) FROM authority.revoked_keys`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("revoked_keys rows = %d (%v), want 1", n, err)
	}
}
