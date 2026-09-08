package middleware

import (
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/gin-gonic/gin"
)

const (
	nightwatchVerdictKey = "nightwatch_verdict"
	nightwatchHandledKey = "nightwatch_handled"
)

// NightwatchVerdict is a closed middleware/control outcome for the observer.
type NightwatchVerdict struct {
	EventType  securityevent.EventType
	Outcome    securityevent.Outcome
	Severity   securityevent.Severity
	Reason     securityevent.ReasonCode
	AuthMethod securityevent.AuthMethod
}

// MarkNightwatchVerdict stores one closed middleware/control outcome.
func MarkNightwatchVerdict(c *gin.Context, verdict NightwatchVerdict) {
	c.Set(nightwatchVerdictKey, verdict)
}

// NightwatchVerdictFromContext returns the current closed middleware verdict.
func NightwatchVerdictFromContext(c *gin.Context) (NightwatchVerdict, bool) {
	verdict, ok := c.Get(nightwatchVerdictKey)
	value, typed := verdict.(NightwatchVerdict)
	return value, ok && typed
}

// MarkNightwatchHandled suppresses only the observer's coarse route fallback.
func MarkNightwatchHandled(c *gin.Context) { c.Set(nightwatchHandledKey, true) }

// NightwatchHandled reports whether a domain handler already emitted its decision.
func NightwatchHandled(c *gin.Context) bool {
	handled, ok := c.Get(nightwatchHandledKey)
	value, typed := handled.(bool)
	return ok && typed && value
}
