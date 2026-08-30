package main

import (
	"os"
	"strings"
	"testing"
)

func TestDispatchControlPlaneSubcommandRoutesSiblings(t *testing.T) {
	type call struct {
		name string
		args []string
	}
	var calls []call
	runners := controlPlaneSubcommandRunners{
		admin: func(args []string) int {
			calls = append(calls, call{name: "admin", args: args})
			return 7
		},
		activityHistory: func(args []string) int {
			calls = append(calls, call{name: "activity-history", args: args})
			return 9
		},
	}

	code, handled := dispatchControlPlaneSubcommandWithRunners(
		[]string{"control-plane", "admin", "bootstrap"}, runners,
	)
	if !handled || code != 7 || len(calls) != 1 || calls[0].name != "admin" ||
		len(calls[0].args) != 1 || calls[0].args[0] != "bootstrap" {
		t.Fatalf("admin dispatch = code:%d handled:%v calls:%v", code, handled, calls)
	}

	calls = nil
	code, handled = dispatchControlPlaneSubcommandWithRunners(
		[]string{"control-plane", "activity-history", "preflight", "--confirm-drained"}, runners,
	)
	if !handled || code != 9 || len(calls) != 1 || calls[0].name != "activity-history" ||
		strings.Join(calls[0].args, " ") != "preflight --confirm-drained" {
		t.Fatalf("activity-history dispatch = code:%d handled:%v calls:%v", code, handled, calls)
	}

	for _, args := range [][]string{
		nil,
		{"control-plane"},
		{"control-plane", "serve"},
	} {
		calls = nil
		code, handled = dispatchControlPlaneSubcommandWithRunners(args, runners)
		if handled || code != 0 || len(calls) != 0 {
			t.Fatalf("ordinary startup dispatch for %v = code:%d handled:%v calls:%v", args, code, handled, calls)
		}
	}
}

func TestDispatchControlPlaneSubcommandProductionWrapperLeavesStartupUnhandled(t *testing.T) {
	code, handled := dispatchControlPlaneSubcommand([]string{"control-plane"})
	if code != 0 || handled {
		t.Fatalf("production dispatch wrapper = code:%d handled:%v", code, handled)
	}
}

func TestDispatchControlPlaneSubcommandUnknownNonEmptyReturnsUsageExit64(t *testing.T) {
	code, handled := dispatchControlPlaneSubcommandWithRunners(
		[]string{"control-plane", "unknown-subcommand"}, controlPlaneSubcommandRunners{},
	)
	if !handled || code != 64 {
		t.Fatalf("unknown dispatch = code:%d handled:%v, want exit 64 and handled", code, handled)
	}
}

func TestMainDispatchesControlPlaneSubcommandsBeforeConfigLoad(t *testing.T) {
	source, err := os.ReadFile("main.go") // #nosec G304 -- fixed test-only source path
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)
	dispatch := strings.Index(text, "dispatchControlPlaneSubcommand(os.Args)")
	load := strings.Index(text, "config.Load()")
	if dispatch < 0 || load < 0 || dispatch >= load {
		t.Fatalf("subcommand dispatch=%d must precede config load=%d", dispatch, load)
	}
	if strings.Contains(text, `os.Args[1] == "admin"`) {
		t.Fatal("main retains the retired admin-only dispatch branch")
	}
}
