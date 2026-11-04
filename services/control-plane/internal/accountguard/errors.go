package accountguard

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"net/http"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

var (
	// ErrActivated means the activation marker (accountguard_state.activated_at) is set. A
	// binary built before PR7a refuses every runner write while it is (spec 2-B): the marker
	// is read under the users lock, before any policy runs, and the pass rolls back.
	ErrActivated = errors.New("accountguard: activated")
	// ErrLockConflict means a lock wait ended in a deadlock (40P01) or ran past the runner's
	// lock_timeout (55P03), or pass 2's wait for the presence sender gate ran past
	// decide.LockTimeout. Run wraps such a failure with it, so a caller (the sweeper skips
	// on it) need not read SQLSTATEs. The transaction rolled back, or never opened.
	ErrLockConflict = errors.New("accountguard: lock conflict")
)

// The response codes. Each is a fixed string: a body never carries an id, a class, a version
// or a driver message (A-16, observability.md).
const (
	codeLockConflict   = "lock_conflict"
	codeGuardInactive  = "guard_unavailable"
	codeKeyUnavailable = "key_unavailable"
	codeUnavailable    = "service_unavailable"
	codePresence       = "presence_unavailable"
	codeAuthRequired   = "authentication_required"
	codeTokenInvalid   = "guard_token_invalid"
	codeChallenge      = "challenge_invalid"
	codeEffectUnwired  = "guard_effect_unwired"
	codeStale          = "guard_stale"
	codeSourceRefused  = "guard_source_refused"
	codeContract       = "guard_contract"
	codeInternal       = "internal_error"
)

// statusClasses maps the package's sentinels, the loginfamily classes the runner passes
// through, a lock conflict, a presence readiness refusal, a step-up subject 503 and a
// connection-class driver failure to a status and a code. The order is the precedence
// (ruling 3): every 503 class, then every 401, then every 500, so an error wrapping causes of
// two classes maps by the first that matches. A 503 wins because the request may succeed when
// retried, and saying 401 to it would end a session the server never judged. A lock conflict
// leads, because a lock timeout reaches the runner wrapped in loginfamily's ErrUnavailable and
// must still answer lock_conflict.
//
// One outage, one status (final review M2): a broken connection is the outage loginfamily
// already answers 503 for at the barrier, so it is a 503 wherever in the chain it sits, and not
// a contract break. It is the last 503 class, so every earlier 503 (the lock conflict,
// ErrActivated, key unavailable, loginfamily unavailable, a presence or step-up 503) keeps its
// own code, and it yields to the package's own contract sentinels (connectionOutage), so it
// never turns a pass the runner judged broken into a retry.
//
// ErrStale is a 500 and not a 409: every row it fences is locked before Decide runs, so under
// the runner a stale write is a contract break, not a race.
var statusClasses = [...]struct {
	match  func(error) bool
	status int
	code   string
}{
	{isLockConflict, http.StatusServiceUnavailable, codeLockConflict},
	{is(ErrActivated), http.StatusServiceUnavailable, codeGuardInactive},
	{is(ErrKeyUnavailable), http.StatusServiceUnavailable, codeKeyUnavailable},
	{is(loginfamily.ErrUnavailable), http.StatusServiceUnavailable, codeUnavailable},
	{presenceRetryable, http.StatusServiceUnavailable, codePresence},
	{subjectRetryable, http.StatusServiceUnavailable, codeUnavailable},
	{connectionOutage, http.StatusServiceUnavailable, codeUnavailable},
	{is(loginfamily.ErrDenied), http.StatusUnauthorized, codeAuthRequired},
	{is(ErrTokenInvalid), http.StatusUnauthorized, codeTokenInvalid},
	{is(ErrChallengeInvalid), http.StatusUnauthorized, codeChallenge},
	{is(ErrEffectUnwired), http.StatusInternalServerError, codeEffectUnwired},
	{is(ErrStale), http.StatusInternalServerError, codeStale},
	{is(ErrSourceRefused), http.StatusInternalServerError, codeSourceRefused},
	{is(ErrContract), http.StatusInternalServerError, codeContract},
	{is(loginfamily.ErrContract), http.StatusInternalServerError, codeContract},
}

