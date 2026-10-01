package auth_test

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/stretchr/testify/assert"
)

func TestEffectivePasswordLoginDisabled_DeploymentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, mode   string
		stored, want bool
	}{
		{name: "self hosted stored disabled", mode: "self-hosted", stored: true, want: false},
		{name: "self hosted stored enabled", mode: "self-hosted", stored: false, want: false},
		{name: "case normalization", mode: "SELF-HOSTED", stored: true, want: false},
		{name: "whitespace normalization", mode: "  self-hosted  ", stored: true, want: false},
		{name: "SaaS stored disabled", mode: "saas", stored: true, want: true},
		{name: "SaaS stored enabled", mode: "saas", stored: false, want: false},
		{name: "blank keeps stored disabled", stored: true, want: true},
		{name: "blank keeps stored enabled", stored: false, want: false},
		{name: "unknown keeps stored disabled", mode: "unrecognized", stored: true, want: true},
		{name: "unknown keeps stored enabled", mode: "unrecognized", stored: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, auth.EffectivePasswordLoginDisabled(tc.mode, tc.stored))
		})
	}
}
