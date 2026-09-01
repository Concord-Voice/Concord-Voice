package media

// Chunked attachment upload sessions (#2157 PR 2, spec §4.2/§4.3).
//
// The client encrypts an attachment into a v2 envelope -- a 28-byte header
// followed by N independently-sealed 8 MiB chunks -- and uploads it one chunk
// per request against an S3 multipart upload. Per-chunk requests are the only
// transport that survives their own duration: apiFetch replays a request on a
// 401 refresh, a 256 MiB body cannot be replayed, and Cloudflare 413s anything
// over 100 MB at the edge regardless.
//
// THE SERVER NEVER PARSES THE v2 HEADER. Everything below validates arithmetic
// on part lengths and nothing else. A server-side header parse would make the
// server the format selector, which is exactly the trust inversion the in-band,
// AAD-bound discriminator exists to avoid -- Concord is self-hostable, so an
// honest server is not an assumption the client gets to make.
//
// session_id is a BEARER CAPABILITY scoped to one user. It is never logged, at
// any level, in any message.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// Route paths for the four session routes, relative to the protected /media
// group. They live here rather than inline in router.go so the registration and
// the route-shape test cannot drift apart -- and so the storage-not-configured
// stubs register on exactly the same paths.
//
// Every one is registered WITHOUT a trailing slash. Gin's 301 for a trailing
// slash fires during the router tree walk, BEFORE the handler chain, so a
// trailing-slash form silently skips all middleware including auth and the
// rate limiter.
const (
	RouteUploadSession       = "/upload/attachment/session"
	RouteUploadSessionChunk  = "/upload/attachment/session/:session_id/chunk/:index"
	RouteUploadSessionCommit = "/upload/attachment/session/:session_id/commit"
	RouteUploadSessionCancel = "/upload/attachment/session/:session_id"
)

const (
	// uploadSessionSlidingTTL refreshes on every chunk, so a slow upload does not
	// expire under itself.
	uploadSessionSlidingTTL = 30 * time.Minute

	// uploadSessionHardTTL is the ceiling the sliding window cannot extend past.
	// It matches SessionSweeper's abort threshold so the two agree about when a
	// session is dead.
	uploadSessionHardTTL = 2 * time.Hour

	// maxOpenUploadSessions caps concurrent sessions per user. An estimate with
	// no telemetry behind it; review trigger is the first month of 429 rates.
	maxOpenUploadSessions = 3

	// uploadBudgetBytesPerWindow is the fail-closed ingress budget reserved at
	// init, in DECLARED plaintext bytes, per uploadBudgetWindow.
	uploadBudgetBytesPerWindow int64 = 1 << 30 // 1 GiB
	uploadBudgetWindow               = 10 * time.Minute

	// initRequestMaxBytes caps the init JSON body. Every ingress control on this
	// route runs AFTER the bind, so without it the admission point was the one
	// unbounded read in the feature.
	initRequestMaxBytes = 4096

	// maxMimeTypeLen mirrors media_files.mime_type VARCHAR(100).
	maxMimeTypeLen = 100

	// uploadSessionIngressFactor bounds TOTAL bytes a session may push, as a
	// multiple of what it declared.
	//
	// The byte budget is charged ONCE at init on declared bytes and never
	// consulted again, and PutObjectPart OVERWRITES a part -- so re-PUTting one
	// index was an unmetered channel. A red-team PoC measured 32.5x over the
	// reservation in a single minute, and projected ~281 GiB from a 32 MiB
	// reservation over one session's 2h TTL. The refund comment reasoned
	// carefully about "init, send, cancel, repeat" and closed that loop; this one
	// is strictly cheaper and was wide open. Storage never grows (the part is
	// overwritten), so no storage-side backstop could ever see it.
	//
	// Factor 3, not 1, because in-session repair is a DESIGNED path: a 409 names
	// missing indices and the client re-sends them. Three whole uploads of
	// headroom keeps every legitimate repair working while making the total
	// bounded.
	uploadSessionIngressFactor = 3

	// maxMultipartParts is S3's hard ceiling on parts in one multipart upload.
	// Every backend ADR-0024 commits to (minio|s3|r2|b2) enforces it, so a
	// session declaring more can never complete.
	//
	// Checking it at INIT does two things. It turns a store-level failure at part
	// 10,001 -- after the client has already uploaded 10,000 -- into a
	// diagnosable 400 before any bytes move. And it bounds total_chunks
	// independently of the entitlement cap, which is what makes the later
	// int(index) narrowing in PutUploadChunk provably safe rather than merely
	// safe-in-practice: the index is validated against total_chunks, so a bound
	// here is a bound there.
	maxMultipartParts = 10_000

	// uploadSessionIDBytes is the capability's entropy. 256 bits, base64url with
	// no padding -- 43 characters, all of them outside Redis's key separator.
	uploadSessionIDBytes = 32
	uploadSessionIDLen   = 43
)

const (
	errMsgSessionNotFound   = "Upload session not found"
	errMsgSessionExpired    = "Upload session has expired"
	errMsgSessionState      = "Upload session state unavailable"
	errMsgObjectStoreFailed = "Object storage error"
	errMsgUploadBusy        = "Too many concurrent uploads"
)

// --- Redis keys -----------------------------------------------------------
//
// A session id is validated against the base64url alphabet before it reaches
// any of these, so it can never contain the ':' that separates the namespaces
// below -- attachSessionKey("user:x") is unreachable by construction.

// attachSessionPrefix namespaces every session key. One constant so the four
// builders below cannot drift apart -- they share a keyspace, and a rename that
// reached three of them would silently orphan whatever the fourth wrote.
const attachSessionPrefix = "attach_sess:"

func attachSessionKey(sessionID string) string { return attachSessionPrefix + sessionID }
func attachSessionPartsKey(sessionID string) string {
	return attachSessionPrefix + sessionID + ":parts"
}
func attachUserSessionsKey(userID string) string { return attachSessionPrefix + "user:" + userID }
func attachBudgetKey(userID string) string       { return "attach_budget:" + userID }
func attachIngressKey(sessionID string) string {
	return attachSessionPrefix + sessionID + ":ingress"
}

// orphanedObjectsKey holds storage keys of COMPLETED objects whose metadata
// write failed and whose delete then also failed.
//
// Nothing else can find them. The session sweeper's work queue is
// ListMultipartUploads, which by definition cannot see a completed object, and
// the bucket lifecycle rule only aborts INCOMPLETE uploads. Without this list a
// failed delete strands up to 256 MiB permanently, with no database row
// pointing at it and no process that will ever look.
const orphanedObjectsKey = "attach_orphaned_objects"

// orphanedObjectEntry encodes one queue entry as the PAIR that identifies the
// object, not the bare key.
//
// A bare key is unambiguous only while exactly one backend exists. After the
// Wave C write-default flip it names an object in an unstated bucket, and the
// failure mode is silent: an S3 DELETE of a key that is absent from the target
// bucket returns SUCCESS, so whoever eventually drains this list would record
// a reclamation that never happened and leave the real blob behind forever.
// This queue is the record of LAST resort -- no sweep can see a completed
// object -- so an ambiguous entry defeats its whole purpose.
//
// Encoding is deliberately backward-compatible rather than versioned: the
// legacy backend emits the bare key, byte-identical to every entry this queue
// has ever held, because legacy is the only backend that has ever been written
// to. Only a non-legacy backend -- impossible before the flip -- adds a
// "<backend>\t" prefix. So no existing entry is orphaned and no migration is
// needed; a reader splits on the first tab and treats a tab-less entry as
// legacy, which is exactly what it is.
func orphanedObjectEntry(backend, storageKey string) string {
	if backend == "" {
		return storageKey
	}
	return backend + "\t" + storageKey
}

