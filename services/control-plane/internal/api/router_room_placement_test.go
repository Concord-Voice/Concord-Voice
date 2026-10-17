package api

import (
	"os"
	"strings"
	"testing"
)

func TestNewRouterPropagatesRoomPlacementGateToJoinAndLifecycleHandlers(t *testing.T) {
	source, err := os.ReadFile("router.go") // #nosec G304 -- fixed test-only source path
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	text := string(source)
	for _, wiring := range []string{
		"roomPlacementEnabled := dependencies.RoomPlacementEnabled",
		"RoomPlacementEnabled: roomPlacementEnabled",
		"voiceSub.SetLifecyclePlacementEnabled(roomPlacementEnabled)",
	} {
		if !strings.Contains(text, wiring) {
			t.Errorf("NewRouter is missing room placement gate wiring %q", wiring)
		}
	}
}
