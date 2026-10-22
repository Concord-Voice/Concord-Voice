// Package billing holds Concord's Stripe integration (epic #2033).
//
// It began as the catalog side only (PR-1): the identifiers that Stripe
// objects and later billing code key on, a client constructor, and Bootstrap,
// which the operator tool cmd/stripe-bootstrap runs. It now also holds the
// Service, the subscription classifier and, as this PR lands them, Reconcile,
// the webhook, the Portal session and the background workers. All of it stays
// dormant until Stripe keys are configured: with no keys the control plane
// registers no billing route and starts no billing worker. Design:
// [internal]specs/2026-10-15-2033-stripe-billing-design.md §6, §7.
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

// ReturnURLPortal is where Stripe sends a customer leaving the Customer Portal.
// Like the Checkout return URLs it is a constant, never a request value, so a
// client cannot steer the redirect (spec §11).
const ReturnURLPortal = "https://concordvoice.com/billing/return?result=portal"
