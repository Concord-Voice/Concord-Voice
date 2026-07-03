package entitlements

import (
	"encoding/json"
	"time"
)

// EntitlementDTO is the wire shape of the capability set. It is the SINGLE
// definition of the JSON contract — both GET /entitlements and the
// entitlements_changed WS push serialize through ToDTO, so the wire shape
// cannot drift. Kept separate from Entitlement so the source-of-truth limit
// table (entitlements.go) stays free of transport tags, and so the
// time.Duration username interval is published as an explicit integer-seconds
// field rather than Go's default raw-nanosecond integer.
type EntitlementDTO struct {
	Tier                          string   `json:"tier"`
	AllowCustomScheme             bool     `json:"allowCustomScheme"`
	AllowedAudioTiers             []string `json:"allowedAudioTiers"`
	MinPtimeMs                    int      `json:"minPtimeMs"`
	AllowMusicMode                bool     `json:"allowMusicMode"`
	MaxAudioLastN                 int      `json:"maxAudioLastN"`
	StreamMaxHeight               int      `json:"streamMaxHeight"`
	StreamMaxFps                  int      `json:"streamMaxFps"`
	StreamMaxBitrate              int      `json:"streamMaxBitrate"`
	CameraMaxHeight               int      `json:"cameraMaxHeight"`
	CameraMaxFps                  int      `json:"cameraMaxFps"`
	CameraMaxBitrate              int      `json:"cameraMaxBitrate"`
	MaxManualBitrateBps           int      `json:"maxManualBitrateBps"`
	MaxWebcamPublishers           int      `json:"maxWebcamPublishers"`
	MaxScreensharePublishers      int      `json:"maxScreensharePublishers"`
	MaxMessageChars               int      `json:"maxMessageChars"`
	MaxAttachmentBytes            int64    `json:"maxAttachmentBytes"`
	MaxAvatarBytes                int64    `json:"maxAvatarBytes"`
	MaxBannerBytes                int64    `json:"maxBannerBytes"`
	AllowAnimatedProfile          bool     `json:"allowAnimatedProfile"`
	UsernameChangeIntervalSeconds int64    `json:"usernameChangeIntervalSeconds"`
	MaxServersCreated             int      `json:"maxServersCreated"`
	MessageHistorySearchDays      int      `json:"messageHistorySearchDays"`
}

// ToDTO maps the internal capability set to its wire shape. Pure (no I/O).
func ToDTO(e Entitlement) EntitlementDTO {
	return EntitlementDTO{
		Tier:                          e.Tier,
		AllowCustomScheme:             e.AllowCustomScheme,
		AllowedAudioTiers:             e.AllowedAudioTiers,
		MinPtimeMs:                    e.MinPtimeMs,
		AllowMusicMode:                e.AllowMusicMode,
		MaxAudioLastN:                 e.MaxAudioLastN,
		StreamMaxHeight:               e.StreamMaxHeight,
		StreamMaxFps:                  e.StreamMaxFps,
		StreamMaxBitrate:              e.StreamMaxBitrate,
		CameraMaxHeight:               e.CameraMaxHeight,
		CameraMaxFps:                  e.CameraMaxFps,
		CameraMaxBitrate:              e.CameraMaxBitrate,
		MaxManualBitrateBps:           e.MaxManualBitrateBps,
		MaxWebcamPublishers:           e.MaxWebcamPublishers,
		MaxScreensharePublishers:      e.MaxScreensharePublishers,
		MaxMessageChars:               e.MaxMessageChars,
		MaxAttachmentBytes:            e.MaxAttachmentBytes,
		MaxAvatarBytes:                e.MaxAvatarBytes,
		MaxBannerBytes:                e.MaxBannerBytes,
		AllowAnimatedProfile:          e.AllowAnimatedProfile,
		UsernameChangeIntervalSeconds: int64(e.UsernameChangeInterval / time.Second),
		MaxServersCreated:             e.MaxServersCreated,
		MessageHistorySearchDays:      e.MessageHistorySearchDays,
	}
}

// DTOToMap converts a DTO to the map[string]interface{} shape the WebSocket
// hub's OutgoingMessage.Data requires, via a JSON round-trip so the wire keys
// always equal the DTO's json tags (no hand-maintained key list to drift).
func DTOToMap(dto EntitlementDTO) (map[string]interface{}, error) {
	b, err := json.Marshal(dto)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
