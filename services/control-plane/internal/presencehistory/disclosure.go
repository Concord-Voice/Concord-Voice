package presencehistory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	disclosureVersion = 1

	saasOperatorName = "Concord Voice LLC"
	saasRequiredText = "Persistent activity history is stored on Concord servers. This data may be subject to legal subpoena. Disable to delete all history."
	saasPrivacyURL   = "https://concordvoice.com/privacy-policy"

	acknowledgementLabel = "I understand and consent to server-readable Activity History under the terms above."
)

var disclosureDetails = []string{
	"History includes your Custom Status text and emoji and is not end-to-end encrypted.",
	"Visibility tiers and recipient exceptions control sharing, but do not stop opted-in history from storing your status for you or make it unreadable to the server operator.",
	"History starts with your next Custom Status change; your current status and earlier activity are not added.",
	"At the retention cutoff, records become unavailable. Daily cleanup physically removes expired active-database rows, normally within 24 hours. Backup or legal-hold copies may persist when the operator is required to retain them.",
	"If the operator changes these terms, new recording pauses until you review them. Existing history remains available only until its retention cutoff unless you delete it sooner.",
}

// RequiredConsent is immutable server-owned disclosure copy presented to a
// user before Activity History can be enabled.
type RequiredConsent struct {
	Version              int16    `json:"version"`
	CopyHash             string   `json:"copy_hash"`
	OperatorName         string   `json:"operator_name"`
	RequiredText         string   `json:"required_text"`
	Details              []string `json:"details"`
	PrivacyPolicyURL     string   `json:"privacy_policy_url"`
	AcknowledgementLabel string   `json:"acknowledgement_label"`
}

// DisclosureState is unavailable, rather than partially configured, when a
// self-hosted operator cannot provide valid disclosure metadata.
type DisclosureState struct {
	Available       bool             `json:"available"`
	RequiredConsent *RequiredConsent `json:"required_consent,omitempty"`
}

// DisclosureOptions contains immutable startup configuration.
type DisclosureOptions struct {
	InstanceType     string
	OperatorName     string
	PrivacyPolicyURL string
	Development      bool
}

// BuildDisclosure returns an available SaaS disclosure or an operator-specific
// self-hosted disclosure. Invalid self-hosted configuration fails closed
// without returning a startup error.
func BuildDisclosure(options DisclosureOptions) DisclosureState {
	if !strings.EqualFold(strings.TrimSpace(options.InstanceType), "self-hosted") {
		return availableDisclosure(saasOperatorName, saasRequiredText, saasPrivacyURL)
	}

	operatorName := strings.TrimSpace(options.OperatorName)
	privacyURL := strings.TrimSpace(options.PrivacyPolicyURL)
	if operatorName == "" || strings.ContainsRune(operatorName, '\x00') ||
		strings.Contains(strings.ToLower(operatorName), strings.ToLower(saasOperatorName)) ||
		privacyURL != options.PrivacyPolicyURL || !validPrivacyPolicyURL(privacyURL, options.Development) {
		return DisclosureState{}
	}

	requiredText := fmt.Sprintf(
		"Persistent activity history is stored on servers operated by %s. This data may be subject to legal process served on %s. Disable to delete all history.",
		operatorName,
		operatorName,
	)
	return availableDisclosure(operatorName, requiredText, privacyURL)
}

func availableDisclosure(operatorName, requiredText, privacyURL string) DisclosureState {
	consent := &RequiredConsent{
		Version:              disclosureVersion,
		OperatorName:         operatorName,
		RequiredText:         requiredText,
		Details:              append([]string(nil), disclosureDetails...),
		PrivacyPolicyURL:     privacyURL,
		AcknowledgementLabel: acknowledgementLabel,
	}
	consent.CopyHash = disclosureHash(*consent)
	return DisclosureState{Available: true, RequiredConsent: consent}
}

func disclosureHash(consent RequiredConsent) string {
	fields := make([]string, 0, 3+len(consent.Details)+2)
	fields = append(fields,
		strconv.FormatInt(int64(consent.Version), 10),
		consent.OperatorName,
		consent.RequiredText,
	)
	fields = append(fields, consent.Details...)
	fields = append(fields, consent.PrivacyPolicyURL, consent.AcknowledgementLabel)
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:])
}

func validPrivacyPolicyURL(raw string, development bool) bool {
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Host == "" || parsed.Hostname() == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return development && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
