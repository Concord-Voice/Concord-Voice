package loginfamily

import (
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

var (
	// ErrDenied marks a conclusive identity or family authority denial.
	ErrDenied = errors.New("loginfamily: denied")
	// ErrTargetNotFound marks indistinguishable missing or foreign target membership.
	ErrTargetNotFound = errors.New("loginfamily: target_not_found")
	// ErrConflict marks a proven immutable identity collision.
	ErrConflict = errors.New("loginfamily: conflict")
	// ErrUnavailable marks dependency failure or unknown commit outcome.
	ErrUnavailable = errors.New("loginfamily: unavailable")
	// ErrContract marks caller misuse or an ownership invariant failure.
	ErrContract = errors.New("loginfamily: contract")
)

// ErrorReason is a closed internal diagnostic reason that is never a wire response.
type ErrorReason string

// Closed failure reasons mapped to their sentinel class by NewError.
const (
	ReasonAccessIdentity    ErrorReason = "access_identity"
	ReasonAccessExpired     ErrorReason = "access_expired"
	ReasonEpochMismatch     ErrorReason = "epoch_mismatch"
	ReasonFamilyTerminal    ErrorReason = "family_terminal"
	ReasonPreserveUnknown   ErrorReason = "preserve_unknown"
	ReasonTargetUnknown     ErrorReason = "target_unknown"
	ReasonIdentityConflict  ErrorReason = "identity_conflict"
	ReasonSQLUnavailable    ErrorReason = "sql_unavailable"
	ReasonCommitUnknown     ErrorReason = "commit_unknown"
	ReasonCapabilityInvalid ErrorReason = "capability_invalid"
	ReasonRotateRepeated    ErrorReason = "rotate_repeated"
	ReasonOwnershipInvalid  ErrorReason = "ownership_invalid"
	ReasonArgumentInvalid   ErrorReason = "argument_invalid"
)

// Error exposes a sanitized class and reason while retaining its cause for control flow.
type Error struct {
	class  error
	reason ErrorReason
	cause  error
}

// Error returns fixed classification text without the underlying cause.
func (e *Error) Error() string { return e.class.Error() + "/" + string(e.reason) }

// Is matches the failure's sentinel class.
func (e *Error) Is(target error) bool { return target == e.class }

// Unwrap retains the original cause; callers must not log it as public diagnostics.
func (e *Error) Unwrap() error { return e.cause }

// Reason returns the closed internal diagnostic reason.
func (e *Error) Reason() ErrorReason { return e.reason }

// NewError classifies a failure; unknown reasons become contract argument failures.
func NewError(reason ErrorReason, cause error) error {
	var class error
	switch reason {
	case ReasonAccessIdentity, ReasonAccessExpired, ReasonEpochMismatch, ReasonFamilyTerminal, ReasonPreserveUnknown:
		class = ErrDenied
	case ReasonTargetUnknown:
		class = ErrTargetNotFound
	case ReasonIdentityConflict:
		class = ErrConflict
	case ReasonSQLUnavailable, ReasonCommitUnknown:
		class = ErrUnavailable
	case ReasonCapabilityInvalid, ReasonRotateRepeated, ReasonOwnershipInvalid, ReasonArgumentInvalid:
		class = ErrContract
	default:
		class = ErrContract
		reason = ReasonArgumentInvalid
	}
	return &Error{class: class, reason: reason, cause: cause}
}
func sqlFailure(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return NewError(ReasonCapabilityInvalid, err)
	}
	var pg *pq.Error
	if errors.As(err, &pg) && pg.Code.Class() == "23" {
		return NewError(ReasonOwnershipInvalid, err)
	}
	return NewError(ReasonSQLUnavailable, err)
}
