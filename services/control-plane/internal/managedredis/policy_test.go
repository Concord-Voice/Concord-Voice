package managedredis

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var policyTestNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func policyTestScope() SecretScope {
	return SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_AUTH_POLICY"}
}

func policyTestValue() Policy {
	scope := policyTestScope()
	return Policy{
		Version: 1, Scope: scope, RotationID: "rotation-12",
		Services: map[string]PolicyService{
			"control-plane": {TargetGeneration: 12, Generations: []PolicyGeneration{
				{Generation: 11, Username: "cv_cp_11", PasswordHash: "#0000000000000000000000000000000000000000000000000000000000000000", ValidFrom: policyTestNow.Add(-time.Hour), ValidUntil: policyTestNow.Add(4 * time.Minute)},
				{Generation: 12, Username: "cv_cp_12", PasswordHash: "#1111111111111111111111111111111111111111111111111111111111111111", ValidFrom: policyTestNow.Add(-time.Minute), ValidUntil: policyTestNow.Add(time.Hour)},
			}},
			"media-plane": {TargetGeneration: 12, Generations: []PolicyGeneration{
				{Generation: 11, Username: "cv_mp_11", PasswordHash: "#2222222222222222222222222222222222222222222222222222222222222222", ValidFrom: policyTestNow.Add(-time.Hour), ValidUntil: policyTestNow.Add(4 * time.Minute)},
				{Generation: 12, Username: "cv_mp_12", PasswordHash: "#3333333333333333333333333333333333333333333333333333333333333333", ValidFrom: policyTestNow.Add(-time.Minute), ValidUntil: policyTestNow.Add(time.Hour)},
			}},
		},
	}
}

func marshalPolicy(t *testing.T, policy Policy) []byte {
	t.Helper()
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	return raw
}

func TestParsePolicyAcceptsScopedTwoGenerationOverlap(t *testing.T) {
	scope := policyTestScope()
	got, err := ParsePolicy(marshalPolicy(t, policyTestValue()), scope, policyTestNow)

	require.NoError(t, err)
	require.Equal(t, "rotation-12", got.RotationID)
	require.Equal(t, uint64(12), got.Services["control-plane"].TargetGeneration)
	require.Len(t, got.Services["media-plane"].Generations, 2)
}

// Re-reading an in-flight rotation must keep parsing as its old generation
// runs out; refusing it was the rotation deadlock. The controller checks the
// overlap when it accepts a new policy instead.
func TestParsePolicyLeavesOverlapToTheController(t *testing.T) {
	policy := policyTestValue()
	for name, service := range policy.Services {
		service.Generations[0].ValidUntil = policyTestNow
		policy.Services[name] = service
	}
	_, err := ParsePolicy(marshalPolicy(t, policy), policyTestScope(), policyTestNow)
	require.NoError(t, err, "an expired old generation still parses")

	short := policyTestValue()
	service := short.Services[controlPlaneService]
	service.Generations[0].ValidUntil = policyTestNow.Add(time.Hour)
	service.Generations[1].ValidUntil = policyTestNow.Add(MinimumOverlap - time.Second)
	short.Services[controlPlaneService] = service
	_, err = ParsePolicy(marshalPolicy(t, short), policyTestScope(), policyTestNow)
	require.NoError(t, err)

	controller, _ := newControllerFixture(t, &fakeRedisAdmin{users: map[string]bool{}})
	require.NoError(t, controller.PrepareInitial(initialSameRotationPolicy()))
	require.ErrorIs(t, controller.Reconcile(context.Background(), short), errController,
		"a short-lived target cannot replace a longer-lived old user")
}

func TestParsePolicyRejectsGenerationBeyondMediaSafeInteger(t *testing.T) {
	policy := policyTestValue()
	service := policy.Services["media-plane"]
	service.Generations = []PolicyGeneration{service.Generations[1]}
	service.TargetGeneration = maxJSONSafeGeneration
	service.Generations[0].Generation = service.TargetGeneration
	service.Generations[0].Username = canonicalUsername(mediaPlaneService, service.TargetGeneration)
	policy.Services["media-plane"] = service
	_, err := ParsePolicy(marshalPolicy(t, policy), policyTestScope(), policyTestNow)
	require.NoError(t, err)
	service.TargetGeneration++
	service.Generations[0].Generation = service.TargetGeneration
	service.Generations[0].Username = canonicalUsername(mediaPlaneService, service.TargetGeneration)
	policy.Services["media-plane"] = service
	_, err = ParsePolicy(marshalPolicy(t, policy), policyTestScope(), policyTestNow)
	require.Error(t, err)
}

func TestParsePolicyRejectsDuplicateAndUnknownFields(t *testing.T) {
	scope := policyTestScope()
	raw := marshalPolicy(t, policyTestValue())
	var policy map[string]any
	require.NoError(t, json.Unmarshal(raw, &policy))
	policy["unexpected"] = true
	withUnknown, err := json.Marshal(policy)
	require.NoError(t, err)

	_, err = ParsePolicy(withUnknown, scope, policyTestNow)
	require.Error(t, err)

	duplicate := []byte(`{"version":1,"version":1}`)
	_, err = ParsePolicy(duplicate, scope, policyTestNow)
	require.Error(t, err)
}

