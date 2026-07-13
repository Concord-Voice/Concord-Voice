package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestShutdownControlPlaneWaitsForHTTPDrain(t *testing.T) {
	httpStarted := make(chan struct{})
	releaseHTTP := make(chan struct{})
	events := make(chan string, 6)
	result := make(chan error, 1)

	go func() {
		result <- shutdownControlPlane(
			func() { events <- "cancel" },
			func() error {
				close(httpStarted)
				<-releaseHTTP
				events <- "http"
				return nil
			},
			func() { events <- "activity" },
			func() { events <- "hub" },
			func() error { events <- "metrics"; return nil },
			func() { events <- "nats" },
		)
	}()

	<-httpStarted
	if event := <-events; event != "cancel" {
		t.Fatalf("first shutdown event = %s, want cancel", event)
	}
	select {
	case event := <-events:
		t.Fatalf("dependency closed before HTTP drain completed: %s", event)
	default:
	}

	close(releaseHTTP)
	if err := <-result; err != nil {
		t.Fatalf("shutdownControlPlane returned error: %v", err)
	}

	got := []string{"cancel", <-events, <-events, <-events, <-events, <-events}
	want := []string{"cancel", "http", "activity", "hub", "metrics", "nats"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestShutdownControlPlaneCleansUpAfterHTTPError(t *testing.T) {
	wantErr := errors.New("HTTP drain failed")
	metricsErr := errors.New("metrics shutdown failed")
	events := make([]string, 0, 6)

	gotErr := shutdownControlPlane(
		func() { events = append(events, "cancel") },
		func() error {
			events = append(events, "http")
			return wantErr
		},
		func() { events = append(events, "activity") },
		func() { events = append(events, "hub") },
		func() error { events = append(events, "metrics"); return metricsErr },
		func() { events = append(events, "nats") },
	)

	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("shutdownControlPlane error = %v, want %v", gotErr, wantErr)
	}
	if !errors.Is(gotErr, metricsErr) {
		t.Fatalf("shutdownControlPlane error = %v, want joined error %v", gotErr, metricsErr)
	}
	wantEvents := []string{"cancel", "http", "activity", "hub", "metrics", "nats"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("shutdown order = %v, want %v", events, wantEvents)
	}
}
