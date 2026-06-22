// Package redemption implements the universal redemption-code engine (#1303):
// the generic /redeem flow, an extensible grant-effect catalog, issuer code
// generation (CLI + admin HTTP), and batch CSV export.
//
// Security model (design spec §1): codes are full-entropy random values (the
// "random-opaque + registry" model). The code carries NO meaning; the
// redemption_codes row holds all grant semantics. There is no signing key
// anywhere — a valid code is one that already exists in the registry, so the
// security requirement collapses from "protect an algorithm/key" to "protect
// write-access to the registry" (issuer authz + audit, see issuer.go). Only the
// SHA-256 hash is persisted; plaintext is returned exactly once at issue time.
package redemption

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Crockford Base32 alphabet (RFC-free; drops the ambiguous I, L, O, U). Used
// for both the payload symbols and the single trailing checksum symbol. This is
// a public encoding alphabet, not a secret.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" //nolint:gosec // public Base32 alphabet, not a credential // pragma: allowlist secret

// Code geometry. payloadSymbols carries the entropy; one extra checksum symbol
// is appended. 25 Crockford symbols × 5 bits = 125 bits of entropy, comfortably
// above the spec's ≥128-bit target once combined with the prefix's non-secret
// role (the spec counts the prefix as NON-entropy, so we size the payload to
// clear 128 bits on its own: 26 symbols × 5 = 130 bits). Grouped XXXXX-XXXXX-...
const (
	payloadSymbols = 26 // 26 × 5 = 130 bits of entropy (≥128 per spec §3)
	groupSize      = 5  // hyphen-grouped for legibility
)

// errBadChecksum is returned by NormalizeAndValidate when the checksum symbol
// does not match the payload — a typo or random probe. It is deliberately
// distinct from "code not found" so the engine can reject pre-DB (anti-
// enumeration) while the HTTP layer still collapses BOTH into one generic
// user-facing error (no oracle — see engine.go / handler.go).
var errBadChecksum = errors.New("redemption: checksum mismatch")

// generateRawCode draws payloadSymbols Crockford symbols from crypto/rand,
// appends a checksum symbol, and returns the ungrouped plaintext (no hyphens,
// no prefix). crypto/rand is the only entropy source — no counter, no userdata,
// no key (spec §3).
//
// The alphabet is exactly 32 symbols (a power of two), so each symbol is the
// low 5 bits of a CSPRNG byte (b & 0x1F). This is unbiased BY CONSTRUCTION — no
// rejection sampling needed because 256 is an exact multiple of 32; every byte's
// low 5 bits are uniform over [0,32). A panic-free read of one byte per symbol.
func generateRawCode() (string, error) {
	const mask = byte(0x1F) // 5 bits → [0,32), exactly len(crockfordAlphabet)
	buf := make([]byte, payloadSymbols)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, payloadSymbols)
	for i := 0; i < payloadSymbols; i++ {
		out[i] = crockfordAlphabet[buf[i]&mask]
	}
	payload := string(out)
	return payload + string(checksumSymbol(payload)), nil
}

// checksumSymbol computes the single Crockford check symbol for a payload as a
// sum of symbol values mod 32. This is a typo/probe pre-filter, NOT a security
// control (the security is the 130-bit entropy + registry lookup) — so a simple
// mod-32 sum is sufficient and intentionally not a keyed MAC.
func checksumSymbol(payload string) byte {
	sum := 0
	for i := 0; i < len(payload); i++ {
		sum += crockfordValue(payload[i])
	}
	return crockfordAlphabet[sum%len(crockfordAlphabet)]
}

// crockfordValue maps an (already upper-cased, de-aliased) Crockford symbol to
// its 0–31 value, or -1 if the rune is not a valid symbol.
func crockfordValue(c byte) int {
	idx := strings.IndexByte(crockfordAlphabet, c)
	return idx
}

// formatCode groups the raw code into hyphenated runs and prepends an optional
// non-secret prefix (e.g. "KS-"). The grouped form is what the issuer hands to
// the user; NormalizeAndValidate inverts it. Prefix is upper-cased and its own
// hyphen joined: "KS" + "ABCDE-FGHIJ-..." → "KS-ABCDE-FGHIJ-...".
func formatCode(raw, prefix string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i += groupSize {
		end := i + groupSize
		if end > len(raw) {
			end = len(raw)
		}
		if b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteString(raw[i:end])
	}
	grouped := b.String()
	if prefix == "" {
		return grouped
	}
	return strings.ToUpper(prefix) + "-" + grouped
}

// NormalizeAndValidate strips all formatting (hyphens, spaces, case, and any
// leading non-secret prefix), maps Crockford aliases (I/L→1, O→0), verifies the
// trailing checksum symbol, and returns the canonical ungrouped payload+checksum
// string ready for hashing. It rejects typos and random probes WITHOUT a DB hit
// (spec §4 step 1). A nil error means the checksum passed; the caller still must
// look the hash up in the registry (checksum-valid ≠ exists).
//
// Anti-enumeration note: a checksum failure returns errBadChecksum so the engine
// can short-circuit before touching the DB, but the HTTP layer MUST map this to
// the SAME generic "code not valid" response as a not-found code (no oracle).
func NormalizeAndValidate(input string) (string, error) {
	cleaned := normalizeSymbols(input)
	if len(cleaned) < 2 {
		return "", errBadChecksum
	}
	payload := cleaned[:len(cleaned)-1]
	check := cleaned[len(cleaned)-1]
	// Every payload symbol must be a valid Crockford value (normalizeSymbols
	// already dropped separators; a stray invalid rune leaves a -1 lookup).
	for i := 0; i < len(payload); i++ {
		if crockfordValue(payload[i]) < 0 {
			return "", errBadChecksum
		}
	}
	if crockfordValue(check) < 0 || checksumSymbol(payload) != check {
		return "", errBadChecksum
	}
	return cleaned, nil
}

// normalizeSymbols canonicalizes user input for hashing: trim + upper-case, drop
// separators (hyphen, space, underscore), fold Crockford aliases (I/L→1, O→0),
// then — because the optional non-secret prefix (KS-, PROMO-) has no fixed length
// — keep only the TRAILING payloadSymbols+1 symbols when the cleaned string is
// longer than canonical. The code itself is always exactly that trailing run, so
// any prefix is discarded uniformly. NormalizeAndValidate then checksum-validates
// the trailing run; a wrong prefix that happened to add symbols would shift the
// trailing window and fail the checksum (rejected pre-DB).
func normalizeSymbols(input string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(input)) {
		switch r {
		case '-', ' ', '_':
			continue
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	canonical := payloadSymbols + 1
	if len(s) > canonical {
		// Trim the leading non-secret prefix run; the code is the trailing run.
		s = s[len(s)-canonical:]
	}
	return s
}

// HashCode returns the lowercase hex SHA-256 of the canonical (normalized)
// code string — the value stored in redemption_codes.code_hash and looked up
// at redeem time. Plain SHA-256 is correct here (NOT Argon2/bcrypt): these are
// full-entropy random codes, not low-entropy human secrets, so slow hashing /
// peppering would be cargo-cult (spec §3). Equality is enforced by the DB unique
// index on code_hash (a single deterministic lookup), so no separate constant-
// time compare is needed — there is no secret-dependent branch on the server.
func HashCode(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
