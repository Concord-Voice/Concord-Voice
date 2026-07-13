package presencehistory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxSettingsBodyBytes = 16 * 1024

// Handler exposes only authenticated-subject Activity History routes.
type Handler struct {
	service *Service
}

// NewHandler constructs an Activity History HTTP handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterRoutes mounts the fixed self-only route surface relative to the
// authenticated /users group, with distinct read and mutation limits.
func (h *Handler) RegisterRoutes(
	users *gin.RouterGroup,
	readLimiter gin.HandlerFunc,
	mutationLimiter gin.HandlerFunc,
) {
	history := users.Group("/me/presence-history")
	history.GET("/settings", readLimiter, h.getSettings)
	history.PATCH("/settings", mutationLimiter, h.updateSettings)
	history.GET("", readLimiter, h.list)
	history.DELETE("", mutationLimiter, h.disableAndDelete)
}

// DecodeUpdateSettingsRequest strictly decodes a presence-aware PATCH body.
func DecodeUpdateSettingsRequest(reader io.Reader) (UpdateSettingsRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxSettingsBodyBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxSettingsBodyBytes {
		return UpdateSettingsRequest{}, invalidRequestError()
	}
	fields, err := decodeExactJSONObject(raw,
		"enabled",
		"retention_days",
		"acknowledged",
		"consent_version",
		"consent_copy_hash",
	)
	if err != nil || len(fields) == 0 {
		return UpdateSettingsRequest{}, invalidRequestError()
	}

	var request UpdateSettingsRequest
	for name, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return UpdateSettingsRequest{}, invalidRequestError()
		}
		if err := decodeUpdateSettingsField(&request, name, value); err != nil {
			return UpdateSettingsRequest{}, err
		}
	}
	return request, nil
}

func decodeUpdateSettingsField(
	request *UpdateSettingsRequest,
	name string,
	value json.RawMessage,
) error {
	switch name {
	case "enabled":
		return decodeSettingsValue(value, &request.Enabled)
	case "retention_days":
		return decodeSettingsValue(value, &request.RetentionDays)
	case "acknowledged":
		return decodeSettingsValue(value, &request.Acknowledged)
	case "consent_version":
		return decodeSettingsValue(value, &request.ConsentVersion)
	case "consent_copy_hash":
		return decodeSettingsValue(value, &request.ConsentCopyHash)
	default:
		return invalidRequestError()
	}
}

func decodeSettingsValue[T any](raw json.RawMessage, target **T) error {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return invalidRequestError()
	}
	*target = &value
	return nil
}

func (h *Handler) getSettings(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		return
	}
	settings, err := h.service.GetSettings(c.Request.Context(), userID)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, settings)
}

func (h *Handler) updateSettings(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSettingsBodyBytes)
	request, err := DecodeUpdateSettingsRequest(c.Request.Body)
	if err != nil {
		writeServiceError(c, invalidRequestError())
		return
	}
	settings, err := h.service.UpdateSettings(c.Request.Context(), userID, request)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, settings)
}

func (h *Handler) list(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		return
	}
	options, err := parseListOptions(c)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	page, err := h.service.List(c.Request.Context(), userID, options)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, page)
}

func (h *Handler) disableAndDelete(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		return
	}
	if err := h.service.DisableAndDelete(c.Request.Context(), userID); err != nil {
		writeServiceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func authenticatedUserID(c *gin.Context) (uuid.UUID, bool) {
	raw, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "unauthorized"})
		return uuid.Nil, false
	}
	value, ok := raw.(string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "unauthorized"})
		return uuid.Nil, false
	}
	userID, err := uuid.Parse(value)
	if err != nil || userID == uuid.Nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "unauthorized"})
		return uuid.Nil, false
	}
	return userID, true
}

func parseListOptions(c *gin.Context) (ListOptions, error) {
	options := ListOptions{Limit: defaultPageLimit}
	if values, present := c.GetQueryArray("limit"); present {
		if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] {
			return ListOptions{}, invalidRequestError()
		}
		limit, err := strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > maxPageLimit {
			return ListOptions{}, invalidRequestError()
		}
		options.Limit = limit
	}
	if values, present := c.GetQueryArray("before"); present {
		if len(values) != 1 || values[0] == "" {
			return ListOptions{}, invalidRequestError()
		}
		cursor, err := DecodeCursor(values[0])
		if err != nil {
			return ListOptions{}, invalidRequestError()
		}
		options.Before = &cursor
	}
	return options, nil
}

func writeServiceError(c *gin.Context, err error) {
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "activity_history_internal_error"})
		return
	}
	if serviceErr.RetryAfter > 0 {
		seconds := int(serviceErr.RetryAfter.Seconds())
		c.Header("Retry-After", strconv.Itoa(seconds))
	}
	body := gin.H{"code": serviceErr.Code}
	if serviceErr.Disclosure != nil {
		body["required_consent"] = serviceErr.Disclosure
	}
	c.JSON(serviceErr.Status, body)
}
