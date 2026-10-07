package rbac

import "context"

// ServerPermissionsWithMFARestriction returns userID's effective server-scope
// permissions in serverID and whether the server's MFA enforcement is
// withholding any of them right now (#3454, X7). It backs
// GET /servers/:id/permissions, whose mfa_restricted flag tells a member ahead
// of time that enrolling would restore something.
//
// Both answers come from ONE uncached read, so the flag can never disagree
// with the bitfield beside it. The permissions are exactly what
// GetEffectivePermissions returns for the server scope (the mask applied once,
// at this entry point's exit), and "withholding" is the mask's own definition
// rather than a second encoding of it: the mask changed the raw value. That
// holds exactly when the server enforces, the member has no inline factor, and
// the member is the owner or holds a dangerous bit or Administrator through a
// role.
//
// Server scope only (X18): a dangerous grant that exists only as a channel
// ALLOW does not set the flag; that member learns through the RS5 refusal
// instead. Both values are about the caller alone.
func (r *Resolver) ServerPermissionsWithMFARestriction(ctx context.Context, serverID, userID string) (Permission, bool, error) {
	// resolveServerPermissionsFresh already returns OwnerPermissions for the
	// owner and the raw role aggregate for everyone else, with the mask beside
	// it.
	raw, _, mask, err := r.resolveServerPermissionsFresh(ctx, serverID, userID)
	if err != nil {
		return 0, false, err
	}
	perms := mask.Apply(raw)
	return perms, perms != raw, nil
}
