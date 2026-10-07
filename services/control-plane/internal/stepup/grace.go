package stepup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/permgen"
)

// GraceTTL is how long one verified confirmation covers repeats (#3454 D-2).
// Fixed, never configuration, and never extended: a grace-covered action does
// not grant, so the window cannot slide.
const GraceTTL = 10 * time.Minute

// FailureClassGraceGrant is the failure_class of the Warn a caller logs when
// GraceStore.Grant fails. One fixed value for every route, D1 and soft-lock.
const FailureClassGraceGrant = "step_up_grace_grant_failed"

// GraceStrength is the factor class a grace was earned with.
type GraceStrength string

// The two strengths a grace may carry. A confirmation verified through
// VerifyMFAFactorTx (TOTP, a backup code, or a WebAuthn token) earns mfa; a
// password step-up token spent through VerifyOwnRuleTx's password arm earns
// password.
const (
	GraceStrengthMFA      GraceStrength = "mfa"
	GraceStrengthPassword GraceStrength = "password"
)

func (s GraceStrength) valid() bool { return s == GraceStrengthMFA || s == GraceStrengthPassword }

// GraceRule is the rule governing the action a grace is judged for. The zero
// value is not a rule and covers nothing.
type GraceRule int

const (
	// GraceServerRule governs every D1 gate and the soft-lock's enforcing
	// arm. It accepts only an mfa grace, and only while the actor still has an
	// inline factor.
	GraceServerRule GraceRule = iota + 1
	// GraceOwnRule governs an own-rule soft-lock confirmation. It accepts an
	// mfa grace, and a password grace only while the actor has no inline
	// factor: an account that has since enrolled is asked for its MFA.
	GraceOwnRule
)

func (r GraceRule) accepts(s GraceStrength, subj Subject) bool {
	switch r {
	case GraceServerRule:
		return s == GraceStrengthMFA && subj.MFAEnabled
	case GraceOwnRule:
		return s == GraceStrengthMFA || (s == GraceStrengthPassword && !subj.MFAEnabled)
	default:
		return false
	}
}

// GraceScope is what one grace covers. Build it with DangerousActionGraceScope
// or DeleteGraceScope (softlock.go, beside the other key spellings); the zero
// value covers nothing.
type GraceScope struct {
	tail      string
	serverID  uuid.UUID
	hasServer bool
}

// GraceActor is the session a grace belongs to. Build it with
// GraceActorFromContext in a handler.
type GraceActor struct {
	UserID uuid.UUID
	// SessionID is the access token's sid claim. A token without one, or one
	// that is not a uuid, gets no grace: a grace is per session, never per
	// account.
	SessionID string
	// Epoch is the access token's cred_epoch claim, stamped into a grant and
	// compared on a read. Empty for an account that has never rotated, which
	// is still exact: LockSubjectTx admits an empty claim only while the
	// stored epoch is empty too, and a rotation never returns it to empty.
	Epoch string
}

// GraceActorFromContext reads the actor's session claims from the
// authenticated request. userID is the caller's parsed user_id.
func GraceActorFromContext(c *gin.Context, userID uuid.UUID) GraceActor {
	return GraceActor{
		UserID:    userID,
		SessionID: middleware.TokenSessionID(c),
		Epoch:     middleware.TokenCredentialEpoch(c),
	}
}

// graceStamp is a grace key's value. Every field is a stamp a read must match
// (#3454 D-2): the session's epoch claim, and the user's and server's
// permission generations read before the confirming transaction.
type graceStamp struct {
	CredEpoch string        `json:"cred_epoch"`
	UserGen   string        `json:"user_gen"`
	ServerGen string        `json:"server_gen"`
	Strength  GraceStrength `json:"strength"`
}

// GraceRead is one GraceStore.Read: the stored grace, if any, and the
// generations current when it was read. The zero value covers nothing and
// grants nothing.
//
// It is read BEFORE BeginTx and judged under the gate's locks (#3454 A-9):
// no Redis call may sit inside a gate transaction. Judging against the
// pre-read generations only lengthens the window a writer's post-commit bump
// already has, by the time between the read and the lock.
type GraceRead struct {
	key          string
	epoch        string
	userGenKey   string
	serverGenKey string
	userGen      string
	serverGen    string
	hasServer    bool
	stamp        graceStamp
	found        bool
}

