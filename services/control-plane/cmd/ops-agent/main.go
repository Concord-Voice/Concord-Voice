// Command ops-agent publishes aggregate host and container operations metrics.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/opsmetrics"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/config"
	concordnats "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

const (
	defaultNATSURL  = "nats://localhost:4222"
	hostProcRoot    = "/host/proc"
	hostDiskPath    = "/host/proc/1/root"
	dockerSocketURL = "unix:///var/run/docker.sock"
)

func main() {
	os.Exit(run())
}

func run() int {
	metricsConfig, err := config.LoadOpsMetricsConfig()
	if err != nil {
		log.Print("ops-agent startup failed: reason=config")
		return 1
	}
	if !metricsConfig.Enabled {
		log.Print("ops-agent disabled")
		return 0
	}

	natsURL := strings.TrimSpace(os.Getenv("NATS_URL"))
	if natsURL == "" {
		natsURL = defaultNATSURL
	}
	natsClient, err := concordnats.Connect(natsURL)
	if err != nil {
		log.Print("ops-agent startup failed: reason=nats_connect")
		return 1
	}
	defer natsClient.Close()

	dockerReader, err := opsmetrics.NewDockerReader(dockerSocketURL)
	if err != nil {
		log.Print("ops-agent startup failed: reason=docker_client")
		return 1
	}
	hostReader := opsmetrics.NewHostReader(hostProcRoot, hostDiskPath)
	agent, err := opsmetrics.NewAgent(
		opsmetrics.AgentConfig{
			Enabled:      metricsConfig.Enabled,
			NodeID:       metricsConfig.NodeID,
			SharedSecret: []byte(metricsConfig.SharedSecret),
			Interval:     metricsConfig.Interval,
		},
		hostReader,
		dockerReader,
		natsClient,
		log.Default(),
	)
	if err != nil {
		log.Print("ops-agent startup failed: reason=agent_config")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Run(ctx); err != nil {
		log.Print("ops-agent stopped: reason=runtime")
		return 1
	}
	return 0
}
