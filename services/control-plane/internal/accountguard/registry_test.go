package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

// ---------------------------------------------------------------------------
// Fixtures
//
// These tests need no database. covers is pure (ruling 5), and registry.apply must refuse an
// unwired key before it touches the transaction, so apply is called with a nil *sql.Tx here:
// an implementation that reached for the transaction first would panic and fail the test.
// ---------------------------------------------------------------------------

// regOwners are the only owners an unwired production key may name.
var regOwners = []string{"PR4", "PR6", "C5", "C6", "C10c"}

// regSink is an in-test recording sink and resyncer. It has pointer receivers, so a nil
// *regSink is the typed-nil hazard (R6).
type regSink struct{ calls int }

func (s *regSink) InsertTx(context.Context, *sql.Tx, uuid.UUID, map[int]uuid.UUID, []decide.Notice) error {
	s.calls++
	return nil
}

func (s *regSink) ResyncTx(context.Context, *sql.Tx, uuid.UUID) error {
	s.calls++
	return nil
}

// The nilable kinds a typed-nil interface can wrap. Each has value receivers, so a nil value
// of the type still satisfies both interfaces.
type (
	regMapSink    map[string]int
	regFuncSink   func()
	regChanSink   chan int
	regSliceSink  []int
	regStructSink struct{}
)

func (regMapSink) InsertTx(context.Context, *sql.Tx, uuid.UUID, map[int]uuid.UUID, []decide.Notice) error {
	return nil
}
func (regMapSink) ResyncTx(context.Context, *sql.Tx, uuid.UUID) error { return nil }
func (regFuncSink) InsertTx(context.Context, *sql.Tx, uuid.UUID, map[int]uuid.UUID, []decide.Notice) error {
	return nil
}
func (regFuncSink) ResyncTx(context.Context, *sql.Tx, uuid.UUID) error { return nil }
func (regChanSink) InsertTx(context.Context, *sql.Tx, uuid.UUID, map[int]uuid.UUID, []decide.Notice) error {
	return nil
}
func (regChanSink) ResyncTx(context.Context, *sql.Tx, uuid.UUID) error { return nil }
func (regSliceSink) InsertTx(context.Context, *sql.Tx, uuid.UUID, map[int]uuid.UUID, []decide.Notice) error {
	return nil
}
func (regSliceSink) ResyncTx(context.Context, *sql.Tx, uuid.UUID) error { return nil }
func (regStructSink) InsertTx(context.Context, *sql.Tx, uuid.UUID, map[int]uuid.UUID, []decide.Notice) error {
	return nil
}
func (regStructSink) ResyncTx(context.Context, *sql.Tx, uuid.UUID) error { return nil }

var (
	_ NoticeInserter = (*regSink)(nil)
	_ FlagsResyncer  = (*regSink)(nil)
)

type regTextMarshaler interface{ MarshalText() ([]byte, error) }

// regSpell is a subtest-name helper: the text form, or "-" for a zero value that has none.
func regSpell(m regTextMarshaler) string {
	b, err := m.MarshalText()
	if err != nil {
		return "-"
	}
	return string(b)
}

func regKeyName(k effectKey) string {
	return regSpell(k.Kind) + "/" + regSpell(k.Cred) + "/" + regSpell(k.Req)
}

// The enumerations walk decide's own tables, so a kind added there appears here.
func regEffectKinds() []decide.EffectKind {
	var out []decide.EffectKind
	for k := decide.EffectRemove; ; k++ {
		if _, err := k.MarshalText(); err != nil {
			return out
		}
		out = append(out, k)
	}
}

func regCredKinds() []decide.CredKind {
	var out []decide.CredKind
	for k := decide.CredPassword; ; k++ {
		if _, err := k.MarshalText(); err != nil {
			return out
		}
		out = append(out, k)
	}
}

func regReqKinds() []decide.ReqKind {
	var out []decide.ReqKind
	for k := decide.ReqCredentialRemove; ; k++ {
		if _, err := k.MarshalText(); err != nil {
			return out
		}
		out = append(out, k)
	}
}

// regWant is one expected registry row. ownerPinned is true when the rulings name the owner;
// a key the rulings leave out (a password removal, a social-recovery completion) must still
// be unwired, and if it is present its owner must be one of regOwners.
type regWant struct {
	key         effectKey
	wired       bool
	owner       string
	ownerPinned bool
	// ownerIfPresent pins the owner of an unwired key that need not exist (ruling 22).
	ownerIfPresent string
}

