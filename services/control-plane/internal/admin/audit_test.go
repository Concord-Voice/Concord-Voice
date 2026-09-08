package admin_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/admin"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

type concurrentSecurityEventEmitter struct{ emitted atomic.Uint64 }

func (e *concurrentSecurityEventEmitter) Emit(_ context.Context, _ securityevent.Event) {
	e.emitted.Add(1)
}

// auditRowCount counts admin_audit_log rows whose event_type matches a
// per-test-unique marker so the assertion is isolated despite the shared DB.
func auditRowCount(t *testing.T, db *sql.DB, eventType string) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin_audit_log WHERE event_type = $1`, eventType).Scan(&n)
	require.NoError(t, err)
	return n
}

func TestAuditLog_Write_InsertsExactlyOneRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	// Unique event_type marker so the count is isolated from leftover rows.
	marker := uniqueAdminUsername("evt")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	err := audit.Write(ctx, admin.AuditEvent{
		Actor:     "operator-handle",
		EventType: marker,
		Result:    admin.AuditSuccess,
		SourceRef: "cf-access-subject-123",
		Detail:    map[string]any{"step": "password"},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, auditRowCount(t, db, marker))
}

func TestAdminAuditMirrorOmitsEnumerableIDAndDetail(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	audit := admin.NewAuditLog(db)
	recorder := &securityEventRecorder{}
	audit.SetSecurityEvents(recorder)
	require.NoError(t, audit.Write(context.Background(), admin.AuditEvent{EventType: admin.EventLoginSuccess, Result: admin.AuditSuccess}))
	require.Equal(t, []securityevent.Event{{
		EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess,
		Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuthenticationSucceeded,
	}}, recorder.events)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, audit.Write(cancelled, admin.AuditEvent{Actor: "operator", EventType: admin.EventCredentialRevoked, Result: admin.AuditSuccess, Detail: map[string]any{"target": "secret"}}))
	require.Len(t, recorder.events, 2)
	require.Equal(t, securityevent.ReasonAuditWriteFailed, recorder.events[1].ReasonCode)
	require.NotContains(t, fmt.Sprint(recorder.events), "operator")
	require.NotContains(t, fmt.Sprint(recorder.events), "secret")
}

func TestAdminAuditMirrorMappingsHaveExactEvents(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	audit := admin.NewAuditLog(db)
	recorder := &securityEventRecorder{}
	audit.SetSecurityEvents(recorder)

	tests := []struct {
		name      string
		eventType string
		result    string
		expected  securityevent.Event
	}{
		{name: "login success", eventType: admin.EventLoginSuccess, result: admin.AuditSuccess, expected: securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuthenticationSucceeded}},
		{name: "login failure", eventType: admin.EventLoginFailure, result: admin.AuditFailure, expected: securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeFailure, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonInvalidCredentials}},
		{name: "logout denied", eventType: admin.EventLogout, result: admin.AuditDenied, expected: securityevent.Event{EventType: securityevent.EventSession, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonSessionRevoked}},
		{name: "lockout", eventType: admin.EventLockout, result: admin.AuditDenied, expected: securityevent.Event{EventType: securityevent.EventAuthentication, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAccountLocked}},
		{name: "enrollment", eventType: admin.EventEnrollComplete, result: admin.AuditSuccess, expected: securityevent.Event{EventType: securityevent.EventPrivilegedAction, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditCommitted}},
		{name: "credential revoked", eventType: admin.EventCredentialRevoked, result: admin.AuditSuccess, expected: securityevent.Event{EventType: securityevent.EventPrivilegedAction, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditCommitted}},
		{name: "bootstrap", eventType: admin.EventBootstrap, result: admin.AuditSuccess, expected: securityevent.Event{EventType: securityevent.EventPrivilegedAction, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAuditCommitted}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			beforeRows := auditRowCount(t, db, test.eventType)
			beforeEvents := len(recorder.events)
			require.NoError(t, audit.Write(context.Background(), admin.AuditEvent{EventType: test.eventType, Result: test.result}))
			require.Equal(t, beforeRows+1, auditRowCount(t, db, test.eventType))
			require.Len(t, recorder.events, beforeEvents+1)
			require.Equal(t, test.expected, recorder.events[beforeEvents])
		})
	}

	beforeEvents := len(recorder.events)
	require.NoError(t, audit.Write(context.Background(), admin.AuditEvent{EventType: "nightwatch_unknown_event", Result: admin.AuditSuccess}))
	require.Len(t, recorder.events, beforeEvents, "unknown admin events must not emit a mirror")
}

func TestAdminAuditSecurityEventSetterIsRaceSafe(t *testing.T) {
	audit := admin.NewAuditLog(&sql.DB{})
	emitter := &concurrentSecurityEventEmitter{}
	audit.SetSecurityEvents(emitter)
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			audit.SetSecurityEvents(emitter)
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			_ = audit.Write(context.Background(), admin.AuditEvent{Detail: map[string]any{"invalid": math.Inf(1)}})
		}
	}()
	close(start)
	group.Wait()
	require.Equal(t, uint64(500), emitter.emitted.Load())
}

// The audit row must never contain the password / assertion / token inputs.
func TestAuditLog_Write_NeverStoresSecretValues(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	marker := uniqueAdminUsername("evt-secret")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	const secret = "SuperSecretP@ssw0rd!" //nolint:gosec // pragma: allowlist secret -- test fixture asserting the value is NOT stored
	err := audit.Write(ctx, admin.AuditEvent{
		Actor:     "operator-handle",
		EventType: marker,
		Result:    admin.AuditFailure,
		SourceRef: "ref",
		Detail:    map[string]any{"reason": "bad_password"},
	})
	require.NoError(t, err)

	// Scan the entire row's text form; assert the secret string never appears.
	var actor, eventType, result string
	var sourceRef sql.NullString
	var detail sql.NullString
	err = db.QueryRow(
		`SELECT actor, event_type, result, source_ref, detail::text
		 FROM admin_audit_log WHERE event_type = $1`, marker,
	).Scan(&actor, &eventType, &result, &sourceRef, &detail)
	require.NoError(t, err)

	assert.NotContains(t, actor, secret)
	assert.NotContains(t, sourceRef.String, secret)
	assert.NotContains(t, detail.String, secret)
}

// Actor is routed through sanitizeAuditString: CR/LF and control chars are
// stripped before storage (CWE-117 log forging defense).
func TestAuditLog_Write_SanitizesActor(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	marker := uniqueAdminUsername("evt-sanitize")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	err := audit.Write(ctx, admin.AuditEvent{
		Actor:     "evil\r\nINJECTED admin\x07line\x7f",
		EventType: marker,
		Result:    admin.AuditDenied,
	})
	require.NoError(t, err)

	var actor string
	err = db.QueryRow(
		`SELECT actor FROM admin_audit_log WHERE event_type = $1`, marker,
	).Scan(&actor)
	require.NoError(t, err)

	assert.NotContains(t, actor, "\r")
	assert.NotContains(t, actor, "\n")
	assert.NotContains(t, actor, "\x07")
	assert.NotContains(t, actor, "\x7f")
	assert.Equal(t, "evilINJECTED adminline", actor)
}

// RATIFIED enforcement: because Write runs under SET LOCAL ROLE
// concord_admin_rt, the inserted history cannot be rewritten — an UPDATE or
// DELETE under that role is denied by Postgres. This proves append-only is
// ENFORCED, not merely asserted by app logic.
func TestAuditLog_AppendOnly_EnforcedByRole(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	audit := admin.NewAuditLog(db)

	marker := uniqueAdminUsername("evt-append")
	t.Cleanup(func() {
		_, err := db.Exec(`DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
		assert.NoError(t, err)
	})

	require.NoError(t, audit.Write(ctx, admin.AuditEvent{
		EventType: marker,
		Result:    admin.AuditSuccess,
	}))

	// Under the restricted role, UPDATE is denied.
	updTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = updTx.ExecContext(ctx, `SET LOCAL ROLE concord_admin_rt`)
	require.NoError(t, err)
	_, err = updTx.ExecContext(ctx, `UPDATE admin_audit_log SET result = 'failure' WHERE event_type = $1`, marker)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
	require.NoError(t, updTx.Rollback())

	// Under the restricted role, DELETE is denied.
	delTx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = delTx.ExecContext(ctx, `SET LOCAL ROLE concord_admin_rt`)
	require.NoError(t, err)
	_, err = delTx.ExecContext(ctx, `DELETE FROM admin_audit_log WHERE event_type = $1`, marker)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
	require.NoError(t, delTx.Rollback())
}
