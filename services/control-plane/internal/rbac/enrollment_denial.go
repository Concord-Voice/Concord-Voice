package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

// EnrollmentDenial is the RS5 refusal (#3454): it decides whether a permission
// denial the caller has ALREADY made is one an inline MFA factor would lift.
//
// It returns stepup.EnrollmentRequired when, and only when, the server
// enforces, the actor has no inline factor (policy P1), and the actor would
// hold requiredBit without the mask at the denial's scope: as the owner, or
// through raw bits that carry requiredBit or Administrator. Otherwise it
// returns nil and the caller writes its existing generic 403, byte-identical
// to today's.
//
// channelID is the denial's scope; empty means server scope. At channel scope
// the raw bits are the resolver's own channel computation before its mask:
// the server-scope role aggregate, then that channel's overrides unless raw
// Administrator bypasses them. A dangerous grant that exists only as a channel
// ALLOW therefore earns this refusal at that channel and nowhere else, which
// is how such a member learns about enrollment (X18: mfa_restricted is server
// scope only). A channel outside serverID is denied by the resolver as a
// non-member, which no factor lifts, so it returns nil.
//
// requiredBit is the single permission bit the denied check asked for. A bit
// the mask never withholds returns nil before any statement runs: the mask
// cannot have caused that denial, so naming enrollment would be false. That
// branch depends only on the caller's constant, which is what makes it safe
// to call from a generic choke point such as a RequirePermission denial.
//
// Constant shape (A.7): for a dangerous requiredBit and a given scope the same
// statements run on every call, whatever the flag, the ownership, the
// membership or the enrollment, so the work done on a denial does not reveal
// whether the server enforces.
//
// q is the querier the denial came from: the caller's transaction when one is
// open, never a second pooled connection while a transaction holds locks. A
// read failure is a 500 *stepup.Error with Cause, for the caller's error
// writer to log; it never widens the denial into a grant.
//
// Call it only on a denial. It must not precede the caller's own permission
// check (invariant I7, see the mfaenforce package comment).
func EnrollmentDenial(ctx context.Context, q rowQuerier, serverID, channelID, actorID string, requiredBit Permission) *stepup.Error {
	if requiredBit&DangerousPermissions == 0 {
		return nil
	}

	s, err := readEnrollmentDenialState(ctx, q, serverID, channelID, actorID)
	if err != nil {
		return stepup.VerificationFailed(err)
	}
	wouldHold := s.isOwner || s.raw.Has(requiredBit)
	if s.enforcing && !s.enrolled && s.inScope && wouldHold {
		return stepup.EnrollmentRequired()
	}
	return nil
}

// enrollmentDenialState is everything EnrollmentDenial's verdict reads.
type enrollmentDenialState struct {
	enforcing bool
	isOwner   bool
	// inScope is false when a channel scope names a channel outside the server.
	inScope  bool
	raw      Permission
	enrolled bool
}

// readEnrollmentDenialState runs every statement unconditionally, on q, so the
// statement trace depends only on whether channelID is empty.
func readEnrollmentDenialState(ctx context.Context, q rowQuerier, serverID, channelID, actorID string) (enrollmentDenialState, error) {
	s := enrollmentDenialState{inScope: true}
	var err error
	if s.enforcing, s.isOwner, err = readServerOwnership(ctx, q, serverID, actorID); err != nil {
		return s, err
	}
	if s.raw, err = RawRolePermissions(ctx, q, serverID, actorID); err != nil {
		return s, fmt.Errorf("rbac: read raw role permissions: %w", err)
	}
	if channelID != "" {
		if s.raw, s.inScope, err = rawChannelPermissions(ctx, q, serverID, channelID, actorID, s.raw); err != nil {
			return s, err
		}
	}
	methods, err := stepup.InlineMFAMethods(ctx, q, actorID)
	if err != nil {
		return s, fmt.Errorf("rbac: read MFA enrollment: %w", err)
	}
	s.enrolled = len(methods) > 0
	return s, nil
}

// readServerOwnership reads the enforcement flag and whether actorID owns the
// server. A server that no longer exists reads as neither, after issuing the
// same statement a live one does.
func readServerOwnership(ctx context.Context, q rowQuerier, serverID, actorID string) (enforcing, isOwner bool, err error) {
	var ownerID string
	err = q.QueryRowContext(ctx, serverOwnerQuery, serverID).Scan(&ownerID, &enforcing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("rbac: read server owner: %w", err)
	}
	return enforcing, ownerID == actorID, nil
}

// rawChannelPermissions is the channel half of ResolveChannelPermissionsTx
// before its mask: the overrides apply to base unless raw Administrator
// bypasses them. Both statements run whatever base holds and whether or not
// the channel is in the server.
func rawChannelPermissions(
	ctx context.Context, q rowQuerier, serverID, channelID, actorID string, base Permission,
) (Permission, bool, error) {
	inScope := true
	if err := requireChannelsInServer(ctx, q, serverID, []string{channelID}); errors.Is(err, ErrChannelNotInServer) {
		inScope = false
	} else if err != nil {
		return 0, false, err
	}
	overrides, err := readChannelOverrides(ctx, q, serverID, actorID, channelID)
	if err != nil {
		return 0, false, err
	}
	if base.Has(PermAdministrator) {
		return base, inScope, nil
	}
	return overrides.apply(base), inScope, nil
}