func TestParsePolicyRejectsEveryScopeMismatch(t *testing.T) {
	want := policyTestScope()
	for _, tc := range []struct {
		name   string
		mutate func(*SecretScope)
	}{
		{name: "project", mutate: func(s *SecretScope) { s.Project = "other" }},
		{name: "environment", mutate: func(s *SecretScope) { s.Environment = "staging" }},
		{name: "path", mutate: func(s *SecretScope) { s.Path = "/other" }},
		{name: "name", mutate: func(s *SecretScope) { s.Name = "REDIS_ADMIN_BOOTSTRAP" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := policyTestValue()
			tc.mutate(&policy.Scope)
			_, err := ParsePolicy(marshalPolicy(t, policy), want, policyTestNow)
			require.Error(t, err)
		})
	}
}

func TestParsePolicyRejectsInvalidGenerationSets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Policy)
	}{
		{name: "more than two generations", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.Generations = append(s.Generations, s.Generations[1])
			p.Services["control-plane"] = s
		}},
		{name: "target missing", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.TargetGeneration = 13
			p.Services["control-plane"] = s
		}},
		{name: "cross-service username reuse", mutate: func(p *Policy) {
			s := p.Services["media-plane"]
			s.Generations[1].Username = "cv_cp_12"
			p.Services["media-plane"] = s
		}},
		{name: "wrong generation username", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.Generations[1].Username = "cv_cp_11"
			p.Services["control-plane"] = s
		}},
		{name: "arbitrary ACL-safe username", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.Generations[1].Username = "cv_controller_next"
			p.Services["control-plane"] = s
		}},
		{name: "ACL username injection", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.Generations[1].Username = "cv_cp_12 on +@all ~*"
			p.Services["control-plane"] = s
		}},
		{name: "reserved default username", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.Generations[1].Username = "default"
			p.Services["control-plane"] = s
		}},
		{name: "ACL hash injection", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.Generations[1].PasswordHash = "#1111111111111111111111111111111111111111111111111111111111111111 on +@all"
			p.Services["control-plane"] = s
		}},
		{name: "expired target", mutate: func(p *Policy) {
			s := p.Services["control-plane"]
			s.Generations[1].ValidUntil = policyTestNow
			p.Services["control-plane"] = s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := policyTestValue()
			tc.mutate(&policy)
			_, err := ParsePolicy(marshalPolicy(t, policy), policyTestScope(), policyTestNow)
			require.Error(t, err)
		})
	}
}

func TestParsePolicyAcceptsExactMinimumGenerationOverlap(t *testing.T) {
	policy := policyTestValue()
	for service, config := range policy.Services {
		config.Generations[0].ValidUntil = policyTestNow.Add(MinimumOverlap)
		policy.Services[service] = config
	}

	_, err := ParsePolicy(marshalPolicy(t, policy), policyTestScope(), policyTestNow)
	require.NoError(t, err)
}

func TestParseAdminBootstrapRequiresExactScopedValue(t *testing.T) {
	scope := SecretScope{Project: "project-test", Environment: "prod", Path: "/redis", Name: "REDIS_ADMIN_BOOTSTRAP"}
	good := []byte(`{"version":1,"scope":{"projectId":"project-test","environment":"prod","path":"/redis","name":"REDIS_ADMIN_BOOTSTRAP"},"username":"cv_admin","password":"fixture-password"}`)

	got, err := ParseAdminBootstrap(good, scope)
	require.NoError(t, err)
	require.Equal(t, "cv_admin", got.Username)

	var fields map[string]any
	require.NoError(t, json.Unmarshal(good, &fields))
	fields["extra"] = "value"
	bad, err := json.Marshal(fields)
	require.NoError(t, err)
	_, err = ParseAdminBootstrap(bad, scope)
	require.Error(t, err)

	var bootstrap AdminBootstrap
	require.NoError(t, json.Unmarshal(good, &bootstrap))
	bootstrap.Username = "default"
	bad = []byte(strings.Replace(string(good), `"cv_admin"`, `"default"`, 1))
	_, err = ParseAdminBootstrap(bad, scope)
	require.Error(t, err)

	wrongScope := scope
	wrongScope.Path = "/other"
	_, err = ParseAdminBootstrap(good, wrongScope)
	require.Error(t, err)

	for _, username := range []string{"cv_cp_999", "cv_mp_999"} {
		t.Run("reserved service namespace "+username, func(t *testing.T) {
			reserved := []byte(strings.Replace(string(good), `"cv_admin"`, `"`+username+`"`, 1))
			_, err := ParseAdminBootstrap(reserved, scope)
			require.Error(t, err)
		})
	}
}
