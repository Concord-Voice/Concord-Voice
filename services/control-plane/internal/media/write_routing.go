package media

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// Purpose-based write routing on the WRITE rail (ADR-0038 / #2759 unit B3).
//
// This is the write-side counterpart to StoreResolver (backend_store.go), and
// it is a DIFFERENT mechanism on purpose -- conflating the two is how a
// tier-1 write ends up following a knob that was only ever meant to steer
// tier-2 writes. StoreResolver answers "where does THIS EXISTING row live",
// keyed on a media_files.storage_backend column value that was already
// decided; WriteRouter answers "where should a BRAND NEW object of this kind
// go", keyed on the CALLER'S PURPOSE, because there is no row yet to read a
// column from.
//
// Tier 1 (media_tier = 1: avatars/, banners/, server-icons/, server-banners/,
// dm-icons/) is a STATIC rule: ADR-0038 keeps every one of those objects on
// the legacy backend unconditionally and forever, so ResolveTier1WriteStore
// takes no parameter and consults nothing but the legacy identifier -- there
// is no "current write default" question for it to ask, and no per-object
// resolver could express that it must never be asked one. Tier 2
// (attachments/) is the only caller of ResolveAttachmentWriteStore, which is
// free to move where NEW objects land (via
// storage.Registry.AttachmentWriteBackendID) without a single tier-1 call
// site changing.
//
// THE FLIP IS NOT THIS UNIT'S JOB. ResolveAttachmentWriteStore resolves to
// the legacy backend today, because AttachmentWriteBackendID does -- see that
// method's doc comment. What this unit builds is the structural split, so the
// day the flip lands it is a change to ONE function's body, not a hunt
// through every attachments/ call site for one that still assumed the
// process-wide store.
//
// Once a tier-2 object exists, resolving IT again (a chunked session's
// PutUploadChunk/CommitUploadSession/CancelUploadSession, or a single-shot
// abandon-on-failure cleanup) is NOT a write-routing decision any more -- it
// is exactly the per-object question StoreResolver already answers, keyed on
// the backend the object was actually given at open. Those call sites use
// storeForRow, never WriteRouter.

// WriteRouter resolves the object store for a NEW write, by purpose rather
// than by reading an existing row's column.
//
// Declared at the consumer, mirroring StoreResolver/DeleterResolver.
type WriteRouter interface {
	// ResolveTier1WriteStore always resolves the legacy backend.
	ResolveTier1WriteStore() (ObjectStore, error)

	// ResolveAttachmentWriteStore resolves the CURRENT write default for the
	// attachments/ prefix, and returns the identifier to persist on that
	// object's media_files.storage_backend column. An empty backendID means
	// the legacy backend -- leave the column NULL (migration 000114's
	// permanent spelling), never write the literal "legacy".
	ResolveAttachmentWriteStore() (store ObjectStore, backendID string, err error)
}

// registryWriteRouter adapts the boot-time *storage.Registry to WriteRouter.
type registryWriteRouter struct {
	registry *storage.Registry
}

// NewRegistryWriteRouter wires the boot-time backend registry into the
// write-routing rule above.
func NewRegistryWriteRouter(registry *storage.Registry) WriteRouter {
	return registryWriteRouter{registry: registry}
}

func (r registryWriteRouter) ResolveTier1WriteStore() (ObjectStore, error) {
	client, err := r.registry.Resolve(storage.LegacyBackendID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		// Defensive, matching registryStoreResolver: Resolve returns an error
		// whenever it has no client, so this should be unreachable.
		return nil, fmt.Errorf("media: legacy storage backend resolved to no client")
	}
	return client, nil
}

func (r registryWriteRouter) ResolveAttachmentWriteStore() (ObjectStore, string, error) {
	id := r.registry.AttachmentWriteBackendID()
	client, err := r.registry.Resolve(id)
	if err != nil {
		return nil, "", err
	}
	if client == nil {
		return nil, "", fmt.Errorf("media: attachment write backend %q resolved to no client", string(id))
	}
	if id == storage.LegacyBackendID {
		return client, "", nil
	}
	return client, string(id), nil
}

// SetWriteRouter wires purpose-based write routing. Injected rather than
// passed to NewHandler for the same reason as SetStoreResolver: the
// constructor signature stays put. An unwired router keeps the pre-ADR-0038
// behaviour exactly -- every write call site falls back to the single
// process-wide h.store, matching what happened before this unit existed.
func (h *Handler) SetWriteRouter(router WriteRouter) {
	h.writeRouter = router
}

// requireTier1WriteStore resolves the store for a NEW tier-1 write. STATIC:
// it consults nothing but the legacy identifier, ever. Do not replace a call
// to this with requireObjectStore at a tier-1 call site on the theory that
// they resolve to the same thing today -- h.store and "the legacy backend"
// are two different concepts that only coincide because nothing has moved
// h.store away from it, and this function is what keeps tier-1 correct on the
// day something does.
func (h *Handler) requireTier1WriteStore(c *gin.Context) (ObjectStore, bool) {
	if h.writeRouter == nil {
		return h.requireObjectStore(c)
	}
	store, err := h.writeRouter.ResolveTier1WriteStore()
	if err != nil {
		h.log.Error("Could not resolve the legacy storage backend for a tier-1 write", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgStorageUnavailable})
		return nil, false
	}
	return store, true
}

// requireAttachmentWriteStore resolves the store AND the backend identifier
// for a NEW tier-2 attachment write, consulting the current write default.
// backendID is "" when the object belongs on the legacy backend -- pass it
// straight through to attachmentParams.storageBackend (or the chunked
// session record's backend field), which leaves media_files.storage_backend
// NULL.
func (h *Handler) requireAttachmentWriteStore(c *gin.Context) (ObjectStore, string, bool) {
	if h.writeRouter == nil {
		store, ok := h.requireObjectStore(c)
		return store, "", ok
	}
	store, backendID, err := h.writeRouter.ResolveAttachmentWriteStore()
	if err != nil {
		h.log.Error("Could not resolve the attachment write backend", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgStorageUnavailable})
		return nil, "", false
	}
	return store, backendID, true
}

// checkAttachmentDiskWatermark applies the shared-disk occupancy gate, but ONLY
// to a write that lands on the legacy backend.
//
// The watermark statfs's the local host disk because that is the harm it
// exists to prevent: MinIO shares that disk with Postgres, so an unbounded
// upload path can fill it and stop the database accepting writes. A write that
// lands at a VENDOR backend consumes none of that disk. Gating it on local
// occupancy would be backwards — the vendor is the relief valve for exactly
// this pressure, so refusing there would turn a full disk into a total
// attachment outage instead of the migration that resolves it.
//
// backendID is what requireAttachmentWriteStore returned: empty means legacy
// (the column stays NULL), which is the only case this gate covers. Resolving
// the backend must therefore come FIRST — a reviewer suggestion to swap the two
// for efficiency was declined for this reason, since it forecloses knowing
// which disk the bytes are about to land on.
func (h *Handler) checkAttachmentDiskWatermark(c *gin.Context, backendID string) bool {
	if backendID != "" {
		return true
	}
	if err := h.diskWatermark.Check(); err != nil {
		c.JSON(http.StatusInsufficientStorage, gin.H{"error": errMsgAttachmentStorageAtCapacity})
		return false
	}
	return true
}
