package config

import "fmt"

// Non-legacy object-storage backends (ADR-0038 / #2759).
//
// ADR-0038 DEMOTES MinIO, it does not remove it: MinIO stays permanently as
// the self-host / dev / air-gapped backend and keeps ALL profile media
// (avatars/, server-icons/, dm-icons/) unconditionally and forever.
//
// "Forever" is a PLAINTEXT decision, not a limitation of the mechanism below.
// A tier-2 attachment reaches the store as client-encrypted ciphertext, so a
// vendor holding the attachment corpus holds bytes it cannot read. Profile
// media is re-encoded server-side by processTier1Image to enforce dimension
// and format limits, so it is plaintext wherever it lands. Renting a blind S3
// ceiling works only while the ceiling stays blind; attachments keep it blind
// and profile media would not. Extending per-object resolution to tier-1 would
// not change that -- see ADR-0038 § Decision, reason 0. What moves
// is the SaaS write target for the attachments/ prefix, and it moves PER
// OBJECT — media_files.storage_backend names the store each object actually
// lives in, and no object migrates.
//
// This file is the declarative table of the NON-LEGACY stores the control
// plane may be asked to read from. The legacy backend is deliberately absent
// from it: it is constructed from the existing STORAGE_* block and resolved
// through a code constant (storage.LegacyBackendID) precisely so that no
// configuration change — least of all the future write-default flip — can
// re-point the rows that carry a NULL storage_backend. See that constant's
// doc comment for the full argument.
//
// ADDING A BACKEND IS AN ENTRY IN AttachmentBackends, NOT A CODE CHANGE
// ELSEWHERE. The owner has already provisioned the EU and Indo-Pacific
// buckets; bringing one online is a new candidate row here plus its pinned
// destination block and credential pair. Nothing in internal/storage knows a
// vendor name, so the registry gains a backend without gaining a branch.

// AttachmentBackendR2USEast identifies the Cloudflare R2 US-East attachment
// bucket.
//
// Region-qualified on purpose. The EU and Indo-Pacific buckets join this same
// identifier namespace later, and an identifier that has reached
// media_files.storage_backend can never be renamed — a rename orphans every
// row carrying the old value, which fails closed (503) rather than serving,
// but fails closed forever. A bare "r2" would have needed exactly that rename.
const AttachmentBackendR2USEast = "r2-useast"

// ObjectBackend is one non-legacy object-storage destination plus the
// credential that reaches it.
//
// Endpoint/Region/Bucket are non-secret destination LITERALS pinned in
// docker-compose.production.yml, never sourced from an operator-writable env
// var — see CloudflareR2Config for why a concord-writable destination is a
// redirection channel for a compromised host. AccessKeyID/SecretAccessKey are
// the bucket-scoped Object Read+Write credential from Infisical.
type ObjectBackend struct {
	// ID is the value that appears in media_files.storage_backend for objects
	// held by this backend. Stable forever once a row carries it.
	ID       string
	Endpoint string // Absolute https:// URL; TLS is mandatory, there is no *_USE_SSL escape hatch
	// ExpectedHostSuffix is the vendor domain Endpoint's PARSED host must end
	// in. It is a literal declared in the candidate row below — never read
	// from the environment — and that asymmetry is the whole point: the
	// environment supplies the destination, reviewed source states which
	// vendor that destination is allowed to be, and storage.newVendorClient
	// refuses to build a client when they disagree.
	//
	// Checked against the parsed host rather than the raw URL, because the
	// raw string is exactly what a redirect hides in. An endpoint ending
	// "#.r2.cloudflarestorage.com" -- or "?.r2.cloudflarestorage.com" --
	// ENDS with the suffix as text while url.Parse resolves its host to
	// whatever preceded the "#" or "?". Suffix-matching the raw string would
	// wave both through.
	//
	// Userinfo ("https://a.r2.cloudflarestorage.com@evil.example.com") is a
	// different animal and is called out here because it is easy to lump in
	// with the other two: it misleads a HUMAN reading left to right, but it
	// cannot fool a raw suffix match, since the host has to come last for the
	// raw string to end in the suffix at all. Parsing catches it either way.
	//
	// It must also be a DOTTED suffix, so that a lookalike registration like
	// "notr2.cloudflarestorage.com" cannot satisfy it. assertExpectedHost
	// enforces that rather than trusting this comment.
	//
	// Empty is a CODE bug, not a permissive default: newVendorClient fails
	// closed on it rather than skipping the check, so a future backend row
	// that forgets this field is refused loudly instead of silently
	// unguarded.
	ExpectedHostSuffix string
	Region             string
	Bucket             string
	AccessKeyID        string // #nosec G117 -- config field name, secret loaded from env
	SecretAccessKey    string // #nosec G101 -- config field name, secret loaded from env
}

