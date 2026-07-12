package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestShutdownControlPlaneWaitsForHTTPDrain(t *testing.T) {
	httpStarted := make(chan struct{})
	releaseHTTP := make(chan struct{})
	events := make(chan string, 3)
	result := make(chan error, 1)

	go func() {
		result <- shutdownControlPlane(
			func() error {
				close(httpStarted)
				<-releaseHTTP
				events <- "http"
				return nil
			},
			func() { events <- "hub" },
			func() { events <- "nats" },
		)
	}()

	<-httpStarted
	select {
	case event := <-events:
		t.Fatalf("dependency closed before HTTP drain completed: %s", event)
	default:
	}

	close(releaseHTTP)
	if err := <-result; err != nil {
		t.Fatalf("shutdownControlPlane returned error: %v", err)
	}

	got := []string{<-events, <-events, <-events}
	want := []string{"http", "hub", "nats"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestShutdownControlPlaneCleansUpAfterHTTPError(t *testing.T) {
	wantErr := errors.New("HTTP drain failed")
	events := make([]string, 0, 3)

	gotErr := shutdownControlPlane(
		func() error {
			events = append(events, "http")
			return wantErr
		},
		func() { events = append(events, "hub") },
		func() { events = append(events, "nats") },
	)

	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("shutdownControlPlane error = %v, want %v", gotErr, wantErr)
	}
	wantEvents := []string{"http", "hub", "nats"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("shutdown order = %v, want %v", events, wantEvents)
	}
}
