package config

import (
	"strings"
	"testing"
)

// The legacy object-storage destination guards (ADR-0038 follow-up).
//
// This is the backend that actually holds everything: every pre-cutover
// attachment, and ALL profile media permanently. Profile media is the sharper
// case — the server re-encodes it, so unlike an E2EE attachment it is
// PLAINTEXT wherever it lands. A redirected endpoint exfiltrates identifiable
// user images, and it does so silently, because uploads simply succeed
// against the attacker's bucket.

func TestValidateProduction_RejectsAStorageEndpointOutsideTheCluster(t *testing.T) {
	cases := []struct{ name, endpoint string }{
		{"public host", "evil.example.com:9000"},
		{"public host, no port", "evil.example.com"},
		{"public IP literal", "203.0.113.10:9000"},
		{"a name that merely contains minio", "minio.evil.example.com:9000"},

		// The USERINFO class, and the reason this function no longer uses
		// net.SplitHostPort. Each of these was ACCEPTED by the first version
		// -- host="minio", err=nil, because SplitHostPort splits at the last
		// colon and never validates the port -- while minio-go builds
		// "http://"+endpoint, reads the left side as userinfo, and dials the
		// host on the right in cleartext with the SigV4 credential attached.
		// The guard did not miss the redirect, it certified it.
		{"userinfo redirect", "minio:9000@evil.example.com"},
		{"userinfo redirect via localhost", "localhost:9000@evil.example.com"},
		{"userinfo redirect via loopback literal", "127.0.0.1:9000@evil.example.com"},
		{"userinfo redirect to an IP", "minio:9000@203.0.113.10"},
		{"userinfo with an empty port", "minio:@evil.example.com"},

		// The wider class the same defect belongs to: SplitHostPort accepts
		// ANY port text without a further colon, so path- and fragment-shaped
		// payloads rode in too.
		{"path payload", "minio:9000/../evil"},
		{"fragment payload", "minio:9000#evil.example.com"},
		{"query payload", "minio:9000?x=evil.example.com"},
		{"backslash payload", "minio:9000\\evil.example.com"},
		// Rejected by net/url's own port validation (the err branch), not by the
		// digits check further down — that branch is unreachable today and says so.
		{"non-numeric port", "minio:notaport"},
		{"embedded space", "minio:9000 evil.example.com"},

		// Not a destination.
		{"unspecified v4", "0.0.0.0:9000"},
		{"unspecified v6", "[::]:9000"},
		{"double trailing dot", "minio..:9000"},

		// PRIVATE IS NOT OURS. Every one of these was ACCEPTED before the
		// SaaS/self-hosted split (CodeRabbit, PR #3007). SaaS ships the compose
		// service NAME, so an attacker who can rewrite /opt/concord/.env had
		// only to name a host inside the same VPC or overlay -- their own
		// container -- to receive the static credentials in cleartext.
		{"RFC1918 10/8", "10.1.2.3:9000"},
		{"RFC1918 172.16/12", "172.20.0.5:9000"},
		{"RFC1918 192.168/16", "192.168.1.50:9000"},
		{"unique local v6", "[fd00::1]:9000"},
		{"CLOUD INSTANCE METADATA", "169.254.169.254:9000"},

		// Unicode case-folding forgery: strings.ToLower maps U+0130 to ASCII
		// 'i', so this lowercases to exactly "minio" and satisfies every other
		// screen, while minio-go dials the distinct label. Same validator/dialer
		// split as the userinfo bypass, reached a different way.
		{"U+0130 folds to the service name", "m\u0130nio:9000"},
		{"non-ASCII generally", "min\u0131o:9000"},
		{"link-local v4", "169.254.1.1:9000"},
		{"link-local v6", "[fe80::1]:9000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validProductionConfig()
			cfg.StorageEndpoint = tc.endpoint
			cfg.StorageUseSSL = true // isolate the PLACEMENT rule from the TLS rule
			err := cfg.validateProduction()
			if err == nil {
				t.Fatalf("endpoint %q was accepted on a SaaS deployment", tc.endpoint)
			}
			if !strings.Contains(err.Error(), "STORAGE_ENDPOINT") {
				t.Fatalf("error should name STORAGE_ENDPOINT, got: %v", err)
			}
		})
	}
}

