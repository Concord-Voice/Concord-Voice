package stepup

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

// ErrMsgStepUpTokenInvalid is the 403 body for a step_up_token that matched no
// live row: spent, expired, minted for another route, or stranded by a
// credential change. The refusal also carries password_required and
// step_up_token_invalid, and the desktop routes on those flags, never on this
// text, to ask for the password again.
const ErrMsgStepUpTokenInvalid = "Your confirmation expired. Enter your password again." // pragma: allowlist secret

// VerifyOwnRuleTx is the step-up a member's own require_auth_before_purge
// setting asks for: the account's inline MFA when it has any, and its password
// otherwise. It is the one seam every own-rule route shares — DM Clear, and
// #3455's deletes and self-purges when the server rule does not govern — so a
// member meets the same factor for the same setting wherever it applies.
//
// The password never reaches the route (#3509). The member proves it at
// POST /api/v1/auth/step-up/password, which mints a single-use token bound to
// the route's purpose, and the route spends that token here, on tx, so a
// rollback restores it.
//
// An MFA account is never offered the password: a present token does not
// substitute for a missing code. v is only consulted on the MFA path, and a
// nil v there is a wiring fault reported as a 500, never a skipped check.
// route binds both factors to the route's purpose (#3467) and supplies its
// password-path copy.
func VerifyOwnRuleTx(
	ctx context.Context, tx *sql.Tx, v MFATxCodeVerifier, userID string, route OwnRuleRoute,
	in Input, subj Subject,
) *Error {
	if subj.MFAEnabled {
		if v == nil {
			return &Error{Status: http.StatusInternalServerError, Body: gin.H{"error": ErrMsgVerificationFailed}}
		}
		return VerifyMFAFactorTx(ctx, tx, v, userID, route.Purpose, in.MFACode, subj.MFAMethods)
	}
	return spendPasswordTokenTx(ctx, tx, userID, route, in.StepUpToken, subj)
}

// spendPasswordTokenTx is VerifyOwnRuleTx's password arm, for an account with
// no inline MFA. In order, mirroring VerifyPasswordFactor:
//
//  1. No usable password factor: the actionable 400 NoFactors, flagged
//     step_up_unavailable (noFactorsError). Evaluated
//     first, because an empty stored hash is not an actor who sent nothing.
//  2. No token: 403 CredentialRequired with password_required, the prompt.
//  3. A token that matches no live row: 403 ErrMsgStepUpTokenInvalid with
//     password_required and step_up_token_invalid, and nothing consumed.
//  4. A database fault: 500 with Cause.
func spendPasswordTokenTx(
	ctx context.Context, tx *sql.Tx, userID string, route OwnRuleRoute, token string, subj Subject,
) *Error {
	if subj.PasswordHash == "" {
		return noFactorsError(route.Copy.NoFactors)
	}
	if token == "" {
		return &Error{Status: http.StatusForbidden, Body: gin.H{
			"error": route.Copy.CredentialRequired, "password_required": true,
		}}
	}
	spent, err := SpendToken(ctx, tx, userID, FactorPassword, route.Purpose, token)
	if err != nil {
		return verificationFailed(err)
	}
	if !spent {
		return &Error{Status: http.StatusForbidden, Body: gin.H{
			"error": ErrMsgStepUpTokenInvalid, "password_required": true, "step_up_token_invalid": true,
		}}
	}
	return nil
}

// noFactorsError is an own-rule route's NoFactors 400, from the route seam and
// from the password mint alike: the account has neither a password nor an
// inline MFA method, so no step-up can confirm the action. step_up_unavailable
// is what tells it apart from a malformed body's 400, which has the same
// status and only an error string, so a client never offers a password
// prompt that cannot succeed (review of #3509).
func noFactorsError(text string) *Error {
	return &Error{Status: http.StatusBadRequest, Body: gin.H{"error": text, "step_up_unavailable": true}}
}

// The own-rule routes' password-path copy. It lives here rather than with the
// routes because the password mint answers an account with no password factor
// with the same NoFactors copy the route it was minted for answers with
// (#3509 review, security L5); OwnRuleCopy is the one lookup both use.
var (
	// clearOwnRuleCopy is DM Clear's. It may name the setting: Clear is the
	// setting's own action, and turning it off is a remedy Clear can offer.
	clearOwnRuleCopy = Copy{
		NoFactors:          "Clear history requires verification, but this account has no password and no MFA method. Set a password, enable MFA, or turn off \"Require authentication before purging\" in Privacy & Security.",
		CredentialRequired: "Current password required to clear history",
	}
	// dmMessageDeleteOwnRuleCopy is a DM message delete's past the soft-lock.
	// Neither string names require_auth_before_purge (H3): the refusal is
	// about the delete rate, and turning the setting off is not its remedy.
	dmMessageDeleteOwnRuleCopy = Copy{
		NoFactors:          "Deleting more messages right now requires verification, but this account has no password and no MFA method. Set a password or enable MFA, or wait and try again.",
		CredentialRequired: "Current password required to keep deleting messages",
	}
	// softLockOwnRuleCopy is every channel and server soft-locked route's: the
	// channel delete and both self-purges. It names no setting and no server
	// state (H3).
	softLockOwnRuleCopy = Copy{
		NoFactors:          "Deleting messages this quickly needs verification, but this account has no password and no MFA method. Set a password or enable MFA to continue.",
		CredentialRequired: "Current password required to keep deleting messages",
	}
)

// OwnRuleCopy returns the password-path copy of the own-rule route p names. It
// returns the zero Copy for a purpose outside OwnRulePurposes, which no caller
// passes: the mint refuses such a purpose first.
func OwnRuleCopy(p Purpose) Copy {
	switch p {
	case PurposeDMClear:
		return clearOwnRuleCopy
	case PurposeDMMessageDelete:
		return dmMessageDeleteOwnRuleCopy
	case PurposeMessageDelete, PurposeChannelPurge, PurposeServerPurge:
		return softLockOwnRuleCopy
	default:
		return Copy{}
	}
}

// OwnRuleRoute is what a route binds into VerifyOwnRuleTx: the purpose both of
// its factors are spent under (#3467), and the copy its password path answers
// with.
type OwnRuleRoute struct {
	Purpose Purpose
	Copy    Copy
}