// parseOrphanedObjectEntry is the inverse. Exported behaviour is pinned by
// tests so a future drainer cannot quietly disagree with the writer.
func parseOrphanedObjectEntry(entry string) (backend, storageKey string) {
	if tab := strings.IndexByte(entry, '\t'); tab >= 0 {
		return entry[:tab], entry[tab+1:]
	}
	return "", entry
}

// attachmentPartSize returns the exact on-the-wire length of upload part
// `index` (0-based), derived purely from the arithmetic in
// upload_session_sizing.go -- never from anything the client sent in-band.
//
// Part 0 carries the 28-byte envelope header because multipart exempts only the
// LAST part from its 5 MiB minimum, so the header cannot be a part of its own.
// Every part is IV + ciphertext + tag for one chunk; the final chunk holds the
// remainder.
//
// WHAT THE VERSION BUYS. Under v2 that header is pure ADDITION, so part 0 is 28
// bytes larger than every other non-trailing part. S3 and MinIO allow that;
// Cloudflare R2 does not -- error 10048 / InvalidPart, "All non-trailing parts
// must have the same size" -- so any attachment needing >= 3 parts (plaintext
// over 16 MiB) is unuploadable there. v3 pays for the header out of chunk 0's
// PLAINTEXT budget instead, which makes every non-trailing part identical while
// leaving the total length identity untouched.
func attachmentPartSize(index, totalChunks, plaintextBytes int64, versions ...EnvelopeVersion) int64 {
	version := envelopeVersionOrDefault(versions)
	size := AttachmentChunkOverheadBytes +
		chunkPlaintextAt(version, index, totalChunks, plaintextBytes)
	if index == 0 {
		size += AttachmentEnvelopeHeaderBytes
	}
	return size
}

// SetSessionRedis injects the Redis client backing chunked upload sessions.
// Mirrors SetOpsCounter: an optional dependency the handler works without
// (every session route answers 503 when it is absent) rather than a
// constructor-signature change rippling through every NewHandler call site.
func (h *Handler) SetSessionRedis(rdb *redis.Client) {
	h.sessionRedis = rdb
}

// RegisterUploadSessionRoutes registers the four chunked-attachment session
// routes on the /media group.
//
// Posture is per-route and deliberate: fail-closed at init where the byte
// budget is reserved, fail-open on the chunk PUT so a Redis blip cannot destroy
// a 20-minute upload that is 90% done. The closed door sits at admission, which
// is the only place it buys anything.
func RegisterUploadSessionRoutes(group *gin.RouterGroup, h *Handler, rdb *redis.Client) {
	// INIT: fail-CLOSED. This is the one door that must not open when Redis is
	// unreachable, because it is where the byte budget is reserved -- and a
	// budget that cannot be read is a budget that cannot be enforced. Admitting
	// sessions blind is how one client turns a Redis blip into an unbounded
	// object-store bill.
	group.POST(RouteUploadSession,
		middleware.RateLimitByUserFailClosed(rdb, 10, 1*time.Minute), h.InitUploadSession)

	// CHUNK PUT: fail-OPEN, deliberately, and this is the whole reason ingress
	// control lives at init. A 256 MiB upload is 32 chunk requests over many
	// minutes; failing closed here would let a momentary Redis blip destroy a
	// 20-minute upload that is 90% done, for no security gain -- the session
	// already exists, its bytes are already budgeted, and its ceiling is already
	// fixed. 300/min is a runaway guard, not an admission control.
	group.PUT(RouteUploadSessionChunk,
		middleware.RateLimitByUser(rdb, 300, 1*time.Minute), h.PutUploadChunk)

	// COMMIT: fail-closed. It is the expensive operation (the object store
	// concatenates every part) and it is cheap to retry, so refusing under
	// uncertainty costs the user one button press.
	group.POST(RouteUploadSessionCommit,
		middleware.RateLimitByUserFailClosed(rdb, 20, 1*time.Minute), h.CommitUploadSession)

	// CANCEL: fail-open. Refusing a cancel strands bytes, which is the opposite
	// of what any limiter here is for. The sweeper is the backstop, but making
	// it do work the client already asked for is a bad trade.
	group.DELETE(RouteUploadSessionCancel,
		middleware.RateLimitByUser(rdb, 60, 1*time.Minute), h.CancelUploadSession)
}

// --- session record -------------------------------------------------------

type uploadSession struct {
	id              string
	userID          string
	fileID          string
	storageKey      string
	uploadID        string
	channelID       string
	conversationID  string
	fileType        FileType
	mimeType        string
	keyVersion      int
	envelopeVersion EnvelopeVersion
	totalChunks     int64
	plaintextBytes  int64
	ciphertextBytes int64
	createdAt       time.Time
	// backend is the media_files.storage_backend value the session's multipart
	// upload was OPENED against (ADR-0038 / #2759). Empty means the legacy
	// backend -- the column stays NULL. It is persisted with the session
	// precisely so a write-default flip mid-session cannot send later chunks,
	// the commit, or the cancel to a backend that does not hold the upload.
	backend string
}

// backendPtr renders the session's backend as a media_files.storage_backend
// column value: nil for legacy, matching a NULL column.
func (s *uploadSession) backendPtr() *string {
	if s.backend == "" {
		return nil
	}
	return &s.backend
}

func newUploadSessionID() (string, error) {
	buf := make([]byte, uploadSessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("upload session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// validUploadSessionID checks shape before the value is ever concatenated into
// a Redis key. Length plus the base64url alphabet; nothing else is accepted.
func validUploadSessionID(id string) bool {
	if len(id) != uploadSessionIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		ch := id[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

func (h *Handler) requireSessionRedis(c *gin.Context) (*redis.Client, bool) {
	if h.sessionRedis == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return nil, false
	}
	return h.sessionRedis, true
}

// loadOwnedSession resolves the :session_id param to a session owned by the
// caller, or writes the response and returns false.
//
// 404 -- NOT 403 -- for a session owned by another user: a 403 would confirm to
// a non-owner that the session exists. The unknown-session and wrong-owner
// answers are byte-identical for the same reason.
func (h *Handler) loadOwnedSession(c *gin.Context, rdb *redis.Client, userID string) (*uploadSession, bool) {
	sessionID := c.Param("session_id")
	if !validUploadSessionID(sessionID) {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgSessionNotFound})
		return nil, false
	}

	fields, err := rdb.HGetAll(c.Request.Context(), attachSessionKey(sessionID)).Result()
	if err != nil {
		h.log.Error("Failed to read upload session", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return nil, false
	}
	if len(fields) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgSessionNotFound})
		return nil, false
	}
	// Constant-time so ownership cannot be probed by timing.
	if subtle.ConstantTimeCompare([]byte(fields["user_id"]), []byte(userID)) != 1 {
		c.JSON(http.StatusNotFound, gin.H{"error": errMsgSessionNotFound})
		return nil, false
	}

	sess, err := decodeUploadSession(sessionID, fields)
	if err != nil {
		h.log.Error("Corrupt upload session record", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return nil, false
	}

	// The sliding window can be refreshed forever by a client that keeps
	// PUTting; the hard cap is what actually ends the session.
	if time.Since(sess.createdAt) > uploadSessionHardTTL {
		h.discardSession(c, rdb, sess)
		c.JSON(http.StatusGone, gin.H{"error": errMsgSessionExpired})
		return nil, false
	}
	return sess, true
}

func decodeUploadSession(sessionID string, fields map[string]string) (*uploadSession, error) {
	sess := &uploadSession{
		id:             sessionID,
		userID:         fields["user_id"],
		fileID:         fields["file_id"],
		storageKey:     fields["storage_key"],
		uploadID:       fields["upload_id"],
		channelID:      fields["channel_id"],
		conversationID: fields["conversation_id"],
		fileType:       FileType(fields["file_type"]),
		mimeType:       fields["mime_type"],
		backend:        fields["backend"],
	}
	if sess.fileID == "" || sess.storageKey == "" || sess.uploadID == "" {
		return nil, errors.New("session record is missing its object-store identity")
	}

	for _, f := range []struct {
		name string
		dst  *int64
	}{
		{"total_chunks", &sess.totalChunks},
		{"plaintext_bytes", &sess.plaintextBytes},
		{"ciphertext_bytes", &sess.ciphertextBytes},
	} {
		v, err := strconv.ParseInt(fields[f.name], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("session field %s: %w", f.name, err)
		}
		*f.dst = v
	}
	// Bound the decoded record, NOT just the one call site that narrows it.
	//
	// total_chunks was validated at init, but that reasoning only covers records
	// this init path wrote. decodeUploadSession parses whatever the Redis hash
	// holds, so a corrupt or externally written record can carry any int64 --
	// and every reader of the record inherits that, not only PutUploadChunk.
	// Rejecting here makes the bound a property of the DECODED SESSION, which is
	// what the int(index)+1 narrowing actually depends on; the point-of-use check
	// in PutUploadChunk stays as the locally-provable statement CodeQL can see.
	if sess.totalChunks < 1 || sess.totalChunks > maxMultipartParts {
		return nil, fmt.Errorf("session field total_chunks out of range: %d", sess.totalChunks)
	}
	if sess.plaintextBytes < 0 || sess.ciphertextBytes < 0 {
		return nil, errors.New("session record carries a negative byte count")
	}

	keyVersion, err := strconv.Atoi(fields["key_version"])
	if err != nil {
		return nil, fmt.Errorf("session field key_version: %w", err)
	}
	sess.keyVersion = keyVersion

	// ABSENT means v2, and this is a rolling-deploy requirement rather than a
	// convenience: a session opened by the previous build carries no
	// envelope_version field at all, and its parts are v2-shaped. Failing to
	// decode it -- or decoding it as the new default -- would reject every
	// remaining chunk of an upload that is already half done.
	//
	// A PRESENT but unrecognised value fails closed: the record then describes
	// geometry this build cannot size, and sizing it wrong is how a part lands
	// at the wrong length.
	sess.envelopeVersion = EnvelopeVersionDefault
	if raw := fields["envelope_version"]; raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("session field envelope_version: %w", err)
		}
		if sess.envelopeVersion = EnvelopeVersion(v); !sess.envelopeVersion.Valid() {
			return nil, fmt.Errorf("session field envelope_version out of range: %d", v)
		}
	}

	createdAt, err := strconv.ParseInt(fields["created_at"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("session field created_at: %w", err)
	}
	sess.createdAt = time.Unix(createdAt, 0)
	return sess, nil
}

