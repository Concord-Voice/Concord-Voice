package expiration

import (
	"errors"
	"net/http"
)

// StartErrorStatus maps shared policy-service errors to their public status.
func StartErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrInvalidWindow):
		return http.StatusBadRequest
	case errors.Is(err, ErrScopeNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrBackfillPending):
		return http.StatusConflict
	case errors.Is(err, ErrBackfillNotFound), errors.Is(err, ErrRevisionMismatch):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// NormalizeResume preserves an authoritative resumed policy unless the service
// could not read one, in which case the accepted start policy remains pending.
func NormalizeResume(start, resumed Policy, resumeErr error) (Policy, int) {
	if resumeErr == nil {
		return resumed, http.StatusOK
	}
	if resumed.UpdatedAt == nil {
		resumed = start
		resumed.BackfillPending = true
	}
	if errors.Is(resumeErr, ErrBackfillPending) {
		return resumed, http.StatusServiceUnavailable
	}
	if errors.Is(resumeErr, ErrBackfillNotFound) || errors.Is(resumeErr, ErrRevisionMismatch) {
		return resumed, http.StatusConflict
	}
	return resumed, http.StatusServiceUnavailable
}
