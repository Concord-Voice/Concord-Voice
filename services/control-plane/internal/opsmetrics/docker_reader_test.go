package opsmetrics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	testContainerID      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testContainerListURI = "/v1.44/containers/json?all=1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func dockerResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func healthyContainerList() string {
	return `[
		{
			"Id":"` + testContainerID + `",
			"Names":["/concordvoice-control-plane"],
			"State":"running",
			"Status":"Up 2 minutes (healthy)",
			"Labels":{"secret.label":"must-not-escape"},
			"Env":["INTERNAL_VALUE=must-not-escape"],
			"Mounts":[{"Source":"/sensitive/host/path"}],
			"NetworkSettings":{"Networks":{"default":{"IPAddress":"192.0.2.10"}}}
		},
		{
			"Id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"Names":["/not-concord"],
			"State":"running",
			"Status":"Up 2 minutes (healthy)"
		}
	]`
}

func healthyContainerStats() string {
	return `{
		"cpu_stats":{"cpu_usage":{"total_usage":200},"system_cpu_usage":1000,"online_cpus":2},
		"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":500},
		"memory_stats":{"usage":1234},
		"name":"must-not-escape",
		"networks":{"eth0":{"rx_bytes":999}}
	}`
}

func TestDockerReaderUsesOnlyFixedGetEndpointsAndDiscardsMetadata(t *testing.T) {
	var requests []*http.Request
	reader := newDockerReader(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.Clone(request.Context()))
		switch request.URL.RequestURI() {
		case testContainerListURI:
			return dockerResponse(http.StatusOK, healthyContainerList()), nil
		case "/v1.44/containers/" + testContainerID + "/stats?stream=false":
			return dockerResponse(http.StatusOK, healthyContainerStats()), nil
		default:
			t.Fatalf("unexpected Docker endpoint %q", request.URL.RequestURI())
			return nil, nil
		}
	}))

	metrics, err := reader.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1.0, metrics[MetricServiceControlPlaneRunning])
	require.Equal(t, 1.0, metrics[MetricServiceControlPlaneHealthy])
	require.InDelta(t, 40.0, metrics[MetricServiceControlPlaneCPUPercent], 1e-9)
	require.Equal(t, 1234.0, metrics[MetricServiceControlPlaneMemoryBytes])
	require.Len(t, metrics, len(concordServiceAllowlist)*4)

	encoded, err := json.Marshal(metrics)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "must-not-escape")
	require.NotContains(t, string(encoded), "192.0.2.10")
	require.Len(t, requests, 2)
	for _, request := range requests {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "http", request.URL.Scheme)
		require.Equal(t, dockerAPIHost, request.URL.Host)
	}
}

func TestNewDockerReaderRequiresUnixSocketURLAndRejectsRedirects(t *testing.T) {
	reader, err := NewDockerReader("unix:///var/run/docker.sock")
	require.NoError(t, err)
	require.Equal(t, "/var/run/docker.sock", reader.socketPath)
	require.Equal(t, dockerRequestTimeout, reader.client.Timeout)
	transport, ok := reader.client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.DialContext)

	redirectErr := reader.client.CheckRedirect(&http.Request{}, []*http.Request{{}})
	require.ErrorIs(t, redirectErr, http.ErrUseLastResponse)

	invalidURLs := []string{
		"http://localhost/var/run/docker.sock",
		"tcp://localhost:2375",
		"unix://remote/var/run/docker.sock",
		"unix:///%2e%2e/docker.sock",
		"unix:///var/run/docker.sock?option=value",
		"unix:///var/run/../docker.sock",
		"unix://relative.sock",
		"not-a-url",
	}
	for _, rawURL := range invalidURLs {
		t.Run(rawURL, func(t *testing.T) {
			_, err := NewDockerReader(rawURL)
			require.Error(t, err)
		})
	}
}