// regExpectations is the R6 table (ruling 6). It fails on any effect, credential or request
// kind it does not classify, so a new kind cannot slip in unwired-by-omission unnoticed.
func regExpectations(t *testing.T) []regWant {
	t.Helper()
	wiredCred := map[decide.CredKind]bool{
		decide.CredTOTP: true, decide.CredWebAuthn: true, decide.CredSSO: true, decide.CredRecoveryKey: true,
	}
	credOwner := map[decide.CredKind]string{
		decide.CredBackupCodeSet: "PR6", decide.CredEmailMFA: "PR6",
		decide.CredAddress: "C5", decide.CredBackupAddress: "C5",
		decide.CredRecoveryDevice: "C6",
	}
	wiredReq := map[decide.ReqKind]bool{decide.ReqCredentialRemove: true, decide.ReqBreakGlass: true}
	// Ruling 22: social_recovery and circle_change are #3662 section 6.15, delivered by C10c.
	// Their Open key is pinned; a Complete key for them need not exist, but if it does it
	// names C10c. recovery_key keeps whatever owner the producer gives it.
	openOwner := map[decide.ReqKind]string{
		decide.ReqAddressChange: "C5", decide.ReqSocialRecovery: "C10c", decide.ReqCircleChange: "C10c",
	}
	completeOwner := map[decide.ReqKind]string{decide.ReqAddressChange: "C5"}
	completeIfPresent := map[decide.ReqKind]string{decide.ReqSocialRecovery: "C10c", decide.ReqCircleChange: "C10c"}
	unpinnedReq := []decide.ReqKind{decide.ReqRecoveryKey}

	var out []regWant
	for _, ek := range regEffectKinds() {
		switch ek {
		case decide.EffectRemove, decide.EffectContest:
			for _, ck := range regCredKinds() {
				owner, pinned := credOwner[ck]
				if !wiredCred[ck] && !pinned && ck != decide.CredPassword {
					t.Fatalf("credential kind %s has no registry expectation: classify it", regSpell(ck))
				}
				out = append(out, regWant{key: effectKey{Kind: ek, Cred: ck}, wired: wiredCred[ck], owner: owner, ownerPinned: pinned})
			}
		case decide.EffectComplete:
			for _, rk := range regReqKinds() {
				owner, pinned := completeOwner[rk]
				ifPresent := completeIfPresent[rk]
				if !wiredReq[rk] && !pinned && ifPresent == "" && !slices.Contains(unpinnedReq, rk) {
					t.Fatalf("request kind %s has no registry expectation: classify it", regSpell(rk))
				}
				out = append(out, regWant{key: effectKey{Kind: ek, Req: rk}, wired: wiredReq[rk], owner: owner, ownerPinned: pinned, ownerIfPresent: ifPresent})
			}
		case decide.EffectOpen:
			// Ruling 13: the Open key carries the request kind. PR3 can store only a
			// credential_remove and a break_glass; any other kind must never be opened.
			for _, rk := range regReqKinds() {
				owner, pinned := openOwner[rk]
				if !wiredReq[rk] && !pinned && !slices.Contains(unpinnedReq, rk) {
					t.Fatalf("request kind %s has no registry expectation: classify it", regSpell(rk))
				}
				out = append(out, regWant{key: effectKey{Kind: ek, Req: rk}, wired: wiredReq[rk], owner: owner, ownerPinned: pinned})
			}
		case decide.EffectCancel, decide.EffectDispute, decide.EffectReturnToPending,
			decide.EffectHold, decide.EffectReleaseReservation:
			out = append(out, regWant{key: effectKey{Kind: ek}, wired: true})
		case decide.EffectRestorePrimary:
			out = append(out, regWant{key: effectKey{Kind: ek}, owner: "C5", ownerPinned: true})
		case decide.EffectResyncMFAFlags:
			out = append(out, regWant{key: effectKey{Kind: ek}, owner: "PR6", ownerPinned: true})
		default:
			t.Fatalf("effect kind %s has no registry expectation: classify it", regSpell(ek))
		}
	}
	return out
}

