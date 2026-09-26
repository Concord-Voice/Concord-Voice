package rbac_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// Regression for the #3453 Phase-4 red-team finding: every spelling of one
// uuid must address ONE permission-cache entry and ONE generation, because
// PostgreSQL resolves them all to one row. With raw-string keys, an entry
// published under a non-canonical spelling escaped every canonical bump and
// invalidation. The server-generation bump is #3453's; the invalidation arms
// are the same class, pre-existing, and closed by the same change.
func TestPermissionCache_EveryUUIDSpellingIsOneEntry(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	ctx := context.Background()

	noHyphens := func(id string) string { return strings.ReplaceAll(id, "-", "") }
	braced := func(id string) string { return "{" + id + "}" }

	// publishVia plants an entry through the given spellings of server, user
	// and channel, and returns the canonical triple.
	publishVia := func(t *testing.T, spell func(string) string) (sid, uid, cid string) {
		t.Helper()
		sid, uid, cid = uuid.NewString(), uuid.NewString(), uuid.NewString()
		testhelpers.PublishPermissionCache(t, ts.Redis, spell(sid), spell(uid), spell(cid), rbac.PermKick)
		return sid, uid, cid
	}
	servedCanonically := func(sid, uid, cid string) bool {
		_, ok, _ := cache.Get(ctx, sid, uid, cid)
		return ok
	}

	for name, spell := range map[string]func(string) string{
		"uppercase":  strings.ToUpper,
		"no hyphens": noHyphens,
		"braced":     braced,
	} {
		t.Run(name+"/read through the canonical spelling", func(t *testing.T) {
			sid, uid, cid := publishVia(t, spell)
			assert.True(t, servedCanonically(sid, uid, cid), "one uuid, one entry")
		})
		t.Run(name+"/BumpServer reaches it", func(t *testing.T) {
			sid, uid, cid := publishVia(t, spell)
			require.NoError(t, cache.BumpServer(ctx, sid))
			_, ok, _ := cache.Get(ctx, spell(sid), spell(uid), spell(cid))
			assert.False(t, ok, "a canonical server bump must reach an entry published under %s", name)
		})
		t.Run(name+"/InvalidateServer reaches it", func(t *testing.T) {
			sid, uid, cid := publishVia(t, spell)
			require.NoError(t, cache.InvalidateServer(ctx, sid))
			_, ok, _ := cache.Get(ctx, spell(sid), spell(uid), spell(cid))
			assert.False(t, ok)
		})
		t.Run(name+"/InvalidateChannel reaches it", func(t *testing.T) {
			sid, uid, cid := publishVia(t, spell)
			require.NoError(t, cache.InvalidateChannel(ctx, sid, cid))
			_, ok, _ := cache.Get(ctx, spell(sid), spell(uid), spell(cid))
			assert.False(t, ok)
		})
		t.Run(name+"/Invalidate reaches it", func(t *testing.T) {
			sid, uid, cid := publishVia(t, spell)
			require.NoError(t, cache.Invalidate(ctx, sid, uid))
			_, ok, _ := cache.Get(ctx, spell(sid), spell(uid), spell(cid))
			assert.False(t, ok)
		})
	}

	t.Run("distinct uuids stay distinct", func(t *testing.T) {
		sid, uid, cid := publishVia(t, strings.ToUpper)
		assert.False(t, servedCanonically(uuid.NewString(), uid, cid))
		assert.False(t, servedCanonically(sid, uuid.NewString(), cid))
		assert.False(t, servedCanonically(sid, uid, uuid.NewString()))
	})
	t.Run("a non-uuid id is used as given", func(t *testing.T) {
		testhelpers.PublishPermissionCache(t, ts.Redis, "srv-Not-A-UUID", "usr-X", "", rbac.PermKick)
		assert.True(t, servedCanonically("srv-Not-A-UUID", "usr-X", ""))
		assert.False(t, servedCanonically("srv-not-a-uuid", "usr-x", ""), "case folding applies only to uuids")
	})
}
