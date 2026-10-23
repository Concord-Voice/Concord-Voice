package erasureclear

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSealAndOpenCanonicalAudience(t *testing.T) {
	key := DeriveKey("synthetic-test-secret")
	envelope, err := Seal(key, "sender", 0, []string{"viewer-b", "viewer-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(envelope, ".")) != 5 {
		t.Fatal("audience must use compact JWE serialization")
	}
	if strings.Contains(envelope, "viewer-a") || strings.Contains(envelope, "viewer-b") {
		t.Fatal("the shared bus must not expose viewer identities")
	}
	viewers, err := Open(key, "sender", 0, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(viewers) != 2 || viewers[0] != "viewer-a" || viewers[1] != "viewer-b" {
		t.Fatalf("opened audience must be canonical, got %v", viewers)
	}
}

func TestOpenRejectsModifiedSenderOrEnvelope(t *testing.T) {
	key := DeriveKey("synthetic-test-secret")
	envelope, err := Seal(key, "sender", 2, []string{"authorized-viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(key, "different-sender", 2, envelope); err == nil {
		t.Fatal("an envelope must be bound to its sender")
	}
	if _, err := Open(key, "sender", 3, envelope); err == nil {
		t.Fatal("an envelope must be bound to its chunk number")
	}
	tampered := "A" + envelope[1:]
	if envelope[0] == 'A' {
		tampered = "B" + envelope[1:]
	}
	if _, err := Open(key, "sender", 2, tampered); err == nil {
		t.Fatal("a modified audience envelope must not open")
	}
	if _, err := Open(DeriveKey("another-secret"), "sender", 2, envelope); err == nil {
		t.Fatal("another service key must not open the audience")
	}
}

func TestOpenRejectsNoncanonicalJWE(t *testing.T) {
	key := DeriveKey("synthetic-test-secret")
	envelope, err := Seal(key, "sender", 1, []string{"authorized-viewer"})
	if err != nil {
		t.Fatal(err)
	}
	// Noncanonical spellings cannot bypass the raw compact-JWE replay identity.
	if _, err := Open(key, "sender", 1, envelope[:8]+"\n"+envelope[8:]); err == nil {
		t.Fatal("an alternate JWE spelling must be refused")
	}
}

func TestSealedAudienceLengthHidesExactViewerCount(t *testing.T) {
	key := DeriveKey("synthetic-test-secret")
	viewer := "11111111-2222-4333-8444-555555555555"
	first, err := Seal(key, "sender", 0, []string{viewer})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Seal(key, "sender", 0, []string{viewer, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("ciphertext length exposed the exact small-audience count: %d vs %d", len(first), len(second))
	}
	opened, err := Open(key, "sender", 0, second)
	if err != nil || len(opened) != 2 {
		t.Fatalf("padding changed the authenticated audience: %v, %v", opened, err)
	}
}

func TestSealAndOpenEmptyAudience(t *testing.T) {
	key := DeriveKey("synthetic-test-secret")
	if batches := Batches(nil); len(batches) != 1 || len(batches[0]) != 0 {
		t.Fatalf("empty audience still needs a sender-state event, got %v", batches)
	}
	envelope, err := Seal(key, "sender", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	viewers, err := Open(key, "sender", 0, envelope)
	if err != nil || len(viewers) != 0 {
		t.Fatalf("empty audience must remain empty: %v, %v", viewers, err)
	}
	if _, err := Seal(DeriveKey(""), "sender", 0, nil); err == nil {
		t.Fatal("an unwired secret cannot seal an event")
	}
	if _, err := Seal(key, "sender", 0, make([]string, MaxViewersPerEvent+1)); err == nil {
		t.Fatal("an oversized batch must be refused before publishing")
	}
}

func TestLargeAudienceBatchesStayUnderNATSPayloadLimit(t *testing.T) {
	const audienceSize = 22_000 // One envelope at this size exceeds 1 MiB.
	const senderID = "11111111-2222-4333-8444-555555555555"
	ids := make([]string, audienceSize)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012x", i)
	}
	key := DeriveKey("synthetic-test-secret")
	batches := Batches(ids)
	if len(batches) != 3 {
		t.Fatalf("expected three bounded batches, got %d", len(batches))
	}
	covered := 0
	for chunk, viewers := range batches {
		envelope, err := Seal(key, senderID, chunk, viewers)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{
			"user_id": senderID, "chunk": chunk, "audience": envelope,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) > MaxEventPayloadBytes {
			t.Fatalf("chunk %d exceeds the accepted event bound: %d bytes", chunk, len(payload))
		}
		opened, err := Open(key, senderID, chunk, envelope)
		if err != nil || len(opened) != len(viewers) {
			t.Fatalf("chunk %d did not retain its viewers: %d, %v", chunk, len(opened), err)
		}
		covered += len(opened)
	}
	if covered != audienceSize {
		t.Fatalf("batches covered %d of %d viewers", covered, audienceSize)
	}
}
