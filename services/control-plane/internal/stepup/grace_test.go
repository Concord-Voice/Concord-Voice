package stepup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/permgen"
)

// The grace store runs on miniredis for the same reason the soft-lock does
// (softlock_test.go): its frozen clock lets a test read a TTL exactly and move
// past GraceTTL without sleeping.

// testBit is a single permission bit (bit 3); rbac's own constants are not
// importable here, and the store only cares that exactly one bit is set.
const testBit int64 = 1 << 3

var (
	enrolled   = Subject{MFAEnabled: true, MFAMethods: []string{"totp"}}
	unenrolled = Subject{}
)

type graceFixture struct {
	rdb    *redis.Client
	mr     *miniredis.Miniredis
	store  GraceStore
	actor  GraceActor
	server uuid.UUID
	scope  GraceScope
}

// newGraceFixture seeds both permission generations, as the permission cache
// would have, and returns a session on a server with a D1 scope.
func newGraceFixture(t *testing.T) graceFixture {
	t.Helper()
	rdb, mr := newSoftLockRedis(t)
	f := graceFixture{
		rdb: rdb, mr: mr, store: NewGraceStore(rdb),
		actor:  GraceActor{UserID: uuid.New(), SessionID: uuid.NewString(), Epoch: "epoch-1"},
		server: uuid.New(),
	}
	f.scope = DangerousActionGraceScope(f.server, testBit)
	require.NoError(t, mr.Set(permgen.UserKey(f.actor.UserID.String()), "ugen-1"))
	require.NoError(t, mr.Set(permgen.ServerKey(f.server.String()), "sgen-1"))
	return f
}

// grant runs the verified path: read before the transaction, grant after it.
func (f graceFixture) grant(t *testing.T, strength GraceStrength) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.store.Grant(ctx, f.store.Read(ctx, f.actor, f.scope), strength))
}

func (f graceFixture) read() GraceRead {
	return f.store.Read(context.Background(), f.actor, f.scope)
}

func (f graceFixture) key() string {
	return "stepup:grace:" + f.actor.UserID.String() + ":" + f.actor.SessionID + ":" + f.server.String() + ":3"
}

// TestGraceKeys_Spelling pins both key shapes (A-10). A key that moved would
// orphan every live grace on deploy, which only prompts, but a key that
// collided across scopes would let one cover the other.
func TestGraceKeys_Spelling(t *testing.T) {
	uid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	sid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	srv := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	require.Equal(t, "stepup:grace:"+uid.String()+":"+sid.String()+":"+srv.String()+":3",
		graceKey(uid, sid, DangerousActionGraceScope(srv, testBit)))
	require.Equal(t, "stepup:grace:"+uid.String()+":"+sid.String()+":"+srv.String()+":62",
		graceKey(uid, sid, DangerousActionGraceScope(srv, 1<<62)))
	require.Equal(t, "stepup:grace:"+uid.String()+":"+sid.String()+":dm:delete",
		graceKey(uid, sid, DeleteGraceScope(DMDeleteScope())))
	require.Equal(t, "stepup:grace:"+uid.String()+":"+sid.String()+":server:"+srv.String()+":delete",
		graceKey(uid, sid, DeleteGraceScope(ServerDeleteScope(srv))))

	// The soft-lock counter and its grace spell the scope the same way.
	l := NewDeleteSoftLock(nil)
	require.Equal(t, "stepup:delete_softlock:"+uid.String()+":server:"+srv.String(), l.Key(uid, ServerDeleteScope(srv)))
	require.Equal(t, "stepup:delete_softlock:"+uid.String()+":dm", l.Key(uid, DMDeleteScope()))
}

