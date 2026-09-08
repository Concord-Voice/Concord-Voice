// Package securityevent writes the closed Nightwatch security-event.v1 stream.
package securityevent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const (
	// ControlPlanePath is the sole control-plane Nightwatch ingestion stream.
	ControlPlanePath = "/var/log/concord-security/control-plane/events.jsonl"
	maxLineBytes     = 2048
	maxFileBytes     = 20 << 20
	maxUniqueEvents  = 100
	maxEventCount    = 65535
	retryDelay       = time.Second
)

// Service identifies the emitting application service.
type Service string

// Service values are the closed service vocabulary.
const (
	ServiceControlPlane Service = "control-plane"
	ServiceMediaPlane   Service = "media-plane"
)

// EventType identifies the closed security event category.
type EventType string

// EventType values are the security-event.v1 event vocabulary.
const (
	// #nosec G101 -- public closed-schema vocabulary, not credentials.
	EventAuthentication EventType = "authentication"
	EventMFA            EventType = "mfa"
	EventSession        EventType = "session"
	// #nosec G101 -- public schema enum, not a credential.
	EventCredentialEpoch    EventType = "credential_epoch"
	EventPrivilegedAction   EventType = "privileged_action"
	EventAudit              EventType = "audit"
	EventMediaAdmission     EventType = "media_admission"
	EventMediaAuthorization EventType = "media_authorization"
	EventMediaIntegrity     EventType = "media_integrity"
	EventSecurityControl    EventType = "security_control"
	EventDependency         EventType = "dependency"
	EventPipelineHealth     EventType = "pipeline_health"
)

// Outcome identifies the closed event result.
type Outcome string

// Outcome values are the security-event.v1 outcome vocabulary.
const (
	OutcomeSuccess  Outcome = "success"
	OutcomeFailure  Outcome = "failure"
	OutcomeDenied   Outcome = "denied"
	OutcomeDegraded Outcome = "degraded"
	OutcomeRestored Outcome = "restored"
)

// Severity identifies the closed event severity.
type Severity string

// Severity values are the security-event.v1 severity vocabulary.
const (
	SeverityInformational Severity = "informational"
	SeverityLow           Severity = "low"
	SeverityMedium        Severity = "medium"
	SeverityHigh          Severity = "high"
	SeverityCritical      Severity = "critical"
)

// ReasonCode identifies the closed reason vocabulary.
type ReasonCode string