// discardSession aborts the multipart upload and drops the session's keys. Used
// by the expiry path and by cancel; every step is best-effort because the
// object-store sweeper (SessionSweeper) is the backstop that cannot be skipped.
func (h *Handler) discardSession(c *gin.Context, rdb *redis.Client, sess *uploadSession) {
	// Detached from the request context: a cancelled request is precisely when
	// this cleanup matters, and inheriting that cancellation would strand bytes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
	defer cancel()

	// Per-object: the multipart upload lives on the backend the session opened
	// against, so aborting it on h.store would leave the real one dangling for
	// the sweeper while reporting success.
	if store, err := h.storeForRow(sess.backendPtr()); err != nil {
		h.log.Error("Could not resolve the backend to abort an abandoned multipart upload",
			"error", err, "storage_key", sess.storageKey)
	} else if err := store.AbortMultipartUpload(ctx, sess.storageKey, sess.uploadID); err != nil {
		h.log.Error("Failed to abort abandoned multipart upload",
			"error", err, "storage_key", sess.storageKey)
	}
	h.dropSessionKeys(ctx, rdb, sess)
}

func (h *Handler) dropSessionKeys(ctx context.Context, rdb *redis.Client, sess *uploadSession) {
	if err := rdb.Del(ctx, attachSessionKey(sess.id), attachSessionPartsKey(sess.id),
		attachIngressKey(sess.id)).Err(); err != nil {
		h.log.Error("Failed to drop upload session keys", "error", err, "user_id", sess.userID)
	}
	if err := rdb.SRem(ctx, attachUserSessionsKey(sess.userID), sess.id).Err(); err != nil {
		h.log.Error("Failed to release upload session slot", "error", err, "user_id", sess.userID)
	}
}

// touchSession refreshes the sliding window on both session keys.
func touchSession(ctx context.Context, rdb *redis.Client, sessionID string) error {
	if err := rdb.Expire(ctx, attachSessionKey(sessionID), uploadSessionSlidingTTL).Err(); err != nil {
		return fmt.Errorf("refresh session ttl: %w", err)
	}
	if err := rdb.Expire(ctx, attachIngressKey(sessionID), uploadSessionSlidingTTL).Err(); err != nil {
		return fmt.Errorf("refresh ingress ledger ttl: %w", err)
	}
	if err := rdb.Expire(ctx, attachSessionPartsKey(sessionID), uploadSessionSlidingTTL).Err(); err != nil {
		return fmt.Errorf("refresh session parts ttl: %w", err)
	}
	return nil
}

// --- init -----------------------------------------------------------------

type initUploadSessionRequest struct {
	ChannelID      string `json:"channel_id"`
	ConversationID string `json:"conversation_id"`
	KeyVersion     int    `json:"key_version"`
	FileType       string `json:"file_type"`
	MimeType       string `json:"mime_type"`
	// EnvelopeVersion is OPTIONAL, and absent (zero) means v2 -- see
	// NormalizeEnvelopeVersion. A client predating the uniform-part-geometry
	// change sends no field and keeps working unchanged.
	EnvelopeVersion         int   `json:"envelope_version"`
	ChunkSize               int64 `json:"chunk_size"`
	TotalChunks             int64 `json:"total_chunks"`
	DeclaredCiphertextBytes int64 `json:"declared_ciphertext_bytes"`
}

// requireUploadSessionWriteStore keeps v2 and omitted sessions on legacy while
// dormant, and rejects v2 once the attachment write selector is armed so its
// historical envelope layout never follows the v3 write selector.
func (h *Handler) requireUploadSessionWriteStore(c *gin.Context, version EnvelopeVersion) (ObjectStore, string, bool) {
	if version == EnvelopeVersionV2 && h.cfg.AttachmentWriteBackendArmed() {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":             "envelope_version must be 3 while the attachment write backend is armed",
			"envelope_versions": []EnvelopeVersion{EnvelopeVersionV3},
		})
		return nil, "", false
	}
	if version == EnvelopeVersionV2 {
		store, ok := h.requireTier1WriteStore(c)
		return store, "", ok
	}
	return h.requireAttachmentWriteStore(c)
}