// TestDangerousActionGraceScope_RefusesAnythingButOneBit: a mask of two bits,
// zero, or the sign bit has no key, so it reads as no grace and grants
// nothing rather than covering a combination no route asked for.
func TestDangerousActionGraceScope_RefusesAnythingButOneBit(t *testing.T) {
	f := newGraceFixture(t)
	for _, bit := range []int64{0, 3, -1, -1 << 63, testBit | 1<<4} {
		f.scope = DangerousActionGraceScope(f.server, bit)
		require.Equal(t, GraceScope{}, f.scope, "bit %d", bit)
		f.grant(t, GraceStrengthMFA)
		require.Len(t, f.mr.Keys(), 2, "bit %d must grant nothing beyond the two generations", bit)
	}
}

// TestGraceStore_GrantThenCover is the happy path: nothing covers before a
// grant, the grant is stored for exactly GraceTTL, and a read within it
// covers an eligible purpose under the server rule.
func TestGraceStore_GrantThenCover(t *testing.T) {
	f := newGraceFixture(t)
	require.False(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "no grace before a grant")

	f.grant(t, GraceStrengthMFA)
	require.Equal(t, GraceTTL, f.mr.TTL(f.key()))
	require.True(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled))

	f.mr.FastForward(GraceTTL - time.Second)
	require.True(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "still inside the window")
	f.mr.FastForward(2 * time.Second)
	require.False(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "the window has ended")
}

// TestGraceRead_EachStampMismatchVoids: every stamp must equal its current
// value. Mutation: drop any one comparison from Covers and its row fails.
func TestGraceRead_EachStampMismatchVoids(t *testing.T) {
	cases := map[string]func(t *testing.T, f *graceFixture){
		"credential epoch rotated": func(_ *testing.T, f *graceFixture) { f.actor.Epoch = "epoch-2" },
		"user generation bumped": func(t *testing.T, f *graceFixture) {
			require.NoError(t, f.mr.Set(permgen.UserKey(f.actor.UserID.String()), "ugen-2"))
		},
		"server generation bumped": func(t *testing.T, f *graceFixture) {
			require.NoError(t, f.mr.Set(permgen.ServerKey(f.server.String()), "sgen-2"))
		},
		"user generation lost": func(_ *testing.T, f *graceFixture) {
			f.mr.Del(permgen.UserKey(f.actor.UserID.String()))
		},
		"server generation lost": func(_ *testing.T, f *graceFixture) {
			f.mr.Del(permgen.ServerKey(f.server.String()))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newGraceFixture(t)
			f.grant(t, GraceStrengthMFA)
			require.True(t, f.read().Covers(PurposeMemberBan, GraceServerRule, enrolled), "precondition: the grace covers")
			mutate(t, &f)
			require.False(t, f.read().Covers(PurposeMemberBan, GraceServerRule, enrolled))
		})
	}
}

// TestGraceRead_StrengthAgainstRule: the server rule takes only an mfa grace
// from an actor who still has an inline factor; the own rule takes mfa, and
// password only from an actor with no inline factor (A-9).
func TestGraceRead_StrengthAgainstRule(t *testing.T) {
	cases := []struct {
		name     string
		strength GraceStrength
		rule     GraceRule
		subj     Subject
		want     bool
	}{
		{"server rule, mfa", GraceStrengthMFA, GraceServerRule, enrolled, true},
		{"server rule, password, enrolled", GraceStrengthPassword, GraceServerRule, enrolled, false},
		{"server rule, password, unenrolled", GraceStrengthPassword, GraceServerRule, unenrolled, false},
		{"server rule, mfa, factors since removed", GraceStrengthMFA, GraceServerRule, unenrolled, false},
		{"own rule, mfa", GraceStrengthMFA, GraceOwnRule, enrolled, true},
		{"own rule, mfa, factors since removed", GraceStrengthMFA, GraceOwnRule, unenrolled, true},
		{"own rule, password, unenrolled", GraceStrengthPassword, GraceOwnRule, unenrolled, true},
		{"own rule, password, since enrolled", GraceStrengthPassword, GraceOwnRule, enrolled, false},
		{"no rule", GraceStrengthMFA, GraceRule(0), enrolled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGraceFixture(t)
			f.scope = DeleteGraceScope(ServerDeleteScope(f.server))
			f.grant(t, tc.strength)
			require.Equal(t, tc.want, f.read().Covers(PurposeMessageDelete, tc.rule, tc.subj))
		})
	}
}