// ReasonCode values are the security-event.v1 reason vocabulary.
const (
	// #nosec G101 -- public closed-schema vocabulary, not credentials.
	ReasonAuthenticationSucceeded ReasonCode = "authentication_succeeded"
	// #nosec G101 -- public schema enum, not a credential.
	ReasonInvalidCredentials ReasonCode = "invalid_credentials"
	ReasonAccountLocked      ReasonCode = "account_locked"
	ReasonAccountDisabled    ReasonCode = "account_disabled"
	ReasonChallengeRequired  ReasonCode = "challenge_required"
	ReasonChallengeVerified  ReasonCode = "challenge_verified"
	ReasonChallengeInvalid   ReasonCode = "challenge_invalid"
	ReasonChallengeExpired   ReasonCode = "challenge_expired"
	ReasonChallengeLocked    ReasonCode = "challenge_locked"
	ReasonFactorEnabled      ReasonCode = "factor_enabled"
	ReasonFactorDisabled     ReasonCode = "factor_disabled"
	ReasonRecoveryVerified   ReasonCode = "recovery_verified"
	ReasonRecoveryReset      ReasonCode = "recovery_reset"
	ReasonRefreshRotated     ReasonCode = "refresh_rotated"
	ReasonRefreshReplay      ReasonCode = "refresh_replay"
	// #nosec G101 -- public schema enum, not a credential.
	ReasonTokenTheftSuspected                ReasonCode = "token_theft_suspected"
	ReasonSessionRevoked                     ReasonCode = "session_revoked"
	ReasonSessionsRevoked                    ReasonCode = "sessions_revoked"
	ReasonRevocationModeChanged              ReasonCode = "revocation_mode_changed"
	ReasonCredentialEpochMismatch            ReasonCode = "credential_epoch_mismatch"              // #nosec G101 -- public schema enum, not a credential.
	ReasonCredentialEpochOperationInProgress ReasonCode = "credential_epoch_operation_in_progress" // #nosec G101 -- public schema enum, not a credential.
	ReasonCredentialEpochBackendUnavailable  ReasonCode = "credential_epoch_backend_unavailable"   // #nosec G101 -- public schema enum, not a credential.
	ReasonRateLimitExceeded                  ReasonCode = "rate_limit_exceeded"
	ReasonRateLimitBackendUnavailable        ReasonCode = "rate_limit_backend_unavailable"
	ReasonSourceBanned                       ReasonCode = "source_banned"
	ReasonSourceBanCreated                   ReasonCode = "source_ban_created"
	ReasonCloudflareAccessDenied             ReasonCode = "cloudflare_access_denied"
	ReasonAttestationRejected                ReasonCode = "attestation_rejected"
	ReasonAttestationIssued                  ReasonCode = "attestation_issued"
	ReasonAttestationCacheDegraded           ReasonCode = "attestation_cache_degraded"
	ReasonAuditCommitted                     ReasonCode = "audit_committed"
	ReasonAuditWriteFailed                   ReasonCode = "audit_write_failed"
	ReasonPrivilegedRouteDenied              ReasonCode = "privileged_route_denied"
	ReasonOriginRejected                     ReasonCode = "origin_rejected"
	ReasonAdmissionRejected                  ReasonCode = "admission_rejected"
	ReasonAdmissionGateInactive              ReasonCode = "admission_gate_inactive"
	ReasonSocketRateLimited                  ReasonCode = "socket_rate_limited"
	ReasonSocketHandlerFailed                ReasonCode = "socket_handler_failed"
	ReasonRoomClaimConflict                  ReasonCode = "room_claim_conflict"
	ReasonAuthorizationDenied                ReasonCode = "authorization_denied"
	ReasonAuthorizationRevoked               ReasonCode = "authorization_revoked"
	ReasonIdentityAuthorityMismatch          ReasonCode = "identity_authority_mismatch"
	ReasonIdentityAuthorityMissing           ReasonCode = "identity_authority_missing"
	ReasonCryptoVersionInvalid               ReasonCode = "crypto_version_invalid"
	ReasonPermissionDenied                   ReasonCode = "permission_denied"
	ReasonRevocationEnforced                 ReasonCode = "revocation_enforced"
	ReasonStructuralLimitExceeded            ReasonCode = "structural_limit_exceeded"
	ReasonMediaSchemaRejected                ReasonCode = "media_schema_rejected"
	ReasonDependencyUnavailable              ReasonCode = "dependency_unavailable"
	ReasonDependencyRecovered                ReasonCode = "dependency_recovered"
	ReasonSignedTelemetryRejected            ReasonCode = "signed_telemetry_rejected"
	ReasonDiskWatermark                      ReasonCode = "disk_watermark"
	ReasonWriterValidationDrop               ReasonCode = "writer_validation_drop"
	ReasonWriterBudgetDrop                   ReasonCode = "writer_budget_drop"
	ReasonWriterWriteFailed                  ReasonCode = "writer_write_failed"
	ReasonWriterRecovered                    ReasonCode = "writer_recovered"
	ReasonWriterSizeRefused                  ReasonCode = "writer_size_refused"
)

// AuthMethod identifies the closed authentication mechanism.
type AuthMethod string

// AuthMethod values are the security-event.v1 authentication vocabulary.
const (
	AuthPassword   AuthMethod = "password"
	AuthWebAuthn   AuthMethod = "webauthn"
	AuthTOTP       AuthMethod = "totp"
	AuthBackupCode AuthMethod = "backup_code"
	AuthSSO        AuthMethod = "sso"
	AuthRecovery   AuthMethod = "recovery"
	AuthSession    AuthMethod = "session"
)

// RouteTemplate identifies a closed route pattern.
type RouteTemplate string

