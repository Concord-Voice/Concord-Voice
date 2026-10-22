package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/managedredis"
)

const lostStateMode = "recover-lost-state"

type prepareInputs struct {
	endpoint, project, environment, path, clientID string
	controlUID, mediaUID                           uint32
	aclGID                                         int
}

func readPrepareInputs() (prepareInputs, error) {
	var input prepareInputs
	var err error
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"REDIS_ENDPOINT", &input.endpoint},
		{"REDIS_INFISICAL_PROJECT_ID", &input.project},
		{"REDIS_INFISICAL_ENVIRONMENT", &input.environment},
		{"REDIS_INFISICAL_PATH", &input.path},
		{"REDIS_INFISICAL_CLIENT_ID", &input.clientID},
	} {
		*field.value, err = requiredEnv(field.name)
		if err != nil {
			return prepareInputs{}, errServe
		}
	}
	input.controlUID, err = requiredPeerUID("REDIS_CONTROL_UID")
	if err != nil {
		return prepareInputs{}, errServe
	}
	input.mediaUID, err = requiredPeerUID("REDIS_MEDIA_UID")
	if err != nil {
		return prepareInputs{}, errServe
	}
	input.aclGID, err = requiredUID("REDIS_ACL_GID")
	if err != nil {
		return prepareInputs{}, errServe
	}
	return input, nil
}

func readLostStateProof(mode string, input io.Reader) ([]byte, error) {
	if mode != lostStateMode {
		return nil, nil
	}
	proof, err := io.ReadAll(io.LimitReader(input, 16*1024+1))
	if err != nil || len(proof) == 0 || len(proof) > 16*1024 {
		return nil, errServe
	}
	return proof, nil
}

// prepare is the one-shot, non-serving cold ACL initializer.
func prepare(mode string) error {
	return prepareAt(mode, managedredis.ClientSecretFile, aclFile, journalFile, activationMarkerFile)
}

func prepareAt(mode, clientSecretPath, aclPath, journalPath, markerPath string) (retErr error) {
	if os.Getenv("REDIS_CREDENTIAL_MODE") != "managed" || mode != "first-activation" && mode != "recover" && mode != lostStateMode {
		return errServe
	}
	proof, err := readLostStateProof(mode, os.Stdin)
	if err != nil {
		return errServe
	}
	input, err := readPrepareInputs()
	if err != nil {
		return errServe
	}
	tlsMode := os.Getenv("REDIS_TLS")
	if tlsMode != "true" && tlsMode != "false" {
		return errServe
	}
	tlsConfig, err := database.ManagedRedisTLSConfig(input.endpoint, tlsMode == "true", os.Getenv("REDIS_TLS_CA_FILE"))
	if err != nil {
		return errServe
	}
	baseURL := os.Getenv("REDIS_INFISICAL_URL")
	if baseURL == "" {
		baseURL = "https://app.infisical.com"
	}
	provider, err := managedredis.NewInfisicalReader(baseURL, input.clientID, clientSecretPath, nil)
	if err != nil {
		return errServe
	}
	if aclDir, err := os.Stat(filepath.Dir(aclPath)); err != nil || !aclDir.IsDir() || aclDir.Mode().Perm() != 0o710 || !managedSetgid(aclDir.Mode()) {
		return errServe
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	bootstrapScope := secretScope(input.project, input.environment, input.path, "REDIS_ADMIN_BOOTSTRAP")
	policyScope := secretScope(input.project, input.environment, input.path, "REDIS_AUTH_POLICY")
	bootstrapRaw, name, err := provider.ReadExact(ctx, bootstrapScope)
	if err != nil || name != bootstrapScope.Name {
		return errServe
	}
	bootstrap, err := managedredis.ParseAdminBootstrap(bootstrapRaw, bootstrapScope)
	if err != nil {
		return errServe
	}
	admin, err := newRedisAdmin(input.endpoint, tlsConfig, bootstrap)
	if err != nil {
		return errServe
	}
	defer func() {
		if err := admin.client.Close(); err != nil {
			retErr = errServe
		}
	}()
	controller, err := managedredis.NewController(managedredis.ControllerOptions{
		Admin: admin, ACLPath: aclPath, JournalPath: journalPath, MarkerPath: markerPath, Bootstrap: bootstrap, Now: time.Now,
		ExpectedUIDs: map[string]uint32{"control-plane": input.controlUID, "media-plane": input.mediaUID},
		ACLUID:       os.Geteuid(), ACLGID: input.aclGID,
	})
	if err != nil {
		return errServe
	}
	return applyPreparation(ctx, mode, controller, provider, policyScope, proof)
}

type exactSecretReader interface {
	ReadExact(context.Context, managedredis.SecretScope) ([]byte, string, error)
}

func applyPreparation(ctx context.Context, mode string, controller *managedredis.Controller, provider exactSecretReader, policyScope managedredis.SecretScope, proof []byte) error {
	if mode == "recover" && controller.Restore() != nil {
		return errServe
	}
	policyRaw, name, err := provider.ReadExact(ctx, policyScope)
	if err != nil || name != policyScope.Name {
		return errServe
	}
	policy, err := managedredis.ParsePolicy(policyRaw, policyScope, time.Now())
	if err != nil {
		return errServe
	}
	if mode == "recover" {
		if controller.PrepareRecovery(policy) != nil {
			return errServe
		}
	} else if mode == lostStateMode {
		if controller.PrepareLostStateRecovery(policy, proof) != nil {
			return errServe
		}
	} else if controller.PrepareInitial(policy) != nil {
		return errServe
	}
	return nil
}