// regOutcomeFor is an outcome of one effect with the given key, plus the requests a Complete
// needs so covers can read its kind.
func regOutcomeFor(k effectKey) (decide.Outcome, []decide.Req) {
	e := decide.Effect{Kind: k.Kind, RequestID: "req-1", Cred: decide.CredRef{Kind: k.Cred, ID: "cred-1", Version: 2}}
	var reqs []decide.Req
	if k.Kind == decide.EffectComplete {
		reqs = []decide.Req{{ID: "req-1", Kind: k.Req, Status: decide.StatusPending}}
	}
	if k.Kind == decide.EffectOpen {
		e.Open.Kind = k.Req
	}
	return decide.Outcome{Effects: []decide.Effect{e}}, reqs
}

// regWire returns a copy of the production registry with the given keys wired. A key without
// an applier gets a no-op one, so only covers is exercised.
func regWire(keys ...effectKey) registry {
	r := productionRegistry()
	r.effects = maps.Clone(r.effects)
	for _, k := range keys {
		a := r.effects[k]
		a.wired = true
		if a.apply == nil {
			a.apply = func(context.Context, *sql.Tx, applyCtx, decide.Effect) error { return nil }
		}
		r.effects[k] = a
	}
	return r
}

var (
	regOpenEffect   = decide.Effect{Kind: decide.EffectOpen, Open: decide.Opening{Kind: decide.ReqCredentialRemove}}
	regRemoveTOTP   = decide.Effect{Kind: decide.EffectRemove, Cred: decide.CredRef{Kind: decide.CredTOTP, ID: "u-1", Version: 3}}
	regRemoveBackup = decide.Effect{Kind: decide.EffectRemove, Cred: decide.CredRef{Kind: decide.CredBackupCodeSet, ID: "set-1", Version: 3}}
	regResyncEffect = decide.Effect{Kind: decide.EffectResyncMFAFlags}
	regResyncKey    = effectKey{Kind: decide.EffectResyncMFAFlags}
	regNotice       = decide.Notice{Kind: decide.NoticeChangeRequested}
)

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRegistry_ProductionWiredSetPinned is the row PR4 and PR6 flip: every
// (EffectKind, CredKind/ReqKind) combination decide's tables yield, wired or not, with the
// owner of every unwired one. It checks the map and, separately, what covers does with the key,
// so a key missing from the map and a key present but unflagged both show.
func TestRegistry_ProductionWiredSetPinned(t *testing.T) {
	reg := productionRegistry()

	// Control: there is no production sink or resyncer in PR3, a plain nil.
	assert.True(t, reg.notices == nil, "production notices must be nil in PR3 (owner PR4)")
	assert.True(t, reg.resync == nil, "production resyncer must be nil in PR3")

	want := regExpectations(t)
	require.NotEmpty(t, want)
	known := make(map[effectKey]bool, len(want))
	for _, w := range want {
		known[w.key] = true
		t.Run(regKeyName(w.key), func(t *testing.T) {
			got, present := reg.effects[w.key]
			switch {
			case w.wired:
				require.True(t, present, "a wired key must be in the map")
				assert.True(t, got.wired)
				assert.NotNil(t, got.apply, "a wired key needs an applier")
			case w.ownerPinned:
				require.True(t, present, "an unwired key the rulings name must be present with its owner")
				assert.False(t, got.wired)
				assert.Equal(t, w.owner, got.owner)
			case present:
				assert.False(t, got.wired)
				if w.ownerIfPresent != "" {
					assert.Equal(t, w.ownerIfPresent, got.owner)
				} else {
					assert.Contains(t, regOwners, got.owner)
				}
			}

			o, reqs := regOutcomeFor(w.key)
			err := regCovers(reg, o, reqs)
			if w.wired {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrEffectUnwired)
			}
		})
	}

	t.Run("map holds no key outside the enumeration", func(t *testing.T) {
		for k, a := range reg.effects {
			assert.True(t, known[k], "unexpected key %s", regKeyName(k))
			if !a.wired {
				assert.Contains(t, regOwners, a.owner, "unwired key %s needs an owner", regKeyName(k))
			}
		}
	})
}

