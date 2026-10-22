// Package managedredis enforces scoped Redis credentials and ACL rotation.
package managedredis

import (
	// embed is required by the acl-manifest.json directive below.
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed acl-manifest.json
var aclManifestJSON []byte

type aclSelector struct {
	Commands            []string `json:"commands"`
	KeyPatterns         []string `json:"keyPatterns"`
	ReadOnlyKeyPatterns []string `json:"readOnlyKeyPatterns"`
	EvalKeyPatterns     []string `json:"evalKeyPatterns"`
}

type aclManifest struct {
	ConnectionCommands         []string               `json:"connectionCommands"`
	OptionalConnectionCommands []string               `json:"optionalConnectionCommands"`
	Services                   map[string]aclSelector `json:"services"`
	BluegreenOperator          aclSelector            `json:"bluegreenOperator"`
}

var checkedACLManifest = func() aclManifest {
	var manifest aclManifest
	if err := json.Unmarshal(aclManifestJSON, &manifest); err != nil {
		panic("invalid checked Redis ACL manifest")
	}
	return manifest
}()

func aclRule(selector aclSelector) string {
	parts := make([]string, 0, len(checkedACLManifest.ConnectionCommands)+len(checkedACLManifest.OptionalConnectionCommands)+len(selector.Commands)+len(selector.KeyPatterns))
	for _, command := range checkedACLManifest.ConnectionCommands {
		parts = append(parts, "+"+strings.ToLower(command))
	}
	for _, command := range checkedACLManifest.OptionalConnectionCommands {
		parts = append(parts, "+"+strings.ToLower(command))
	}
	for _, command := range selector.Commands {
		if command == "EVAL" && len(selector.EvalKeyPatterns) != 0 {
			continue
		}
		parts = append(parts, "+"+strings.ToLower(command))
	}
	readOnly := make(map[string]struct{}, len(selector.ReadOnlyKeyPatterns))
	for _, key := range selector.ReadOnlyKeyPatterns {
		readOnly[key] = struct{}{}
	}
	for _, key := range selector.KeyPatterns {
		if _, limited := readOnly[key]; limited {
			continue
		}
		parts = append(parts, "~"+key)
	}
	for _, key := range selector.ReadOnlyKeyPatterns {
		parts = append(parts, "%R~"+key)
	}
	if len(selector.EvalKeyPatterns) != 0 {
		// Redis checks EVAL's declared KEYS as writable before Lua runs. A
		// separate command-only selector admits the mixed key vector while
		// redis.call still checks each nested GET/SET against the base grants.
		eval := make([]string, 0, 1+len(selector.EvalKeyPatterns))
		eval = append(eval, "+eval")
		for _, key := range selector.EvalKeyPatterns {
			eval = append(eval, "~"+key)
		}
		parts = append(parts, "("+strings.Join(eval, " ")+")")
	}
	return strings.Join(parts, " ")
}

func serviceACLRule(service string) string { return aclRule(checkedACLManifest.Services[service]) }

func operatorACLRule() string { return aclRule(checkedACLManifest.BluegreenOperator) }