// String redacts both credential fields, mirroring CloudflareR2Config.String.
func (b ObjectBackend) String() string {
	return fmt.Sprintf("ObjectBackend{ID:%q Endpoint:%q Region:%q Bucket:%q AccessKeyID:[REDACTED %d bytes] SecretAccessKey:[REDACTED %d bytes]}",
		b.ID, b.Endpoint, b.Region, b.Bucket, len(b.AccessKeyID), len(b.SecretAccessKey))
}

// AttachmentBackends returns every non-legacy backend this deployment holds a
// complete credential for. The result is the registry's whole input: a
// backend that is not returned here is never constructed, and a row naming it
// fails closed at resolution rather than reading from some other store.
//
// The CREDENTIAL PAIR is the gate, not the destination literals, and that is a
// deliberate reading of the current deployment: docker-compose.production.yml
// pins CLOUDFLARE_R2_USEAST_ENDPOINT/_BUCKET as REAL literals, while the
// credential pair resolves from Infisical through a `:-` empty default and may
// or may not be populated on a given host — self-host never populates it.
// Keying on the destination would therefore register a live entry for a bucket
// nobody can authenticate to. An
// operator supplying the credential is the one unambiguous expression of "this
// backend is real"; a destination that is then missing or malformed is a
// misconfiguration the registry reports as UNAVAILABLE rather than silently
// skipping.
func (c *Config) AttachmentBackends() []ObjectBackend {
	if c == nil {
		return nil
	}

	live := make([]ObjectBackend, 0)
	for _, backend := range attachmentBackendCandidates(c) {
		if backend.AccessKeyID == "" || backend.SecretAccessKey == "" {
			continue
		}
		live = append(live, backend)
	}
	return live
}

// attachmentBackendCandidates is the UNFILTERED declaration table -- every
// backend this build knows how to reach, before the credential gate decides
// which of them this deployment actually holds.
//
// Split out from AttachmentBackends for one reason: a dormant row (the EU and
// Indo-Pacific buckets, whose credentials are empty until an operator supplies
// them) is invisible in the filtered result, so a guard test written against
// the filtered result silently skips exactly the rows most likely to have been
// added in a hurry. An omitted ExpectedHostSuffix on such a row still fails
// closed at boot, but "discovered at boot" is a worse place to learn it than
// "discovered in CI".
func attachmentBackendCandidates(c *Config) []ObjectBackend {
	if c == nil {
		return nil
	}
	return []ObjectBackend{
		{
			ID:       AttachmentBackendR2USEast,
			Endpoint: c.CloudflareR2.Endpoint,
			// Reviewed-source counterweight to the env-supplied Endpoint above.
			ExpectedHostSuffix: ".r2.cloudflarestorage.com",
			Region:             c.CloudflareR2.Region,
			Bucket:             c.CloudflareR2.Bucket,
			AccessKeyID:        c.CloudflareR2.AccessKeyID,
			SecretAccessKey:    c.CloudflareR2.SecretAccessKey,
		},
		// The dormant EU and Indo-Pacific buckets land here: one row each,
		// plus their own pinned destination block on Config. No change to
		// internal/storage, to the resolution path, or to the download handler.
		// Every row MUST declare ExpectedHostSuffix; the guard test iterates
		// this function precisely so a row that forgets it reds CI.
	}
}

// AttachmentBackendCandidatesForTest exposes the unfiltered declaration table
// to the storage package's guard test. The candidate list is deliberately
// unexported -- callers must go through AttachmentBackends so the credential
// gate is never bypassed in production -- but the guard has to see the DORMANT
// rows the gate removes, which is the whole reason it exists.
func AttachmentBackendCandidatesForTest(c *Config) []ObjectBackend {
	return attachmentBackendCandidates(c)
}