// InitUploadSession opens a chunked attachment upload session.
// POST /api/v1/media/upload/attachment/session
//
// Every guard runs before the allocating operation: the multipart upload is
// only started once the context, the permissions, the arithmetic, the
// entitlement cap, the byte budget and the concurrency cap have all passed.
func (h *Handler) InitUploadSession(c *gin.Context) {
	userID := c.GetString("user_id")

	// Cap the JSON body BEFORE binding. [internal]rules/backend.md mandates
	// MaxBytesReader for Gin JSON bodies needing a byte cap, and the chunk PUT on
	// this same file gets it right -- init did not. `mime_type` is an unbounded
	// string, so an authenticated caller could POST a multi-gigabyte document and
	// the server would buffer it before ANY of the guards below ran: the byte
	// budget, the concurrency cap and the entitlement ceiling all sit after the
	// bind. 4 KiB is generous for a handful of ids and two integers.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, initRequestMaxBytes)

	var req initUploadSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	envelopeVersion, plaintextBytes, ok := validateInitArithmetic(c, &req)
	if !ok {
		return
	}

	rdb, ok := h.requireSessionRedis(c)
	if !ok {
		return
	}
	store, backendID, ok := h.requireUploadSessionWriteStore(c, envelopeVersion)
	if !ok {
		return
	}

	// Shared-disk occupancy gate (#2759 unit A1), legacy-only, checked once at
	// session open rather than per chunk -- refusing here means no Redis
	// budget/slot reservation and no multipart upload get created for a write
	// that would only be refused later.
	if !h.checkAttachmentDiskWatermark(c, backendID) {
		return
	}

	// Context + membership + CV-CAN-004 send/attach permissions, identical to
	// the single-shot route.
	if !h.validateSessionContext(c, userID, req.ChannelID, req.ConversationID) {
		return
	}

	if !h.validateAttestedEpoch(c, req.ChannelID, req.ConversationID, req.KeyVersion) {
		return
	}

	// #1556 closes here: channel_id arrives in a JSON body BEFORE any bytes
	// flow, so the composed max(user, server) limit is resolvable at cap time --
	// which it never was on the multipart route, where channel_id arrives inside
	// the body the cap has to be set before.
	maxPlaintext := h.attachmentPlaintextCap(c.Request.Context(), userID, req.ChannelID)
	if plaintextBytes > maxPlaintext {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":    fmt.Sprintf("File exceeds maximum upload size of %d bytes", maxPlaintext),
			"max_size": maxPlaintext,
		})
		return
	}

	sessionID, err := newUploadSessionID()
	if err != nil {
		h.log.Error("Failed to mint upload session id", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": errMsgInternalServer})
		return
	}

	if !h.reserveIngress(c, rdb, userID, sessionID, plaintextBytes) {
		return
	}

	fileID := uuid.New().String()
	storageKey := attachmentStorageKey(fileID)
	uploadID, err := store.NewMultipartUpload(c.Request.Context(), storageKey, mimeOctetStream)
	if err != nil {
		rctx, cancel := compensationCtx(c)
		h.releaseSessionSlot(rctx, rdb, userID, sessionID)
		h.refundUploadBudget(rctx, rdb, userID, plaintextBytes)
		cancel()
		h.log.Error("Failed to start multipart upload", "error", err, "user_id", userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": errMsgObjectStoreFailed})
		return
	}

	sess := &uploadSession{
		id: sessionID, userID: userID, fileID: fileID, storageKey: storageKey,
		uploadID: uploadID, channelID: req.ChannelID, conversationID: req.ConversationID,
		fileType: normalizeFileType(req.FileType), mimeType: normalizeMimeType(req.MimeType),
		keyVersion: req.KeyVersion, envelopeVersion: envelopeVersion,
		totalChunks:    req.TotalChunks,
		plaintextBytes: plaintextBytes, ciphertextBytes: req.DeclaredCiphertextBytes,
		createdAt: time.Now(), backend: backendID,
	}
	if err := h.persistSession(c.Request.Context(), rdb, sess); err != nil {
		h.discardSession(c, rdb, sess)
		rctx, cancel := compensationCtx(c)
		h.refundUploadBudget(rctx, rdb, userID, plaintextBytes)
		cancel()
		h.log.Error("Failed to persist upload session", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return
	}

	h.log.Info("Upload session opened",
		"file_id", fileID, "user_id", userID,
		"total_chunks", sess.totalChunks, "ciphertext_bytes", sess.ciphertextBytes)

	c.JSON(http.StatusCreated, gin.H{
		"session_id": sessionID,
		"file_id":    fileID,
		"chunk_size": AttachmentChunkPlaintextBytes,
		"expires_at": sess.createdAt.Add(uploadSessionSlidingTTL).UTC().Format(time.RFC3339),
	})
}

// validateInitArithmetic checks everything about the declared envelope that can
// be checked without touching a database, and returns the wire format plus the
// plaintext size the client will actually send.
func validateInitArithmetic(
	c *gin.Context, req *initUploadSessionRequest,
) (EnvelopeVersion, int64, bool) {
	if req.ChannelID == "" && req.ConversationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Either channel_id or conversation_id is required for attachments"})
		return 0, 0, false
	}
	if req.ChannelID != "" && req.ConversationID != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Exactly one of channel_id or conversation_id must be provided"})
		return 0, 0, false
	}
	// #2843: the epoch is sender-attested, never invented by the server.
	if req.KeyVersion < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsgKeyVersionRequired})
		return 0, 0, false
	}
	// The accepted set is CLOSED. Absent (zero) is v2 so an older client keeps
	// working; anything else outside {2, 3} is a format whose part geometry this
	// server cannot derive, and admitting it would mean sizing its parts by
	// guess.
	envelopeVersion, ok := NormalizeEnvelopeVersion(req.EnvelopeVersion)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":             fmt.Sprintf("envelope_version must be %d or %d", EnvelopeVersionV2, EnvelopeVersionV3),
			"envelope_versions": []EnvelopeVersion{EnvelopeVersionV2, EnvelopeVersionV3},
		})
		return 0, 0, false
	}
	// chunk_size is an allowlist, not a sizing input. It exists so the value is
	// bound into every chunk's AAD; a client that disagrees with the compile-time
	// constant is producing ciphertext this server's arithmetic cannot describe.
	if req.ChunkSize != AttachmentChunkPlaintextBytes {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      fmt.Sprintf("chunk_size must be %d", AttachmentChunkPlaintextBytes),
			"chunk_size": AttachmentChunkPlaintextBytes,
		})
		return 0, 0, false
	}
	// mime_type reaches a VARCHAR(100) column at commit. Unvalidated, an
	// over-long value survived the entire upload and failed at the INSERT --
	// AFTER CompleteMultipartUpload had concatenated up to 256 MiB, which then
	// had to be deleted. Cheap for the caller, expensive for the server, and it
	// turned a deterministic client error into an operator-visible 500.
	if len(req.MimeType) > maxMimeTypeLen {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":    fmt.Sprintf("mime_type must be at most %d characters", maxMimeTypeLen),
			"max_size": maxMimeTypeLen,
		})
		return 0, 0, false
	}
	if req.TotalChunks < 1 || req.TotalChunks > maxMultipartParts {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      fmt.Sprintf("total_chunks must be between 1 and %d", maxMultipartParts),
			"max_chunks": maxMultipartParts,
		})
		return 0, 0, false
	}
	// total_chunks is client-supplied and therefore untrusted: ChunkedPlaintextBytes
	// verifies it against the arithmetic rather than believing it -- under the
	// version the client declared, so a v3 client cannot be measured with v2
	// geometry or the reverse.
	plaintextBytes, err := ChunkedPlaintextBytes(
		req.DeclaredCiphertextBytes, req.TotalChunks, envelopeVersion)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Declared attachment size is inconsistent"})
		return 0, 0, false
	}
	return envelopeVersion, plaintextBytes, true
}

func normalizeFileType(raw string) FileType {
	ft := FileType(raw)
	if !isValidFileType(ft) {
		return FileTypeFile
	}
	return ft
}

func normalizeMimeType(raw string) string {
	if raw == "" {
		return mimeOctetStream
	}
	return raw
}

// validateSessionContext mirrors validateAttachmentContext for a JSON body: the
// ids arrive as fields rather than form values, but the membership and
// permission gates behind them are the same ones.
func (h *Handler) validateSessionContext(c *gin.Context, userID, channelID, conversationID string) bool {
	if channelID != "" {
		return validateChannelAttachment(c, h, userID, channelID)
	}
	return validateDMAttachment(c, h, userID, conversationID)
}

