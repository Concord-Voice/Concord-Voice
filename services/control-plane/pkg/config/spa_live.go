// Package config — LiveSpaConfig provides hot-reloadable SPA configuration.
//
// When SPA_CONFIG_FILE is set, the control plane mounts spa.env as a Docker
// volume and periodically re-reads it. This allows deploy.sh to update SPA
// config without rebuilding or restarting the container.
package config

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LiveSpaConfig provides thread-safe, hot-reloadable SPA configuration.
// When a config file path is provided, it periodically re-reads the file
// to pick up changes without requiring a server restart.
type LiveSpaConfig struct {
	mu             sync.RWMutex
	spaURL         string
	spaIpcContract int
	filePath       string
	stopCh         chan struct{}
	stopOnce       sync.Once
}

// NewLiveSpaConfig creates a LiveSpaConfig initialized from the given Config.
// If filePath is non-empty, starts a background goroutine that re-reads
// the file every pollInterval.
func NewLiveSpaConfig(cfg *Config, filePath string, pollInterval time.Duration) *LiveSpaConfig {
	lsc := &LiveSpaConfig{
		spaURL:         cfg.SpaURL,
		spaIpcContract: cfg.SpaIpcContract,
		filePath:       filePath,
		stopCh:         make(chan struct{}),
	}

	if filePath != "" {
		// Initial read from file (may override env var values with mounted file)
		lsc.reload()
		go lsc.watch(pollInterval)
		log.Printf("[SpaConfig] Hot-reload enabled: watching %s every %s", filePath, pollInterval)
	} else {
		log.Printf("[SpaConfig] Static mode: SPA_URL=%q SPA_IPC_CONTRACT=%d", cfg.SpaURL, cfg.SpaIpcContract)
	}

	return lsc
}

// SpaURL returns the current SPA URL.
func (l *LiveSpaConfig) SpaURL() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.spaURL
}

// SpaIpcContract returns the current SPA IPC contract version.
func (l *LiveSpaConfig) SpaIpcContract() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.spaIpcContract
}

// Stop stops the background file watcher. Safe to call multiple times.
func (l *LiveSpaConfig) Stop() {
	l.stopOnce.Do(func() { close(l.stopCh) })
}

func (l *LiveSpaConfig) watch(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.reload()
		case <-l.stopCh:
			return
		}
	}
}

func (l *LiveSpaConfig) reload() {
	data, err := os.ReadFile(l.filePath)
	if err != nil {
		// File might not exist yet or be temporarily unavailable during git pull
		log.Printf("[SpaConfig] Warning: failed to read %s: %v", l.filePath, err)
		return
	}

	vars := parseDotenv(data)

	newURL := vars["SPA_URL"]
	newContract := 0
	if v, ok := vars["SPA_IPC_CONTRACT"]; ok {
		if n, parseErr := strconv.Atoi(v); parseErr == nil {
			newContract = n
		} else {
			log.Printf("[SpaConfig] Warning: SPA_IPC_CONTRACT=%q is not a valid integer, treating as 0", v)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.spaURL != newURL || l.spaIpcContract != newContract {
		log.Printf("[SpaConfig] Hot-reload: SPA_URL=%q SPA_IPC_CONTRACT=%d", newURL, newContract)
		l.spaURL = newURL
		l.spaIpcContract = newContract
	}
}

// parseDotenv parses a simple KEY=VALUE file (ignoring comments and blank lines).
func parseDotenv(data []byte) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}
