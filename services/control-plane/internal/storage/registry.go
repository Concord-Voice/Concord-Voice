package storage

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// Boot-time object-storage backend registry (ADR-0038 / #2759).
//
// Placement is per object, not per process: media_files.storage_backend names
// the store each object lives in, and no object migrates. The registry is the
// one place that turns such a name into a client, and it is the VALID SET for
// that column — migration 000114 deliberately carries no CHECK constraint
// precisely so a new backend is a config entry rather than a migration.
//
// One live entry today (the legacy backend), plus one per credentialed
// non-legacy backend from config.AttachmentBackends. The four-backend
// construction switch in New (minio|s3|r2|b2) is untouched: this multiplies
// INSTANCES, not code paths.

// BackendID is a media_files.storage_backend value.
type BackendID string

// LegacyBackendID is the backend a NULL storage_backend resolves to.
//
// IT IS A CODE CONSTANT AND IT IS NEVER READ FROM CONFIGURATION, which is the
// entire point of it existing separately. NULL means "the configured legacy
// backend" permanently — migration 000114 states that it is never backfilled —
// so the key NULL resolves through must be one that the future write-default
// flip cannot move. If NULL resolved through the write default (or through
// STORAGE_BACKEND, which is the same value wearing a different name today),
// then on the day the flip lands every pre-cutover attachment and all profile
// media would silently re-point at a store that does not hold their bytes:
// a 404 for an object that exists, at the moment of the flip, for the entire
// historical corpus. Flipping the write target must change where NEW objects
// are written and nothing else.
//
// The value names the ROLE, not the vendor. The legacy store is MinIO for SaaS
// and for every self-hosted / dev / air-gapped deployment, but a self-hoster
// may point STORAGE_BACKEND at s3/r2/b2, and their pre-cutover rows are still
// "legacy". Registering it under the vendor name would make that claim false
// and would collide with the vendor identifiers in config's backend table.
const LegacyBackendID BackendID = "legacy"

var (
	// ErrBackendUnknown is returned for a storage_backend value no registry
	// entry claims. It is deliberately NOT a fallback to any default — see
	// Resolve.
	ErrBackendUnknown = errors.New("storage: unknown storage backend")

	// ErrBackendUnavailable is returned for a backend that is registered but
	// could not be constructed, or was constructed with no client.
	ErrBackendUnavailable = errors.New("storage: storage backend unavailable")
)

// Registry maps backend identifiers to constructed clients.
//
// Populated once, at boot, by NewRegistry and never mutated afterwards, which
// is what makes the lock-free map reads on the download path safe. Anything
// that would register or replace a backend at runtime needs a lock first —
// there is no reason to want one: a backend set that can change under a
// request is a backend set that can change between resolving a row and
// reading its bytes.
type Registry struct {
	entries map[BackendID]backendEntry
	// attachmentWrite is the selector captured at boot. It intentionally has no
	// fallback: an armed but unusable backend must fail the new write closed.
	attachmentWrite BackendID
	// order preserves registration order (legacy first, then every non-legacy
	// backend that actually got an entry) -- see BackendIDs. A plain map
	// iteration would work for Resolve/ResolveRow, which only ever look up one
	// key, but a caller that must operate over EVERY backend (the session
	// sweeper, ADR-0038 / #2759 unit B3) needs a stable list, not Go's
	// randomized map order, or its per-backend log lines reshuffle every run
	// for no reason.
	order []BackendID
}

// backendEntry is one registered backend. Exactly one of client/err is set:
// a non-nil err marks the backend UNAVAILABLE, which is a distinct state from
// unknown — both fail closed, but only one of them names a backend the
// operator meant to have.
type backendEntry struct {
	client *Client
	err    error
}

