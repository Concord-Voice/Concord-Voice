package entitlements

import "fmt"

// Cosmetic count-gates (#1522). The emoji/sticker/soundboard FEATURES are not
// built yet (only Unicode reactions exist); these helpers are the enforcement
// seam their future create/upload endpoints call — hard server-side reject at
// the boundary, since the real cost is storage. Wired here so the feature build
// slots in with no gate rewrite (the #1294 modularity contract).

// CosmeticKind names a server-scoped cosmetic asset class.
type CosmeticKind string

// The known cosmetic kinds, matching the ServerEntitlement count-gate fields.
const (
	CosmeticEmoji      CosmeticKind = "emoji"
	CosmeticSticker    CosmeticKind = "sticker"
	CosmeticSoundboard CosmeticKind = "soundboard"
)

// limitFor returns the tier's count limit for the kind, and whether the kind is
// known. Unknown kinds report false and gate closed.
func (e ServerEntitlement) limitFor(kind CosmeticKind) (int, bool) {
	switch kind {
	case CosmeticEmoji:
		return e.MaxServerCustomEmoji, true
	case CosmeticSticker:
		return e.MaxServerStickers, true
	case CosmeticSoundboard:
		return e.MaxServerSoundboards, true
	default:
		return 0, false
	}
}

// AllowsCosmetic reports whether one more cosmetic of the given kind may be
// created when currentCount already exist. Negative limits mean unlimited
// (selfhost). Unknown kinds fail closed.
func (e ServerEntitlement) AllowsCosmetic(kind CosmeticKind, currentCount int) bool {
	limit, ok := e.limitFor(kind)
	if !ok {
		return false
	}
	if limit < 0 {
		return true
	}
	return currentCount < limit
}

// ErrCosmeticCapReached is the typed rejection the future create handlers
// translate to HTTP 403 with a machine-readable code. PII-safe by construction:
// it carries the kind and limit only — never user content or identifiers.
type ErrCosmeticCapReached struct {
	Kind  CosmeticKind
	Limit int
}

func (e *ErrCosmeticCapReached) Error() string {
	return fmt.Sprintf("%s cap reached (limit %d)", e.Kind, e.Limit)
}

// CosmeticCapError builds the typed cap error for the tier + kind.
func CosmeticCapError(ent ServerEntitlement, kind CosmeticKind) error {
	limit, _ := ent.limitFor(kind)
	return &ErrCosmeticCapReached{Kind: kind, Limit: limit}
}
