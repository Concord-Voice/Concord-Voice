package billing

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stripe/stripe-go/v87"
)

func TestChargedUnitAmount(t *testing.T) {
	usd := func(amount int64) *stripe.Price {
		return &stripe.Price{Currency: stripe.CurrencyUSD, UnitAmount: amount}
	}
	assert.Equal(t, sql.NullInt64{Int64: 999, Valid: true}, chargedUnitAmount(usd(999), false))
	assert.Equal(t, sql.NullInt64{Int64: foundersUnitAmount(999), Valid: true}, chargedUnitAmount(usd(999), true))
	assert.Equal(t, sql.NullInt64{Valid: true}, chargedUnitAmount(usd(0), false), "a free price is a known amount")
	assert.False(t, chargedUnitAmount(nil, false).Valid)
	assert.False(t, chargedUnitAmount(usd(-1), false).Valid)
	assert.False(t, chargedUnitAmount(&stripe.Price{Currency: stripe.CurrencyEUR, UnitAmount: 999}, false).Valid)
}
