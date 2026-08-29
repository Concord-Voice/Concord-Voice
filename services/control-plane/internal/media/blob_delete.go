package media

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// Per-object storage-backend resolution on the DELETE rail (ADR-0038 / #2759).
//
// THE INVARIANT THIS FILE EXISTS TO PROTECT:
//
//	A media_files row may be recorded as erased ONLY after its object has been
//	deleted from the backend THAT ROW NAMES.
//
// What breaks it is a property of S3 rather than of our code: a DELETE against
// a key that is absent from the target bucket returns SUCCESS. So a
// single-store delete rail hands a vendor-resident key to MinIO, gets nil back,
// and stamps the erasure marker — after which the row leaves the straggler
// sweep's candidate set (`blob_reaped_at IS NULL`) PERMANENTLY while the object
// survives at the vendor. Postgres records an erasure that did not happen, and
// the retry signal that would have caught it has been destroyed. Message
// purges (000090) run through that rail.
//
// ACCOUNT DELETION (000059) DOES NOT, and this file must not be read as if it
// did. deleteAccount issues `DELETE FROM users WHERE id = $1` and holds no
// reference to object storage of any kind; `media_files.uploader_id` is
// `REFERENCES users(id) ON DELETE CASCADE` (migration 000042), so the rows are
// HARD-deleted by the cascade and the objects are simply left on the backend.
// Nothing reclaims them afterwards either — the straggler sweep selects from
// media_files, and the rows it would need are the ones that just vanished.
//
// That makes the residue permanent rather than delayed, and it is worth stating
// rather than implying away: GDPR Article 17 erasure is implemented AS account
// deletion, so those bytes outlive the erasure meant to remove them. Closing it
// needs a design this change does not carry — capture the BlobRefs inside the
// deletion transaction before the cascade fires, or add a backend-side orphan
// reaper that can work without a row to start from.
//
// Note the asymmetry with the read rail in backend_store.go: an unresolvable
// backend there fails CLOSED (503 — the client is told nothing was served).
// Here the equivalent failure is silent and irreversible, so every path in this
// file refuses to record what it did not achieve and leaves the row a retry
// candidate instead.

// BlobRef identifies one stored object: the storage key AND the backend that
// holds it.
//
// The pair is the identity, not the key. Two rows may carry the same
// storage_key on different backends (the partial unique index in migration
// 000042 is on `storage_key` alone and covers only LIVE rows, so soft-deleted
// rows may freely share a key), and those are two distinct objects in two
// distinct buckets. Every delete-rail query is therefore keyed on the pair —
// carrying the key alone is exactly what let one delete stamp another
// backend's row.
type BlobRef struct {
	// Key is media_files.storage_key.
	Key string
	// Backend is media_files.storage_backend. NIL IS A NULL COLUMN, which
	// means the legacy backend permanently — migration 000114 states it is
	// never backfilled — and is the state of every pre-cutover attachment and
	// of all profile media.
	Backend *string
}

// NewBlobRef builds a BlobRef from a scanned nullable storage_backend column.
//
// It copies the string rather than taking the address of the caller's
// sql.NullString: scan loops reuse one variable per row, so an escaping pointer
// would leave every ref in the batch aliased to the last row read.
func NewBlobRef(key string, backend sql.NullString) BlobRef {
	if !backend.Valid {
		return BlobRef{Key: key}
	}
	value := backend.String
	return BlobRef{Key: key, Backend: &value}
}

// BackendLabel renders this ref's backend for a log field, keeping NULL
// distinguishable from any string a row could legitimately carry.
func (b BlobRef) BackendLabel() string {
	return describeBackend(b.Backend)
}

// DeleterResolver resolves a media_files.storage_backend column value to the
// store that actually holds that row's object, narrowed to the one operation
// the delete rail performs.
//
// Declared at the consumer and narrower than StoreResolver on purpose: the
// reaper's fakes need one method, not the eleven ObjectStore carries.
type DeleterResolver interface {
	// ResolveDeleter returns the store for a storage_backend value. A nil
	// backend is a NULL column and MUST resolve to the legacy backend; any
	// value the resolver does not recognize MUST return an error rather than
	// a default store.
	ResolveDeleter(backend *string) (ObjectDeleter, error)
}

// NewDeleterResolver builds the delete rail's resolver from whatever the
// deployment has.
//
// stores (the boot-time backend registry, via NewRegistryStoreResolver) WINS
// whenever it is present, and the legacy store is then ignored entirely. That
// is deliberate: the registry holds the legacy client as a concrete
// *storage.Client and compares it against nil correctly, whereas legacy here is
// an interface that main may have filled with a typed-nil pointer. Preferring
// the registry keeps the production delete rail off that hazard.
//
// With no registry the resolver still refuses to guess: NULL and legacy rows
// resolve to the process-wide store exactly as they did before ADR-0038, and
// every other identifier fails closed rather than being deleted from a store
// that does not hold it. This is the same two-arm shape as Handler.storeForRow,
// and it is safe only because it refuses to guess.
func NewDeleterResolver(stores StoreResolver, legacy ObjectDeleter) DeleterResolver {
	if stores != nil {
		return registryDeleterResolver{stores: stores}
	}
	return legacyDeleterResolver{legacy: legacy}
}

// registryDeleterResolver adapts the read rail's StoreResolver — and through it
// the one boot-time *storage.Registry — to the delete rail.
//
// Wrapping rather than taking a second registry handle is what makes it
// impossible to wire one rail and forget the other: reads and deletes resolve
// through the identical ResolveRow call, so they cannot disagree about where an
// object lives.
type registryDeleterResolver struct {
	stores StoreResolver
}

func (r registryDeleterResolver) ResolveDeleter(backend *string) (ObjectDeleter, error) {
	store, err := r.stores.ResolveStore(backend)
	if err != nil {
		return nil, err
	}
	if store == nil {
		// Defensive, for the same reason as the twin check in
		// registryStoreResolver: a nil boxed into ObjectDeleter is a non-nil
		// interface, which would turn a fail-closed skip into a panic.
		return nil, fmt.Errorf("media: storage backend %s resolved to no store",
			describeBackend(backend))
	}
	return store, nil
}

// legacyDeleterResolver is the no-registry arm.
type legacyDeleterResolver struct {
	legacy ObjectDeleter
}

func (r legacyDeleterResolver) ResolveDeleter(backend *string) (ObjectDeleter, error) {
	if !isLegacyBackend(backend) {
		return nil, fmt.Errorf(
			"media: no storage backend registry is wired; refusing to delete a %q object from the legacy store",
			*backend)
	}
	if r.legacy == nil {
		return nil, errors.New("media: object storage is not configured")
	}
	return r.legacy, nil
}

// isLegacyBackend reports whether a storage_backend column value names the
// legacy backend — either by being NULL (the permanent state of every
// pre-cutover object) or by naming it explicitly.
//
// Shared by both rails so the two cannot drift apart on what "legacy" means;
// a read path and a delete path that disagree on that would serve an object
// from one bucket and erase it from another.
func isLegacyBackend(backend *string) bool {
	return backend == nil || *backend == string(storage.LegacyBackendID)
}