// TestRegistry_PartialOutcomeRefusedWhole: one unwired effect refuses the whole outcome
// before any applier runs, whatever its position. (The zero-rows-written half runs through the
// runner in Task 10.)
func TestRegistry_PartialOutcomeRefusedWhole(t *testing.T) {
	reg := productionRegistry()

	t.Run("control: every effect wired", func(t *testing.T) {
		o := decide.Outcome{Effects: []decide.Effect{regOpenEffect, regRemoveTOTP}}
		assert.NoError(t, regCovers(reg, o, nil))
	})

	for _, tc := range []struct {
		name    string
		effects []decide.Effect
	}{
		{"unwired last", []decide.Effect{regOpenEffect, regRemoveBackup}},
		{"unwired first", []decide.Effect{regRemoveBackup, regOpenEffect}},
		{"unwired between wired", []decide.Effect{regOpenEffect, regRemoveBackup, regRemoveTOTP}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := regCovers(reg, decide.Outcome{Effects: tc.effects}, nil)
			assert.ErrorIs(t, err, ErrEffectUnwired)
		})
	}

	t.Run("a Complete whose request is not in reqs is refused", func(t *testing.T) {
		o := decide.Outcome{Effects: []decide.Effect{{Kind: decide.EffectComplete, RequestID: "req-1"}}}
		assert.Error(t, regCovers(reg, o, nil))
		assert.Error(t, regCovers(reg, o, []decide.Req{{ID: "other", Kind: decide.ReqCredentialRemove}}))
	})

	t.Run("an outcome of no effects and no notices is covered", func(t *testing.T) {
		assert.NoError(t, regCovers(reg, decide.Outcome{}, nil))
	})
}

// TestRegistry_NoticesWithoutSinkRefused: an outcome that emits a notice is refused whole when
// the registry has no sink (the intended dormant state of PR3), and a sink never rescues an
// unwired effect.
func TestRegistry_NoticesWithoutSinkRefused(t *testing.T) {
	withNotice := decide.Outcome{Effects: []decide.Effect{regOpenEffect}, Notices: []decide.Notice{regNotice}}

	t.Run("production registry has no sink", func(t *testing.T) {
		assert.ErrorIs(t, regCovers(productionRegistry(), withNotice, nil), ErrEffectUnwired)
	})
	t.Run("notices alone, no effects, still need a sink", func(t *testing.T) {
		o := decide.Outcome{Notices: []decide.Notice{regNotice}}
		assert.ErrorIs(t, regCovers(productionRegistry(), o, nil), ErrEffectUnwired)
	})
	t.Run("a recording sink covers the same outcome", func(t *testing.T) {
		reg := productionRegistry()
		reg.notices = &regSink{}
		assert.NoError(t, regCovers(reg, withNotice, nil))
	})
	t.Run("an outcome with no notices needs no sink", func(t *testing.T) {
		o := decide.Outcome{Effects: []decide.Effect{regOpenEffect}}
		assert.NoError(t, regCovers(productionRegistry(), o, nil))
	})
	t.Run("a sink does not rescue an unwired effect", func(t *testing.T) {
		reg := productionRegistry()
		reg.notices = &regSink{}
		o := decide.Outcome{Effects: []decide.Effect{regRemoveBackup}, Notices: []decide.Notice{regNotice}}
		assert.ErrorIs(t, regCovers(reg, o, nil), ErrEffectUnwired)
	})
	t.Run("covers is pure: it does not call the sink", func(t *testing.T) {
		sink := &regSink{}
		reg := productionRegistry()
		reg.notices = sink
		require.NoError(t, regCovers(reg, withNotice, nil))
		assert.Zero(t, sink.calls)
	})
}

// TestRegistry_ResyncEffectNeedsResyncer: EffectResyncMFAFlags is unwired in production (PR6),
// and even once wired it needs a resyncer, which a notice sink does not stand in for.
func TestRegistry_ResyncEffectNeedsResyncer(t *testing.T) {
	o := decide.Outcome{Effects: []decide.Effect{regResyncEffect}}

	t.Run("production: unwired", func(t *testing.T) {
		assert.ErrorIs(t, regCovers(productionRegistry(), o, nil), ErrEffectUnwired)
	})
	t.Run("wired but no resyncer is refused", func(t *testing.T) {
		assert.ErrorIs(t, regCovers(regWire(regResyncKey), o, nil), ErrEffectUnwired)
	})
	t.Run("a notice sink is not a resyncer", func(t *testing.T) {
		reg := regWire(regResyncKey)
		reg.notices = &regSink{}
		assert.ErrorIs(t, regCovers(reg, o, nil), ErrEffectUnwired)
	})
	t.Run("wired with a resyncer is covered", func(t *testing.T) {
		reg := regWire(regResyncKey)
		reg.resync = &regSink{}
		assert.NoError(t, regCovers(reg, o, nil))
	})
	t.Run("a typed-nil resyncer is unwired", func(t *testing.T) {
		reg := regWire(regResyncKey)
		var nilSink *regSink
		reg.resync = nilSink
		assert.ErrorIs(t, regCovers(reg, o, nil), ErrEffectUnwired)
	})
	t.Run("an outcome without the effect needs no resyncer", func(t *testing.T) {
		reg := regWire(regResyncKey)
		assert.NoError(t, regCovers(reg, decide.Outcome{Effects: []decide.Effect{regOpenEffect}}, nil))
	})
}

