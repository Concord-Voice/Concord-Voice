// Package klipy provides shared validation helpers and HTTP handlers for the
// KLIPY GIF integration. The validation helpers are imported by the messages
// REST handler and the WebSocket hub so the slug rules are defined in exactly
// one place.
package klipy

import "strings"

// MaxSlugLength is the maximum allowed length of a KLIPY GIF slug.
// KLIPY's slugs are derived from GIF titles and tend to stay well under this,
// but we leave headroom for unusually long slugs without inviting abuse.
const MaxSlugLength = 100

// ValidateSlug returns true if the given slug is a valid KLIPY GIF slug.
// Valid slugs are 1..MaxSlugLength characters of [a-zA-Z0-9-] only.
// A nil pointer or empty string is treated as "no slug attached" and returns true.
func ValidateSlug(slug *string) bool {
	if slug == nil || *slug == "" {
		return true
	}
	s := *slug
	if len(s) > MaxSlugLength {
		return false
	}
	for _, ch := range s {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}

// SlugValidationError returns the standard error message for an invalid slug.
// Used by handlers that need to surface a 400 to the client. Kept as a function
// rather than a const so callers can build it once and pass to gin.H.
func SlugValidationError(slug *string) string {
	if slug == nil {
		return ""
	}
	if len(*slug) > MaxSlugLength {
		return "gif_slug exceeds maximum length"
	}
	return "gif_slug contains invalid characters"
}

// NormalizeSlug returns nil if the input pointer is nil or refers to an
// empty/whitespace-only string, otherwise it returns a pointer to the
// trimmed value. Use this on REST request bodies before persisting so
// `"gif_slug": ""` and `"gif_slug": null` both round-trip to a NULL DB
// column ("no GIF attached") instead of an empty-string column.
func NormalizeSlug(slug *string) *string {
	if slug == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*slug)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