func TestDockerReaderMapsStoppedAndUnhealthyContainersWithoutInspectingStoppedStats(t *testing.T) {
	mediaID := strings.Repeat("c", 64)
	list := `[
		{"Id":"` + testContainerID + `","Names":["/concordvoice-control-plane"],"State":"exited","Status":"Exited (1)"},
		{"Id":"` + mediaID + `","Names":["/concordvoice-media-plane"],"State":"running","Status":"Up 1 minute (unhealthy)"}
	]`
	var requested []string
	reader := newDockerReader(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requested = append(requested, request.URL.RequestURI())
		if request.URL.RequestURI() == testContainerListURI {
			return dockerResponse(http.StatusOK, list), nil
		}
		if request.URL.RequestURI() == "/v1.44/containers/"+mediaID+"/stats?stream=false" {
			return dockerResponse(http.StatusOK, healthyContainerStats()), nil
		}
		t.Fatalf("unexpected Docker endpoint %q", request.URL.RequestURI())
		return nil, nil
	}))

	metrics, err := reader.Read(context.Background())
	require.NoError(t, err)
	require.Zero(t, metrics[MetricServiceControlPlaneRunning])
	require.Zero(t, metrics[MetricServiceControlPlaneHealthy])
	require.Zero(t, metrics[MetricServiceControlPlaneCPUPercent])
	require.Zero(t, metrics[MetricServiceControlPlaneMemoryBytes])
	require.Equal(t, 1.0, metrics[MetricServiceMediaPlaneRunning])
	require.Zero(t, metrics[MetricServiceMediaPlaneHealthy])
	require.NotContains(t, requested, "/v1.44/containers/"+testContainerID+"/stats?stream=false")
}

func TestDockerReaderRejectsOversizedMalformedAndRedirectResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("x", maxDockerResponseBytes+1), wantErr: "response exceeds"},
		{name: "malformed JSON", status: http.StatusOK, body: `[{`, wantErr: "decode Docker response"},
		{name: "trailing JSON", status: http.StatusOK, body: `[] {}`, wantErr: "trailing JSON"},
		{name: "redirect", status: http.StatusFound, body: `{}`, wantErr: "unexpected Docker status 302"},
		{name: "server error", status: http.StatusInternalServerError, body: `{}`, wantErr: "unexpected Docker status 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := newDockerReader(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return dockerResponse(tt.status, tt.body), nil
			}))
			_, err := reader.Read(context.Background())
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestDockerReaderRejectsMalformedStatsAndInvalidAllowlistedIDs(t *testing.T) {
	t.Run("malformed stats", func(t *testing.T) {
		reader := newDockerReader(roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.RequestURI() == testContainerListURI {
				return dockerResponse(http.StatusOK, healthyContainerList()), nil
			}
			return dockerResponse(http.StatusOK, `{not-json}`), nil
		}))
		_, err := reader.Read(context.Background())
		require.ErrorContains(t, err, "decode Docker response")
	})

	t.Run("invalid allowlisted id", func(t *testing.T) {
		list := `[{"Id":"../containers/json","Names":["/concordvoice-control-plane"],"State":"running","Status":"healthy"}]`
		requests := 0
		reader := newDockerReader(roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return dockerResponse(http.StatusOK, list), nil
		}))
		_, err := reader.Read(context.Background())
		require.ErrorContains(t, err, "invalid Docker container id")
		require.Equal(t, 1, requests)
	})
}

func TestDockerReaderRejectsAmbiguousAllowlistedContainers(t *testing.T) {
	list := `[
		{"Id":"` + testContainerID + `","Names":["/concordvoice-control-plane"],"State":"running","Status":"healthy"},
		{"Id":"` + strings.Repeat("d", 64) + `","Names":["/concordvoice-control-plane"],"State":"running","Status":"healthy"}
	]`
	reader := newDockerReader(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return dockerResponse(http.StatusOK, list), nil
	}))

	_, err := reader.Read(context.Background())
	require.ErrorContains(t, err, "duplicate allowlisted Docker container")
}

func TestDockerReaderChecksTransportAndBodyCloseErrors(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		reader := newDockerReader(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed")
		}))
		_, err := reader.Read(context.Background())
		require.ErrorContains(t, err, "docker request failed")
	})

	t.Run("body close", func(t *testing.T) {
		reader := newDockerReader(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &closeErrorBody{Reader: strings.NewReader(`[]`)}}, nil
		}))
		_, err := reader.Read(context.Background())
		require.ErrorContains(t, err, "close Docker response")
	})
}

func TestDockerReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	reader := newDockerReader(roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected request")
	}))

	_, err := reader.Read(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
}

type closeErrorBody struct {
	io.Reader
}

func (*closeErrorBody) Close() error {
	return errors.New("close failed")
}
