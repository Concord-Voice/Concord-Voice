package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ADR-0038 keeps files under 1 MiB on the proxy permanently, so the env var
// may raise the direct-download floor but never lower it.
func TestStoragePresignMinBytesNeverBelowOneMiB(t *testing.T) {
	for value, want := range map[string]int64{
		"":        1 << 20,
		"0":       1 << 20,
		"-5":      1 << 20,
		"4194304": 4 << 20,
	} {
		clearBillingEnv(t)
		t.Setenv("ENVIRONMENT", "development")
		t.Setenv("STORAGE_PRESIGN_MIN_BYTES", value)
		cfg, err := Load()
		require.NoError(t, err)
		require.Equal(t, want, cfg.StoragePresignMinBytes, "STORAGE_PRESIGN_MIN_BYTES=%q", value)
	}
}