// RouteTemplate values are the security-event.v1 route vocabulary.
const (
	RouteAuthLogin                 RouteTemplate = "POST /api/v1/auth/login"
	RouteAuthRefresh               RouteTemplate = "POST /api/v1/auth/refresh"
	RouteAuthLogout                RouteTemplate = "POST /api/v1/auth/logout"
	RouteAuthMFAVerify             RouteTemplate = "POST /api/v1/auth/mfa/verify"
	RouteRecoveryVerifyCode        RouteTemplate = "POST /api/v1/auth/recovery/verify-code"
	RouteRecoveryResetPassword     RouteTemplate = "POST /api/v1/auth/recovery/reset-password" // #nosec G101 -- public route template, not a credential.
	RouteRecoveryResetAccount      RouteTemplate = "POST /api/v1/auth/recovery/reset-account"
	RouteSessionDelete             RouteTemplate = "DELETE /api/v1/sessions/:id"
	RouteSessionsRevokeAll         RouteTemplate = "POST /api/v1/sessions/revoke-all"
	RouteSessionsRevocationMode    RouteTemplate = "PUT /api/v1/sessions/revocation-mode"
	RouteServerMemberPatch         RouteTemplate = "PATCH /api/v1/servers/:id/members/:user_id"
	RouteServerMemberDelete        RouteTemplate = "DELETE /api/v1/servers/:id/members/:user_id"
	RouteServerBanCreate           RouteTemplate = "POST /api/v1/servers/:id/bans/:user_id"
	RouteServerBanDelete           RouteTemplate = "DELETE /api/v1/servers/:id/bans/:user_id"
	RouteServerRoleCreate          RouteTemplate = "POST /api/v1/servers/:id/roles"
	RouteServerRolePatch           RouteTemplate = "PATCH /api/v1/servers/:id/roles/:role_id"
	RouteServerRoleDelete          RouteTemplate = "DELETE /api/v1/servers/:id/roles/:role_id"
	RouteServerMemberRoleCreate    RouteTemplate = "POST /api/v1/servers/:id/members/:user_id/roles"
	RouteServerMemberRoleDelete    RouteTemplate = "DELETE /api/v1/servers/:id/members/:user_id/roles/:role_id"
	RouteServerTransferOwnership   RouteTemplate = "POST /api/v1/servers/:id/transfer-ownership"
	RouteServerTransferOwnershipOK RouteTemplate = "POST /api/v1/servers/:id/transfer-ownership/confirm"
	RouteSocketJoin                RouteTemplate = "socket.join"
	RouteSocketProduce             RouteTemplate = "socket.produce"
	RouteSocketConsume             RouteTemplate = "socket.consume"
	RouteSocketPermissionsUpdate   RouteTemplate = "socket.permissions_update"
	RouteSocketForceDisconnect     RouteTemplate = "socket.force_disconnect"
)

// Emitter is deliberately a single-method interface so application outcomes
// cannot depend on telemetry delivery.
type Emitter interface{ Emit(context.Context, Event) }

// Event contains only producer-controlled selections from the closed schema.
type Event struct {
	EventType     EventType
	Outcome       Outcome
	Severity      Severity
	ReasonCode    ReasonCode
	AuthMethod    AuthMethod
	RouteTemplate RouteTemplate
	EvidenceRef   string
}

// HealthStatus summarizes writer availability.
type HealthStatus string

// HealthStatus values are the writer availability states.
const (
	HealthDisabled HealthStatus = "disabled"
	HealthHealthy  HealthStatus = "healthy"
	HealthDegraded HealthStatus = "degraded"
)

// Health is a race-safe snapshot of bounded writer counters.
type Health struct {
	Status                                           HealthStatus
	Accepted, Coalesced, Dropped, ValidationFailures uint64
	WriteFailures, StaleFileRetries, SizeRefusals    uint64
	PendingUnique                                    int
}

type discardEmitter struct{}

func (discardEmitter) Emit(context.Context, Event) {
	// Intentionally discard telemetry when the sink is disabled.
}

// Discard accepts telemetry without persisting it outside production.
var Discard Emitter = discardEmitter{}

type eventKey struct {
	eventType EventType
	outcome   Outcome
	severity  Severity
	reason    ReasonCode
	auth      AuthMethod
	route     RouteTemplate
	evidence  string
}

type pendingEvent struct {
	event      Event
	count      uint16
	corr       string
	occurredAt time.Time
}

type appendResult uint8

const (
	appendWritten appendResult = iota
	appendRetry
	appendDiscard
)

type partialWriteRepair struct {
	file *os.File
	size int64
}

