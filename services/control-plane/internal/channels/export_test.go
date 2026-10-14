package channels

import "time"

// SetResponseBudgetForTest overrides the expiration PATCH's response budget,
// so a test can show the handler hands its deadline to the work after commit.
func (h *Handler) SetResponseBudgetForTest(d time.Duration) { h.responseBudget = d }
