package managedredis

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinimumOverlap is the validity both generations must have left when the
// controller accepts a new two-generation policy.
const MinimumOverlap = 180 * time.Second

const maxJSONSafeGeneration = 1<<53 - 1

var aclHash = regexp.MustCompile(`^#[0-9a-fA-F]{64}$`)

// Policy describes the exact two-service ACL rotation.
type Policy struct {
	Version    int                      `json:"version"`
	Scope      SecretScope              `json:"scope"`
	RotationID string                   `json:"rotationId"`
	Services   map[string]PolicyService `json:"services"`
}

// PolicyService lists at most two generations for one service.
type PolicyService struct {
	TargetGeneration uint64             `json:"targetGeneration"`
	Generations      []PolicyGeneration `json:"generations"`
}

// PolicyGeneration binds a Redis username and hash to a validity window.
type PolicyGeneration struct {
	Generation   uint64    `json:"generation"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash"`
	ValidFrom    time.Time `json:"validFrom"`
	ValidUntil   time.Time `json:"validUntil"`
}

// AdminBootstrap is the protected controller-only Redis identity.
type AdminBootstrap struct {
	Version  int         `json:"version"`
	Scope    SecretScope `json:"scope"`
	Username string      `json:"username"`
	Password string      `json:"password"`
}

// ParsePolicy validates the bounded monotonic ACL policy. The target must be
// valid now; an old generation may be past its validUntil, so re-reading an
// in-flight rotation never refuses it. The controller checks MinimumOverlap
// only when it accepts a new policy, and it removes an expired old user itself.
func ParsePolicy(raw []byte, expectedScope SecretScope, now time.Time) (Policy, error) {
	var policy Policy
	if err := decodeExact(raw, &policy); err != nil || policy.Version != 1 ||
		policy.Scope != expectedScope || !validScope(expectedScope) ||
		!rotationID.MatchString(policy.RotationID) || len(policy.Services) != 2 {
		return Policy{}, errInvalid
	}
	seenUsers := make(map[string]struct{}, 4)
	for _, serviceName := range []string{"control-plane", "media-plane"} {
		service, ok := policy.Services[serviceName]
		if !ok || validatePolicyService(serviceName, service, now, seenUsers) != nil {
			return Policy{}, errInvalid
		}
	}
	return policy, nil
}

func canonicalUsername(service string, generation uint64) string {
	prefix := "cv_cp_"
	if service == mediaPlaneService {
		prefix = "cv_mp_"
	}
	return prefix + strconv.FormatUint(generation, 10)
}

func canonicalUsernameBefore(service string, maxGeneration uint64, username string) bool {
	prefix := "cv_cp_"
	if service == mediaPlaneService {
		prefix = "cv_mp_"
	}
	generation, err := strconv.ParseUint(strings.TrimPrefix(username, prefix), 10, 64)
	return err == nil && generation > 0 && generation <= maxGeneration && username == canonicalUsername(service, generation)
}

func reservedServiceUsername(username string) bool {
	return canonicalUsernameBefore(controlPlaneService, maxJSONSafeGeneration, username) ||
		canonicalUsernameBefore(mediaPlaneService, maxJSONSafeGeneration, username)
}

func validPolicyGeneration(generation PolicyGeneration, now time.Time, allowExpired bool) bool {
	return generation.Generation != 0 && generation.Generation <= maxJSONSafeGeneration && aclUsername.MatchString(generation.Username) && generation.Username != "default" &&
		aclHash.MatchString(generation.PasswordHash) && !generation.ValidFrom.IsZero() &&
		generation.ValidUntil.After(generation.ValidFrom) && !now.Before(generation.ValidFrom) && (allowExpired || now.Before(generation.ValidUntil))
}

func validatePolicyService(serviceName string, service PolicyService, now time.Time, seenUsers map[string]struct{}) error {
	if service.TargetGeneration == 0 || service.TargetGeneration > maxJSONSafeGeneration || len(service.Generations) == 0 || len(service.Generations) > 2 {
		return errInvalid
	}
	seenGenerations := make(map[uint64]struct{}, 2)
	targetPresent := false
	for _, generation := range service.Generations {
		if err := validateAndRecordGeneration(serviceName, generation, service.TargetGeneration, now, seenUsers, seenGenerations); err != nil {
			return err
		}
		targetPresent = targetPresent || generation.Generation == service.TargetGeneration
	}
	if !targetPresent {
		return errInvalid
	}
	return nil
}

func validateAndRecordGeneration(service string, generation PolicyGeneration, target uint64, now time.Time, seenUsers map[string]struct{}, seenGenerations map[uint64]struct{}) error {
	if generation.Generation > target || !validPolicyGeneration(generation, now, generation.Generation != target) ||
		generation.Username != canonicalUsername(service, generation.Generation) {
		return errInvalid
	}
	if _, duplicate := seenUsers[generation.Username]; duplicate {
		return errInvalid
	}
	if _, duplicate := seenGenerations[generation.Generation]; duplicate {
		return errInvalid
	}
	seenUsers[generation.Username] = struct{}{}
	seenGenerations[generation.Generation] = struct{}{}
	return nil
}

// ParseAdminBootstrap validates the scoped controller identity.
func ParseAdminBootstrap(raw []byte, expectedScope SecretScope) (AdminBootstrap, error) {
	var value AdminBootstrap
	if err := decodeExact(raw, &value); err != nil || value.Version != 1 ||
		value.Scope != expectedScope || !validScope(expectedScope) ||
		!aclUsername.MatchString(value.Username) || value.Username == "default" || reservedServiceUsername(value.Username) || value.Password == "" {
		return AdminBootstrap{}, errInvalid
	}
	return value, nil
}