// TestRegistry_TypedNilSinkIsUnwired: a NoticeInserter or FlagsResyncer whose dynamic value is a
// nil pointer, map, func, chan or slice is absent (ruling 2); a non-nil value of any shape,
// including a struct, is present.
func TestRegistry_TypedNilSinkIsUnwired(t *testing.T) {
	withNotice := decide.Outcome{Effects: []decide.Effect{regOpenEffect}, Notices: []decide.Notice{regNotice}}
	withResync := decide.Outcome{Effects: []decide.Effect{regResyncEffect}}

	var (
		nilPtr   *regSink
		nilMap   regMapSink
		nilFunc  regFuncSink
		nilChan  regChanSink
		nilSlice regSliceSink
	)
	nils := map[string]interface {
		NoticeInserter
		FlagsResyncer
	}{
		"nil pointer": nilPtr, "nil map": nilMap, "nil func": nilFunc, "nil chan": nilChan, "nil slice": nilSlice,
	}
	for name, v := range nils {
		t.Run("typed nil "+name+" as notice sink", func(t *testing.T) {
			reg := productionRegistry()
			reg.notices = v
			assert.ErrorIs(t, regCovers(reg, withNotice, nil), ErrEffectUnwired)
		})
		t.Run("typed nil "+name+" as resyncer", func(t *testing.T) {
			reg := regWire(regResyncKey)
			reg.resync = v
			assert.ErrorIs(t, regCovers(reg, withResync, nil), ErrEffectUnwired)
		})
	}

	live := map[string]interface {
		NoticeInserter
		FlagsResyncer
	}{
		"pointer": &regSink{}, "empty non-nil map": regMapSink{}, "empty non-nil slice": regSliceSink{},
		"non-nil func": regFuncSink(func() {}), "non-nil chan": make(regChanSink), "struct": regStructSink{},
	}
	for name, v := range live {
		t.Run("live "+name+" as notice sink", func(t *testing.T) {
			reg := productionRegistry()
			reg.notices = v
			assert.NoError(t, regCovers(reg, withNotice, nil))
		})
		t.Run("live "+name+" as resyncer", func(t *testing.T) {
			reg := regWire(regResyncKey)
			reg.resync = v
			assert.NoError(t, regCovers(reg, withResync, nil))
		})
	}
}

// TestRegistry_IsNilInterface pins the one reflect helper (ruling 2).
func TestRegistry_IsNilInterface(t *testing.T) {
	var (
		nilPtr   *regSink
		nilMap   regMapSink
		nilFunc  regFuncSink
		nilChan  regChanSink
		nilSlice regSliceSink
	)
	for name, v := range map[string]any{
		"untyped nil": nil, "nil pointer": nilPtr, "nil map": nilMap, "nil func": nilFunc,
		"nil chan": nilChan, "nil slice": nilSlice,
	} {
		t.Run("nil: "+name, func(t *testing.T) { assert.True(t, isNilInterface(v)) })
	}
	for name, v := range map[string]any{
		"pointer": &regSink{}, "empty map": regMapSink{}, "empty slice": regSliceSink{}, "struct": regStructSink{},
		"int": 0, "string": "", "func": regFuncSink(func() {}), "chan": make(regChanSink),
	} {
		t.Run("live: "+name, func(t *testing.T) { assert.False(t, isNilInterface(v)) })
	}
}

