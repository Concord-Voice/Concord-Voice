package entitlements

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/redis/go-redis/v9"
)

// HTTPHandler serves GET /api/v1/entitlements. It owns its own read-through
// Cache (NOT borrowed from auth.Handler — internal/auth is a protected path).
type HTTPHandler struct {
	cache *Cache
	log   *logger.Logger
}

// NewHTTPHandler builds the handler from the shared DB + Redis handles.
func NewHTTPHandler(db *sql.DB, redisClient *redis.Client, log *logger.Logger) *HTTPHandler {
	return &HTTPHandler{cache: NewCache(redisClient, db), log: log}
}

// NewHTTPHandlerForInstance builds the handler with the deployment-mode seam.
func NewHTTPHandlerForInstance(db *sql.DB, redisClient *redis.Client, log *logger.Logger, instanceType string) *HTTPHandler {
	return &HTTPHandler{cache: NewCacheForInstance(redisClient, db, instanceType), log: log}
}

// Get returns the acting user's entitlement capability set as JSON. The tier
// read fails closed to free on any cache/DB error (Cache.GetTier never errors),
// so this endpoint always returns 200 with a valid set — never a 500 that would
// leave the client in an unknown gating state.
func (h *HTTPHandler) Get(c *gin.Context) {
	userID := c.GetString("user_id")
	tier := h.cache.GetTier(c.Request.Context(), userID)
	c.JSON(http.StatusOK, ToDTO(For(tier)))
}
