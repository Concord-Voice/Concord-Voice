package users_test

import (
	"context"

	"github.com/google/uuid"
)

// permitAllPresence is the explicit "this test does not exercise the
// base-presence gate" value for the Rich Presence constructors' required
// resolver parameter (#2444). Passing nil would fail closed and suppress every
// emission, silently changing what the surrounding tests assert.
type permitAllPresence struct{}

func (permitAllPresence) RichPresenceEmissionPermitted(context.Context, uuid.UUID) bool {
	return true
}

func (d permitAllPresence) RichPresenceEmissionState(
	ctx context.Context, senderID uuid.UUID,
) (bool, error) {
	// Test double: always DETERMINED, so it exercises the
	// suppression path rather than the indeterminate one.
	return d.RichPresenceEmissionPermitted(ctx, senderID), nil
}