// NewRegistry builds the boot-time registry.
//
// legacy is the process-wide client main already constructs (and may be nil
// when object storage is unconfigured); it is registered under
// LegacyBackendID rather than reconstructed, so there is exactly one legacy
// client and its existing retry/fatal boot semantics are unchanged.
//
// PER-BACKEND FAULT ISOLATION: this never returns an error and never aborts
// boot. A non-legacy backend that cannot be constructed registers as
// UNAVAILABLE and yields a fail-closed error for the rows that name it —
// nothing else. An R2 credential or destination problem must not be able to
// stop the control plane from starting, because that would take down auth,
// messaging, voice and the entire MinIO-resident corpus over a store that
// (while the write default still points at MinIO) holds no objects at all.
func NewRegistry(cfg *config.Config, legacy *Client, log *logger.Logger) *Registry {
	registry := &Registry{
		entries:         make(map[BackendID]backendEntry, 2),
		attachmentWrite: LegacyBackendID,
	}

	if legacy != nil {
		registry.entries[LegacyBackendID] = backendEntry{client: legacy}
	} else {
		// Unconfigured object storage. Registered rather than omitted so a
		// NULL row reports "the legacy backend is unavailable" instead of
		// "legacy is not a backend", which is a materially different thing to
		// read in a log at 3am.
		registry.entries[LegacyBackendID] = backendEntry{
			err: errors.New("object storage is not configured (STORAGE_ENDPOINT/MINIO_ENDPOINT empty)"),
		}
	}
	registry.order = append(registry.order, LegacyBackendID)

	if cfg == nil {
		return registry
	}
	registry.attachmentWrite = BackendID(cfg.AttachmentWriteBackend)
	if registry.attachmentWrite == "" {
		registry.attachmentWrite = LegacyBackendID
	}
	for _, backend := range cfg.AttachmentBackends() {
		registry.register(backend, log)
	}
	if _, err := registry.Resolve(registry.attachmentWrite); err != nil {
		log.Error("ATTACHMENT WRITE BACKEND UNUSABLE", "backend", string(registry.attachmentWrite), "error", err)
	} else {
		log.Info("Attachment write default", "backend", string(registry.attachmentWrite))
	}
	return registry
}

// register adds one non-legacy backend, recording a construction failure as an
// UNAVAILABLE entry rather than propagating it.
func (r *Registry) register(backend config.ObjectBackend, log *logger.Logger) {
	id := BackendID(backend.ID)

	// A config entry that claims the legacy identifier would re-point every
	// NULL row — the whole pre-cutover corpus — at a vendor bucket. Refuse it
	// outright; the legacy entry already in the map stays authoritative.
	// An empty identifier is refused for the same reason the legacy one is:
	// it would be REACHABLE from data. A media_files row whose storage_backend
	// is a non-NULL empty string would resolve to this entry, when the
	// documented contract is that only NULL means legacy and any unrecognised
	// value fails closed. Nothing declares an empty ID today, so this is
	// defence in depth -- but it belongs beside the other two guards, in the
	// one function that owns registration, rather than being an invariant the
	// candidate table is merely trusted to uphold.
	if id == "" {
		log.Error("Refusing an object storage backend with an empty identifier")
		return
	}
	if id == LegacyBackendID {
		log.Error("Refusing an object storage backend that claims the legacy identifier",
			"backend", string(id))
		return
	}
	if _, exists := r.entries[id]; exists {
		log.Error("Duplicate object storage backend identifier; keeping the entry already registered",
			"backend", string(id))
		return
	}

	client, err := newVendorClient(backend, log)
	if err != nil {
		r.entries[id] = backendEntry{err: err}
		r.order = append(r.order, id)
		log.Error("Object storage backend unavailable; objects naming it will fail closed",
			"backend", string(id), "error", err)
		return
	}

	r.entries[id] = backendEntry{client: client}
	r.order = append(r.order, id)
	// Endpoint/Region/Bucket are non-secret pinned literals; the credential is
	// never logged in any form.
	log.Info("Object storage backend registered",
		"backend", string(id), "endpoint", backend.Endpoint, "bucket", backend.Bucket)
}