// Writer coalesces and persists closed security events.
type Writer struct {
	mu sync.Mutex

	path    string
	service Service
	log     *logger.Logger
	now     func() time.Time
	newID   func() string

	bucket       time.Time
	pending      map[eventKey]*pendingEvent
	pendingOrder []eventKey
	health       Health
	closed       bool
	closeDone    bool
	failed       bool
	timer        timer
	timerID      uint64
	afterFunc    func(time.Duration, func()) timer
	repair       *partialWriteRepair
	fileWrite    func(*os.File, []byte) (int, error)
	fileTruncate func(*os.File, int64) error
	fileClose    func(*os.File) error

	// beforeAppend is test-only timing control for rotation; production leaves it nil.
	beforeAppend func()
	// beforeOpen is test-only write failure control; production leaves it nil.
	beforeOpen func() bool
}

// Open verifies that the bounded security-event sink is available.
func Open(path string, service Service, log *logger.Logger) (*Writer, error) {
	if !validService(service) {
		return nil, errors.New("security event service is invalid")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640) // #nosec G302,G304 -- production pre-creates a root-owned 0620 stream; Wazuh reads it as host root.
	if err != nil {
		return nil, errors.New("security event sink unavailable")
	}
	if err := f.Close(); err != nil {
		return nil, errors.New("security event sink unavailable")
	}
	return newWriter(path, service, log, time.Now, newUUIDv4), nil
}

func newWriter(path string, service Service, log *logger.Logger, now func() time.Time, newID func() string) *Writer {
	return &Writer{
		path: path, service: service, log: log, now: now, newID: newID, pending: make(map[eventKey]*pendingEvent),
		afterFunc:    func(delay time.Duration, callback func()) timer { return time.AfterFunc(delay, callback) },
		fileWrite:    func(file *os.File, payload []byte) (int, error) { return file.Write(payload) },
		fileTruncate: func(file *os.File, size int64) error { return file.Truncate(size) },
		fileClose:    func(file *os.File) error { return file.Close() },
	}
}

// Emit validates and coalesces an event without affecting the caller's outcome.
func (w *Writer) Emit(ctx context.Context, event Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if !validEvent(event) {
		w.health.ValidationFailures++
		return
	}
	now := w.now().UTC()
	bucket := now.Truncate(time.Second)
	if !w.bucket.IsZero() && !bucket.Equal(w.bucket) {
		w.stopTimerLocked()
		if err := w.flushLocked(); err != nil && len(w.pending) != 0 {
			w.startRetryTimerLocked()
		}
	}
	w.bucket = bucket
	corr := CorrelationFromContext(ctx)
	if corr == "" {
		var err error
		corr, err = newCorrelation()
		if err != nil {
			w.health.Dropped++
			return
		}
	}
	w.enqueueLocked(event, corr, bucket)
	if w.timer == nil {
		w.startTimerLocked(bucket, now)
	}
}

func (w *Writer) enqueueLocked(event Event, corr string, occurredAt time.Time) {
	key := eventKey{event.EventType, event.Outcome, event.Severity, event.ReasonCode, event.AuthMethod, event.RouteTemplate, event.EvidenceRef}
	if pending := w.pending[key]; pending != nil {
		if pending.count < maxEventCount {
			pending.count++
		}
		w.health.Coalesced++
		return
	}
	if len(w.pending) >= maxUniqueEvents {
		w.health.Dropped++
		return
	}
	w.pending[key] = &pendingEvent{event: event, count: 1, corr: corr, occurredAt: occurredAt}
	w.pendingOrder = append(w.pendingOrder, key)
	w.health.Accepted++
}

type timer interface{ Stop() bool }

func (w *Writer) startTimerLocked(bucket, now time.Time) {
	delay := bucket.Add(time.Second).Sub(now)
	if delay <= 0 {
		delay = time.Nanosecond
	}
	w.timerID++
	timerID := w.timerID
	w.timer = w.afterFunc(delay, func() { w.flushTimer(timerID) })
}

func (w *Writer) startRetryTimerLocked() {
	w.timerID++
	timerID := w.timerID
	w.timer = w.afterFunc(retryDelay, func() { w.flushTimer(timerID) })
}

func (w *Writer) stopTimerLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.timerID++
}

func (w *Writer) flushTimer(timerID uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.timer == nil || timerID != w.timerID {
		return
	}
	w.timer = nil
	w.timerID++
	if err := w.flushLocked(); err != nil && len(w.pending) != 0 {
		w.startRetryTimerLocked()
	}
	w.bucket = time.Time{}
}

