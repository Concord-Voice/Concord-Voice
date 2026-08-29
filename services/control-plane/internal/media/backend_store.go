package media

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// Per-object storage-backend resolution on the READ path (ADR-0038 / #2759).
//
// A media_files row names the store its object lives in. Reads must resolve
// that name instead of assuming the one process-wide store, because after the
// write-default flip the two stop being the same thing for attachments/ while
// staying the same thing forever for profile media.
//
// SCOPE: the tier-2 attachment download only. The tier-1 proxies
// (proxyTier1Media, proxyInviteIcon, proxyFriendCodeAvatarObject) derive their
// key deterministically from the subject and read no media_files row, and
// ADR-0038 keeps ALL profile media on MinIO unconditionally and forever — so
// they correctly keep using the process-wide store, and giving them a resolver
// would imply a placement decision that does not exist for them. Write and
// delete paths are out of scope here (units B2/B3).

// StoreResolver resolves a media_files.storage_backend column value to the
// object store that actually holds that row's object.
//
// Declared at the CONSUMER and returning ObjectStore rather than
// *storage.Client so the download path stays mockable with the same fakes
// every other media test uses.
type StoreResolver interface {
	// ResolveStore returns the store for a storage_backend value. A nil
	// backend is a NULL column and MUST resolve to the legacy backend; any
	// value the resolver does not recognize MUST return an error rather than
	// a default store.
	ResolveStore(backend *string) (ObjectStore, error)
}

// registryStoreResolver adapts the boot-time *storage.Registry to
// StoreResolver.
type registryStoreResolver struct {
	registry *storage.Registry
}

// NewRegistryStoreResolver wires the boot-time backend registry into the media
// handler. Built here rather than in internal/api so the router keeps its
// current import set and the adapter lives beside the interface it satisfies.
func NewRegistryStoreResolver(registry *storage.Registry) StoreResolver {
	return registryStoreResolver{registry: registry}
}

func (r registryStoreResolver) ResolveStore(backend *string) (ObjectStore, error) {
	client, err := r.registry.ResolveRow(backend)
	if err != nil {
		return nil, err
	}
	if client == nil {
		// Defensive: ResolveRow returns an error whenever it has no client.
		// Boxing a nil *storage.Client into ObjectStore would produce a
		// non-nil interface holding nil, defeating every downstream nil check
		// and turning a fail-closed 503 into a panic.
		return nil, fmt.Errorf("media: storage backend %s resolved to no client",
			describeBackend(backend))
	}
	return client, nil
}

// SetStoreResolver wires per-object backend resolution. Injected rather than
// passed to NewHandler for the same reason as SetSessionRedis and
// SetDiskWatermark: the constructor signature stays put. An unwired resolver
// keeps the pre-ADR-0038 behaviour for legacy rows and fails closed for every
// other backend — see storeForRow.
func (h *Handler) SetStoreResolver(resolver StoreResolver) {
	h.backends = resolver
}

// storeForRow returns the object store holding the object for a media_files
// row whose storage_backend column is backend (nil for a NULL column).
func (h *Handler) storeForRow(backend *string) (ObjectStore, error) {
	if h.backends != nil {
		return h.backends.ResolveStore(backend)
	}

	// No registry wired. The process-wide store IS the legacy backend, so a
	// NULL or legacy row resolves exactly as it did before this change, and
	// every other identifier fails closed rather than being read out of a
	// store that does not hold it. This is the one arm where "no resolver" is
	// safe, and it is safe only because it refuses to guess.
	if isLegacyBackend(backend) {
		if h.store == nil {
			return nil, errors.New("media: object storage is not configured")
		}
		return h.store, nil
	}
	return nil, fmt.Errorf(
		"media: no storage backend registry is wired; refusing to read a %q object from the legacy store",
		*backend)
}

// requireObjectStoreForRow is requireObjectStore's per-object twin: it answers
// 503 with the identical body, so a client cannot tell an unresolvable backend
// from an unconfigured store.
func (h *Handler) requireObjectStoreForRow(c *gin.Context, backend *string) (ObjectStore, bool) {
	store, err := h.storeForRow(backend)
	if err != nil {
		h.log.Error("Could not resolve the storage backend for a media file",
			"error", err, "storage_backend", describeBackend(backend))
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgStorageUnavailable})
		return nil, false
	}
	return store, true
}

// describeBackend renders a nullable storage_backend for a log field, keeping
// NULL distinguishable from any string a row could legitimately carry.
func describeBackend(backend *string) string {
	if backend == nil {
		return "<null>"
	}
	return *backend
}
