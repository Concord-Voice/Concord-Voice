package loginfamily_test

import (
	"errors"
	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestClassifiedErrorsKeepCausePrivateAndPreserveControlFlow(t *testing.T) {
	cause := errors.New("synthetic confidential SQL diagnostic")
	cases := []struct {
		why   lf.ErrorReason
		class error
	}{
		{lf.ReasonAccessIdentity, lf.ErrDenied}, {lf.ReasonAccessExpired, lf.ErrDenied}, {lf.ReasonEpochMismatch, lf.ErrDenied}, {lf.ReasonFamilyTerminal, lf.ErrDenied}, {lf.ReasonPreserveUnknown, lf.ErrDenied},
		{lf.ReasonTargetUnknown, lf.ErrTargetNotFound}, {lf.ReasonIdentityConflict, lf.ErrConflict}, {lf.ReasonSQLUnavailable, lf.ErrUnavailable}, {lf.ReasonCommitUnknown, lf.ErrUnavailable},
		{lf.ReasonCapabilityInvalid, lf.ErrContract}, {lf.ReasonOwnershipInvalid, lf.ErrContract}, {lf.ReasonArgumentInvalid, lf.ErrContract}, {lf.ReasonRotateRepeated, lf.ErrContract},
	}
	for _, tc := range cases {
		t.Run(string(tc.why), func(t *testing.T) {
			err := lf.NewError(tc.why, cause)
			reason(t, err, tc.class, tc.why)
			require.ErrorIs(t, err, cause)
			require.NotContains(t, err.Error(), cause.Error())
		})
	}
	reason(t, lf.NewError(lf.ErrorReason("unknown"), cause), lf.ErrContract, lf.ReasonArgumentInvalid)
	require.Equal(t, lf.Binding{}, (lf.LiveFamily{}).Binding())
}