func (w *Writer) flushLocked() error {
	if len(w.pending) == 0 {
		return nil
	}
	for index, key := range w.pendingOrder {
		item := w.pending[key]
		if item == nil {
			continue
		}
		wasFailed := w.failed
		writeFailures := w.health.WriteFailures
		switch w.appendEventLocked(item.event, item.count, item.corr, item.occurredAt) {
		case appendRetry:
			// A sink failure is transient until proven otherwise. Retaining this
			// entry also preserves write order: later events stay behind it.
			w.pendingOrder = w.pendingOrder[index:]
			return errors.New("security event sink write failed")
		case appendDiscard:
			w.recordDropLocked()
		case appendWritten:
			if wasFailed && writeFailures == w.health.WriteFailures {
				w.failed = false
				w.health.Status = HealthHealthy
				if w.log != nil {
					w.log.Info("Security event sink restored")
				}
			}
		}
		delete(w.pending, key)
	}
	w.pendingOrder = w.pendingOrder[:0]
	return nil
}

func (w *Writer) appendEventLocked(event Event, count uint16, corr string, occurredAt time.Time) appendResult {
	if !w.repairPartialWriteLocked() {
		return appendRetry
	}
	eventID := w.newID()
	if !validUUID(eventID) {
		w.health.ValidationFailures++
		return appendDiscard
	}
	record := struct {
		SchemaVersion string        `json:"schema_version"`
		EventID       string        `json:"event_id"`
		OccurredAt    string        `json:"occurred_at"`
		Service       Service       `json:"service"`
		EventType     EventType     `json:"event_type"`
		Outcome       Outcome       `json:"outcome"`
		Severity      Severity      `json:"severity"`
		ReasonCode    ReasonCode    `json:"reason_code"`
		AuthMethod    AuthMethod    `json:"auth_method,omitempty"`
		RouteTemplate RouteTemplate `json:"route_template,omitempty"`
		Count         uint16        `json:"count"`
		Correlation   string        `json:"correlation_ref"`
		EvidenceRef   string        `json:"evidence_ref,omitempty"`
	}{"security-event.v1", eventID, occurredAt.UTC().Format(time.RFC3339Nano), w.service, event.EventType, event.Outcome, event.Severity, event.ReasonCode, event.AuthMethod, event.RouteTemplate, count, corr, event.EvidenceRef}
	line, err := json.Marshal(record)
	if err != nil {
		return appendDiscard
	}
	if len(line)+1 > maxLineBytes {
		w.recordSizeRefusalLocked()
		return appendDiscard
	}
	return w.writeLineLocked(line)
}

func (w *Writer) writeLineLocked(line []byte) appendResult {
	if len(line)+1 > maxLineBytes {
		w.recordSizeRefusalLocked()
		return appendDiscard
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, info, stale := w.openCurrentFileLocked()
		if f == nil {
			if stale {
				continue
			}
			return appendRetry
		}
		return w.writeOpenFileLocked(f, info, line)
	}
	w.recordWriteFailureLocked()
	return appendRetry
}

// openCurrentFileLocked opens the sink and confirms that rotation did not
// replace it before the append. The stale result is retryable; all other
// failures have already recorded their degraded state.
func (w *Writer) openCurrentFileLocked() (*os.File, os.FileInfo, bool) {
	if w.beforeOpen != nil && w.beforeOpen() {
		w.recordWriteFailureLocked()
		return nil, nil, false
	}
	// #nosec G302 -- the producer appends to its pre-created root-owned 0620 stream; Wazuh reads it as host root.
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		w.recordWriteFailureLocked()
		return nil, nil, false
	}
	info, err := f.Stat()
	if err == nil && w.beforeAppend != nil {
		w.beforeAppend()
		w.beforeAppend = nil
	}
	current, currentErr := os.Stat(w.path)
	if err != nil || currentErr != nil {
		if !w.closeFileLocked(f) {
			return nil, nil, false
		}
		w.recordWriteFailureLocked()
		return nil, nil, false
	}
	if !os.SameFile(info, current) {
		if !w.closeFileLocked(f) {
			return nil, nil, false
		}
		w.health.StaleFileRetries++
		return nil, nil, true
	}
	return f, info, false
}