// TestGraceRead_AlwaysFreshPurposesIgnoreAValidGrace: a valid grace on the
// same server and bit covers nothing for a purpose outside the eligible set.
func TestGraceRead_AlwaysFreshPurposesIgnoreAValidGrace(t *testing.T) {
	f := newGraceFixture(t)
	f.grant(t, GraceStrengthMFA)
	r := f.read()
	require.True(t, r.Covers(PurposeRoleDelete, GraceServerRule, enrolled), "precondition: the grace is valid")
	for _, p := range Purposes() {
		if !p.GraceEligible() {
			require.False(t, r.Covers(p, GraceServerRule, enrolled), "%q is always fresh", p)
		}
	}
}

// TestGraceRead_OtherSessionServerOrBitGetsNoGrace: a grace belongs to one
// session, one server and one bit.
func TestGraceRead_OtherSessionServerOrBitGetsNoGrace(t *testing.T) {
	f := newGraceFixture(t)
	f.grant(t, GraceStrengthMFA)
	require.True(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "precondition")

	other := f
	other.actor.SessionID = uuid.NewString()
	require.False(t, other.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "another session")

	other = f
	other.server = uuid.New()
	require.NoError(t, f.mr.Set(permgen.ServerKey(other.server.String()), "sgen-1"))
	other.scope = DangerousActionGraceScope(other.server, testBit)
	require.False(t, other.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "another server")

	other = f
	other.scope = DangerousActionGraceScope(f.server, testBit<<1)
	require.False(t, other.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "another bit")

	other = f
	other.actor.UserID = uuid.New()
	require.False(t, other.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "another user")

	// A DM delete grace and a server delete grace are distinct too.
	f.scope = DeleteGraceScope(DMDeleteScope())
	require.False(t, f.read().Covers(PurposeDMMessageDelete, GraceOwnRule, enrolled), "the delete scope has no grace")
}

// TestGraceStore_SessionIDIsCanonicalised: the sid is parsed, so one session
// in another spelling is the same grace, and a sid that is not a uuid (or a
// legacy token with none) gets none.
func TestGraceStore_SessionIDIsCanonicalised(t *testing.T) {
	f := newGraceFixture(t)
	f.grant(t, GraceStrengthMFA)

	braced := f
	braced.actor.SessionID = "{" + strings.ToUpper(f.actor.SessionID) + "}"
	require.True(t, braced.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled))

	for _, sid := range []string{"", "not-a-session", f.actor.SessionID + ":x"} {
		bad := f
		bad.actor.SessionID = sid
		require.Equal(t, GraceRead{}, bad.read(), "sid %q", sid)
		require.NoError(t, bad.store.Grant(context.Background(), bad.read(), GraceStrengthMFA))
	}
	require.Len(t, f.mr.Keys(), 3, "no further grace was granted")
}

// TestGraceStore_RedisFailureMeansNoGrace: a dead or absent Redis reads as
// no grace, so the actor is prompted (D-2), and a grant from that read is a
// silent no-op. Read never fails the request.
func TestGraceStore_RedisFailureMeansNoGrace(t *testing.T) {
	ctx := context.Background()
	f := newGraceFixture(t)
	f.grant(t, GraceStrengthMFA)

	dead := deadRedisClient()
	t.Cleanup(func() { _ = dead.Close() })
	for name, store := range map[string]GraceStore{"nil client": NewGraceStore(nil), "unreachable": NewGraceStore(dead)} {
		r := store.Read(ctx, f.actor, f.scope)
		require.Equal(t, GraceRead{}, r, name)
		require.False(t, r.Covers(PurposeChannelDelete, GraceServerRule, enrolled), name)
		require.NoError(t, store.Grant(ctx, r, GraceStrengthMFA), name)
	}

	r := f.read()
	f.mr.Close()
	require.False(t, f.store.Read(ctx, f.actor, f.scope).Covers(PurposeChannelDelete, GraceServerRule, enrolled))
	require.Error(t, f.store.Grant(ctx, r, GraceStrengthMFA), "a failed SET is reported for the caller's Warn")
}

