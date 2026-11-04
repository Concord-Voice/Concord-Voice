package accountguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

var (
	// ErrEffectUnwired means an outcome holds an effect this build cannot apply: its key is
	// absent from the registry or present but not wired, or it emits notices with no notice
	// sink, or a ResyncMFAFlags with no resyncer. The whole outcome is refused, never the
	// effect alone, so nothing of it is written.
	ErrEffectUnwired = errors.New("accountguard: effect unwired")
	// ErrContract means the caller broke the applier contract: a context field the write
	// needs is missing, an effect names a request the caller did not pass, or the outcome
	// asks for a write its effects did not make (SetsPassword, see registry.finish).
	ErrContract = errors.New("accountguard: applier contract violated")
)

// The registry's invariant (#3680 PR3, ruling 6). An effect reaches the database only when
// its key is in registry.effects AND that entry is wired AND it carries an applier. The zero
// applier is unwired, so a key nobody registered (a new EffectKind, a new CredKind, a request
// kind decide adds) is refused without anyone writing a line: the set is closed by its zero
// value, not by a list someone must remember to extend.
//
// covers checks the whole outcome before the first write and reads no database; apply
// repeats the per-effect check, so a caller that skipped covers still cannot apply an unwired
// effect. Three things would break it: a typed-nil sink read as present (isNilInterface is
// the one place that decides), wiring read from anything but the wired flag, and a key built
// from data the applier does not write against. The last is why a Complete applier guards its
// UPDATE on the request kind it was keyed by: a reqs entry that lies about the kind finds no
// row and is refused as stale. An Open is keyed by the kind it inserts, read from the same
// Opening the applier writes, so its key and its row cannot disagree (ruling 13).

// effectKey selects an applier. Cred is set for EffectRemove and EffectContest; Req for
// EffectComplete (the kind of the request the effect names) and for EffectOpen (the kind it
// opens); both are zero otherwise.
type effectKey struct {
	Kind decide.EffectKind
	Cred decide.CredKind
	Req  decide.ReqKind
}

// applier is one registry entry. It is a concrete value and never an interface compared with
// nil (the typed-nil hazard, R6): wired is the only wiring signal. owner names the PR that
// wires an unwired entry (PR4, PR6, C5 or C6).
type applier struct {
	wired bool
	owner string
	apply func(ctx context.Context, tx *sql.Tx, a applyCtx, e decide.Effect) error
}

// NoticeInserter writes an outcome's notices in the runner's transaction. requestIDs maps an
// effect index to the request id the runner minted for it. PR3 has no production
// implementation (PR4 builds the outbox), so every outcome that emits a notice is refused.
type NoticeInserter interface {
	InsertTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID, requestIDs map[int]uuid.UUID, ns []decide.Notice) error
}

