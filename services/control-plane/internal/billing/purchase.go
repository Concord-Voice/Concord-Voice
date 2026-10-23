package billing

import (
	"strings"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// purchaseAllowed reports whether userID may start a purchase under
// BILLING_PURCHASE_MODE (#2033 spec §11). It is the one gate behind both
// /billing/plans' purchase_available and Checkout's 503 purchase_unavailable,
// so the two cannot disagree. Off and "not on the allowlist" are one answer,
// and no caller may log which one applied (observability.md principle 7).
func (s *Service) purchaseAllowed(userID uuid.UUID) bool {
	switch s.Billing.PurchaseMode {
	case config.BillingPurchaseModeOn:
		return true
	case config.BillingPurchaseModeAllowlist:
		for _, entry := range s.Billing.PurchaseAllowlist {
			// config lower-cases entries; compare as UUIDs anyway so the
			// spelling of either side cannot matter.
			if id, err := uuid.Parse(strings.TrimSpace(entry)); err == nil && id == userID {
				return true
			}
		}
	}
	return false
}