// TestGraceStore_MalformedValueIsNoGrace: anything but the exact stamp shape
// reads as no grace.
func TestGraceStore_MalformedValueIsNoGrace(t *testing.T) {
	stamp := `{"cred_epoch":"epoch-1","user_gen":"ugen-1","server_gen":"sgen-1","strength":"mfa"}`
	f := newGraceFixture(t)
	require.NoError(t, f.mr.Set(f.key(), stamp))
	require.True(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "control: the exact shape covers")

	for _, raw := range []string{
		"garbage",
		stamp + " {}",
		`{"cred_epoch":"epoch-1","user_gen":"ugen-1","server_gen":"sgen-1","strength":"mfa","extra":1}`,
		`{"cred_epoch":"epoch-1","user_gen":"ugen-1","server_gen":"sgen-1","strength":"sms"}`,
		`{"cred_epoch":"epoch-1","user_gen":"","server_gen":"sgen-1","strength":"mfa"}`,
		`["epoch-1","ugen-1","sgen-1","mfa"]`,
	} {
		require.NoError(t, f.mr.Set(f.key(), raw))
		require.False(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled), "%s", raw)
	}
}

// TestGraceStore_GrantSeedsAnAbsentGeneration: a subject whose generation the
// permission cache never seeded (a DM-only session, or one past the 24-hour
// TTL) is seeded by Grant the way the cache seeds it, and the grace it stamps
// covers. Before T1e such a subject could never be granted grace, so a DM
// delete prompted on every trip. Mutation: return early when a generation is
// absent (the pre-T1e Grant).
func TestGraceStore_GrantSeedsAnAbsentGeneration(t *testing.T) {
	for name, key := range map[string]func(f graceFixture) string{
		"user":   func(f graceFixture) string { return permgen.UserKey(f.actor.UserID.String()) },
		"server": func(f graceFixture) string { return permgen.ServerKey(f.server.String()) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newGraceFixture(t)
			f.mr.Del(key(f))
			read := f.read()
			require.False(t, f.mr.Exists(key(f)), "Read writes nothing")
			require.NoError(t, f.store.Grant(context.Background(), read, GraceStrengthMFA))

			require.True(t, f.mr.Exists(key(f)), "Grant seeds the absent generation")
			gen, err := f.mr.Get(key(f))
			require.NoError(t, err)
			require.Len(t, gen, 16, "seeded the way the permission cache seeds: 64 bits as hex")
			require.Equal(t, permgen.TTL, f.mr.TTL(key(f)), "seeded with the cache's TTL")
			require.True(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled))
		})
	}
}

// TestGraceStore_BumpAfterTheReadVoidsTheGrant: the pre-transaction read found
// a generation absent, and an MFA change or an enforcement flip wrote it after
// the confirming transaction committed but before Grant ran. Grant must not
// stamp that value: doing so folds the change into the grace and honours it
// for ten minutes (Codex review of #3454). Its SET NX finds the key and it
// grants nothing. Mutation: re-read and stamp the stored generation when the
// SET NX did not create it.
func TestGraceStore_BumpAfterTheReadVoidsTheGrant(t *testing.T) {
	for name, key := range map[string]func(f graceFixture) string{
		"user":   func(f graceFixture) string { return permgen.UserKey(f.actor.UserID.String()) },
		"server": func(f graceFixture) string { return permgen.ServerKey(f.server.String()) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newGraceFixture(t)
			f.mr.Del(key(f))
			read := f.read()
			require.NoError(t, f.mr.Set(key(f), "bumped-after-the-read"))

			require.NoError(t, f.store.Grant(context.Background(), read, GraceStrengthMFA))
			require.False(t, f.mr.Exists(f.key()), "no grace is granted")
			require.False(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled))
		})
	}
}

