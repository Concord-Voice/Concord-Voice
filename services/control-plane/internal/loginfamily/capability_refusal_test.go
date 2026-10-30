package loginfamily_test

import (
	"testing"

	lf "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A zero UserAuthority or Targets is not a capability: every capture and
// revocation entry point refuses it before touching the transaction.
func TestZeroCapabilitiesAreRefusedBeforeSQL(t *testing.T) {
	var none lf.UserAuthority
	_, err := lf.ResolveMatchedSIDsTx(ctx, nil, none, []uuid.UUID{uuid.New()})
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.CaptureDisplacementTargetsTx(ctx, nil, none, "machine", "198.51.100.7", "agent")
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.CaptureUserTargetsTx(ctx, nil, none, nil)
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.CaptureEpochTargetsTx(ctx, nil, none, "")
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.RevokeFamiliesForEpochTx(ctx, nil, lf.Targets{}, lf.EpochOwner{})
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.RevokeFamiliesTx(ctx, nil, lf.Targets{}, lf.AuthCause{Scope: lf.ScopeUser, Reason: lf.ReasonTheft})
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
}

func TestRevocationRefusesCausesOutsideTheClosedSet(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	i := newFamily(t, db, user, expires())
	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	targets, err := lf.ResolveTargetSIDTx(ctx, tx, u, i.SID)
	require.NoError(t, err)

	for name, cause := range map[string]lf.AuthCause{
		"unknown scope":              {Scope: lf.AuthScope("device"), Reason: lf.ReasonLogout},
		"empty cause":                {},
		"user scope with logout":     {Scope: lf.ScopeUser, Reason: lf.ReasonLogout},
		"user scope with unknown":    {Scope: lf.ScopeUser, Reason: lf.AuthReason("expiry")},
		"session scope with erasure": {Scope: lf.ScopeSession, Reason: lf.ReasonErasure},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := lf.RevokeFamiliesTx(ctx, tx, targets, cause)
			reason(t, err, lf.ErrContract, lf.ReasonArgumentInvalid)
		})
	}
	require.Zero(t, count(t, db, `SELECT count(*) FROM login_family_revocations`))

	// Every user-scope reason in the closed set is accepted.
	for _, why := range []lf.AuthReason{lf.ReasonExplicitRevoke, lf.ReasonDisplacement, lf.ReasonTheft, lf.ReasonDisablement, lf.ReasonErasure} {
		result, err := lf.RevokeFamiliesTx(ctx, tx, targets, lf.AuthCause{Scope: lf.ScopeUser, Reason: why})
		require.NoError(t, err, why)
		require.Len(t, result.Owners, 1, why)
	}
}

func TestCaptureUserTargetsRefusesForeignPreservedIdentity(t *testing.T) {
	db := database(t)
	user := testdb.CreateUser(t, db)
	stranger := testdb.CreateUser(t, db)
	foreign := newFamily(t, db, stranger, expires())
	newFamily(t, db, user, expires())

	tx := transaction(t, db)
	u := authority(t, tx, user, lf.UserNoKeyUpdate)
	_, err := lf.CaptureUserTargetsTx(ctx, tx, u, &foreign)
	reason(t, err, lf.ErrDenied, lf.ReasonPreserveUnknown)
	// Release the mutation lock before a share-mode authority on the same user.
	require.NoError(t, tx.Rollback())

	// A share-mode authority cannot capture a mutation frontier.
	shared := transaction(t, db)
	readOnly := authority(t, shared, user, lf.UserShare)
	_, err = lf.CaptureEpochTargetsTx(ctx, shared, readOnly, "")
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.ResolveTargetSIDTx(ctx, shared, readOnly, foreign.SID)
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
	_, err = lf.CaptureDisplacementTargetsTx(ctx, shared, readOnly, "machine", "198.51.100.7", "agent")
	reason(t, err, lf.ErrContract, lf.ReasonCapabilityInvalid)
}
