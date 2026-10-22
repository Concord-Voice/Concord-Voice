package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefaultsRedisCredentialModeToLegacy(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("REDIS_CREDENTIAL_MODE", "")
	t.Setenv("REDIS_URL", "")

	cfg, err := Load()

	require.NoError(t, err)
	require.Equal(t, "legacy", cfg.RedisCredentialMode)
	require.Equal(t, defaultDevRedisURL(), cfg.RedisURL)
}

func TestValidateManagedRedisRejectsNonemptyLegacyURL(t *testing.T) {
	config := validManagedRedisConfig()
	t.Setenv("REDIS_URL", "redis://127.0.0.1:6379/0")

	err := config.validate()

	require.Error(t, err)
	require.Contains(t, err.Error(), "REDIS_URL")
}

func TestLoadManagedRedisAllowsEmptyLegacyURL(t *testing.T) {
	setManagedRedisConfig(t)
	t.Setenv("REDIS_URL", "")

	cfg, err := Load()

	require.NoError(t, err)
	require.Equal(t, "managed", cfg.RedisCredentialMode)
	require.Equal(t, "redis:6379", cfg.RedisEndpoint)
}

func TestValidateRejectsUnknownRedisCredentialMode(t *testing.T) {
	config := validManagedRedisConfig()
	config.RedisCredentialMode = "automatic"

	err := config.validate()

	require.Error(t, err)
	require.Contains(t, err.Error(), "REDIS_CREDENTIAL_MODE")
}

func TestValidateManagedRedisRequiresProviderSelectors(t *testing.T) {
	for _, selector := range []struct {
		name  string
		clear func(*Config)
	}{
		{name: "project", clear: func(c *Config) { c.RedisInfisicalProjectID = "" }},
		{name: "environment", clear: func(c *Config) { c.RedisInfisicalEnvironment = "" }},
		{name: "path", clear: func(c *Config) { c.RedisInfisicalPath = "" }},
		{name: "client ID", clear: func(c *Config) { c.RedisInfisicalClientID = "" }},
	} {
		t.Run(selector.name, func(t *testing.T) {
			config := validManagedRedisConfig()
			t.Setenv("REDIS_URL", "")
			selector.clear(config)

			err := config.validate()

			require.Error(t, err)
			require.Contains(t, err.Error(), "managed Redis provider selectors")
		})
	}
}

func TestValidateManagedRedisRequiresBareHostPort(t *testing.T) {
	for _, endpoint := range []string{"", "redis://redis:6379", "user:password@redis:6379", "redis:0", "redis:65536", "redis:abc", "redis:6379/path"} {
		t.Run(endpoint, func(t *testing.T) {
			config := validManagedRedisConfig()
			t.Setenv("REDIS_URL", "")
			config.RedisEndpoint = endpoint

			err := config.validate()

			require.Error(t, err)
			require.Contains(t, err.Error(), "REDIS_ENDPOINT")
		})
	}
}

func TestLoadRejectsInvalidRedisTLSFlag(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("REDIS_TLS", "yes")

	_, err := Load()

	require.Error(t, err)
	require.Contains(t, err.Error(), "REDIS_TLS")
}

func setManagedRedisConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("REDIS_CREDENTIAL_MODE", "managed")
	t.Setenv("REDIS_ENDPOINT", "redis:6379")
	t.Setenv("REDIS_URL", "")
	t.Setenv("REDIS_INFISICAL_PROJECT_ID", "test-project")
	t.Setenv("REDIS_INFISICAL_ENVIRONMENT", "prod")
	t.Setenv("REDIS_INFISICAL_PATH", "/test/redis")
	t.Setenv("REDIS_INFISICAL_CLIENT_ID", "test-client")
	t.Setenv("REDIS_TLS", "false")
}

func validManagedRedisConfig() *Config {
	config := validProductionConfig()
	config.Environment = "development"
	config.RedisCredentialMode = "managed"
	config.RedisEndpoint = "redis:6379"
	config.RedisInfisicalProjectID = "test-project"
	config.RedisInfisicalEnvironment = "prod"
	config.RedisInfisicalPath = "/test/redis"
	config.RedisInfisicalClientID = "test-client"
	return config
}
