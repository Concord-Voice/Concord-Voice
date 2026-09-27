package messages

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/stretchr/testify/assert"
)

func TestPurgeAuthorForPermissions_EnforcesVisibilityAndAuthorScope(t *testing.T) {
	foreign := "foreign-user"
	actor := "actor"
	tests := []struct {
		name      string
		perms     rbac.Permission
		channel   string
		target    *string
		want      *string
		wantAllow bool
	}{
		{name: "text manage all defaults to every author", perms: rbac.PermViewTextChannels | rbac.PermManageAllMessages, channel: "text", wantAllow: true},
		{name: "text manage all accepts requested author", perms: rbac.PermViewTextChannels | rbac.PermManageAllMessages, channel: "text", target: &foreign, want: &foreign, wantAllow: true},
		{name: "manage own defaults to actor", perms: rbac.PermViewTextChannels | rbac.PermManageOwnMessages, channel: "text", want: &actor, wantAllow: true},
		{name: "manage own cannot target another author", perms: rbac.PermViewTextChannels | rbac.PermManageOwnMessages, channel: "text", target: &foreign},
		{name: "voice uses voice visibility", perms: rbac.PermViewVoiceChannels | rbac.PermManageAllMessages, channel: "voice", wantAllow: true},
		{name: "missing visibility is denied", perms: rbac.PermManageAllMessages, channel: "text"},
		{name: "server scope accepts voice-only moderator", perms: rbac.PermViewVoiceChannels | rbac.PermManageAllMessages, wantAllow: true},
		{name: "server scope without visibility is denied", perms: rbac.PermManageAllMessages},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, allowed := purgeAuthorForPermissions(tt.perms, actor, tt.channel, tt.target)
			assert.Equal(t, tt.wantAllow, allowed)
			assert.Equal(t, tt.want, got)
		})
	}
}