// TestRegistry_ApplyDispatchesOnKeyAndRefusesUnwired: registry.apply looks the effect's key up
// and runs that applier, but only if it is wired. An unwired key, with an applier present or
// with no entry at all, is refused with ErrEffectUnwired before the transaction is touched
// (defence in depth behind covers). Here the transaction is nil.
func TestRegistry_ApplyDispatchesOnKeyAndRefusesUnwired(t *testing.T) {
	errMarker := errors.New("applier ran")
	var ran []decide.Effect
	recorder := func(_ context.Context, _ *sql.Tx, _ applyCtx, e decide.Effect) error {
		ran = append(ran, e)
		return errMarker
	}

	reg := productionRegistry()
	reg.effects = maps.Clone(reg.effects)
	wiredKey := effectKey{Kind: decide.EffectRemove, Cred: decide.CredTOTP}
	unwiredKey := effectKey{Kind: decide.EffectRemove, Cred: decide.CredBackupCodeSet}
	completeKey := effectKey{Kind: decide.EffectComplete, Req: decide.ReqSocialRecovery}
	reg.effects[wiredKey] = applier{wired: true, apply: recorder}
	reg.effects[unwiredKey] = applier{wired: false, owner: "PR6", apply: recorder}
	reg.effects[completeKey] = applier{wired: true, apply: recorder}

	userID := uuid.New()
	a := applyCtx{userID: userID, now: snapNow}
	ctx := context.Background()

	t.Run("a wired key runs its applier and returns its error", func(t *testing.T) {
		ran = nil
		err := regApply(ctx, reg, nil, a, 0, regRemoveTOTP)
		assert.ErrorIs(t, err, errMarker)
		require.Len(t, ran, 1)
		assert.Equal(t, regRemoveTOTP, ran[0])
	})
	t.Run("an unwired key with an applier is refused and the applier does not run", func(t *testing.T) {
		ran = nil
		err := regApply(ctx, reg, nil, a, 0, regRemoveBackup)
		assert.ErrorIs(t, err, ErrEffectUnwired)
		assert.Empty(t, ran)
	})
	t.Run("a key with no entry is refused", func(t *testing.T) {
		ran = nil
		e := decide.Effect{Kind: decide.EffectRemove, Cred: decide.CredRef{Kind: decide.CredUnknown}}
		assert.ErrorIs(t, regApply(ctx, reg, nil, a, 0, e), ErrEffectUnwired)
		assert.Empty(t, ran)
	})
	t.Run("a Complete is keyed by the kind of its request in applyCtx.reqs", func(t *testing.T) {
		ran = nil
		e := decide.Effect{Kind: decide.EffectComplete, RequestID: "req-1"}
		withReq := a
		withReq.reqs = []decide.Req{{ID: "req-1", Kind: decide.ReqSocialRecovery, Status: decide.StatusPending}}
		assert.ErrorIs(t, regApply(ctx, reg, nil, withReq, 0, e), errMarker)
		assert.Len(t, ran, 1)

		ran = nil
		addressChange := a
		addressChange.reqs = []decide.Req{{ID: "req-1", Kind: decide.ReqAddressChange, Status: decide.StatusPending}}
		assert.ErrorIs(t, regApply(ctx, reg, nil, addressChange, 0, e), ErrEffectUnwired)

		assert.Error(t, regApply(ctx, reg, nil, a, 0, e), "a Complete whose request is unknown is refused")
		assert.Len(t, ran, 0)
	})
}

// TestRegistry_AddressChangeOpenUnwired (ruling 13): a request whose payload PR3 cannot store
// must never be opened, or it wedges. An Open of an address_change, social_recovery or
// circle_change is refused whole by covers, wherever it sits in the outcome, and apply
// refuses it before any write (the transaction here is nil).
func TestRegistry_AddressChangeOpenUnwired(t *testing.T) {
	reg := productionRegistry()
	openOf := func(k decide.ReqKind) decide.Effect {
		return decide.Effect{Kind: decide.EffectOpen, Open: decide.Opening{Kind: k, Status: decide.StatusPending}}
	}

	t.Run("control: credential_remove and break_glass opens are covered", func(t *testing.T) {
		for _, k := range []decide.ReqKind{decide.ReqCredentialRemove, decide.ReqBreakGlass} {
			assert.NoError(t, regCovers(reg, decide.Outcome{Effects: []decide.Effect{openOf(k)}}, nil), regSpell(k))
		}
	})

	for _, k := range []decide.ReqKind{decide.ReqAddressChange, decide.ReqSocialRecovery, decide.ReqCircleChange} {
		t.Run(regSpell(k)+": covers refuses", func(t *testing.T) {
			assert.ErrorIs(t, regCovers(reg, decide.Outcome{Effects: []decide.Effect{openOf(k)}}, nil), ErrEffectUnwired)
		})
		t.Run(regSpell(k)+": covers refuses it beside a wired open", func(t *testing.T) {
			o := decide.Outcome{Effects: []decide.Effect{openOf(decide.ReqCredentialRemove), openOf(k)}}
			assert.ErrorIs(t, regCovers(reg, o, nil), ErrEffectUnwired)
		})
		t.Run(regSpell(k)+": apply refuses before any write", func(t *testing.T) {
			a := applyCtx{userID: uuid.New(), now: snapNow, requestIDs: map[int]uuid.UUID{0: uuid.New()}}
			assert.ErrorIs(t, regApply(context.Background(), reg, nil, a, 0, openOf(k)), ErrEffectUnwired)
		})
	}
}