// attachmentPlaintextCap composes the per-file PLAINTEXT limit for this upload.
// DM uploads stay user-axis only; a channel upload takes the better of the user
// and server axes (the product rule is max, never min).
func (h *Handler) attachmentPlaintextCap(ctx context.Context, userID, channelID string) int64 {
	user := entitlements.For(h.tiers.GetTier(ctx, userID))
	if channelID == "" {
		return user.MaxAttachmentBytes
	}

	var serverID string
	if err := h.db.QueryRowContext(ctx,
		`SELECT server_id FROM channels WHERE id = $1`, channelID).Scan(&serverID); err != nil {
		// Membership was already verified, so this is a fault rather than a
		// missing channel. Fall back to the USER axis alone: the product rule
		// is max, so the unknown server axis could only ever RAISE the cap, and
		// dropping it is the conservative direction.
		h.log.Error(logMsgChannelServerLookup, "error", err)
		return user.MaxAttachmentBytes
	}

	effective := entitlements.EffectiveAttachmentBytes(
		user, entitlements.ForServer(h.serverTier(ctx, serverID)))
	if effective < 0 {
		// ServerLimitUnlimited (selfhost). Its caller contract requires the
		// sentinel to become a config ceiling before any comparison. UPLOAD_MAX_SIZE
		// is that ceiling -- but it defaults BELOW the free-tier allowance, so it
		// may only ever raise the user axis, never lower it.
		effective = user.MaxAttachmentBytes
		if h.cfg != nil && h.cfg.UploadMaxSize > effective {
			effective = h.cfg.UploadMaxSize
		}
	}
	return effective
}

// validateAttestedEpoch bounds the sender-attested key_version by the highest
// epoch that actually exists for the context.
//
// The epoch is sender-attested by design (#2843) -- the server does not invent
// it -- but "not invented by the server" was implemented as "accepted from the
// client unchecked", and >= 1 was the only bound. That was inert while nothing
// read the value back. It stopped being inert when the attachment download
// began reflecting it as X-File-Key-Version (#2157 PR 2), because the viewer's
// client then uses it to SELECT A KEY: a fabricated 2147483647 was cached as a
// real epoch and drove a monotonic rotation watermark, after which every
// genuine rotation compared <= and was dropped. Proven to media teardown.
//
// The message path has validated its claimed epoch all along
// (enforceChannelEpoch, internal/messages/handlers.go) -- the attachment paths
// were the outlier, and this closes that gap.
//
// Deliberately an UPPER BOUND, not the message path's revocation check. Any
// historical epoch stays uploadable, because an upload racing a rotation is
// legitimate and failing it would be a new outage for a real user. Refusing a
// revoked epoch is a product decision with its own blast radius; refusing an
// epoch that never existed is not.
//
// Fails CLOSED on a database error: this is an admission gate, and the whole
// point is that an unverifiable claim must not be admitted.
func (h *Handler) validateAttestedEpoch(c *gin.Context, channelID, conversationID string, keyVersion int) bool {
	// The SAME "current epoch" resolution the message path uses
	// (enforceChannelEpoch, internal/messages/handlers.go). Not just
	// MAX(key_version): a rotation is ISSUED as a key_revocations successor
	// before its wraps are distributed, so an upload legitimately racing that
	// window attests the new epoch while no channel_keys row carries it yet.
	// Taking the GREATEST of the two admits that upload and still bounds a
	// fabricated epoch, which is the whole job. Two definitions of "current
	// epoch" in one codebase would be a bug waiting to happen, so this is the
	// existing one.
	query := `SELECT GREATEST(
			COALESCE(MAX(successor_epoch), 1),
			COALESCE((SELECT MAX(key_version) FROM channel_keys WHERE channel_id = $1), 1)
		) FROM key_revocations WHERE channel_id = $1`
	contextID := channelID
	if channelID == "" {
		query = `SELECT GREATEST(
				COALESCE(MAX(successor_epoch), 1),
				COALESCE((SELECT MAX(key_version) FROM dm_channel_keys WHERE conversation_id = $1), 1)
			) FROM dm_key_revocations WHERE conversation_id = $1`
		contextID = conversationID
	}

	var maxEpoch int
	if err := h.db.QueryRowContext(c.Request.Context(), query, contextID).Scan(&maxEpoch); err != nil {
		h.log.Error("Failed to resolve the current key epoch", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify key epoch"})
		return false
	}
	// The COALESCE pair above already floors this at 1, which matters: a context
	// can legitimately have no key rows yet, because the initial wrap is
	// hardcoded to epoch 1 (storeWrappedKeys) but distribution is client-driven,
	// so a channel exists before its keys do. Rejecting an upload in that window
	// would be a new outage for a real user. Epoch 1 cannot poison anything --
	// it is the floor of every real context, so it can never exceed a monotonic
	// watermark. The attack needs an epoch ABOVE reality, and that stays bounded.
	if keyVersion > maxEpoch {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         errMsgKeyVersionUnknown,
			"code":          "unknown_epoch",
			"current_epoch": maxEpoch,
		})
		return false
	}
	return true
}

// reserveIngress takes the byte budget and the concurrency slot, in that order.
// Both are reservations, not checks: the INCRBY and the SADD ARE the take, so
// there is no window between deciding and claiming.
func (h *Handler) reserveIngress(
	c *gin.Context, rdb *redis.Client, userID, sessionID string, plaintextBytes int64,
) bool {
	ctx := c.Request.Context()

	// PLACEHOLDER FIRST, then the slot claim.
	//
	// reserveSessionSlot prunes set members whose session hash does not exist --
	// but the hash is only written by persistSession, AFTER a network round trip
	// to the object store. Between the SAdd and that write the member looks like
	// debris, so a CONCURRENT init's prune loop SRem'd a slot that had just been
	// legitimately claimed. The comment there reasons correctly about the
	// SCard half (losing that race over-rejects, which is the safe direction)
	// and is blind to the prune half, which over-ADMITS: an unbounded number of
	// simultaneous sessions and open multipart uploads per user.
	//
	// A short-lived placeholder closes the window: the hash exists from the
	// moment the slot is claimed, so nothing reads the claim as debris.
	// HSet, not Set: persistSession writes the real record as a HASH on this same
	// key, and a string placeholder makes that a WRONGTYPE error. The marker
	// field is overwritten by the real fields and is harmless if it lingers --
	// decodeUploadSession requires the full field set, so a placeholder-only
	// record is correctly unusable rather than half-valid.
	//
	// The TTL rides in the SAME transaction as the write. Separately, with the
	// Expire only logging on failure, a failed Expire left a hash with NO
	// expiry -- and the reject paths below return without deleting it, so
	// nothing would ever have removed it. MULTI/EXEC means the key cannot exist
	// without its expiry, which is a stronger statement than deleting on each
	// reject path: it needs no path to remember.
	//
	// Fail-CLOSED on the pipeline error, matching this route's posture: init is
	// the one door that must not open when Redis is unwell.
	pipe := rdb.TxPipeline()
	pipe.HSet(ctx, attachSessionKey(sessionID), "pending", "1")
	pipe.Expire(ctx, attachSessionKey(sessionID), uploadSessionSlidingTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		h.log.Error("Failed to stake upload session placeholder", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return false
	}

	withinBudget, err := reserveUploadBudget(ctx, rdb, userID, plaintextBytes)
	if err != nil {
		// Fail closed: the budget is the ingress control, and an outage must not
		// open the floodgate.
		h.log.Error("Failed to reserve upload budget", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return false
	}
	if !withinBudget {
		// Refund for symmetry with the two branches below: reserveUploadBudget
		// INCRBYs before comparing, so a rejected attempt that keeps its charge
		// makes every retry during the window extend the user's own lockout.
		rctx, cancel := compensationCtx(c)
		defer cancel()
		h.refundUploadBudget(rctx, rdb, userID, plaintextBytes)
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Upload byte budget exhausted; try again later"})
		return false
	}

	// The budget is charged ABOVE, so every failure below must hand it back --
	// otherwise a user who is merely at their concurrency limit burns ingress
	// budget for a full window on a session that never existed, and starts
	// collecting 429s for uploads that should have been allowed.
	slot, err := h.reserveSessionSlot(ctx, rdb, userID, sessionID)
	if err != nil {
		rctx, cancel := compensationCtx(c)
		h.refundUploadBudget(rctx, rdb, userID, plaintextBytes)
		cancel()
		h.log.Error("Failed to reserve upload session slot", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return false
	}
	if !slot {
		rctx, cancel := compensationCtx(c)
		h.refundUploadBudget(rctx, rdb, userID, plaintextBytes)
		cancel()
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":              errMsgUploadBusy,
			"max_active_uploads": maxOpenUploadSessions,
		})
		return false
	}
	return true
}

// reserveUploadBudget charges plaintextBytes against the user's rolling window.
// Mirrors middleware.AllowUserAction's INCR + TTL-repair, in bytes rather than
// requests: a key left without an expiry would never reset and, once over
// budget, lock the user out permanently.
func reserveUploadBudget(ctx context.Context, rdb *redis.Client, userID string, plaintextBytes int64) (bool, error) {
	key := attachBudgetKey(userID)
	total, err := rdb.IncrBy(ctx, key, plaintextBytes).Result()
	if err != nil {
		return false, fmt.Errorf("incr upload budget: %w", err) // fail closed
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("read upload budget ttl: %w", err)
	}
	if ttl == -1 {
		if err := rdb.Expire(ctx, key, uploadBudgetWindow).Err(); err != nil {
			return false, fmt.Errorf("set upload budget ttl: %w", err)
		}
	}
	return total <= uploadBudgetBytesPerWindow, nil
}

// compensationCtx detaches a compensating write from the request context.
//
// The most likely reason the operation being compensated failed is that the
// request context was CANCELLED -- the user closed the composer, navigated away,
// or the link dropped. A refund on that same context then fails for the same
// reason, dropping the compensation exactly when it is most needed. This file
// already applies the discipline twice (discardSession, abandonCommittedObject);
// the refund path had forgotten it.
func compensationCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
}

