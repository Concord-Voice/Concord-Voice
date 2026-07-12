package presence

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// applyCustomTextOverrides removes the sender's materialized recipient
// exclusions as the final custom-text authorization filter. Query failures are
// returned so callers fail closed rather than using the unfiltered base.
func applyCustomTextOverrides(ctx context.Context, db DBTX, senderID uuid.UUID, audience map[uuid.UUID]bool) (map[uuid.UUID]bool, error) {
	excluded := make(map[uuid.UUID]bool)
	rows, err := db.QueryContext(ctx, `
		SELECT target_user_id
		FROM user_presence_overrides
		WHERE sender_id = $1 AND category = 'custom_text'
	`, senderID)
	if err != nil {
		return nil, fmt.Errorf("presence override audience: query exclusions: %w", err)
	}
	if err := scanIDs(rows, excluded); err != nil {
		return nil, fmt.Errorf("presence override audience: scan exclusions: %w", err)
	}

	for targetID := range excluded {
		delete(audience, targetID)
	}
	return audience, nil
}