func TestValidateProduction_AcceptsInClusterStorageEndpoints(t *testing.T) {
	// Positive control. Without it the rejection table above is satisfied by a
	// guard that refuses everything — which would fail closed, and also fail.
	cases := []struct{ name, endpoint string }{
		{"the pinned compose service", "minio:9000"},
		{"bare service name", "minio"},
		{"localhost", "localhost:9000"},
		{"loopback literal", "127.0.0.1:9000"},
		{"IPv6 loopback in brackets", "[::1]:9000"},
		{"uppercase is the same host", "MinIO:9000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validProductionConfig()
			cfg.StorageEndpoint = tc.endpoint
			cfg.StorageUseSSL = false // in-cluster plaintext is the shipped posture
			if err := cfg.validateProduction(); err != nil {
				t.Fatalf("in-cluster endpoint %q was refused: %v", tc.endpoint, err)
			}
		})
	}
}

func TestValidateProduction_ExplicitSaaSIsHeldToTheSaaSRule(t *testing.T) {
	// docker-compose.production.yml sets INSTANCE_TYPE="saas" EXPLICITLY, so
	// this is the value production actually ships. Without this case a guard
	// weakened to skip the check for InstanceType == "saas" would pass every
	// other test in the file while being disabled in the only deployment that
	// matters.
	cfg := validProductionConfig()
	cfg.InstanceType = InstanceTypeSaaS
	cfg.StorageEndpoint = "evil.example.com:9000"
	cfg.StorageUseSSL = true
	if err := cfg.validateProduction(); err == nil {
		t.Fatal("an explicit saas InstanceType skipped the placement guard")
	}
}

func TestValidateProduction_UnsetInstanceTypeIsHeldToTheSaaSRule(t *testing.T) {
	// Load() normalises InstanceType, but a Config that never went through it
	// must not skip the check. This is the fail-closed direction.
	cfg := validProductionConfig()
	cfg.InstanceType = ""
	cfg.StorageEndpoint = "evil.example.com:9000"
	cfg.StorageUseSSL = true
	if err := cfg.validateProduction(); err == nil {
		t.Fatal("an unset InstanceType skipped the placement guard")
	}
}

func TestValidateProduction_SelfHostedMayUseExternalStorageButOnlyOverTLS(t *testing.T) {
	// A self-hosted operator may legitimately point at storage we know nothing
	// about — so they are exempt from PLACEMENT, and held to TLS instead.
	base := func() *Config {
		cfg := validProductionConfig()
		cfg.InstanceType = InstanceTypeSelfHosted
		cfg.StorageEndpoint = "s3.operator.example:9000"
		return cfg
	}

	over := base()
	over.StorageUseSSL = true
	if err := over.validateProduction(); err != nil {
		t.Fatalf("self-hosted external storage over TLS was refused: %v", err)
	}

	plain := base()
	plain.StorageUseSSL = false
	err := plain.validateProduction()
	if err == nil {
		t.Fatal("self-hosted external storage in PLAINTEXT was accepted")
	}
	if !strings.Contains(err.Error(), "STORAGE_USE_SSL") {
		t.Fatalf("error should name STORAGE_USE_SSL, got: %v", err)
	}
}

func TestValidateProduction_SelfHostedPrivateNetworkMayStayPlaintext(t *testing.T) {
	// Positive control for the TLS rule: a LAN address is inside the operator's
	// own network, so refusing plaintext there would be pointless friction.
	cfg := validProductionConfig()
	cfg.InstanceType = InstanceTypeSelfHosted
	cfg.StorageEndpoint = "192.168.1.50:9000"
	cfg.StorageUseSSL = false
	if err := cfg.validateProduction(); err != nil {
		t.Fatalf("self-hosted private-network plaintext was refused: %v", err)
	}
}

