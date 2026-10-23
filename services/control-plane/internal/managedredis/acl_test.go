package managedredis

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAbsentCommandInventoryDoesNotContradictGrantedACL(t *testing.T) {
	var inventory struct {
		ExcludedAbsentApplicationCommands []string `json:"excludedAbsentApplicationCommands"`
	}
	require.NoError(t, json.Unmarshal(aclManifestJSON, &inventory))
	granted := make(map[string]bool)
	for _, command := range checkedACLManifest.ConnectionCommands {
		granted[command] = true
	}
	for _, command := range checkedACLManifest.OptionalConnectionCommands {
		granted[command] = true
	}
	for _, selector := range checkedACLManifest.Services {
		for _, command := range selector.Commands {
			granted[command] = true
		}
	}
	for _, command := range checkedACLManifest.BluegreenOperator.Commands {
		granted[command] = true
	}
	for _, command := range inventory.ExcludedAbsentApplicationCommands {
		require.False(t, granted[command], "command %s cannot be both granted and absent", command)
	}
}

func TestMediaACLMatchesManifestAndKeepsOwnerDrainKeysReadOnly(t *testing.T) {
	selector := checkedACLManifest.Services["media-plane"]
	require.Contains(t, selector.Commands, "ACL|WHOAMI", "media must prove the authenticated Redis username before ACK")
	rule := serviceACLRule("media-plane")
	rootRule, scopedRule, hasSelector := strings.Cut(rule, "(")
	require.True(t, hasSelector, "media mixed Lua must get a dedicated Redis ACL selector")
	rootTokens := strings.Fields(rootRule)
	tokens := strings.Fields(strings.TrimSuffix(scopedRule, ")"))
	permissions := make(map[string]bool, len(tokens))
	rootPermissions := make(map[string]bool, len(rootTokens))
	for _, token := range rootTokens {
		rootPermissions[token] = true
	}
	for _, token := range tokens {
		permissions[token] = true
	}
	for _, command := range append(append([]string{}, checkedACLManifest.ConnectionCommands...), checkedACLManifest.OptionalConnectionCommands...) {
		require.True(t, rootPermissions["+"+strings.ToLower(command)])
	}
	for _, command := range selector.Commands {
		if command == "EVAL" {
			continue // EVAL is granted only by the paired key selector below.
		}
		require.True(t, rootPermissions["+"+strings.ToLower(command)], "manifest command %q must be emitted", command)
	}
	require.False(t, rootPermissions["+eval"], "EVAL must not have an unpaired root grant")
	require.Equal(t, "+eval", tokens[0], "the extra selector must grant only EVAL")
	require.Len(t, tokens, len(selector.EvalKeyPatterns)+1)
	for i, pattern := range selector.EvalKeyPatterns {
		require.Equal(t, "~"+pattern, tokens[i+1], "EVAL selector key patterns must match the manifest")
	}
	for _, pattern := range selector.KeyPatterns {
		if contains(selector.ReadOnlyKeyPatterns, pattern) {
			require.True(t, rootPermissions["%R~"+pattern], "read-only manifest key %q must use Redis read-only pattern syntax", pattern)
			require.False(t, rootPermissions["~"+pattern], "read-only key %q must not be writable", pattern)
			continue
		}
		require.True(t, rootPermissions["~"+pattern], "writable manifest key %q must be emitted", pattern)
	}
	for _, excluded := range []string{"+keys", "+time", "+config|get", "+publish", "+subscribe", "+hdel", "+hincrby", "+script|load", "+discard", "+select"} {
		require.False(t, rootPermissions[excluded], "excluded command %q must not be emitted", excluded)
	}
	for _, readOnly := range []string{"voice:room_owner:*", "media:colour_draining:blue", "media:colour_draining:green"} {
		require.Contains(t, selector.ReadOnlyKeyPatterns, readOnly)
		require.Contains(t, rule, "%R~"+readOnly)
		require.False(t, rootPermissions["~"+readOnly])
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
