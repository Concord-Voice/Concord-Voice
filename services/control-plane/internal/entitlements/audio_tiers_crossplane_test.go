package entitlements

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestAudioTierMirrorsStayInLockstep(t *testing.T) {
	mediaPlaneRaw, err := os.ReadFile("../../../media-plane/src/config/index.ts")
	if err != nil {
		t.Fatalf("read media-plane config: %v", err)
	}
	mediaPlaneBitrates := assertTypeScriptAudioTiers(t, "services/media-plane/src/config/index.ts", string(mediaPlaneRaw))

	desktopRaw, err := os.ReadFile("../../../../client/desktop/src/renderer/stores/voiceStore.ts")
	if err != nil {
		t.Fatalf("read desktop voice store: %v", err)
	}
	desktopBitrates := assertTypeScriptAudioTiers(t, "client/desktop/src/renderer/stores/voiceStore.ts", string(desktopRaw))

	for _, tier := range audioTierOrder {
		if mediaPlaneBitrates[tier] != desktopBitrates[tier] {
			t.Fatalf("media-plane maxBitrate for %q = %d, want desktop %d", tier, mediaPlaneBitrates[tier], desktopBitrates[tier])
		}
	}

	roomManagerRaw, err := os.ReadFile("../../../media-plane/src/lib/roomManager.ts")
	if err != nil {
		t.Fatalf("read media-plane room manager: %v", err)
	}
	assertRoomManagerBitrateCeilings(t, string(roomManagerRaw), desktopBitrates)
}

func assertTypeScriptAudioTiers(t *testing.T, name, src string) map[string]int {
	t.Helper()
	lastIndex := -1
	bitrates := make(map[string]int, len(audioTierOrder))
	for _, tier := range audioTierOrder {
		idx := strings.Index(src, tier+": {")
		if idx < 0 {
			t.Fatalf("%s missing tier %q", name, tier)
		}
		if idx <= lastIndex {
			t.Fatalf("%s tier %q is out of order", name, tier)
		}
		lastIndex = idx

		re := regexp.MustCompile(tier + `:\s*\{(?s:.*?)preferredFrameSize:\s*(\d+)`)
		matches := re.FindStringSubmatch(src)
		if len(matches) != 2 {
			t.Fatalf("%s missing preferredFrameSize for %q", name, tier)
		}
		got, err := strconv.Atoi(matches[1])
		if err != nil {
			t.Fatalf("parse preferredFrameSize for %q in %s: %v", tier, name, err)
		}
		if got != audioTierPtimeMs[tier] {
			t.Fatalf("%s preferredFrameSize for %q = %d, want %d", name, tier, got, audioTierPtimeMs[tier])
		}

		bitrateRe := regexp.MustCompile(tier + `:\s*\{(?s:.*?)maxBitrate:\s*([0-9_]+)`)
		bitrateMatches := bitrateRe.FindStringSubmatch(src)
		if len(bitrateMatches) != 2 {
			t.Fatalf("%s missing maxBitrate for %q", name, tier)
		}
		bitrates[tier] = parseTSInt(t, bitrateMatches[1], "maxBitrate", tier, name)
	}
	return bitrates
}

func assertRoomManagerBitrateCeilings(t *testing.T, src string, want map[string]int) {
	t.Helper()
	for _, tier := range audioTierOrder {
		re := regexp.MustCompile(tier + `:\s*([0-9_]+)`)
		matches := re.FindStringSubmatch(src)
		if len(matches) != 2 {
			t.Fatalf("roomManager missing bitrate ceiling for %q", tier)
		}
		got := parseTSInt(t, matches[1], "bitrate ceiling", tier, "services/media-plane/src/lib/roomManager.ts")
		if got != want[tier] {
			t.Fatalf("roomManager bitrate ceiling for %q = %d, want %d", tier, got, want[tier])
		}
	}
}

func parseTSInt(t *testing.T, raw, field, tier, name string) int {
	t.Helper()
	got, err := strconv.Atoi(strings.ReplaceAll(raw, "_", ""))
	if err != nil {
		t.Fatalf("parse %s for %q in %s: %v", field, tier, name, err)
	}
	return got
}