func TestValidateProduction_SelfHostedStillRefusesTheMetadataAddress(t *testing.T) {
	// The self-hosted plaintext exemption is "inside the operator's own
	// network", NOT "any address that is not routable". 169.254.169.254 is the
	// cloud instance-metadata endpoint on every major provider -- pointing an
	// S3 client at it is SSRF-shaped, not a LAN deployment -- and 169.254.0.0/16
	// generally is autoconfiguration space nobody deliberately hosts MinIO on.
	// The pre-split predicate accepted the whole range via IsLinkLocalUnicast().
	for _, endpoint := range []string{
		"169.254.169.254:9000",
		"169.254.1.1:9000",
		"[fe80::1]:9000",
	} {
		cfg := validProductionConfig()
		cfg.InstanceType = InstanceTypeSelfHosted
		cfg.StorageEndpoint = endpoint
		cfg.StorageUseSSL = false
		err := cfg.validateProduction()
		if err == nil {
			t.Errorf("%q accepted plaintext on a self-hosted deployment", endpoint)
			continue
		}
		if !strings.Contains(err.Error(), "STORAGE_USE_SSL") {
			t.Errorf("%q: error should name STORAGE_USE_SSL, got: %v", endpoint, err)
		}
	}
}

func TestSaaSApprovedIsStrictlyNarrowerThanPrivateNetwork(t *testing.T) {
	// The whole point of the split: the two predicates must not be the same
	// function wearing two names. A refactor that re-merged them would satisfy
	// every other test in this file, because each table exercises only one of
	// the two paths. This one fails the moment they agree everywhere.
	saasOnly := []string{"minio", "localhost", "127.0.0.1", "::1"}
	privateOnly := []string{"10.1.2.3", "172.20.0.5", "192.168.1.50", "fd00::1"}

	for _, host := range saasOnly {
		if !saasApprovedStorageHost(host) || !privateNetworkStorageHost(host) {
			t.Errorf("%q must satisfy BOTH predicates", host)
		}
	}
	for _, host := range privateOnly {
		if saasApprovedStorageHost(host) {
			t.Errorf("%q is SaaS-approved; only the compose name and loopback may be", host)
		}
		if !privateNetworkStorageHost(host) {
			t.Errorf("%q must still be plaintext-eligible for a self-hosted operator", host)
		}
	}
	for _, host := range []string{"169.254.169.254", "169.254.1.1", "fe80::1"} {
		if saasApprovedStorageHost(host) || privateNetworkStorageHost(host) {
			t.Errorf("%q satisfied a predicate; link-local is refused on both paths", host)
		}
	}
}

func TestStorageHostname_IPv4MappedIsJudgedByTheWrappedAddress(t *testing.T) {
	// An IPv4-mapped IPv6 literal round-trips through url.Parse and EqualFold,
	// so classification falls entirely to net.IP's unwrapping. Both directions
	// matter and only one of them is dangerous: if IsPrivate/IsLoopback failed
	// to unwrap, "[::ffff:203.0.113.10]" would read as an unrecognised v6
	// address and be refused (safe) — but if they unwrapped INCONSISTENTLY,
	// a public address could wear a private-looking coat. Pin both.
	//
	// Raised by an adversarial review of the userinfo fix, which observed that
	// [::ffff:127.0.0.1] is accepted. It should be: it IS loopback, and it is
	// the same destination minio-go would dial. The untested half was the
	// public twin, which is the half that would have been a bypass.
	for _, endpoint := range []string{
		"[::ffff:203.0.113.10]:9000",
		"[::ffff:8.8.8.8]:9000",
		"[::ffff:100.64.0.5]:9000", // CGNAT, wrapped — not private, correctly refused
		"[2001:db8::1]:9000",
		"[::ffff:0.0.0.0]:9000",
	} {
		host, ok := storageHostname(endpoint)
		if ok && (privateNetworkStorageHost(host) || saasApprovedStorageHost(host)) {
			t.Errorf("%q classified in-cluster; a public address must not pass", endpoint)
		}
	}
	for _, endpoint := range []string{
		"[::ffff:127.0.0.1]:9000",
		"[::ffff:10.1.2.3]:9000",
		"[::1]:9000",
		"[fd00::1]:9000",
	} {
		host, ok := storageHostname(endpoint)
		if !ok || !privateNetworkStorageHost(host) {
			t.Errorf("%q refused, but it is genuinely in-cluster", endpoint)
		}
	}
}