// Resolve returns the client for an explicit backend identifier.
//
// FAILS CLOSED on anything it does not recognize. There is no fallback to the
// legacy backend or to any other default, and adding one would be a data bug
// rather than a resilience feature: an unrecognized identifier means the row
// points somewhere this build cannot reach, so reading it from a different
// store either 404s an object that exists or serves bytes from the wrong
// bucket. 503 is the only honest answer.
func (r *Registry) Resolve(id BackendID) (*Client, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: registry not constructed", ErrBackendUnavailable)
	}

	entry, ok := r.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrBackendUnknown, string(id))
	}
	if entry.err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrBackendUnavailable, string(id), entry.err)
	}
	if entry.client == nil {
		return nil, fmt.Errorf("%w %q", ErrBackendUnavailable, string(id))
	}
	return entry.client, nil
}

// ResolveRow returns the client holding the object for a media_files row whose
// storage_backend column is backend (nil for a NULL column).
//
// NULL IS RESOLVED TO THE LEGACY BACKEND BEFORE THE MAP LOOKUP, never as a
// fallback after a miss. A literal lookup on the column value treats NULL as a
// miss, and since NULL is the permanent state of every pre-cutover attachment
// AND of all profile media, that reading 503s essentially the entire corpus.
//
// An EMPTY STRING is not NULL and does not resolve here: a non-NULL value
// naming no backend is a data defect, and it fails closed like any other
// unrecognized identifier.
func (r *Registry) ResolveRow(backend *string) (*Client, error) {
	if backend == nil {
		return r.Resolve(LegacyBackendID)
	}
	return r.Resolve(BackendID(*backend))
}

// BackendIDs returns every backend identifier this registry holds, in
// registration order (legacy first) -- including one this build could not
// construct.
//
// Resolve/ResolveRow answer "where does THIS row live"; this answers "what
// backends exist at all", which is what a caller that must operate over
// EVERY backend needs -- the session sweeper (ADR-0038 / #2759 unit B3) is
// the first one. An UNAVAILABLE backend is still a registered backend, and
// the caller has to know it exists so it can log its own resolution failure
// as a distinct event from "this backend legitimately had no work this
// tick" -- collapsing the two is exactly how a sweeper that silently stopped
// covering a backend reports as a clean run forever.
func (r *Registry) BackendIDs() []BackendID {
	if r == nil {
		return nil
	}
	out := make([]BackendID, len(r.order))
	copy(out, r.order)
	return out
}

// AttachmentWriteBackendID names the backend a NEW tier-2 attachment write
// (the attachments/ prefix) should target right now.
func (r *Registry) AttachmentWriteBackendID() BackendID {
	if r == nil || r.attachmentWrite == "" {
		return LegacyBackendID
	}
	return r.attachmentWrite
}