// HTTPStatus maps an error from Run to the status and fixed code a route answers with (R5).
// Anything unrecognized, nil included, is a 500: no input maps to success. That includes a
// presence refusal of any status but 503: the runner asks the presence service only to begin
// a forced clear, so such a refusal is a contract break, and its code is never passed through.
func HTTPStatus(err error) (status int, code string) {
	for _, c := range statusClasses {
		if c.match(err) {
			return c.status, c.code
		}
	}
	return http.StatusInternalServerError, codeInternal
}

// is matches target anywhere in an error's chain.
func is(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// presenceRetryable matches a presence service refusal with Status 503: the forced clear's
// readiness gate (delivery unbound, a pending operation, the sender not ready) said try again
// (ruling 18). Its own code is never passed through.
func presenceRetryable(err error) bool {
	var se *presencehistory.ServiceError
	return errors.As(err, &se) && se.Status == http.StatusServiceUnavailable
}

// subjectRetryable matches a step-up subject refusal with Status 503: stepup said try again.
// Under the runner subjectError passes every refusal but a 401 through, and only this one is
// retryable; a refusal of any other status is a 500, its body never passed through.
func subjectRetryable(err error) bool {
	var se *stepup.Error
	return errors.As(err, &se) && se.Status == http.StatusServiceUnavailable
}

// connectionOutage matches a connection-class failure (isConnectionFailure) anywhere in the
// chain, unless the chain also carries one of the package's contract sentinels: those are the
// runner's own verdict that the pass broke, and a cause beneath them does not soften it.
func connectionOutage(err error) bool {
	return isConnectionFailure(err) && !isPackageContract(err)
}

// isPackageContract reports whether err carries one of the package's 500 sentinels.
func isPackageContract(err error) bool {
	for _, target := range [...]error{ErrEffectUnwired, ErrStale, ErrSourceRefused, ErrContract} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// isConnectionFailure reports whether err says a connection (database or Redis) broke or timed
// out at the socket, or the server would not serve it, so no statement ran to a verdict and a
// retry may succeed:
// driver.ErrBadConn, sql.ErrConnDone, SQLSTATE class 08 (connection exception), 57P01, 57P02
// or 57P03 (admin shutdown, crash shutdown, cannot connect now), or a net.Error.
//
// A context error is not one: the deadline or the cancel is the caller's own, and keeps its
// mapping (ruling C2). context.DeadlineExceeded satisfies net.Error, as does a net error that
// wraps it, so the context check must come before the net.Error check; moved after it, every
// caller's deadline would read as an outage.
func isConnectionFailure(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	if code, ok := sqlState(err); ok && isConnectionSQLState(code) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// isConnectionSQLState reports whether code is class 08 or one of the three shutdown codes.
// A SQLSTATE is five characters; the length check keeps Class from slicing a short one.
func isConnectionSQLState(code pqerror.Code) bool {
	switch code {
	case pqerror.AdminShutdown, pqerror.CrashShutdown, pqerror.CannotConnectNow:
		return true
	}
	return len(code) >= 2 && code.Class() == pqerror.ClassConnectionException
}

// isLockConflict reports whether err is ErrLockConflict or carries SQLSTATE 40P01 or 55P03.
func isLockConflict(err error) bool {
	if errors.Is(err, ErrLockConflict) {
		return true
	}
	code, ok := sqlState(err)
	return ok && (code == pqerror.TRDeadlockDetected || code == pqerror.LockNotAvailable)
}

// sqlState reads the SQLSTATE err carries, as the driver's error or as the dbFault an applier
// reduced it to (redactDBError).
func sqlState(err error) (pqerror.Code, bool) {
	var pqErr *pq.Error
	var fault *dbFault
	switch {
	case errors.As(err, &pqErr):
		return pqErr.Code, true
	case errors.As(err, &fault):
		return fault.code, true
	default:
		return "", false
	}
}