// gensComplete reports whether every generation the scope needs was present.
// An absent generation is never a stamp: "absent" is the same string every
// time it recurs, so a grace stamped with it could match again after an
// eviction. Covers refuses an incomplete read; Grant seeds the missing
// generation (seedGenerations) and grants only when its own SET NX created it.
func (r GraceRead) gensComplete() bool {
	return r.userGen != "" && (!r.hasServer || r.serverGen != "")
}

// Covers is the pure grace judgement, run under the gate's locks after
// LockGateTx (or LockSubjectTx) admitted the session. It is true only when a
// well-formed grace was read and all of these hold:
//
//   - purpose is grace-eligible (Purpose.GraceEligible);
//   - the grace's cred_epoch equals the session's epoch claim;
//   - its user and server generations equal the ones read with it, and both
//     were present;
//   - its strength satisfies rule for subj, the Subject read under the lock.
//
// A grace-covered action is confirmed without a verifier call, and must not
// grant: the caller settles it without Grant, so the window never slides.
// Nothing the caller logs or counts may tell a covered action from a verified
// one (observability principle 7).
func (r GraceRead) Covers(purpose Purpose, rule GraceRule, subj Subject) bool {
	if !r.found || !purpose.GraceEligible() || !r.gensComplete() {
		return false
	}
	s := r.stamp
	if s.CredEpoch != r.epoch || s.UserGen != r.userGen || s.ServerGen != r.serverGen {
		return false
	}
	return rule.accepts(s.Strength, subj)
}

// GraceStore reads and grants step-up graces. It lives in this package
// because mfaenforce's import allowlist forbids go-redis.
type GraceStore struct {
	rdb *redis.Client
}

// NewGraceStore binds the store to its Redis client. A nil client reads no
// grace and grants nothing: the actor is prompted, which is the safe side.
func NewGraceStore(rdb *redis.Client) GraceStore {
	return GraceStore{rdb: rdb}
}

// Read fetches the grace for (a, s) and the permission generations it is
// judged against, in ONE MGET, before the caller's BeginTx. It never fails
// the request: a Redis error, a malformed value, an unusable sid or an
// invalid scope all read as no grace, so the actor is prompted (D-2). It
// writes nothing: a request on a server that does not enforce, carrying no
// code, must leave Redis exactly as it found it.
func (g GraceStore) Read(ctx context.Context, a GraceActor, s GraceScope) GraceRead {
	sid, err := uuid.Parse(a.SessionID)
	if g.rdb == nil || s.tail == "" || err != nil {
		return GraceRead{}
	}
	keys := []string{graceKey(a.UserID, sid, s), permgen.UserKey(a.UserID.String())}
	if s.hasServer {
		keys = append(keys, permgen.ServerKey(s.serverID.String()))
	}
	vals, err := g.rdb.MGet(ctx, keys...).Result()
	if err != nil || len(vals) != len(keys) {
		return GraceRead{}
	}
	r := GraceRead{key: keys[0], epoch: a.Epoch, hasServer: s.hasServer, userGenKey: keys[1], userGen: redisString(vals[1])}
	if s.hasServer {
		r.serverGenKey = keys[2]
		r.serverGen = redisString(vals[2])
	}
	r.stamp, r.found = parseGraceStamp(redisString(vals[0]))
	return r
}

var errGraceStrength = errors.New("step-up grace: unknown strength")

// Grant records a verified confirmation as a grace for GraceTTL, stamped with
// the epoch and the generations r read before the transaction. Call it only
// after that transaction COMMITTED and only when a factor VERIFIED, on
// SettleContext; never for a grace-covered action, or the window slides. It
// is best-effort, like Budget.Clear: a failure only prompts sooner, and the
// caller logs it at Warn with FailureClassGraceGrant. A read that found a
// generation absent is granted only if Grant's own SET NX creates it (see
// seedGenerations); otherwise it grants nothing.
func (g GraceStore) Grant(ctx context.Context, r GraceRead, strength GraceStrength) error {
	_, err := g.GrantRead(ctx, r, strength)
	return err
}

