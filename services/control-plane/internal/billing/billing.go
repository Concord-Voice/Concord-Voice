// Package billing holds Concord's Stripe integration (epic #2033).
//
// PR-1 ships only the catalog side: the identifiers that Stripe objects and
// later billing code key on, a client constructor, and Bootstrap, which the
// operator tool cmd/stripe-bootstrap runs. Nothing in the control-plane
// server imports this package yet, so it ships dormant. Design:
// [internal]specs/2026-10-15-2033-stripe-billing-design.md §6.
package billing

// Catalog identifiers. They are constants, not config (spec §11): the same
// values exist in Stripe test mode and live mode.
const (
	// CouponFoundersID is the founders coupon's fixed Stripe ID in both modes.
	CouponFoundersID = "founders-50"
	// PortalMetadataKey and PortalMetadataVersion mark the one Customer
	// Portal configuration this code owns. Configurations without the pair
	// belong to someone else and are never changed.
	PortalMetadataKey     = "concord_portal"
	PortalMetadataVersion = "v1"
	// PlanMetadataKey is the price metadata key that entitlement reads. A
	// price's lookup_key only selects what to sell (spec §6, F11).
	PlanMetadataKey = "concord_plan"
	// ProductMetadataKey identifies a catalog product. Its value is the plan.
	ProductMetadataKey = "concord_product"
	// PlanSupersonic and PlanWing are the two sold plans.
	PlanSupersonic = "supersonic"
	PlanWing       = "wing"
)

// Price lookup keys (spec §6). Checkout resolves the price to sell by these.
const (
	LookupKeySupersonicMonthly = "supersonic_monthly"
	LookupKeySupersonicYearly  = "supersonic_yearly"
	LookupKeyWingMonthly       = "wing_monthly"
	LookupKeyWingYearly        = "wing_yearly"
)