// refundUploadBudget returns a reservation to the user's rolling window.
//
// SCOPE IS DELIBERATELY NARROW: this is called only on init paths that fail
// BEFORE the session becomes usable, where it is certain no chunk could have
// been uploaded. It is NOT called on cancel or expiry, and that asymmetry is
// the point.
//
// The budget bounds INGRESS BYTES PER WINDOW, so bytes the client actually sent
// are spent whatever the session's eventual outcome. Refunding on cancel would
// hand back a full reservation after an arbitrary amount of it had already been
// uploaded, making "init 1 GiB, send 900 MiB, cancel, repeat" an unbounded
// ingress channel -- it would defeat the exact control it appears to be
// maintaining. A user who cancels has still used the pipe.
//
// Best-effort by design: a failed refund over-charges the user for at most one
// window, which is the safe direction. Failing the request because a refund did
// not land would turn a recoverable accounting slip into a user-visible error.
func (h *Handler) refundUploadBudget(
	ctx context.Context, rdb *redis.Client, userID string, plaintextBytes int64,
) {
	if err := rdb.DecrBy(ctx, attachBudgetKey(userID), plaintextBytes).Err(); err != nil {
		h.log.Error("Failed to refund upload budget", "error", err, "user_id", userID)
	}
}

// reserveSessionSlot claims one of the user's concurrent-session slots.
//
// It ADDS first and counts after, so two concurrent inits can never both pass a
// count taken before either claimed. Losing a race over-rejects, which is the
// correct direction.
func (h *Handler) reserveSessionSlot(
	ctx context.Context, rdb *redis.Client, userID, sessionID string,
) (bool, error) {
	key := attachUserSessionsKey(userID)

	// Sessions that expired by TTL leave their id behind in the set; pruning them
	// first keeps the cap honest instead of slowly locking the user out.
	members, err := rdb.SMembers(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("read open sessions: %w", err)
	}
	for _, id := range members {
		exists, err := rdb.Exists(ctx, attachSessionKey(id)).Result()
		if err != nil {
			return false, fmt.Errorf("probe open session: %w", err)
		}
		if exists == 0 {
			if err := rdb.SRem(ctx, key, id).Err(); err != nil {
				return false, fmt.Errorf("prune open session: %w", err)
			}
		}
	}

	if err := rdb.SAdd(ctx, key, sessionID).Err(); err != nil {
		return false, fmt.Errorf("claim session slot: %w", err)
	}
	if err := rdb.Expire(ctx, key, uploadSessionHardTTL).Err(); err != nil {
		return false, fmt.Errorf("set session slot ttl: %w", err)
	}
	count, err := rdb.SCard(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("count session slots: %w", err)
	}
	if count > maxOpenUploadSessions {
		h.releaseSessionSlot(ctx, rdb, userID, sessionID)
		return false, nil
	}
	return true, nil
}

func (h *Handler) releaseSessionSlot(ctx context.Context, rdb *redis.Client, userID, sessionID string) {
	if err := rdb.SRem(ctx, attachUserSessionsKey(userID), sessionID).Err(); err != nil {
		h.log.Error("Failed to release upload session slot", "error", err, "user_id", userID)
	}
}

func (h *Handler) persistSession(ctx context.Context, rdb *redis.Client, sess *uploadSession) error {
	key := attachSessionKey(sess.id)
	if err := rdb.HSet(ctx, key,
		"user_id", sess.userID,
		"file_id", sess.fileID,
		"storage_key", sess.storageKey,
		"upload_id", sess.uploadID,
		"channel_id", sess.channelID,
		"conversation_id", sess.conversationID,
		"file_type", string(sess.fileType),
		"mime_type", sess.mimeType,
		"key_version", sess.keyVersion,
		"envelope_version", int(sess.envelopeVersion),
		"total_chunks", sess.totalChunks,
		"plaintext_bytes", sess.plaintextBytes,
		"ciphertext_bytes", sess.ciphertextBytes,
		"created_at", sess.createdAt.Unix(),
		"backend", sess.backend,
	).Err(); err != nil {
		return fmt.Errorf("write session record: %w", err)
	}
	if err := rdb.Expire(ctx, key, uploadSessionSlidingTTL).Err(); err != nil {
		return fmt.Errorf("set session ttl: %w", err)
	}
	return nil
}

// --- chunk PUT ------------------------------------------------------------

// countingBody records how much of the request body was actually consumed and
// the first non-EOF read error, so a store failure caused by the CLIENT
// (truncated or over-long body) is not reported as a backend fault.
type countingBody struct {
	r   io.Reader
	n   int64
	err error
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && b.err == nil {
		b.err = err
	}
	return n, err
}

