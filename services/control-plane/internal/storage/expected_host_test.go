package storage

import (
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
)

// The destination check added with ADR-0038 / #2759.
//
// Every rejection case below is a shape an adversarial audit PROVED
// vendorEndpointHost accepts on its own -- each was executed against the
// verbatim function and against minio-go v7.3.0, and each returned a usable
// client pointed somewhere other than Cloudflare. They are regression cases
// for a hole that was open, not hypotheticals.
//
// The tables hold RAW ENDPOINT URLS and run them through the real
// vendorEndpointHost. An earlier revision held the pre-computed HOST instead
// and carried the raw URL only in a prose field -- which made four rows
// ("plain foreign host", "fragment truncation", "query truncation",
// "userinfo prefix") the same assertion under four names, and left the
// premise that url.Parse reduces those URLs to evil.example.com asserted by a
// comment rather than executed. Running the pair end to end is what makes
// each row distinct.

// productionSuffix reads the value the SHIPPED candidate row declares, rather
// than restating it. A local constant here would mean a wrong or removed
// suffix in pkg/config passes this entire file -- the mutation that the suite
// most needs to catch, since that declaration is the control.
func productionSuffix(t *testing.T) string {
	t.Helper()
	cfg := &config.Config{}
	cfg.CloudflareR2.AccessKeyID = "k"
	cfg.CloudflareR2.SecretAccessKey = "s"
	for _, b := range cfg.AttachmentBackends() {
		if b.ID == config.AttachmentBackendR2USEast {
			return b.ExpectedHostSuffix
		}
	}
	t.Fatalf("no %q row in AttachmentBackends", config.AttachmentBackendR2USEast)
	return ""
}

func TestShippedR2RowPinsTheCloudflareDomain(t *testing.T) {
	// Pins the VALUE, not merely its shape. Without this, changing the
	// declaration to ".attacker.example" leaves every other test green,
	// because they would be measuring the endpoint against the wrong goalpost
	// rather than against Cloudflare.
	if got, want := productionSuffix(t), ".r2.cloudflarestorage.com"; got != want {
		t.Fatalf("shipped ExpectedHostSuffix = %q, want %q", got, want)
	}
}

func TestEndpointsRedirectedAwayFromTheVendorAreRefused(t *testing.T) {
	suffix := productionSuffix(t)
	cases := []struct{ name, endpoint, why string }{
		{"plain foreign host", "https://attacker.example.com",
			"no relationship to the vendor domain at all"},
		{"fragment truncation", "https://evil.example.com#.r2.cloudflarestorage.com",
			`the "#" makes the pinned suffix a fragment; the raw string still ENDS with it`},
		{"query truncation", "https://evil.example.com?.r2.cloudflarestorage.com",
			`same trick with "?"`},
		{"userinfo prefix", "https://a.r2.cloudflarestorage.com@evil.example.com", // pragma: allowlist secret
			"url.Parse drops everything before the @ from Host"},
		{"cloud metadata address", "https://169.254.169.254",
			"SSRF-adjacent; an IP literal is never inside a vendor domain"},
		{"loopback", "https://127.0.0.1", "as above"},
		{"suffix as a left-hand label", "https://abc.r2.cloudflarestorage.com.attacker.example",
			"contains the vendor domain but does not END with it"},
		{"lookalike registration", "https://notr2.cloudflarestorage.com",
			"the dot in the pinned suffix is what rejects this"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, err := vendorEndpointHost(tc.endpoint)
			if err != nil {
				return // rejected even earlier; still refused, which is the point
			}
			if err := assertExpectedHost(host, suffix); err == nil {
				t.Fatalf("endpoint %q reduced to host %q and was ACCEPTED into %q (%s)",
					tc.endpoint, host, suffix, tc.why)
			}
		})
	}
}

func TestGenuineVendorEndpointsAreAccepted(t *testing.T) {
	// The positive control. Without it the rejection table is satisfied by a
	// function that refuses everything -- which fails closed, and also fails.
	suffix := productionSuffix(t)
	cases := []struct{ name, endpoint string }{
		{"the pinned production destination", "https://e489155a831a9f9f30c8c581e7f4b207.r2.cloudflarestorage.com"},
		{"explicit port", "https://abc123.r2.cloudflarestorage.com:443"},
		{"mixed case (DNS is case-insensitive)", "https://ABC123.R2.CloudflareStorage.COM"},
		{"fully-qualified trailing dot", "https://abc123.r2.cloudflarestorage.com."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, err := vendorEndpointHost(tc.endpoint)
			if err != nil {
				t.Fatalf("genuine endpoint %q was rejected by vendorEndpointHost: %v", tc.endpoint, err)
			}
			if err := assertExpectedHost(host, suffix); err != nil {
				t.Fatalf("genuine vendor host %q was refused: %v", host, err)
			}
		})
	}
}