// newVendorClient builds a client for a non-legacy backend.
//
// It performs NO NETWORK I/O AND NEVER CALLS ensureBucket, for two independent
// reasons either of which is sufficient:
//
//   - Fault isolation. ensureBucket is reached from newS3Client on the legacy
//     path, where a failure feeds initStorageClient's retry-then-fatal boot
//     behaviour. That is defensible for the store every deployment depends on;
//     it is indefensible for a vendor backend, where a provider outage or an
//     expired token would stop the entire control plane from booting.
//   - The credential cannot do it. ADR-0038 mandates a bucket-scoped Object
//     Read+Write token, which deliberately CANNOT create a bucket — so
//     ensureBucket's MakeBucket (and, on a tightly scoped token, its
//     BucketExists probe) would fail on a CORRECTLY configured credential.
//
// Construction is therefore pure and local: only a malformed destination can
// fail here, and that failure is recorded as UNAVAILABLE. A live outage
// surfaces per request at GetObject, which is already handled and is already
// scoped to the objects that actually live there.
func newVendorClient(backend config.ObjectBackend, log *logger.Logger) (*Client, error) {
	if backend.Bucket == "" {
		return nil, fmt.Errorf("storage: backend %q has no bucket configured", backend.ID)
	}

	host, err := vendorEndpointHost(backend.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("storage: backend %q: %w", backend.ID, err)
	}

	// The DESTINATION check, and the only one there is. vendorEndpointHost is
	// a scheme-and-shape reduction -- it proves the endpoint is an https URL
	// with a host and no path, and says nothing whatever about WHICH host. On
	// its own it accepts attacker.example.com, 169.254.169.254, and
	// abc.r2.cloudflarestorage.com.attacker.example alike.
	//
	// That is tolerable only while the endpoint is a literal in reviewed
	// source, which makes review the control. This turns that convention into
	// something the code enforces: ExpectedHostSuffix is declared beside the
	// candidate row in pkg/config/object_backends.go and is never read from
	// the environment, so an endpoint redirected through the env cannot also
	// move the goalpost it is measured against.
	if err := assertExpectedHost(host, backend.ExpectedHostSuffix); err != nil {
		return nil, fmt.Errorf("storage: backend %q: %w", backend.ID, err)
	}

	// NewCore for parity with newS3Client: Core embeds *Client, so every
	// method on our Client works identically against either instance, and the
	// multipart primitives stay available. See the NOTE in client.go about
	// the deliberate .Client hop at the call sites.
	core, err := minio.NewCore(host, &minio.Options{
		// Third argument is the STS session token, NOT the region (#1611).
		Creds: credentials.NewStaticV4(backend.AccessKeyID, backend.SecretAccessKey, ""),
		// Not configurable, unlike the in-cluster MinIO dial's STORAGE_USE_SSL:
		// a vendor backend is reached across the public internet and
		// config.validateProduction rejects a non-https endpoint on the way in.
		Secure: true,
		Region: backend.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: create client for backend %q: %w", backend.ID, err)
	}

	return &Client{minio: core, bucket: backend.Bucket, log: log}, nil
}

// vendorEndpointHost reduces a pinned https:// destination URL to the
// host[:port] form minio-go expects.
//
// The legacy STORAGE_ENDPOINT is already host:port ("minio:9000") with TLS
// carried separately by STORAGE_USE_SSL, but a vendor destination is written
// as a full URL (CLOUDFLARE_R2_USEAST_ENDPOINT is validated as https://... in
// config). Handing that URL to minio-go unchanged yields a client that dials a
// host literally named "https", so the reduction is mandatory rather than
// cosmetic.
func vendorEndpointHost(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("endpoint is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse endpoint: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("endpoint %q must use https://", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("endpoint %q has no host", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("endpoint %q must not carry a path", raw)
	}
	return parsed.Host, nil
}

// assertExpectedHost requires host to sit inside the vendor domain the backend
// declares in reviewed source.
//
// host arrives from vendorEndpointHost, i.e. it is url.Parse's view of the
// destination and may still carry ":port". Matching happens on the hostname
// alone -- a port cannot redirect anything once the name is pinned, so
// rejecting one would buy no safety and would break a future backend that
// needs a non-443 vendor endpoint.
//
// The comparison is case-folded because DNS names are case-insensitive and
// url.Parse does not normalise them, so "R2.CloudflareStorage.Com" is the same
// destination and must not be read as a different one.
//
// An empty suffix is refused rather than treated as "no opinion". A backend
// row that omits it is a code defect, and the fail-open reading of that defect
// is precisely the state this function was added to end.
func assertExpectedHost(host, suffix string) error {
	if suffix == "" {
		return errors.New("no ExpectedHostSuffix declared for this backend (code defect: every candidate row must pin its vendor domain)")
	}
	// The leading dot is what makes this a DOMAIN check rather than a string
	// check. Without it, a suffix of "r2.cloudflarestorage.com" is satisfied by
	// the separately-registrable "notr2.cloudflarestorage.com". Enforced rather
	// than merely documented, because the undotted form is the easy mistake and
	// nothing else in the pipeline would catch it.
	if !strings.HasPrefix(suffix, ".") {
		return fmt.Errorf("ExpectedHostSuffix %q must begin with a dot, or a lookalike registration satisfies it", suffix)
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if !strings.HasSuffix(name, strings.ToLower(suffix)) {
		return fmt.Errorf("endpoint host %q is not inside %q", host, suffix)
	}
	return nil
}
