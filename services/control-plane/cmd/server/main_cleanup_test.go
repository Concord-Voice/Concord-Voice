package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noExpiryPTTL and missingKeyPTTL mirror the go-redis v9 PTTL sentinels
// UNSCALED, per internal/presence/activity_store.go lines ~110-116: the
// DurationCmd precision multiplier is applied only to non-sentinel values, so
// -1/-2 arrive as raw nanoseconds, never as -1*time.Second / -2*time.Second.
const (
	noExpiryPTTL   = -1 * time.Nanosecond
	missingKeyPTTL = -2 * time.Nanosecond
)

type stubPresenceStore struct {
	// pages holds one entry per SCAN cursor page. Several entries exercise the
	// cursor loop, which real SCAN reaches routinely at COUNT 100.
	pages   [][]string
	ttls    map[string]time.Duration
	deleted []string
	matches []string
	scanErr error
	evalErr error
}

func (s *stubPresenceStore) Scan(_ context.Context, cursor uint64, match string, _ int64) *redis.ScanCmd {
	s.matches = append(s.matches, match)
	if s.scanErr != nil {
		return redis.NewScanCmdResult(nil, 0, s.scanErr)
	}
	// Compare in uint64 space: len() is never negative, so widening it is always
	// safe, whereas narrowing the cursor is the conversion gosec rejects (G115).
	total := uint64(len(s.pages))
	if cursor >= total {
		return redis.NewScanCmdResult(nil, 0, nil)
	}
	next := cursor + 1
	if next >= total {
		next = 0 // 0 terminates the SCAN loop
	}
	return redis.NewScanCmdResult(s.pages[cursor], next, nil)
}

// stubPipeliner implements only the one method cleanupStalePresence pipelines.
// Embedding the interface satisfies redis.Pipeliner at compile time; any other
// method panics, which is the intent — a future batch of some OTHER command
// should fail loudly here rather than silently return a zero value.
type stubPipeliner struct {
	redis.Pipeliner
	store *stubPresenceStore
	cmds  []redis.Cmder
}