// GrantRead is Grant that also returns the read it stamped, carrying any
// generation it seeded, or the zero GraceRead when it granted nothing. A caller
// granting a second grace in the same settle passes that second read through
// AdoptGenerations first: the first grant's SET NX created the shared
// generation, so the second's would find it and refuse.
func (g GraceStore) GrantRead(ctx context.Context, r GraceRead, strength GraceStrength) (GraceRead, error) {
	if !strength.valid() {
		return GraceRead{}, errGraceStrength
	}
	if g.rdb == nil || r.key == "" {
		return GraceRead{}, nil
	}
	if !r.gensComplete() {
		seeded, created, err := g.seedGenerations(ctx, r)
		if err != nil {
			return GraceRead{}, err
		}
		if !created {
			return GraceRead{}, nil
		}
		r = seeded
	}
	value, err := json.Marshal(graceStamp{
		CredEpoch: r.epoch, UserGen: r.userGen, ServerGen: r.serverGen, Strength: strength,
	})
	if err != nil {
		return GraceRead{}, fmt.Errorf("encode step-up grace: %w", err)
	}
	if err := g.rdb.Set(ctx, r.key, value, GraceTTL).Err(); err != nil {
		return GraceRead{}, fmt.Errorf("grant step-up grace: %w", err)
	}
	return r, nil
}

// AdoptGenerations fills each generation r found absent from from, a read a
// grace was just stamped with in the same settle, when both name the same
// generation key. Only a value from's own read saw, or from's own SET NX
// created, can arrive this way, and both predate anything written since; a
// bump between the two grants still voids the second, because its stamp no
// longer matches. Anything else is left as r had it.
func (r GraceRead) AdoptGenerations(from GraceRead) GraceRead {
	if r.userGen == "" && r.userGenKey != "" && r.userGenKey == from.userGenKey {
		r.userGen = from.userGen
	}
	if r.hasServer && r.serverGen == "" && r.serverGenKey != "" && r.serverGenKey == from.serverGenKey {
		r.serverGen = from.serverGen
	}
	return r
}

// seedGenerations creates each generation the read found absent, the way the
// permission cache does (permgen.New under SET NX, permgen.TTL). Without it a
// subject whose generation the cache never seeded (a DM-only session, or after
// the 24-hour TTL) could never be granted grace (#3454 T1b finding).
//
// It reports created only when EVERY SET NX it issued created its key, and
// then returns the read carrying the values it wrote. A key that already
// existed means something wrote that generation after the read: an MFA change
// or an enforcement flip bumping it after the confirmation committed, or the
// permission cache seeding it. Grant then grants nothing. It must never re-read
// and stamp the stored value, because that folds a post-confirmation bump into
// the grace and honours it for ten minutes (Codex review of #3454). NX never
// overwrites what the other writer stored. Seeding stays here, after the
// commit, rather than in Read, because Read runs on every gated request and a
// request that changes nothing must write nothing.
func (g GraceStore) seedGenerations(ctx context.Context, r GraceRead) (GraceRead, bool, error) {
	missing := r.missingGenerationKeys()
	gens := make([]string, len(missing))
	cmds := make([]*redis.BoolCmd, len(missing))
	for i := range missing {
		gen, err := permgen.New()
		if err != nil {
			return r, false, err
		}
		gens[i] = gen
	}
	_, err := g.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, key := range missing {
			cmds[i] = pipe.SetNX(ctx, key, gens[i], permgen.TTL)
		}
		return nil
	})
	if err != nil {
		return r, false, fmt.Errorf("seed permission generation for step-up grace: %w", err)
	}
	for i, key := range missing {
		if !cmds[i].Val() {
			return r, false, nil
		}
		if key == r.userGenKey {
			r.userGen = gens[i]
		} else {
			r.serverGen = gens[i]
		}
	}
	return r, true, nil
}

// missingGenerationKeys lists the generation keys the read found absent.
func (r GraceRead) missingGenerationKeys() []string {
	var keys []string
	if r.userGen == "" {
		keys = append(keys, r.userGenKey)
	}
	if r.hasServer && r.serverGen == "" {
		keys = append(keys, r.serverGenKey)
	}
	return keys
}

// parseGraceStamp accepts exactly one JSON object with the four known fields,
// a non-empty user generation and a known strength. Anything else, including
// an absent key, is no grace.
func parseGraceStamp(raw string) (graceStamp, bool) {
	if raw == "" {
		return graceStamp{}, false
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var s graceStamp
	if err := dec.Decode(&s); err != nil || dec.More() {
		return graceStamp{}, false
	}
	if s.UserGen == "" || !s.Strength.valid() {
		return graceStamp{}, false
	}
	return s, true
}

// redisString is an MGET element as a string: "" for an absent key.
func redisString(v any) string {
	s, _ := v.(string)
	return s
}