// TestGraceStore_SeedNeverOverwritesAGeneration: another writer (the
// permission cache) created the generation between the read and the grant.
// SET NX keeps its value, so the cache's entries stay valid, and the grant
// is refused rather than stamped with a value it did not read. Mutation: SET
// instead of SET NX.
func TestGraceStore_SeedNeverOverwritesAGeneration(t *testing.T) {
	f := newGraceFixture(t)
	userKey := permgen.UserKey(f.actor.UserID.String())
	f.mr.Del(userKey)
	read := f.read()
	require.NoError(t, f.mr.Set(userKey, "ugen-cache"))

	require.NoError(t, f.store.Grant(context.Background(), read, GraceStrengthMFA))

	gen, err := f.mr.Get(userKey)
	require.NoError(t, err)
	require.Equal(t, "ugen-cache", gen)
	require.False(t, f.mr.Exists(f.key()))
}

// TestGraceStore_SecondGrantAdoptsTheFirstsSeed: one settle grants two graces
// (the composed purge's soft-lock and D1 graces) from two reads that both
// found the generations absent. The first grant's SET NX creates them, so the
// second's would find them and refuse; AdoptGenerations hands it the values
// the first created. A bump between the two grants still voids the second.
// Mutation: AdoptGenerations copying nothing (the second grace is refused).
func TestGraceStore_SecondGrantAdoptsTheFirstsSeed(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (graceFixture, GraceRead, GraceRead, GraceScope) {
		f := newGraceFixture(t)
		f.mr.Del(permgen.UserKey(f.actor.UserID.String()))
		f.mr.Del(permgen.ServerKey(f.server.String()))
		second := DangerousActionGraceScope(f.server, 1<<4)
		return f, f.read(), f.store.Read(ctx, f.actor, second), second
	}
	t.Run("both graces cover", func(t *testing.T) {
		f, first, other, second := setup(t)
		stamped, err := f.store.GrantRead(ctx, first, GraceStrengthMFA)
		require.NoError(t, err)
		require.NoError(t, f.store.Grant(ctx, other.AdoptGenerations(stamped), GraceStrengthMFA))
		require.True(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled))
		require.True(t, f.store.Read(ctx, f.actor, second).Covers(PurposeChannelDelete, GraceServerRule, enrolled))
	})
	t.Run("a bump between the grants voids the second", func(t *testing.T) {
		f, first, other, second := setup(t)
		stamped, err := f.store.GrantRead(ctx, first, GraceStrengthMFA)
		require.NoError(t, err)
		require.NoError(t, f.mr.Set(permgen.UserKey(f.actor.UserID.String()), "bumped-between-grants"))
		require.NoError(t, f.store.Grant(ctx, other.AdoptGenerations(stamped), GraceStrengthMFA))
		require.False(t, f.store.Read(ctx, f.actor, second).Covers(PurposeChannelDelete, GraceServerRule, enrolled))
	})
	t.Run("another actor's generation is never adopted", func(t *testing.T) {
		f, first, _, _ := setup(t)
		stamped, err := f.store.GrantRead(ctx, first, GraceStrengthMFA)
		require.NoError(t, err)
		other := GraceActor{UserID: uuid.New(), SessionID: uuid.NewString(), Epoch: "epoch-1"}
		adopted := f.store.Read(ctx, other, f.scope).AdoptGenerations(stamped)
		require.Empty(t, adopted.userGen, "a different user's generation key")
		require.Equal(t, stamped.serverGen, adopted.serverGen, "the same server's generation key")
	})
}

// TestGraceStore_FailedSeedGrantsNothing: a Redis failure while seeding
// returns an error (the caller logs it at Warn) and writes no grace, so the
// next action prompts.
func TestGraceStore_FailedSeedGrantsNothing(t *testing.T) {
	f := newGraceFixture(t)
	f.mr.Del(permgen.UserKey(f.actor.UserID.String()))
	read := f.read()
	f.mr.SetError("seed refused")

	require.Error(t, f.store.Grant(context.Background(), read, GraceStrengthMFA))
	f.mr.SetError("")
	require.False(t, f.mr.Exists(f.key()))
}