// PutUploadChunk stores one chunk of an open session.
// PUT /api/v1/media/upload/attachment/session/:session_id/chunk/:index
//
// The body is raw application/octet-stream and is streamed straight to the
// object store: it never reaches ParseMultipartForm, so there is no temp-file
// spill and the server never holds a chunk in memory.
func (h *Handler) PutUploadChunk(c *gin.Context) {
	userID := c.GetString("user_id")

	rdb, ok := h.requireSessionRedis(c)
	if !ok {
		return
	}
	sess, ok := h.loadOwnedSession(c, rdb, userID)
	if !ok {
		return
	}
	// Per-object, NOT the write default: this chunk belongs to a multipart
	// upload already open on sess.backend, so a flip since session open must
	// not redirect it.
	store, ok := h.requireObjectStoreForRow(c, sess.backendPtr())
	if !ok {
		return
	}

	// The ceiling is re-asserted here as well as at decode. It is UNREACHABLE by
	// construction -- decodeUploadSession refuses any record whose total_chunks
	// exceeds maxMultipartParts, so `index >= sess.totalChunks` always fires
	// first -- and it is kept deliberately, for two reasons.
	//
	// It states the precondition the int(index)+1 narrowing below actually
	// depends on, at the line that depends on it, rather than leaving it to be
	// inferred from a decoder in another function. And it is the form CodeQL can
	// see: the decode-time bound lives behind a struct field the analysis does
	// not follow, which is why alert 1305 survived that bound alone.
	//
	// If the decode-time check is ever loosened, this stops being dead.
	index, err := strconv.ParseInt(c.Param("index"), 10, 64)
	if err != nil || index < 0 || index >= sess.totalChunks || index >= maxMultipartParts {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        "Invalid chunk index",
			"total_chunks": sess.totalChunks,
		})
		return
	}

	// The expected length is pure arithmetic over the session's declared sizes.
	// Nothing about the bytes themselves is inspected -- the server never parses
	// the v2 header.
	expected := attachmentPartSize(
		index, sess.totalChunks, sess.plaintextBytes, sess.envelopeVersion)

	// MaxBytesReader BEFORE any read of the body, and before the Content-Length
	// checks below, so a lying Content-Length still cannot make the server read
	// more than one part's worth.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, expected)

	switch declared := c.Request.ContentLength; {
	case declared > expected:
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":         "Chunk exceeds its declared length",
			"expected_size": expected,
		})
		return
	case declared < 0:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Content-Length is required for a chunk upload"})
		return
	case declared != expected:
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "Chunk length disagrees with the session arithmetic",
			"expected_size": expected,
		})
		return
	}

	// RE-CHECK AUTHORIZATION, not just ownership.
	//
	// The chunk path used to verify only that the session belonged to the
	// caller, so a member removed from the server kept writing chunks for the
	// rest of the session's 2h TTL (a red-team PoC accepted 5 PUTs after
	// revocation; commit correctly refused). Nothing lands without a commit, so
	// this is not content injection -- it is the ingress channel outliving the
	// authorization that opened it, which is exactly what the ledger above is
	// meant to bound and what this closes at the source.
	if !h.validateSessionContext(c, userID, sess.channelID, sess.conversationID) {
		return
	}

	// INGRESS LEDGER, charged BEFORE the bytes are read.
	//
	// This is the meter the byte budget is not: the budget is charged once at
	// init on DECLARED bytes, while this counts what the session actually
	// pushes, including every re-PUT of an index already stored. Without it,
	// re-sending index 0 forever was free (PutObjectPart overwrites, so storage
	// never grows and no storage-side backstop can see it).
	//
	// INCRBY is the reservation -- there is no check-then-take window. Fail
	// CLOSED on a Redis error: loadOwnedSession has already proved Redis
	// reachable for this request, so an error here is not the momentary blip the
	// route's fail-open limiter exists to tolerate, and it is the same posture
	// the writes below already take.
	ingressCap := sess.ciphertextBytes * uploadSessionIngressFactor
	used, err := rdb.IncrBy(c.Request.Context(), attachIngressKey(sess.id), expected).Result()
	if err != nil {
		h.log.Error("Failed to meter upload ingress", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return
	}
	if used > ingressCap {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":     "Upload has exceeded its ingress allowance for this session",
			"max_bytes": ingressCap,
		})
		return
	}
	if err := rdb.Expire(c.Request.Context(), attachIngressKey(sess.id), uploadSessionSlidingTTL).Err(); err != nil {
		h.log.Warn("Failed to set ingress ledger TTL", "error", err, "user_id", userID)
	}

	body := &countingBody{r: c.Request.Body}
	// S3 part numbers are 1-based; the wire index is 0-based.
	info, err := store.PutObjectPart(
		c.Request.Context(), sess.storageKey, sess.uploadID, int(index)+1, body, expected)
	if err != nil {
		h.respondChunkStoreError(c, err, body, expected, userID)
		return
	}
	if info.Size != expected {
		h.log.Error("Object store stored a part of unexpected size",
			"storage_key", sess.storageKey, "expected", expected, "stored", info.Size)
		c.JSON(http.StatusBadGateway, gin.H{"error": errMsgObjectStoreFailed})
		return
	}

	// The parts set is the client's progress record, used for resume. It is
	// deliberately NOT what commit trusts.
	// PROGRESS BOOKKEEPING, NOT A CORRECTNESS DEPENDENCY -- and this key has NO
	// production reader. Commit reconciles against the object store's own part
	// listing (reconcileParts) and resume comes from the 409 missing list; the
	// only read of this set anywhere is an assertion in a test.
	//
	// It used to 503 on failure, which made the client throw and cancelSession
	// abort the WHOLE multipart upload: a 20-minute, 90%-done upload destroyed to
	// report the failure of a write nobody consults. That is verbatim the outcome
	// this route's fail-OPEN limiter was chosen to prevent -- the posture argued
	// at the registration site, contradicted 700 lines later in the handler.
	if err := rdb.SAdd(c.Request.Context(), attachSessionPartsKey(sess.id), index).Err(); err != nil {
		h.log.Warn("Failed to record chunk progress; the part IS stored and commit is unaffected",
			"error", err, "user_id", userID)
	}
	// Likewise: a missed TTL refresh can let the session lapse, and the client's
	// 410 restart path handles exactly that. Killing a chunk that is already
	// durably stored does not.
	if err := touchSession(c.Request.Context(), rdb, sess.id); err != nil {
		h.log.Warn("Failed to refresh upload session TTL; the 410 restart path covers a lapse",
			"error", err, "user_id", userID)
	}

	c.JSON(http.StatusOK, gin.H{"index": index, "total_chunks": sess.totalChunks})
}

// respondChunkStoreError separates a client-caused body failure from a genuine
// backend fault. Blaming the store for a client's truncated upload would send
// operators hunting a phantom outage.
func (h *Handler) respondChunkStoreError(
	c *gin.Context, storeErr error, body *countingBody, expected int64, userID string,
) {
	var tooLarge *http.MaxBytesError
	if errors.As(body.err, &tooLarge) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error":         "Chunk exceeds its declared length",
			"expected_size": expected,
		})
		return
	}
	if body.n < expected {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "Chunk body ended before its declared length",
			"expected_size": expected,
		})
		return
	}
	h.log.Error("Failed to store attachment chunk", "error", storeErr, "user_id", userID)
	c.JSON(http.StatusBadGateway, gin.H{"error": errMsgObjectStoreFailed})
}

// --- commit ---------------------------------------------------------------