// FlagsResyncer rewrites users.mfa_methods in the runner's transaction. PR3 has no
// production implementation (PR6).
type FlagsResyncer interface {
	ResyncTx(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error
}

// registry is the closed set of effect appliers, plus the two sinks an outcome may need.
type registry struct {
	effects map[effectKey]applier
	notices NoticeInserter
	resync  FlagsResyncer
}

// applyCtx is everything an applier may read besides its effect. It is a plain struct passed
// by value: no interface and no context value carries a write's inputs (ruling 3).
type applyCtx struct {
	userID uuid.UUID
	// now is the database clock: every instant a statement writes.
	now time.Time
	// outcome is the outcome being applied: covers checks its effects and notices, and apply
	// reads effect i from it. Its EnrolClass is the class a written password carries, and its
	// SetsPassword is what finish holds the password writes to.
	outcome decide.Outcome
	// reqs is the slice covers read; a Complete is keyed by the kind of the request it names.
	reqs []decide.Req
	// pendingPasswordHash is the new password of a break_glass Open, and nothing else's.
	pendingPasswordHash string
	// newGroupID is the outcome's one new dispute group, minted by the caller.
	newGroupID uuid.UUID
	// requestIDs maps an EffectOpen's index to the request id the caller minted for it. The
	// Open applier inserts that id and leaves the map as it found it.
	requestIDs map[int]uuid.UUID
	// passwordWrites counts setPasswordTx's writes. The caller allocates it, because the
	// struct is passed by value; finish reads it.
	passwordWrites *int
	// index is the effect's position in outcome.Effects, set by registry.apply.
	index int
}

// The owners of the unwired entries (ruling 6).
const (
	ownerPR6  = "PR6"
	ownerC5   = "C5"
	ownerC6   = "C6"
	ownerC10c = "C10c"
)

// productionRegistry is the R6 wired set. Wired: Open and Complete of a credential_remove
// and of a break_glass; Cancel, Dispute, ReturnToPending, Hold, ReleaseReservation; Remove
// and Contest of a TOTP, a WebAuthn credential, an SSO link and a recovery key. The rest of
// the keys the rulings name are present, unwired, with their owner. Every other key is
// absent, and so unwired. Notices are PR4's and are tracked by the nil sink, not by a key.
// Each call builds a fresh map, so no caller can rewire another's registry.
func productionRegistry() registry {
	effects := map[effectKey]applier{
		{Kind: decide.EffectOpen, Req: decide.ReqCredentialRemove}:     wiredApplier(applyOpen),
		{Kind: decide.EffectOpen, Req: decide.ReqBreakGlass}:           wiredApplier(applyOpen),
		{Kind: decide.EffectCancel}:                                    wiredApplier(applyCancel),
		{Kind: decide.EffectDispute}:                                   wiredApplier(applyDispute),
		{Kind: decide.EffectReturnToPending}:                           wiredApplier(applyReturnToPending),
		{Kind: decide.EffectHold}:                                      wiredApplier(applyHold),
		{Kind: decide.EffectReleaseReservation}:                        wiredApplier(applyReleaseReservation),
		{Kind: decide.EffectComplete, Req: decide.ReqCredentialRemove}: wiredApplier(applyCompleteCredentialRemove),
		{Kind: decide.EffectComplete, Req: decide.ReqBreakGlass}:       wiredApplier(applyCompleteBreakGlass),
		{Kind: decide.EffectComplete, Req: decide.ReqAddressChange}:    {owner: ownerC5},
		{Kind: decide.EffectRestorePrimary}:                            {owner: ownerC5},
		{Kind: decide.EffectResyncMFAFlags}:                            {owner: ownerPR6},
	}
	for kind, f := range factorWrites {
		effects[effectKey{Kind: decide.EffectRemove, Cred: kind}] = wiredApplier(removeApplier(f))
		effects[effectKey{Kind: decide.EffectContest, Cred: kind}] = wiredApplier(contestApplier(f))
	}
	for kind, owner := range map[decide.CredKind]string{
		decide.CredBackupCodeSet:  ownerPR6,
		decide.CredEmailMFA:       ownerPR6,
		decide.CredAddress:        ownerC5,
		decide.CredBackupAddress:  ownerC5,
		decide.CredRecoveryDevice: ownerC6,
	} {
		effects[effectKey{Kind: decide.EffectRemove, Cred: kind}] = applier{owner: owner}
		effects[effectKey{Kind: decide.EffectContest, Cred: kind}] = applier{owner: owner}
	}
	// Ruling 13: PR3 can store the payload of a credential_remove and of a break_glass only.
	// A request it cannot store must never be opened, or it wedges, so an Open of every other
	// request kind decide declares is present and unwired. A kind decide adds later is absent,
	// and so unwired too. social_recovery and circle_change are the Recovery Circle's (#3662
	// §6.15), delivered by C10c (ruling 22).
	for kind, owner := range map[decide.ReqKind]string{
		decide.ReqAddressChange:  ownerC5,
		decide.ReqSocialRecovery: ownerC10c,
		decide.ReqCircleChange:   ownerC10c,
		decide.ReqRecoveryKey:    ownerC6,
	} {
		effects[effectKey{Kind: decide.EffectOpen, Req: kind}] = applier{owner: owner}
	}
	return registry{effects: effects}
}

func wiredApplier(f func(context.Context, *sql.Tx, applyCtx, decide.Effect) error) applier {
	return applier{wired: true, apply: f}
}

// keyOf builds e's key. An Open is keyed by the kind it opens. A Complete is keyed by the
// kind of the request it names, read from reqs; a Complete whose request is not there has no
// key and is a contract error.
func keyOf(e decide.Effect, reqs []decide.Req) (effectKey, error) {
	k := effectKey{Kind: e.Kind}
	switch e.Kind {
	case decide.EffectRemove, decide.EffectContest:
		k.Cred = e.Cred.Kind
	case decide.EffectOpen:
		k.Req = e.Open.Kind
	case decide.EffectComplete:
		i := -1
		for j := range reqs {
			if reqs[j].ID == e.RequestID {
				i = j
				break
			}
		}
		if i < 0 {
			return effectKey{}, fmt.Errorf("%w: complete names a request the caller did not pass", ErrContract)
		}
		k.Req = reqs[i].Kind
	}
	return k, nil
}

// wiredFor returns the applier for k, or ErrEffectUnwired when k is absent, unwired, carries
// no applier, or is a ResyncMFAFlags with no live resyncer. covers and apply both ask it, so
// the two cannot disagree on what is wired.
func (r registry) wiredFor(k effectKey) (applier, error) {
	a := r.effects[k]
	if !a.wired || a.apply == nil {
		return applier{}, ErrEffectUnwired
	}
	if k.Kind == decide.EffectResyncMFAFlags && isNilInterface(r.resync) {
		return applier{}, fmt.Errorf("%w: no flags resyncer", ErrEffectUnwired)
	}
	return a, nil
}

// covers reports whether every effect of a.outcome can be applied, before any is. It reads
// no database and calls neither sink. It refuses the whole outcome, with ErrEffectUnwired, on
// the first unwired key, on notices with no live sink, and on a ResyncMFAFlags with no live
// resyncer. A Complete whose request is not in a.reqs, and two holds on one existing request
// (holdsUnique), are ErrContract. Every outcome that emits a notice is refused in production
// PR3, whose sink is nil: that is the intended dormant state. The hold check is of the whole
// outcome, so only covers makes it; apply sees one effect.
func (r registry) covers(a applyCtx) error {
	o := a.outcome
	for i, e := range o.Effects {
		k, err := keyOf(e, a.reqs)
		if err != nil {
			return fmt.Errorf("covers: effect %d: %w", i, err)
		}
		if _, err := r.wiredFor(k); err != nil {
			return fmt.Errorf("covers: effect %d: %w", i, err)
		}
	}
	if err := holdsUnique(o.Effects); err != nil {
		return fmt.Errorf("covers: %w", err)
	}
	if len(o.Notices) > 0 && isNilInterface(r.notices) {
		return fmt.Errorf("covers: notices: %w: no notice sink", ErrEffectUnwired)
	}
	return nil
}

// apply applies a.outcome.Effects[i] in tx. An index outside the outcome is a contract error.
// It builds the effect's key and refuses an unwired one before touching tx, so it is defence
// in depth behind covers; then it refuses a context with no account or no clock, and runs the
// applier. The caller (Task 10) calls covers first, then apply for each index applyOrder
// returns, then finish.
func (r registry) apply(ctx context.Context, tx *sql.Tx, a applyCtx, i int) error {
	if i < 0 || i >= len(a.outcome.Effects) {
		return fmt.Errorf("apply effect %d: %w: no such effect", i, ErrContract)
	}
	e := a.outcome.Effects[i]
	k, err := keyOf(e, a.reqs)
	if err != nil {
		return fmt.Errorf("apply effect %d: %w", i, err)
	}
	ap, err := r.wiredFor(k)
	if err != nil {
		return fmt.Errorf("apply effect %d: %w", i, err)
	}
	if a.userID == uuid.Nil || a.now.IsZero() {
		return fmt.Errorf("apply effect %d: %w: no account or no clock", i, ErrContract)
	}
	a.index = i
	if err := ap.apply(ctx, tx, a, e); err != nil {
		return fmt.Errorf("apply effect %d: %w", i, err)
	}
	return nil
}

// finish holds the applied effects to the outcome's SetsPassword flag (decide/doc.go
// "Flags"): exactly one password write when it is set, none when it is not. Task 10 calls it
// after the last effect and before commit; an error means the transaction must not commit. A
// caller that allocated no counter could not have counted, so that is refused too.
func (registry) finish(a applyCtx) error {
	writes := a.passwordWrites
	if writes == nil {
		return fmt.Errorf("finish: %w: no password-write counter", ErrContract)
	}
	want := 0
	if a.outcome.SetsPassword {
		want = 1
	}
	if got := *writes; got != want {
		return fmt.Errorf("finish: %w: %d password writes, the outcome sets %d", ErrContract, got, want)
	}
	return nil
}

// applyOrder is the order the runner applies o's effects in, as indices into o.Effects: every
// hold-fold on an existing request first, in their relative order, then every other effect
// in Outcome order (ruling 16, amending ruling 4). Indices, not effects, because
// applyCtx.requestIDs and every error name an effect by its Outcome index.
//
// The invariant. A fold (isExistingFold) writes held_ms and held_since of one open row and
// nothing else; holdSQL requires the row to be pending or disputed, and no other applier
// reads either column. So a fold commutes with every other effect of the outcome, and running
// it first changes no other write. It must run first: decide sorts by Kind (effectBefore),
// which puts the Cancel or Complete that ends a hold ahead of that hold's fold, and in that
// order the fold finds a terminal row and the whole outcome is refused as stale. decide's
// order stays the canonical, vector-pinned one; only the runner reorders.
//
// Not hoisted: a fold with no RequestID (it names no existing row) and an opening hold (a
// non-zero HeldSince). Both keep their Outcome position. What would break the invariant: an
// effect that reads held_ms or held_since, or one outcome holding both a fold and an opening
// hold of the same request, whose relative order the hoist would swap. decide emits neither:
// it folds only a request whose hold is open (foldEffect) and opens a hold only on one whose
// hold is not (holdIfDue). The second is not left to decide: covers refuses any outcome with
// two holds on one existing request (holdsUnique, ruling 23).
func applyOrder(o decide.Outcome) []int {
	order := make([]int, 0, len(o.Effects))
	for i, e := range o.Effects {
		if isExistingFold(e) {
			order = append(order, i)
		}
	}
	for i, e := range o.Effects {
		if !isExistingFold(e) {
			order = append(order, i)
		}
	}
	return order
}

// holdsUnique refuses, with ErrContract, effects holding more than one EffectHold with the
// same non-empty RequestID, whatever their shape and wherever they sit (ruling 23). Two holds
// of one request have no single meaning, and a fold beside an opening hold is the one pair
// applyOrder would reorder. A hold with no RequestID names a request this outcome opens and
// is out of scope. The error names neither the request nor the hold.
func holdsUnique(effects []decide.Effect) error {
	seen := make(map[string]struct{})
	for i, e := range effects {
		if e.Kind != decide.EffectHold || e.RequestID == "" {
			continue
		}
		if _, dup := seen[e.RequestID]; dup {
			return fmt.Errorf("effect %d: %w: a second hold on one request", i, ErrContract)
		}
		seen[e.RequestID] = struct{}{}
	}
	return nil
}

// isExistingFold reports whether e folds the hold of a request that already exists: an
// EffectHold with a zero HeldSince and a non-empty RequestID (decide/doc.go "Holds").
func isExistingFold(e decide.Effect) bool {
	return e.Kind == decide.EffectHold && e.HeldSince.IsZero() && e.RequestID != ""
}

// isNilInterface reports whether v is nil, or an interface whose dynamic value is a nil
// pointer, map, func, chan or slice (ruling 2). It is the one place reflect is used, and the
// only test of whether a sink or a resyncer is present. reflect.ValueOf never reports an
// Interface kind for a value held in an any, so that kind is not listed (ruling 21).
func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}
