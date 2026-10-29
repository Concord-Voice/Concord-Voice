package billing

import (
	"database/sql"
	"errors"
	"time"

	"github.com/stripe/stripe-go/v87"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
)

// ErrUnactionable marks a Stripe object Concord cannot turn into a row: there is
// no usable period end to store, or the object belongs to nobody here. The
// caller records the event and acknowledges it, because a retry would see the
// same object.
var ErrUnactionable = errors.New("billing: unactionable")

// rowFields is everything classify decides for one subscriptions row.
type rowFields struct {
	Status            string
	PeriodEnd         time.Time
	Plan, Interval    sql.NullString
	Tier              string // entitlements.TierPremium or TierFree
	FoundersRate      bool
	UnitAmount        sql.NullInt64
	ScheduleID        sql.NullString
	PendingPlan       sql.NullString
	PendingInterval   sql.NullString
	PendingChangeAt   sql.NullTime
	CancelAtPeriodEnd bool
	CancelAt          sql.NullTime
	CustomerID        string
	Malformed         string // "" or the fail-closed reason, for the error log
}

// classify maps a fetched subscription (items.data.price and discounts
// expanded) to row fields. The status is stored as Stripe sent it; whether it
// entitles is the live-row predicate's call. Anything that is not exactly one
// item, quantity 1, a known plan and a monthly or yearly price fails closed to
// the free tier with a Malformed reason, because paying for a shape Concord
// never sold must not grant premium. Only a missing period end (or no item to
// read one from) returns ErrUnactionable: there is nothing to store.
func classify(sub *stripe.Subscription) (rowFields, error) {
	if sub.Items == nil || len(sub.Items.Data) == 0 {
		return rowFields{}, ErrUnactionable
	}
	first := sub.Items.Data[0]
	if first == nil || first.CurrentPeriodEnd <= 0 {
		return rowFields{}, ErrUnactionable
	}

	out := rowFields{
		Status:            string(sub.Status),
		PeriodEnd:         time.Unix(first.CurrentPeriodEnd, 0).UTC(),
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
		FoundersRate:      hasFoundersDiscount(sub.Discounts),
	}
	if sub.Customer != nil {
		out.CustomerID = sub.Customer.ID
	}
	if sub.CancelAt > 0 {
		out.CancelAt = sql.NullTime{Time: time.Unix(sub.CancelAt, 0).UTC(), Valid: true}
	}
	if sub.Schedule != nil {
		out.ScheduleID = sql.NullString{String: sub.Schedule.ID, Valid: sub.Schedule.ID != ""}
	}

	out.Plan, out.Interval, out.Malformed = itemFields(first)
	if len(sub.Items.Data) != 1 && out.Malformed == "" {
		out.Malformed = "subscription has more than one item"
	}
	out.Tier = entitlements.TierFree
	if out.Malformed == "" {
		out.Tier = entitlements.TierPremium
		out.UnitAmount = chargedUnitAmount(first.Price, out.FoundersRate)
	}
	return out, nil
}

// chargedUnitAmount is the USD amount the item charges per period, after the
// founders discount, or NULL when the price is not a known USD amount.
func chargedUnitAmount(price *stripe.Price, founders bool) sql.NullInt64 {
	if price == nil || price.Currency != stripe.CurrencyUSD || price.UnitAmount < 0 {
		return sql.NullInt64{}
	}
	amount := price.UnitAmount
	if founders {
		amount = foundersUnitAmount(amount)
	}
	return sql.NullInt64{Int64: amount, Valid: true}
}

// itemFields reads plan and interval from the item's price and reports why the
// item is not a Concord plan, or "" when it is. Plan and interval are NULL
// unless they hold a value the subscriptions CHECK constraints accept.
func itemFields(item *stripe.SubscriptionItem) (plan, interval sql.NullString, malformed string) {
	if item.Price == nil {
		return plan, interval, "item has no price"
	}
	switch p := item.Price.Metadata[PlanMetadataKey]; p {
	case PlanSupersonic, PlanWing:
		plan = sql.NullString{String: p, Valid: true}
	default:
		malformed = "price has no known " + PlanMetadataKey
	}
	if item.Price.Recurring != nil {
		switch iv := item.Price.Recurring.Interval; iv {
		case stripe.PriceRecurringIntervalMonth, stripe.PriceRecurringIntervalYear:
			interval = sql.NullString{String: string(iv), Valid: true}
		}
	}
	if malformed == "" && !interval.Valid {
		malformed = "price interval is not month or year"
	}
	if malformed == "" && item.Quantity != 1 {
		malformed = "item quantity is not 1"
	}
	return plan, interval, malformed
}

// hasFoundersDiscount reports whether any discount came from the founders coupon.
func hasFoundersDiscount(discounts []*stripe.Discount) bool {
	for _, d := range discounts {
		if d != nil && d.Source != nil && d.Source.Coupon != nil && d.Source.Coupon.ID == CouponFoundersID {
			return true
		}
	}
	return false
}
