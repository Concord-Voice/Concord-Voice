package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── parseDotenv ─────────────────────────────────────────────────────────────

func TestParseDotenvEmpty(t *testing.T) {
	result := parseDotenv([]byte(""))
	assert.Empty(t, result)
}

func TestParseDotenvSingleKeyValue(t *testing.T) {
	result := parseDotenv([]byte("KEY=value"))
	assert.Equal(t, map[string]string{"KEY": "value"}, result)
}

func TestParseDotenvMultipleKeys(t *testing.T) {
	input := "SPA_URL=https://example.com\nSPA_IPC_CONTRACT=42\n"
	result := parseDotenv([]byte(input))
	assert.Equal(t, "https://example.com", result["SPA_URL"])
	assert.Equal(t, "42", result["SPA_IPC_CONTRACT"])
}

func TestParseDotenvIgnoresComments(t *testing.T) {
	input := "# this is a comment\nKEY=value\n# another comment"
	result := parseDotenv([]byte(input))
	assert.Equal(t, map[string]string{"KEY": "value"}, result)
}

func TestParseDotenvIgnoresBlankLines(t *testing.T) {
	input := "\n\nKEY=value\n\n"
	result := parseDotenv([]byte(input))
	assert.Equal(t, map[string]string{"KEY": "value"}, result)
}

func TestParseDotenvTrimsWhitespace(t *testing.T) {
	result := parseDotenv([]byte("  KEY  =  value  "))
	assert.Equal(t, "value", result["KEY"])
}

func TestParseDotenvValueWithEquals(t *testing.T) {
	// SplitN(2) keeps extra "=" in the value
	result := parseDotenv([]byte("URL=https://example.com?a=1&b=2"))
	assert.Equal(t, "https://example.com?a=1&b=2", result["URL"])
}

func TestParseDotenvLineWithoutEquals(t *testing.T) {
	result := parseDotenv([]byte("NOEQUALS\nKEY=value"))
	assert.Equal(t, map[string]string{"KEY": "value"}, result)
}

func TestParseDotenvDuplicateKeysLastWins(t *testing.T) {
	input := "KEY=first\nKEY=second"
	result := parseDotenv([]byte(input))
	assert.Equal(t, "second", result["KEY"])
}

// ─── NewLiveSpaConfig ─────────────────────────────────────────────────────────

func TestNewLiveSpaConfigStaticModeNoFilePath(t *testing.T) {
	cfg := &Config{
		SpaURL:         "https://app.concordvoice.chat",
		SpaIpcContract: 7,
	}

	lsc := NewLiveSpaConfig(cfg, "", time.Minute)
	require.NotNil(t, lsc)
	// No background goroutine when filePath is empty — safe to check immediately
	assert.Equal(t, "https://app.concordvoice.chat", lsc.SpaURL())
	assert.Equal(t, 7, lsc.SpaIpcContract())
}

func TestNewLiveSpaConfigStaticModeUsesConfigValues(t *testing.T) {
	cfg := &Config{
		SpaURL:         "https://staging.concordvoice.chat",
		SpaIpcContract: 3,
	}

	lsc := NewLiveSpaConfig(cfg, "", time.Minute)
	assert.Equal(t, "https://staging.concordvoice.chat", lsc.SpaURL())
	assert.Equal(t, 3, lsc.SpaIpcContract())
}

func TestNewLiveSpaConfigHotReloadReadsFileOnInit(t *testing.T) {
	f := writeTempEnvFile(t, "SPA_URL=https://hot.example.com\nSPA_IPC_CONTRACT=9\n")

	cfg := &Config{SpaURL: "https://old.example.com", SpaIpcContract: 1}
	lsc := NewLiveSpaConfig(cfg, f, 10*time.Second)
	require.NotNil(t, lsc)
	defer lsc.Stop()

	// File was read on init — values override Config
	assert.Equal(t, "https://hot.example.com", lsc.SpaURL())
	assert.Equal(t, 9, lsc.SpaIpcContract())
}

func TestNewLiveSpaConfigHotReloadUpdatesOnReload(t *testing.T) {
	f := writeTempEnvFile(t, "SPA_URL=https://v1.example.com\nSPA_IPC_CONTRACT=1\n")

	cfg := &Config{}
	lsc := NewLiveSpaConfig(cfg, f, 10*time.Second)
	require.NotNil(t, lsc)
	defer lsc.Stop()

	assert.Equal(t, "https://v1.example.com", lsc.SpaURL())

	// Update the file and trigger a reload directly
	require.NoError(t, os.WriteFile(f, []byte("SPA_URL=https://v2.example.com\nSPA_IPC_CONTRACT=2\n"), 0o600))
	lsc.reload()

	assert.Equal(t, "https://v2.example.com", lsc.SpaURL())
	assert.Equal(t, 2, lsc.SpaIpcContract())
}

func TestNewLiveSpaConfigHotReloadInvalidIntegerDefaultsToZero(t *testing.T) {
	f := writeTempEnvFile(t, "SPA_URL=https://example.com\nSPA_IPC_CONTRACT=notanumber\n")

	cfg := &Config{}
	lsc := NewLiveSpaConfig(cfg, f, 10*time.Second)
	require.NotNil(t, lsc)
	defer lsc.Stop()

	assert.Equal(t, 0, lsc.SpaIpcContract(), "invalid integer should default to 0")
}

func TestLiveSpaConfigReloadMissingFile(t *testing.T) {
	// File does not exist — reload should log a warning and not panic
	lsc := &LiveSpaConfig{
		filePath: filepath.Join(t.TempDir(), "nonexistent.env"),
		spaURL:   "https://original.example.com",
		stopCh:   make(chan struct{}),
	}

	assert.NotPanics(t, func() { lsc.reload() })
	// Original values preserved after failed reload
	assert.Equal(t, "https://original.example.com", lsc.SpaURL())
}

// ─── Stop ─────────────────────────────────────────────────────────────────────

func TestLiveSpaConfigStopSafeToCallMultipleTimes(t *testing.T) {
	lsc := &LiveSpaConfig{
		stopCh: make(chan struct{}),
	}
	// sync.Once guarantees this doesn't panic on double-close
	assert.NotPanics(t, func() {
		lsc.Stop()
		lsc.Stop()
		lsc.Stop()
	})
}

// ─── Thread safety ────────────────────────────────────────────────────────────

func TestLiveSpaConfigConcurrentReads(t *testing.T) {
	f := writeTempEnvFile(t, "SPA_URL=https://concurrent.example.com\nSPA_IPC_CONTRACT=5\n")

	cfg := &Config{}
	lsc := NewLiveSpaConfig(cfg, f, time.Second)
	defer lsc.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = lsc.SpaURL()
			_ = lsc.SpaIpcContract()
		}()
	}
	wg.Wait() // Should not race-detect or panic
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func writeTempEnvFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, fmt.Sprintf("spa-%d.env", time.Now().UnixNano()))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