// Eval models reapUnexpiringPresenceLua faithfully, INCLUDING its atomicity: the
// script deletes only on the -1 sentinel (exists, no expiry) and returns what DEL
// removed. A key reported as already gone (-2) must contribute 0 and must NOT be
// recorded as deleted — that distinction is the whole point of the guard.
func (p *stubPipeliner) Eval(_ context.Context, script string, keys []string, _ ...any) *redis.Cmd {
	cmd := redis.NewCmd(context.Background())
	if p.store.evalErr != nil {
		cmd.SetErr(p.store.evalErr)
		p.cmds = append(p.cmds, cmd)
		return cmd
	}
	if script != reapUnexpiringPresenceLua {
		cmd.SetErr(errors.New("unexpected script"))
		p.cmds = append(p.cmds, cmd)
		return cmd
	}
	key := keys[0]
	ttl, known := p.store.ttls[key]
	if !known {
		ttl = missingKeyPTTL
	}
	if ttl == noExpiryPTTL {
		p.store.deleted = append(p.store.deleted, key)
		cmd.SetVal(int64(1))
	} else {
		cmd.SetVal(int64(0))
	}
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (s *stubPresenceStore) Pipelined(
	_ context.Context,
	fn func(redis.Pipeliner) error,
) ([]redis.Cmder, error) {
	p := &stubPipeliner{store: s}
	if err := fn(p); err != nil {
		return nil, err
	}
	return p.cmds, s.evalErr
}

func onePageStore(keys []string, ttls map[string]time.Duration) *stubPresenceStore {
	return &stubPresenceStore{pages: [][]string{keys}, ttls: ttls}
}

func runPresenceCleanup(t *testing.T, store *stubPresenceStore) string {
	t.Helper()
	var logs bytes.Buffer
	cleanupStalePresence(context.Background(), store, logger.NewWithWriter(&logs))
	return logs.String()
}

func TestCleanupStalePresenceCountsSuccessfulDeletes(t *testing.T) {
	key := "presence:" + uuid.NewString()
	logs := runPresenceCleanup(t, onePageStore(
		[]string{key},
		map[string]time.Duration{key: noExpiryPTTL},
	))

	assert.Contains(t, logs, "Cleanup: presence pass complete")
	assert.Contains(t, logs, "count=1")
}

func TestCleanupStalePresenceDoesNotCountFailedReaps(t *testing.T) {
	key := "presence:" + uuid.NewString()
	store := onePageStore([]string{key}, map[string]time.Duration{key: noExpiryPTTL})
	store.evalErr = errors.New("redis EVAL failed")
	logs := runPresenceCleanup(t, store)

	assert.Contains(t, logs, "Cleanup: failed to reap unexpiring presence keys")
	assert.Contains(t, logs, "count=0")
	assert.Empty(t, store.deleted)
}

// Regressions for the rich-presence wipe. cleanupStalePresence USED TO trim the
// "presence:" prefix and treat every remainder that failed uuid.Parse as a
// malformed key, deleting it unconditionally — which caught the whole
// "presence:rich:<uuid>:<category>" family. Its connected-user arm ALSO deleted
// every live base key on a boot pass, because runCleanup fires before any client
// has registered. No issue number — ad-hoc hotfix.

// TestCleanupStalePresenceDeletesOnlyUnexpiringBaseKeys is the load-bearing test
// of this set: one mixed pass, asserting the EXACT deleted slice.
//
// Every key except `dead` carries no expiry, so the TTL predicate cannot be what
// saves them — only the allowlist can. That is what makes this test constrain
// the allowlist, and it is why the single-key preservation tests below do not
// suffice alone: a key with a live TTL survives any mutation that keeps the TTL
// gate, so those tests stay green even against a denylist.
//
// It also cannot pass vacuously — a no-op implementation fails on `dead`.
func TestCleanupStalePresenceDeletesOnlyUnexpiringBaseKeys(t *testing.T) {
	live := "presence:" + uuid.NewString()
	dead := "presence:" + uuid.NewString()
	rich := "presence:rich:" + uuid.NewString() + ":" + string(presence.CategoryServerVoice)
	call := "presence:rich:" + uuid.NewString() + ":" + string(presence.CategoryPrivateCall)
	other := "presence:future:" + uuid.NewString() + ":thing"

	store := onePageStore(
		[]string{rich, live, dead, call, other},
		map[string]time.Duration{
			rich:  noExpiryPTTL,
			live:  120 * time.Second,
			dead:  noExpiryPTTL,
			call:  noExpiryPTTL,
			other: noExpiryPTTL,
		},
	)
	runPresenceCleanup(t, store)

	assert.Equal(t, []string{dead}, store.deleted,
		"only an unexpiring BASE presence key may be deleted; every other key here is "+
			"unexpiring too, so anything else in this slice means the allowlist failed")
}

// An unexpiring rich key is not hypothetical: internal/presence declares
// ErrUnexpiringActivityState for exactly this state and internal/activepresence
// handles it. Only the allowlist can save it — the TTL predicate would not.
func TestCleanupStalePresencePreservesUnexpiringRichKey(t *testing.T) {
	rich := "presence:rich:" + uuid.NewString() + ":" + string(presence.CategoryPrivateCall)
	store := onePageStore([]string{rich}, map[string]time.Duration{rich: noExpiryPTTL})
	runPresenceCleanup(t, store)

	assert.Empty(t, store.deleted,
		"an UNEXPIRING rich-presence key was deleted; the TTL predicate cannot save it, so this is the allowlist failing")
}

func TestCleanupStalePresencePreservesRichPresenceKeys(t *testing.T) {
	rich := "presence:rich:" + uuid.NewString() + ":" + string(presence.CategoryServerVoice)
	store := onePageStore([]string{rich}, map[string]time.Duration{rich: 90 * time.Second})
	runPresenceCleanup(t, store)

	assert.Empty(t, store.deleted, "rich-presence key was deleted by the base-presence cleanup job")
}

// A live base key must survive a pass that finds no connected users. This is the
// boot pass, which deleted the fleet's presence on every restart.
func TestCleanupStalePresencePreservesLiveBaseKey(t *testing.T) {
	base := "presence:" + uuid.NewString()
	store := onePageStore([]string{base}, map[string]time.Duration{base: 120 * time.Second})
	runPresenceCleanup(t, store)

	assert.Empty(t, store.deleted, "a base-presence key with a positive TTL was deleted")
}

func TestCleanupStalePresenceSkipsUnrecognizedPresenceFamily(t *testing.T) {
	future := "presence:future:" + uuid.NewString() + ":thing"
	store := onePageStore([]string{future}, map[string]time.Duration{future: noExpiryPTTL})
	runPresenceCleanup(t, store)

	assert.Empty(t, store.deleted,
		"an unrecognized presence:* family was deleted; it carries no TTL, so only the allowlist protects it")
}

func TestCleanupStalePresenceDeletesUnexpiringBaseKey(t *testing.T) {
	base := "presence:" + uuid.NewString()
	store := onePageStore([]string{base}, map[string]time.Duration{base: noExpiryPTTL})
	runPresenceCleanup(t, store)

	assert.Equal(t, []string{base}, store.deleted, "a base-presence key with no TTL was NOT deleted")
}

// uuid.Parse is case-insensitive and also accepts the urn:uuid:, brace-wrapped
// and bare-32-hex spellings. Nothing writes those, but under an ALLOWLIST that
// leniency errs toward DELETING rather than keeping, so the predicate is pinned
// to the exact spelling presence.StatusRedisKey emits.
func TestCleanupStalePresenceRejectsNonCanonicalUUIDSpellings(t *testing.T) {
	id := uuid.New()
	canonical := id.String()
	bare32 := canonical[0:8] + canonical[9:13] + canonical[14:18] + canonical[19:23] + canonical[24:]

	for name, key := range map[string]string{
		"urn":      "presence:urn:uuid:" + canonical,
		"braced":   "presence:{" + canonical + "}",
		"bare32":   "presence:" + bare32,
		"upper":    "presence:" + strings.ToUpper(canonical),
		"trailing": "presence:" + canonical + ":extra",
	} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, isBasePresenceKey(key),
				"a non-canonical spelling must not be treated as a base presence key")

			store := onePageStore([]string{key}, map[string]time.Duration{key: noExpiryPTTL})
			runPresenceCleanup(t, store)
			assert.Empty(t, store.deleted, "a non-canonical spelling was deleted")
		})
	}
}