func (w *Writer) writeOpenFileLocked(f *os.File, info os.FileInfo, line []byte) appendResult {
	if info.Size() >= maxFileBytes || info.Size()+int64(len(line)+1) > maxFileBytes {
		if !w.closeFileLocked(f) {
			return appendRetry
		}
		w.recordSizeRefusalLocked()
		return appendDiscard
	}
	payload := append(line, '\n')
	written, writeErr := w.fileWrite(f, payload)
	if written == len(payload) {
		if writeErr != nil {
			w.recordWriteFailureLocked()
		}
		if err := w.fileClose(f); err != nil {
			w.recordWriteFailureLocked()
		}
		return appendWritten
	}
	if err := w.fileTruncate(f, info.Size()); err != nil {
		w.repair = &partialWriteRepair{file: f, size: info.Size()}
		w.recordWriteFailureLocked()
		return appendRetry
	}
	w.recordWriteFailureLocked()
	if err := w.fileClose(f); err != nil {
		w.recordWriteFailureLocked()
	}
	return appendRetry
}

func (w *Writer) repairPartialWriteLocked() bool {
	if w.repair == nil {
		return true
	}
	repair := w.repair
	if err := w.fileTruncate(repair.file, repair.size); err != nil {
		w.recordWriteFailureLocked()
		return false
	}
	w.repair = nil
	if err := w.fileClose(repair.file); err != nil {
		w.recordWriteFailureLocked()
	}
	return true
}

func (w *Writer) closeFileLocked(f *os.File) bool {
	if err := w.fileClose(f); err != nil {
		w.recordWriteFailureLocked()
		return false
	}
	return true
}

func (w *Writer) recordDropLocked() { w.health.Dropped++ }

func (w *Writer) recordSizeRefusalLocked() {
	w.health.SizeRefusals++
	w.health.Status = HealthDegraded
	if !w.failed && w.log != nil {
		w.log.Error("Security event sink refused oversized record")
	}
	w.failed = true
}

func (w *Writer) recordWriteFailureLocked() {
	w.health.WriteFailures++
	w.health.Status = HealthDegraded
	if !w.failed && w.log != nil {
		w.log.Error("Security event sink write failed")
	}
	w.failed = true
}

// Health returns a point-in-time writer health snapshot.
func (w *Writer) Health() Health {
	w.mu.Lock()
	defer w.mu.Unlock()
	health := w.health
	if health.Status == "" {
		health.Status = HealthHealthy
	}
	health.PendingUnique = len(w.pending)
	return health
}

// Close flushes the final bucket and is safe to call repeatedly.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closeDone {
		return nil
	}
	w.closed = true
	w.stopTimerLocked()
	if err := w.flushLocked(); err != nil {
		return errors.New("security event writer flush failed")
	}
	if w.failed {
		return errors.New("security event writer flush failed")
	}
	w.closeDone = true
	return nil
}

type correlationKey struct{}

// WithNewCorrelation replaces any client-derived correlation with server entropy.
func WithNewCorrelation(ctx context.Context) (context.Context, error) {
	corr, err := newCorrelation()
	if err != nil {
		return ctx, fmt.Errorf("generate security event correlation: %w", err)
	}
	return withCorrelation(ctx, corr), nil
}

// CorrelationFromContext returns only a valid server-generated correlation.
func CorrelationFromContext(ctx context.Context) string {
	corr, _ := ctx.Value(correlationKey{}).(string)
	if validCorrelation(corr) {
		return corr
	}
	return ""
}

func withCorrelation(ctx context.Context, corr string) context.Context {
	return context.WithValue(ctx, correlationKey{}, corr)
}