// TestGraceRead_AbsentGenerationsNeverMatch: a stamp whose server generation
// is empty does not match a server generation that is absent now, although
// the strings are equal. Grant never writes such a stamp; this pins the
// read-side half for one that arrives some other way (an older writer, a
// hand edit). Mutation: drop gensComplete from Covers.
func TestGraceRead_AbsentGenerationsNeverMatch(t *testing.T) {
	f := newGraceFixture(t)
	f.mr.Del(permgen.ServerKey(f.server.String()))
	require.NoError(t, f.mr.Set(f.key(), `{"cred_epoch":"epoch-1","user_gen":"ugen-1","server_gen":"","strength":"mfa"}`))
	require.False(t, f.read().Covers(PurposeChannelDelete, GraceServerRule, enrolled))
}

// TestGraceStore_DMScopeNeedsNoServerGeneration: a DM delete has no server,
// so its grace is stamped with the user's generation alone and still voids
// when that one moves.
func TestGraceStore_DMScopeNeedsNoServerGeneration(t *testing.T) {
	f := newGraceFixture(t)
	f.mr.Del(permgen.ServerKey(f.server.String()))
	f.scope = DeleteGraceScope(DMDeleteScope())
	f.grant(t, GraceStrengthPassword)
	require.True(t, f.read().Covers(PurposeDMMessageDelete, GraceOwnRule, unenrolled))

	require.NoError(t, f.mr.Set(permgen.UserKey(f.actor.UserID.String()), "ugen-2"))
	require.False(t, f.read().Covers(PurposeDMMessageDelete, GraceOwnRule, unenrolled))
}

func TestGraceStore_GrantRefusesAnUnknownStrength(t *testing.T) {
	f := newGraceFixture(t)
	require.ErrorIs(t, f.store.Grant(context.Background(), f.read(), "sms"), errGraceStrength)
	require.False(t, f.mr.Exists(f.key()))
}

func ginContextWithClaims(t *testing.T, claims jwt.MapClaims) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(middleware.JWTClaimsContextKey, claims)
	return c
}

func TestGraceActorFromContext_ReadsTheTokenClaims(t *testing.T) {
	uid := uuid.New()
	c := ginContextWithClaims(t, jwt.MapClaims{"sid": "s-1", "cred_epoch": "e-1"})
	require.Equal(t, GraceActor{UserID: uid, SessionID: "s-1", Epoch: "e-1"}, GraceActorFromContext(c, uid))

	bare := ginContextWithClaims(t, jwt.MapClaims{})
	require.Equal(t, GraceActor{UserID: uid}, GraceActorFromContext(bare, uid))
}

// TestCharge: a request with no code is never charged; one with a code is
// charged once, refused with the flagged 429 when the budget is spent, and
// the flagged 503 with a Cause when it cannot be evaluated.
func TestCharge(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newSoftLockRedis(t)
	b := DangerousActionBudget(rdb)
	require.Equal(t, MFASettingsBudgetPrefix+"u1", b.Key("u1"), "the shared MFA budget, not a per-route one")

	for range BudgetLimit + 2 {
		require.Nil(t, Charge(ctx, b, "u1", ""))
	}
	require.False(t, mr.Exists(b.Key("u1")), "a request with no code charges nothing")

	for i := range BudgetLimit {
		require.Nil(t, Charge(ctx, b, "u1", "123456"), "attempt %d", i+1)
	}
	e := Charge(ctx, b, "u1", "123456")
	require.NotNil(t, e)
	require.Equal(t, http.StatusTooManyRequests, e.Status)
	require.Equal(t, true, e.Body["step_up_budget_exhausted"])

	dead := deadRedisClient()
	t.Cleanup(func() { _ = dead.Close() })
	e = Charge(ctx, DangerousActionBudget(dead), "u1", "123456")
	require.NotNil(t, e)
	require.Equal(t, http.StatusServiceUnavailable, e.Status)
	require.Equal(t, true, e.Body["step_up_budget_unavailable"])
	require.Error(t, e.Cause)
}
