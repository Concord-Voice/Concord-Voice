package presence

import (
	"encoding/json"

	"github.com/google/uuid"
)

// DeliveryPlan is a fully authorized live Rich Presence update/clear delta.
// The Hub must clear revoked viewers before updating the current audience.
type DeliveryPlan struct {
	SenderID         uuid.UUID
	Category         Category
	ClearRecipients  map[uuid.UUID]bool
	UpdateRecipients map[uuid.UUID]bool
	Minimized        bool
	Payload          json.RawMessage
	UpdatedAt        int64
}