func newCorrelation() (string, error) {
	b := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newUUIDv4() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func validService(v Service) bool { return v == ServiceControlPlane || v == ServiceMediaPlane }
func validEvent(e Event) bool {
	return validEventType(e.EventType) && e.EventType != EventPipelineHealth && validOutcome(e.Outcome) && validSeverity(e.Severity) && validReason(e.ReasonCode) &&
		(e.AuthMethod == "" || validAuthMethod(e.AuthMethod)) && (e.RouteTemplate == "" || validRoute(e.RouteTemplate)) &&
		(e.EvidenceRef == "" || validUUID(e.EvidenceRef)) && !isWriterOwnedReason(e.ReasonCode)
}

func isWriterOwnedReason(reason ReasonCode) bool {
	_, ok := writerOwnedReasonSet[reason]
	return ok
}
func validEventType(v EventType) bool   { _, ok := eventTypes[v]; return ok }
func validOutcome(v Outcome) bool       { _, ok := outcomes[v]; return ok }
func validSeverity(v Severity) bool     { _, ok := severities[v]; return ok }
func validReason(v ReasonCode) bool     { _, ok := reasons[v]; return ok }
func validAuthMethod(v AuthMethod) bool { _, ok := authMethods[v]; return ok }
func validRoute(v RouteTemplate) bool   { _, ok := routes[v]; return ok }
func validUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if v[i] != '-' {
				return false
			}
			continue
		}
		if !isHex(v[i]) {
			return false
		}
	}
	return true
}
func validCorrelation(v string) bool {
	if len(v) < 16 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		if !isCorrelationRune(c) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isCorrelationRune(c rune) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

var eventTypes = set(EventAuthentication, EventMFA, EventSession, EventCredentialEpoch, EventPrivilegedAction, EventAudit, EventMediaAdmission, EventMediaAuthorization, EventMediaIntegrity, EventSecurityControl, EventDependency, EventPipelineHealth)
var outcomes = set(OutcomeSuccess, OutcomeFailure, OutcomeDenied, OutcomeDegraded, OutcomeRestored)
var severities = set(SeverityInformational, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical)
var authMethods = set(AuthPassword, AuthWebAuthn, AuthTOTP, AuthBackupCode, AuthSSO, AuthRecovery, AuthSession)
var routes = set(RouteAuthLogin, RouteAuthRefresh, RouteAuthLogout, RouteAuthMFAVerify, RouteRecoveryVerifyCode, RouteRecoveryResetPassword, RouteRecoveryResetAccount, RouteSessionDelete, RouteSessionsRevokeAll, RouteSessionsRevocationMode, RouteServerMemberPatch, RouteServerMemberDelete, RouteServerBanCreate, RouteServerBanDelete, RouteServerRoleCreate, RouteServerRolePatch, RouteServerRoleDelete, RouteServerMemberRoleCreate, RouteServerMemberRoleDelete, RouteServerTransferOwnership, RouteServerTransferOwnershipOK, RouteSocketJoin, RouteSocketProduce, RouteSocketConsume, RouteSocketPermissionsUpdate, RouteSocketForceDisconnect)
var reasons = set(ReasonAuthenticationSucceeded, ReasonInvalidCredentials, ReasonAccountLocked, ReasonAccountDisabled, ReasonChallengeRequired, ReasonChallengeVerified, ReasonChallengeInvalid, ReasonChallengeExpired, ReasonChallengeLocked, ReasonFactorEnabled, ReasonFactorDisabled, ReasonRecoveryVerified, ReasonRecoveryReset, ReasonRefreshRotated, ReasonRefreshReplay, ReasonTokenTheftSuspected, ReasonSessionRevoked, ReasonSessionsRevoked, ReasonRevocationModeChanged, ReasonCredentialEpochMismatch, ReasonCredentialEpochOperationInProgress, ReasonCredentialEpochBackendUnavailable, ReasonRateLimitExceeded, ReasonRateLimitBackendUnavailable, ReasonSourceBanned, ReasonSourceBanCreated, ReasonCloudflareAccessDenied, ReasonAttestationRejected, ReasonAttestationIssued, ReasonAttestationCacheDegraded, ReasonAuditCommitted, ReasonAuditWriteFailed, ReasonPrivilegedRouteDenied, ReasonOriginRejected, ReasonAdmissionRejected, ReasonAdmissionGateInactive, ReasonSocketRateLimited, ReasonSocketHandlerFailed, ReasonRoomClaimConflict, ReasonAuthorizationDenied, ReasonAuthorizationRevoked, ReasonIdentityAuthorityMismatch, ReasonIdentityAuthorityMissing, ReasonCryptoVersionInvalid, ReasonPermissionDenied, ReasonRevocationEnforced, ReasonStructuralLimitExceeded, ReasonMediaSchemaRejected, ReasonDependencyUnavailable, ReasonDependencyRecovered, ReasonSignedTelemetryRejected, ReasonDiskWatermark, ReasonWriterValidationDrop, ReasonWriterBudgetDrop, ReasonWriterWriteFailed, ReasonWriterRecovered, ReasonWriterSizeRefused)
var writerOwnedReasonSet = set(ReasonWriterValidationDrop, ReasonWriterBudgetDrop, ReasonWriterWriteFailed, ReasonWriterRecovered, ReasonWriterSizeRefused)

func set[T comparable](values ...T) map[T]struct{} {
	out := make(map[T]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