func TestAssertExpectedHost_MalformedSuffixFailsClosed(t *testing.T) {
	// A candidate row that omits the field, or declares it undotted, is a CODE
	// defect. Neither may be read as "no opinion".
	t.Run("empty", func(t *testing.T) {
		err := assertExpectedHost("anything.example.com", "")
		if err == nil {
			t.Fatal("an empty ExpectedHostSuffix was treated as permissive")
		}
		if !strings.Contains(err.Error(), "ExpectedHostSuffix") {
			t.Fatalf("error should name the field so the defect is diagnosable, got: %v", err)
		}
	})
	t.Run("undotted admits a lookalike", func(t *testing.T) {
		// Without the dot, "notr2.cloudflarestorage.com" -- a separately
		// registrable name -- satisfies a bare HasSuffix.
		if err := assertExpectedHost("notr2.cloudflarestorage.com", "r2.cloudflarestorage.com"); err == nil {
			t.Fatal("an undotted suffix was accepted; it admits a lookalike registration")
		}
	})
}

func TestNewVendorClient_RefusesEndpointOutsideTheDeclaredVendor(t *testing.T) {
	// End-to-end at the real construction site: the arm that proves the check
	// is WIRED. Without the assertExpectedHost call in newVendorClient,
	// minio.NewCore accepts this host and returns a client aimed at the
	// attacker.
	_, err := newVendorClient(config.ObjectBackend{
		ID:                 config.AttachmentBackendR2USEast,
		Endpoint:           "https://evil.example.com#.r2.cloudflarestorage.com",
		ExpectedHostSuffix: productionSuffix(t),
		Region:             "auto",
		Bucket:             "concord-voice-r2-us-east",
		AccessKeyID:        "k",
		SecretAccessKey:    "s",
	}, nil)
	if err == nil {
		t.Fatal("newVendorClient built a client for a fragment-redirected endpoint")
	}
	if !strings.Contains(err.Error(), "not inside") {
		t.Fatalf("expected the destination check to reject it, got: %v", err)
	}
}

func TestNewVendorClient_AcceptsAGenuineVendorEndpoint(t *testing.T) {
	// Positive control for the wiring test above.
	//
	// It does NOT read docker-compose.production.yml and cannot detect a
	// change to the literal there; an earlier comment claimed otherwise. What
	// guards the compose value is the ExpectedHostSuffix pin plus the
	// deployment's own boot -- a compose endpoint outside the vendor domain
	// now registers UNAVAILABLE instead of dialling.
	c, err := newVendorClient(config.ObjectBackend{
		ID:                 config.AttachmentBackendR2USEast,
		Endpoint:           "https://e489155a831a9f9f30c8c581e7f4b207.r2.cloudflarestorage.com",
		ExpectedHostSuffix: productionSuffix(t),
		Region:             "auto",
		Bucket:             "concord-voice-r2-us-east",
		AccessKeyID:        "k",
		SecretAccessKey:    "s",
	}, nil)
	if err != nil {
		t.Fatalf("a genuine vendor destination was refused: %v", err)
	}
	if c == nil {
		t.Fatal("client is nil despite a nil error")
	}
}

func TestEveryDeclaredCandidatePinsAVendorDomain(t *testing.T) {
	// Iterates the UNFILTERED candidate table, not AttachmentBackends' live
	// result. The credential gate hides dormant rows -- the EU and
	// Indo-Pacific buckets -- from the filtered slice, so a guard written
	// against it skips precisely the rows most likely to have been added in a
	// hurry. Proven vacuous in that form: a second row with the field omitted
	// left the test green.
	candidates := config.AttachmentBackendCandidatesForTest(&config.Config{})
	if len(candidates) == 0 {
		t.Fatal("no candidate rows; the guard would be vacuous")
	}
	for _, b := range candidates {
		if b.ExpectedHostSuffix == "" {
			t.Errorf("backend %q declares no ExpectedHostSuffix", b.ID)
			continue
		}
		if !strings.HasPrefix(b.ExpectedHostSuffix, ".") {
			t.Errorf("backend %q suffix %q must begin with a dot, or a lookalike registration satisfies it",
				b.ID, b.ExpectedHostSuffix)
		}
	}
}
