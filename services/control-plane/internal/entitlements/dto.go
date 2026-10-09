package entitlements

import (
	"encoding/json"
)

// EntitlementDTO is the wire shape of the capability set. It is the SINGLE
// definition of the JSON contract — both GET /entitlements and the
// entitlements_changed WS push serialize through ToDTO, so the wire shape
// cannot drift. Kept separate from Entitlement so the source-of-truth limit
// table (entitlements.go) stays free of transport tags. Calendar months are
// authoritative; the legacy seconds field keeps older desktop clients able
// to parse the full entitlement set during version overlap.
type EntitlementDTO struct {
	Tier                         string   `json:"tier"`
	AllowCustomScheme            bool     `json:"allowCustomScheme"`
	AllowedAudioTiers            []string `json:"allowedAudioTiers"`
	MinPtimeMs                   int      `json:"minPtimeMs"`
	AllowMusicMode               bool     `json:"allowMusicMode"`
	MaxAudioLastN                int      `json:"maxAudioLastN"`
	StreamMaxHeight              int      `json:"streamMaxHeight"`
	StreamMaxFps                 int      `json:"streamMaxFps"`
	StreamMaxPixelRate           int      `json:"streamMaxPixelRate"`
	StreamMaxBitrate             int      `json:"streamMaxBitrate"`
	CameraMaxHeight              int      `json:"cameraMaxHeight"`
	CameraMaxFps                 int      `json:"cameraMaxFps"`
	CameraMaxBitrate             int      `json:"cameraMaxBitrate"`
	MaxManualBitrateBps          int      `json:"maxManualBitrateBps"`
	MaxWebcamPublishers          int      `json:"maxWebcamPublishers"`
	MaxScreensharePublishers     int      `json:"maxScreensharePublishers"`
	MaxMessageChars              int      `json:"maxMessageChars"`
	MaxAttachmentBytes           int64    `json:"maxAttachmentBytes"`
	MaxAvatarBytes               int64    `json:"maxAvatarBytes"`
	MaxBannerBytes               int64    `json:"maxBannerBytes"`
	AllowAnimatedProfile         bool     `json:"allowAnimatedProfile"`
	UsernameChangeIntervalMonths int      `json:"usernameChangeIntervalMonths"`
	// Deprecated: older clients require this field but use the profile's
	// server-computed eligible_at for the actual username cooldown.
	UsernameChangeIntervalSeconds int64 `json:"usernameChangeIntervalSeconds"`
	MaxServersCreated             int   `json:"maxServersCreated"`
	MessageHistorySearchDays      int   `json:"messageHistorySearchDays"`
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
		StreamMaxPixelRate:            e.StreamMaxPixelRate,
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
		UsernameChangeIntervalMonths:  e.UsernameChangeIntervalMonths,
		UsernameChangeIntervalSeconds: legacyUsernameIntervalSeconds(e.Tier),
		MaxServersCreated:             e.MaxServersCreated,
		MessageHistorySearchDays:      e.MessageHistorySearchDays,
	}
}

func legacyUsernameIntervalSeconds(tier string) int64 {
	if tier == TierPremium {
		return 91 * 24 * 60 * 60
	}
	return 365 * 24 * 60 * 60
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