func TestCleanupStalePresenceAcceptsTheKeyTheWriterEmits(t *testing.T) {
	assert.True(t, isBasePresenceKey(presence.StatusRedisKey(uuid.New())),
		"the exact key StatusRedisKey builds must be recognized, or the job reaps nothing")
}

// The fail-safe direction this whole change exists to protect: an unreadable TTL
// must SKIP, never delete. Without this test the mutation
// `if err != nil { deleteStaleKey(key) }` restores a data-loss bug, suite green.
func TestCleanupStalePresenceSkipsKeysWhoseTTLCannotBeRead(t *testing.T) {
	base := "presence:" + uuid.NewString()
	store := onePageStore([]string{base}, map[string]time.Duration{base: noExpiryPTTL})
	store.evalErr = errors.New("redis EVAL failed")
	logs := runPresenceCleanup(t, store)

	assert.Empty(t, store.deleted, "a key whose TTL could not be read was deleted")
	assert.Contains(t, logs, "Cleanup: failed to reap unexpiring presence keys")
	assert.Contains(t, logs, "ttl_errors=1")
}

// A truncated pass must not report as a complete one.
func TestCleanupStalePresenceReportsATruncatedPassAsPartial(t *testing.T) {
	store := &stubPresenceStore{scanErr: errors.New("redis SCAN failed")}
	logs := runPresenceCleanup(t, store)

	assert.Contains(t, logs, "Cleanup: failed to scan presence keys")
	assert.Contains(t, logs, "partial=true")
	assert.Empty(t, store.deleted)
}

