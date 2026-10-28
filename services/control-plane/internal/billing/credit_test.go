package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/redemption"
)

func TestCodeCreditAmount(t *testing.T) {
	cases := []struct {
		name                        string
		subPlan, interval, codePlan string
		founders                    bool
		months                      int
		want                        int64
		matched                     bool
	}{
		{"same plan monthly covers whole renewals", PlanSupersonic, "month", PlanSupersonic, false, 3, 3 * 899, true},
		{"founders monthly renews at the founders price", PlanSupersonic, "month", PlanSupersonic, true, 3, 3 * 449, true},
		{"twelve months covers one yearly renewal", PlanWing, "year", PlanWing, false, 12, 16489, true},
		{"founders yearly", PlanSupersonic, "year", PlanSupersonic, true, 12, 4944, true},
		{"a partial year rounds up to the cent", PlanSupersonic, "year", PlanSupersonic, false, 1, 825, true}, // 9889/12 = 824.08
		{"other plan is face value at the monthly list price", PlanSupersonic, "year", PlanWing, true, 3, 3 * 1499, false},
		{"downward plan is face value too", PlanWing, "month", PlanSupersonic, false, 2, 2 * 899, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, matched, err := codeCreditAmount(redemption.CreditRequest{
				SubscribedPlan: tc.subPlan, Interval: tc.interval, FoundersRate: tc.founders,
				CodePlan: tc.codePlan, Months: tc.months,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.matched, matched)
		})
	}

	_, _, err := codeCreditAmount(redemption.CreditRequest{SubscribedPlan: PlanSupersonic, Interval: "week", CodePlan: PlanSupersonic, Months: 1})
	require.ErrorIs(t, err, errUnknownPlan)
	_, _, err = codeCreditAmount(redemption.CreditRequest{SubscribedPlan: PlanSupersonic, Interval: "month", CodePlan: "platinum", Months: 1})
	require.ErrorIs(t, err, errUnknownPlan)
}

func creditRequest(codePlan string, confirmed bool) redemption.CreditRequest {
	return redemption.CreditRequest{
		SubscribedPlan: PlanSupersonic, Interval: "month", CodePlan: codePlan, Months: 3, Confirmed: confirmed,
	}
}

func TestQuoteCredit_SamePlan(t *testing.T) {
	svc := NewService(Deps{})
	res, err := svc.QuoteCredit(creditRequest(PlanSupersonic, false))
	require.NoError(t, err)
	assert.Equal(t, redemption.CreditResult{
		Amount: 2697, Currency: "usd", Matched: true, StripeDescription: "Concord code credit: 3 month(s) of supersonic",
	}, res)
}

func TestQuoteCredit_OtherPlanNeedsConfirmation(t *testing.T) {
	svc := NewService(Deps{})
	_, err := svc.QuoteCredit(creditRequest(PlanWing, false))
	var confirm *redemption.CreditConfirmationError
	require.ErrorAs(t, err, &confirm)
	assert.Equal(t, redemption.CreditConfirmationError{Amount: 4497, Currency: "usd", CodePlan: PlanWing, SubscribedPlan: PlanSupersonic}, *confirm)

	res, err := svc.QuoteCredit(creditRequest(PlanWing, true))
	require.NoError(t, err)
	assert.Equal(t, int64(4497), res.Amount)
	assert.False(t, res.Matched)
}

func TestQuoteCredit_UnknownPlan(t *testing.T) {
	_, err := NewService(Deps{}).QuoteCredit(redemption.CreditRequest{SubscribedPlan: PlanSupersonic, Interval: "week", CodePlan: PlanSupersonic, Months: 1})
	require.ErrorIs(t, err, errUnknownPlan)
}
