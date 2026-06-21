package auth

import (
	"fmt"
	"regexp"
	"strings"
)

// Username validation constants
const (
	MinUsernameLength = 3
	MaxUsernameLength = 32
)

// Username validation regex
// Allows: a-z, A-Z, 0-9, period, underscore, hyphen
// Must start and end with alphanumeric, no consecutive special chars
var (
	usernameRegex      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*[a-zA-Z0-9]$|^[a-zA-Z0-9]$`)
	consecutiveSpecial = regexp.MustCompile(`[._-]{2,}`)
)

// reservedWords are checked with substring matching to prevent variations like "admin123"
var reservedWords = []string{
	"admin", "administrator", "root", "system", "moderator", "mod",
	"support", "help", "concord", "concordvoice", "official",
	"api", "www", "mail", "smtp", "ftp", "noreply",
	"staff", "employee", "team", "service", "bot",
}

// profanityWords are checked with exact match only to avoid false positives
// (e.g., "ass" matching "classical", "cock" matching "peacock").
// Use a proper library for production.
var profanityWords = []string{
	"fuck", "shit", "damn", "bitch", "ass", "bastard",
	"asshole", "dick", "pussy", "cock", "cunt",
	"nigger", "nigga", "faggot", "retard", "tranny",
}

// Leetspeak substitution map for detecting variations
var leetMap = map[rune]rune{
	'0': 'o',
	'1': 'i',
	'3': 'e',
	'4': 'a',
	'5': 's',
	'7': 't',
	'8': 'b',
	'@': 'a',
	'$': 's',
}

// ValidateUsername performs comprehensive username validation
func ValidateUsername(username string) error {
	// Length check
	if len(username) < MinUsernameLength {
		return fmt.Errorf("username must be at least %d characters", MinUsernameLength)
	}
	if len(username) > MaxUsernameLength {
		return fmt.Errorf("username must be at most %d characters", MaxUsernameLength)
	}

	// Format check (alphanumeric plus periods, underscores, and hyphens)
	if !usernameRegex.MatchString(username) {
		return fmt.Errorf("username must start and end with a letter or number, and can only contain letters, numbers, periods, underscores, and hyphens")
	}

	// No consecutive special characters
	if consecutiveSpecial.MatchString(username) {
		return fmt.Errorf("username cannot contain consecutive special characters (periods, underscores, hyphens)")
	}

	// Check for blocked words
	if err := checkBlockedWords(username); err != nil {
		return err
	}

	return nil
}

// checkBlockedWords checks if username contains offensive or reserved words
func checkBlockedWords(username string) error {
	// Normalize username (lowercase)
	normalized := strings.ToLower(username)

	// Remove common separators to catch variations like "bad_word", "bad-word", or "b.a.d"
	stripped := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(normalized, ".", ""), "_", ""), "-", "")

	// Convert leetspeak to normal characters
	deLeeted := deLeetSpeak(stripped)

	// Check reserved words with substring matching (to catch "admin123", "sysadmin", etc.)
	for _, reserved := range reservedWords {
		if strings.Contains(stripped, reserved) || strings.Contains(deLeeted, reserved) {
			return fmt.Errorf("username is not available")
		}
	}

	// Check profanity with exact match only to avoid false positives
	// (e.g., "ass" matching "classical", "cock" matching "peacock")
	for _, profane := range profanityWords {
		if stripped == profane || deLeeted == profane {
			return fmt.Errorf("username is not available")
		}
	}

	return nil
}

// deLeetSpeak converts leetspeak characters to their normal equivalents
func deLeetSpeak(s string) string {
	var result strings.Builder
	for _, r := range s {
		if replacement, ok := leetMap[r]; ok {
			result.WriteRune(replacement)
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// NormalizeUsername converts username to lowercase for storage
// We store lowercase but preserve the user's preferred capitalization in display_name
func NormalizeUsername(username string) string {
	return strings.ToLower(username)
}
