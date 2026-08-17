package testhelpers

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
)

// SetupTestRedis returns a Redis client scoped to THIS PROCESS's own logical
// database. The signature is unchanged from the pre-#2680 helper so that the
// ~140 call sites did not have to move.
//
// The previous implementation pinned DB 1 whenever REDIS_URL was unset and
// honoured the URL's own DB when it was set. Both branches are deleted. The
// conditional was the defect: CI sets REDIS_URL (build.yml:423) with no DB
// segment, so CI got DB 0 for every package and zero isolation, while local
// runs got DB 1 for every package and zero isolation. The override is now
// unconditional — an explicit DB in REDIS_URL is advisory and is replaced.
func SetupTestRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()

	client := redistest.Client(t)

	if err := redistest.Reset(context.Background(), client); err != nil {
		t.Fatalf("testhelpers: failed to reset the allocated test redis DB: %v", err)
	}

	// redistest.Client already registers t.Cleanup for Close; the returned
	// closure stays for call-site compatibility and is safe to call twice.
	cleanup := func() { _ = client.Close() }

	return client, cleanup
}
