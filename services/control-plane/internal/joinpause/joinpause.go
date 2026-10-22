// Package joinpause holds the durable voice-join pause used by the first
// legacy-to-colour media conversion. concord-ctl.sh owns the key; the control
// plane only reads it, on every voice admission, so no restart can lose it.
package joinpause

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// Key is written by `concord-ctl.sh media-legacy-pause` as
	// `<t0-unix>:<deadline-unix>[:<test-user-uuid>]`.
	Key = "media:legacy_join_pause"
	// Code is the stable refusal code clients match on.
	Code         = "voice_joins_paused"
	message      = "Voice is paused for maintenance"
	redisTimeout = time.Second
)

// lastDeadline is the deadline of the last active pause this process read,
// or 0. It lets a Redis blip during a pause keep refusing (see Refuse).
var lastDeadline atomic.Int64

var valuePattern = regexp.MustCompile(`^([0-9]{1,12}):([0-9]{1,12})(?::([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}))?$`)

// State is the parsed pause. A key that exists but does not parse is still a
// pause: the operator's intent was to stop joins, so Active stays true with a
// zero Deadline and no exception.
type State struct {
	Active    bool  `json:"active"`
	Deadline  int64 `json:"deadline"`
	allowUser string
}

// Read returns the current pause. A missing key is an inactive pause; any
// other Redis failure is returned to the caller.
func Read(ctx context.Context, rdb *redis.Client) (State, error) {
	if rdb == nil {
		return State{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	value, err := rdb.Get(ctx, Key).Result()
	if errors.Is(err, redis.Nil) {
		lastDeadline.Store(0)
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	state := parse(value)
	lastDeadline.Store(state.Deadline)
	return state, nil
}

func parse(value string) State {
	match := valuePattern.FindStringSubmatch(value)
	if match == nil {
		return State{Active: true}
	}
	// The pattern bounds the field to 12 digits, so it always fits an int64.
	deadline, _ := strconv.ParseInt(match[2], 10, 64)
	return State{Active: true, Deadline: deadline, allowUser: match[3]}
}

// Refuse writes the 503 and returns true when userID must not be admitted.
//
// A Redis read failure admits the join (logged), unless this process last
// saw a pause whose deadline has not passed: then it keeps refusing, so a
// blip mid-conversion cannot open a room on legacy. Outside a pause the voice
// routes gain no new outage dependency, matching their fail-open rate limiter.
func Refuse(c *gin.Context, rdb *redis.Client, log *logger.Logger, userID string) bool {
	state, err := Read(c.Request.Context(), rdb)
	if err != nil {
		if time.Now().Unix() < lastDeadline.Load() {
			return writeRefusal(c)
		}
		if log != nil {
			log.Warn("Voice join pause unreadable; admitting", "failure_class", "join_pause")
		}
		return false
	}
	if !state.Active || (state.allowUser != "" && state.allowUser == userID) {
		return false
	}
	return writeRefusal(c)
}

func writeRefusal(c *gin.Context) bool {
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": message, "code": Code})
	return true
}

// Handler reports the pause exactly as the admission handlers see it. It is
// mounted outside /api and /health, and every nginx config keeps it local;
// concord-ctl.sh reads it on 127.0.0.1:8080 to prove a pause before starting
// the clock.
func Handler(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		state, err := Read(c.Request.Context(), rdb)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "pause state unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"voice_join_pause": state})
	}
}
