package email

import "strings"

// NormalizeAddress is the application's normalizer for a user's email address: white space
// trimmed from both ends by strings.TrimSpace, which trims every rune unicode.IsSpace reports
// (NBSP U+00A0 and IDEOGRAPHIC SPACE U+3000 among them), then strings.ToLower.
//
// Its callers are auth's registration (Register, ChangeRegistrationEmail, PendingRepo.UpdateEmail)
// and account-recovery (RecoveryBegin, the recovery verify parse) paths, and internal/accountguard,
// which applies it before PostgreSQL's lower(btrim(...)) fold (btrim trims only ASCII spaces) so
// its address lock key, address id and registration guard agree with what registration stores
// (#3680). Not every path uses it yet: password login compares the address as sent, SSO
// registration stores the provider's address as given, and PendingRepo.InsertOrTakeover only
// lower-cases. C5 moves those onto it.
//
// It validates nothing: the result may still not be an address.
func NormalizeAddress(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
