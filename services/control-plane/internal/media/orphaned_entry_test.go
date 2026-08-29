package media

import "testing"

// The orphan queue is the record of LAST resort for a completed object whose
// metadata write and delete both failed: no sweep can see it, and no database
// row points at it. An entry that does not say WHICH backend holds the object
// is therefore not a lesser record, it is a useless one after the Wave C flip
// -- an S3 DELETE of a key absent from the target bucket returns SUCCESS, so a
// drainer working from a bare key would report a reclamation that never
// happened.

func TestOrphanedObjectEntry_LegacyIsByteIdenticalToTheHistoricalFormat(t *testing.T) {
	// The compatibility guarantee, and the reason this needed no migration:
	// every entry the queue has ever held was written by the legacy backend,
	// because legacy is the only backend that has ever been written to. If
	// this ever stops being a bare key, entries queued by older builds become
	// unparseable by the reader that has to drain them.
	if got := orphanedObjectEntry("", "attachments/abc-123"); got != "attachments/abc-123" {
		t.Fatalf("legacy entry = %q, want the bare key (pre-existing entries would be orphaned)", got)
	}
}

func TestOrphanedObjectEntry_NonLegacyCarriesItsBackend(t *testing.T) {
	got := orphanedObjectEntry("r2-useast", "attachments/abc-123")
	if got != "r2-useast\tattachments/abc-123" {
		t.Fatalf("entry = %q, want backend-prefixed", got)
	}
}

func TestParseOrphanedObjectEntry_RoundTrips(t *testing.T) {
	cases := []struct{ backend, key string }{
		{"", "attachments/abc-123"},
		{"r2-useast", "attachments/abc-123"},
		{"r2-eu", "attachments/has/slashes/in/it"},
	}
	for _, tc := range cases {
		gotB, gotK := parseOrphanedObjectEntry(orphanedObjectEntry(tc.backend, tc.key))
		if gotB != tc.backend || gotK != tc.key {
			t.Errorf("round trip of (%q,%q) = (%q,%q)", tc.backend, tc.key, gotB, gotK)
		}
	}
}

func TestParseOrphanedObjectEntry_TablessHistoricalEntryReadsAsLegacy(t *testing.T) {
	// A drainer meeting an entry written by a build that predates the pair
	// encoding must resolve it to legacy -- not to "unknown", and not to
	// whatever the current write default happens to be.
	backend, key := parseOrphanedObjectEntry("attachments/queued-by-an-old-build")
	if backend != "" {
		t.Fatalf("historical entry resolved to backend %q, want legacy (empty)", backend)
	}
	if key != "attachments/queued-by-an-old-build" {
		t.Fatalf("key = %q", key)
	}
}