// CommitUploadSession completes the multipart upload and records the attachment.
// POST /api/v1/media/upload/attachment/session/:session_id/commit
//
// The response is byte-identical to the single-shot UploadAttachment response,
// so the client's existing parsing is unchanged.
func (h *Handler) CommitUploadSession(c *gin.Context) {
	userID := c.GetString("user_id")

	rdb, ok := h.requireSessionRedis(c)
	if !ok {
		return
	}
	sess, ok := h.loadOwnedSession(c, rdb, userID)
	if !ok {
		return
	}
	// Per-object, for the same reason as PutUploadChunk.
	store, ok := h.requireObjectStoreForRow(c, sess.backendPtr())
	if !ok {
		return
	}

	// Permissions are re-checked here, not merely at init: a member kicked or
	// muted mid-upload must not land the attachment.
	if !h.validateSessionContext(c, userID, sess.channelID, sess.conversationID) {
		return
	}

	stored, err := store.ListObjectParts(c.Request.Context(), sess.storageKey, sess.uploadID)
	if err != nil {
		h.log.Error("Failed to list attachment parts", "error", err, "user_id", userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": errMsgObjectStoreFailed})
		return
	}

	complete, missing := reconcileParts(stored, sess)
	if len(missing) > 0 {
		// The multipart upload stays open so the client can re-PUT exactly these.
		c.JSON(http.StatusConflict, gin.H{
			"error":   "Upload is incomplete",
			"missing": missing,
		})
		return
	}

	if err := store.CompleteMultipartUpload(
		c.Request.Context(), sess.storageKey, sess.uploadID, complete); err != nil {
		h.log.Error("Failed to complete multipart upload", "error", err, "user_id", userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": errMsgObjectStoreFailed})
		return
	}

	if err := createAttachmentRecord(h, c, attachmentParams{
		fileID: sess.fileID, userID: userID, fileType: sess.fileType, mimeType: sess.mimeType,
		storageKey: sess.storageKey, fileSize: sess.ciphertextBytes, keyVersion: sess.keyVersion,
		channelID: sess.channelID, conversationID: sess.conversationID,
		storageBackend: sess.backend,
	}); err != nil {
		h.abandonCommittedObject(c, rdb, sess)
		if errors.Is(err, credepoch.ErrEpochMismatch) || errors.Is(err, credepoch.ErrBlocked) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		h.log.Error("Failed to record attachment metadata", "error", err, "user_id", userID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record file metadata"})
		return
	}

	// The object is live and recorded; the session has nothing left to protect.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
	h.dropSessionKeys(cleanupCtx, rdb, sess)
	cancel()

	h.log.Info("Attachment uploaded", "file_id", sess.fileID, "user_id", userID,
		"size", sess.ciphertextBytes, "type", sess.fileType)

	h.recordSuccessfulUpload()
	c.JSON(http.StatusCreated, gin.H{
		"file_id":     sess.fileID,
		"storage_key": sess.storageKey,
		"file_type":   sess.fileType,
		"file_size":   sess.ciphertextBytes,
	})
}

// reconcileParts answers "what does the OBJECT STORE actually hold" against the
// arithmetic, and returns the parts to complete with plus the wire indices that
// are absent or the wrong length.
//
// The session's own parts set is not consulted. Taking part identity from the
// store is what keeps the store authoritative: a client that misreports its
// progress cannot talk the server into completing an upload that is not there,
// and the ETags CompleteMultipartUpload needs come from the same listing.
func reconcileParts(stored []storage.ObjectPartInfo, sess *uploadSession) ([]storage.ObjectPartInfo, []int64) {
	byNumber := make(map[int]storage.ObjectPartInfo, len(stored))
	for _, p := range stored {
		byNumber[p.PartNumber] = p
	}

	complete := make([]storage.ObjectPartInfo, 0, sess.totalChunks)
	missing := make([]int64, 0)
	for index := int64(0); index < sess.totalChunks; index++ {
		want := attachmentPartSize(
			index, sess.totalChunks, sess.plaintextBytes, sess.envelopeVersion)
		part, present := byNumber[int(index)+1]
		if !present || part.Size != want {
			missing = append(missing, index)
			continue
		}
		complete = append(complete, part)
	}
	return complete, missing
}

// abandonCommittedObject deletes the just-completed object after the metadata
// write failed. The delete is detached because the metadata write can fail
// precisely BECAUSE the request context was cancelled, and a cleanup on that
// same context would strand an orphaned blob no reaper can find (#2201).
func (h *Handler) abandonCommittedObject(c *gin.Context, rdb *redis.Client, sess *uploadSession) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
	defer cancel()
	store, err := h.storeForRow(sess.backendPtr())
	if err != nil {
		// Unresolvable backend: the object cannot be deleted here at all, so go
		// straight to the durable queue rather than losing the record.
		h.log.Error("Orphaned attachment object could not be deleted; queued for retry",
			"error", err, "storage_key", sess.storageKey)
		if qErr := rdb.RPush(ctx, orphanedObjectsKey, orphanedObjectEntry(sess.backend, sess.storageKey)).Err(); qErr != nil {
			h.log.Error("Orphaned attachment object is UNRECLAIMABLE: neither deleted nor queued",
				"error", qErr, "storage_key", sess.storageKey)
		}
		h.dropSessionKeys(ctx, rdb, sess)
		return
	}
	if err := store.DeleteObject(ctx, sess.storageKey); err != nil {
		// A log line cannot be the whole remediation for an UNRECOVERABLE leak.
		// No sweep covers a completed object, so without a durable record this
		// blob is unreclaimable forever -- and the user, having received a 500,
		// will retry and make a second one.
		h.log.Error("Orphaned attachment object could not be deleted; queued for retry",
			"error", err, "storage_key", sess.storageKey)
		if qErr := rdb.RPush(ctx, orphanedObjectsKey, orphanedObjectEntry(sess.backend, sess.storageKey)).Err(); qErr != nil {
			// Distinct, alertable message: "delete failed, queued" and
			// "unreclaimable" are different operational events.
			h.log.Error("Orphaned attachment object is UNRECLAIMABLE: neither deleted nor queued",
				"error", qErr, "storage_key", sess.storageKey)
		}
	}
	h.dropSessionKeys(ctx, rdb, sess)
}

// --- cancel ---------------------------------------------------------------

// CancelUploadSession aborts an upload session and reclaims its bytes.
// DELETE /api/v1/media/upload/attachment/session/:session_id
//
// Idempotent, and uniformly 204: an unknown session, a malformed id and a
// session owned by somebody else all answer identically. Answering 404 for a
// non-owner while answering 204 for a session that never existed would rebuild
// exactly the existence oracle the 404-not-403 rule closes on the other routes.
func (h *Handler) CancelUploadSession(c *gin.Context) {
	userID := c.GetString("user_id")

	rdb, ok := h.requireSessionRedis(c)
	if !ok {
		return
	}

	sessionID := c.Param("session_id")
	if !validUploadSessionID(sessionID) {
		c.Status(http.StatusNoContent)
		// Flush explicitly. c.Status only records the code on Gin's buffered writer;
		// for a bodyless response nothing else triggers a write, and the header is
		// otherwise emitted only by the engine at the end of handleHTTPRequest. That
		// makes the status invisible to any caller that invokes the handler directly
		// -- which is how these are unit-tested -- so the assertion silently reads the
		// recorder's default 200 instead of the 204 the handler set.
		c.Writer.WriteHeaderNow()
		return
	}

	fields, err := rdb.HGetAll(c.Request.Context(), attachSessionKey(sessionID)).Result()
	if err != nil {
		h.log.Error("Failed to read upload session", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return
	}
	if len(fields) == 0 ||
		subtle.ConstantTimeCompare([]byte(fields["user_id"]), []byte(userID)) != 1 {
		c.Status(http.StatusNoContent)
		// Flush explicitly. c.Status only records the code on Gin's buffered writer;
		// for a bodyless response nothing else triggers a write, and the header is
		// otherwise emitted only by the engine at the end of handleHTTPRequest. That
		// makes the status invisible to any caller that invokes the handler directly
		// -- which is how these are unit-tested -- so the assertion silently reads the
		// recorder's default 200 instead of the 204 the handler set.
		c.Writer.WriteHeaderNow()
		return
	}

	sess, err := decodeUploadSession(sessionID, fields)
	if err != nil {
		h.log.Error("Corrupt upload session record", "error", err, "user_id", userID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errMsgSessionState})
		return
	}

	// Per-object, and fail CLOSED: an unresolvable backend must not report a
	// successful cancel for an upload that is still open somewhere.
	store, ok := h.requireObjectStoreForRow(c, sess.backendPtr())
	if !ok {
		return
	}
	if err := store.AbortMultipartUpload(
		c.Request.Context(), sess.storageKey, sess.uploadID); err != nil {
		// Keep the session keys: the client can retry, and until then the
		// object-store sweeper still owns the bytes.
		h.log.Error("Failed to abort multipart upload", "error", err, "user_id", userID)
		c.JSON(http.StatusBadGateway, gin.H{"error": errMsgObjectStoreFailed})
		return
	}

	h.dropSessionKeys(c.Request.Context(), rdb, sess)
	h.log.Info("Upload session cancelled", "file_id", sess.fileID, "user_id", userID)
	c.Status(http.StatusNoContent)
	// Flush explicitly. c.Status only records the code on Gin's buffered writer;
	// for a bodyless response nothing else triggers a write, and the header is
	// otherwise emitted only by the engine at the end of handleHTTPRequest. That
	// makes the status invisible to any caller that invokes the handler directly
	// -- which is how these are unit-tested -- so the assertion silently reads the
	// recorder's default 200 instead of the 204 the handler set.
	c.Writer.WriteHeaderNow()
}