// regCovers asks the registry whether the outcome (and the requests it names) is covered.
// Ruling 18: covers reads a.outcome and a.reqs.
func regCovers(r registry, o decide.Outcome, reqs []decide.Req) error {
	return r.covers(applyCtx{outcome: o, reqs: reqs})
}

// regApply applies e as effect i. Ruling 18: apply reads the effect from a.outcome.Effects[i],
// so the helper puts e there (padding any earlier slots) and leaves the rest of a alone.
func regApply(ctx context.Context, r registry, tx *sql.Tx, a applyCtx, i int, e decide.Effect) error {
	effects := slices.Clone(a.outcome.Effects)
	for len(effects) <= i {
		effects = append(effects, decide.Effect{})
	}
	effects[i] = e
	a.outcome.Effects = effects
	return r.apply(ctx, tx, a, i)
}

// TestRegistry_TwoHoldsOnOneRequestIsContract (ruling 23): applyOrder hoists a fold ahead of
// everything else, which is sound only because decide never emits a fold and an opening hold
// for one request in one outcome. covers does not rely on that silently: more than one
// EffectHold with the same non-empty RequestID is ErrContract, wherever they sit and
// whichever shape they take. Folds on RequestID "" (a request this outcome opens) are out of
// scope. covers is pure, so the refusal comes before any write.
func TestRegistry_TwoHoldsOnOneRequestIsContract(t *testing.T) {
	reg := productionRegistry()
	fold := func(id string) decide.Effect { return decide.Effect{Kind: decide.EffectHold, RequestID: id, HeldMs: 5} }
	opening := func(id string) decide.Effect {
		return decide.Effect{Kind: decide.EffectHold, RequestID: id, HeldMs: 5, HeldSince: snapNow}
	}
	cancel := decide.Effect{Kind: decide.EffectCancel, RequestID: "r9", Reason: decide.CancelSuperseded}
	covers := func(effects ...decide.Effect) error {
		return regCovers(reg, decide.Outcome{Effects: effects}, nil)
	}

	for _, tc := range []struct {
		name    string
		effects []decide.Effect
	}{
		{"a fold and an opening hold", []decide.Effect{fold("r1"), opening("r1")}},
		{"an opening hold and a fold", []decide.Effect{opening("r1"), fold("r1")}},
		{"two folds", []decide.Effect{fold("r1"), fold("r1")}},
		{"two opening holds", []decide.Effect{opening("r1"), opening("r1")}},
		{"separated by another effect", []decide.Effect{fold("r1"), cancel, opening("r1")}},
		{"beside holds on other requests", []decide.Effect{fold("r2"), fold("r1"), opening("r3"), opening("r1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := covers(tc.effects...)
			assert.ErrorIs(t, err, ErrContract)
			assert.NotErrorIs(t, err, ErrEffectUnwired, "Hold is wired; this is a contract refusal")
		})
	}

	for _, tc := range []struct {
		name    string
		effects []decide.Effect
	}{
		{"one hold", []decide.Effect{fold("r1")}},
		{"a fold and an opening hold on different requests", []decide.Effect{fold("r1"), opening("r2")}},
		{"two folds on different requests", []decide.Effect{fold("r1"), fold("r2")}},
		{"two holds with no RequestID", []decide.Effect{fold(""), fold("")}},
		{"a hold with no RequestID beside one with an id", []decide.Effect{fold(""), fold("r1")}},
	} {
		t.Run("control: "+tc.name, func(t *testing.T) {
			assert.NoError(t, covers(tc.effects...))
		})
	}
}