// The summary is this job's only heartbeat. With the predicate narrowed a
// healthy fleet deletes nothing, so a summary gated on staleCount>0 would emit
// no line ever — and a dead job would look identical to a healthy one.
func TestCleanupStalePresenceAlwaysLogsAPassSummary(t *testing.T) {
	base := "presence:" + uuid.NewString()
	logs := runPresenceCleanup(t, onePageStore(
		[]string{base},
		map[string]time.Duration{base: 120 * time.Second},
	))

	assert.Contains(t, logs, "Cleanup: presence pass complete")
	assert.Contains(t, logs, "count=0")
	assert.Contains(t, logs, "partial=false")
	assert.Contains(t, logs, "scanned=1")
	assert.Contains(t, logs, "candidates=1")
}

// SCAN must stay bounded to the presence keyspace. Widening it to "*" would
// still be safe under the allowlist, but it turns a targeted scan into a
// full-keyspace walk every hour.
func TestCleanupStalePresenceScansOnlyThePresenceKeyspace(t *testing.T) {
	base := "presence:" + uuid.NewString()
	store := onePageStore([]string{base}, map[string]time.Duration{base: 120 * time.Second})
	runPresenceCleanup(t, store)

	require.NotEmpty(t, store.matches)
	for _, m := range store.matches {
		assert.Equal(t, "presence:*", m)
	}
}

// The cursor loop must walk every page. A single-page stub would let a mutation
// that stops after page one reduce the job to the first 100 keys.
func TestCleanupStalePresenceWalksEveryScanPage(t *testing.T) {
	first := "presence:" + uuid.NewString()
	second := "presence:" + uuid.NewString()
	store := &stubPresenceStore{
		pages: [][]string{{first}, {second}},
		ttls: map[string]time.Duration{
			first:  120 * time.Second,
			second: noExpiryPTTL,
		},
	}
	logs := runPresenceCleanup(t, store)

	assert.Equal(t, []string{second}, store.deleted,
		"the second page was never scanned, so its unexpiring key survived")
	assert.Contains(t, logs, "scanned=2")
}

// VH-2 regression (red-team, proven with a passing exploit against real Redis).
// A key PTTL reports as ALREADY GONE (-2) must not be deleted: between that read
// and the delete, a reconnecting client can SET the key with a fresh 120s TTL,
// so a non-atomic guard destroys a live registration. The Lua guard deletes only
// on -1, and this pins that it never treats -2 as deletable.
func TestCleanupStalePresenceNeverReapsAKeyReportedAsAlreadyGone(t *testing.T) {
	vanished := "presence:" + uuid.NewString()
	store := onePageStore([]string{vanished}, map[string]time.Duration{vanished: missingKeyPTTL})
	logs := runPresenceCleanup(t, store)

	assert.Empty(t, store.deleted,
		"a key Redis reported as already gone was deleted; a reconnect in that window loses a live registration")
	assert.Contains(t, logs, "count=0")
}

// PTTL 0 means "expires within the current millisecond" — a positive TTL, not a
// sentinel. The key must be left to expire on its own.
func TestCleanupStalePresenceKeepsAKeyExpiringThisMillisecond(t *testing.T) {
	base := "presence:" + uuid.NewString()
	store := onePageStore([]string{base}, map[string]time.Duration{base: 0})
	runPresenceCleanup(t, store)

	assert.Empty(t, store.deleted, "a key with ttl==0 is about to expire on its own and must not be deleted")
}
