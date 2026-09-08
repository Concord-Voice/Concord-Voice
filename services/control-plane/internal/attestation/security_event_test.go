package attestation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/securityevent"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type securityEventRecorder struct{ events []securityevent.Event }

func (r *securityEventRecorder) Emit(_ context.Context, event securityevent.Event) {
	r.events = append(r.events, event)
}

type blockingSecurityEventEmitter struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	firstOnce    sync.Once
	mu           sync.Mutex
	events       []securityevent.Event
}

func newBlockingSecurityEventEmitter() *blockingSecurityEventEmitter {
	return &blockingSecurityEventEmitter{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
}

func (e *blockingSecurityEventEmitter) Emit(_ context.Context, event securityevent.Event) {
	if event.Outcome == securityevent.OutcomeDegraded {
		e.firstOnce.Do(func() { close(e.firstStarted) })
		<-e.releaseFirst
	}
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *blockingSecurityEventEmitter) snapshot() []securityevent.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]securityevent.Event(nil), e.events...)
}

type concurrentSecurityEventEmitter struct{ emitted atomic.Uint64 }

func (e *concurrentSecurityEventEmitter) Emit(_ context.Context, _ securityevent.Event) {
	e.emitted.Add(1)
}

type securityEventReader struct{ err error }

func (r securityEventReader) ListActiveBinaries(context.Context) ([]ReleaseBinary, error) {
	return nil, r.err
}
func (r securityEventReader) ListActiveSPAs(context.Context) ([]ReleaseSPA, error) { return nil, r.err }

func TestCacheSecurityEventsReportDegradationAndRecovery(t *testing.T) {
	recorder := &securityEventRecorder{}
	cache := NewCache(securityEventReader{err: errors.New("unavailable")}, nil, nil, logger.New("test"))
	cache.SetSecurityEvents(recorder)
	require.Error(t, cache.Hydrate(context.Background()))
	require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAttestationCacheDegraded}}, recorder.events)
	cache.repo = securityEventReader{}
	require.NoError(t, cache.Hydrate(context.Background()))
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAttestationCacheDegraded},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func TestCacheDependencyRecoveryRequiresBothPostgresAndRedis(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	repo := &togglingSecurityEventReader{err: errors.New("postgres unavailable")}
	recorder := &securityEventRecorder{}
	cache := NewCache(repo, nil, rdb, logger.New("test"))
	cache.SetSecurityEvents(recorder)
	require.Error(t, cache.Hydrate(context.Background()))
	revoked, err := cache.IsRevoked(context.Background(), "v1")
	require.NoError(t, err)
	require.False(t, revoked)
	require.Len(t, recorder.events, 1, "healthy Redis must not clear PostgreSQL degradation")
	server.Close()
	revoked, err = cache.IsRevoked(context.Background(), "v1")
	require.Error(t, err)
	require.True(t, revoked)
	// Redis is still degraded, so a recovered Postgres hydrate cannot clear it.
	repo.err = nil
	require.NoError(t, cache.Hydrate(context.Background()))
	require.Len(t, recorder.events, 1)

	server = miniredis.RunT(t)
	cache.rdb = redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, cache.rdb.Close()) })
	revoked, err = cache.IsRevoked(context.Background(), "v1")
	require.NoError(t, err)
	require.False(t, revoked)
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAttestationCacheDegraded},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, recorder.events)
}

func TestCacheSecurityEventTransitionsRemainOrdered(t *testing.T) {
	emitter := newBlockingSecurityEventEmitter()
	cache := NewCache(securityEventReader{}, nil, nil, logger.New("test"))
	cache.SetSecurityEvents(emitter)
	degradedDone := make(chan struct{})
	go func() {
		cache.setDegraded(context.Background(), true)
		close(degradedDone)
	}()
	<-emitter.firstStarted
	restoredDone := make(chan struct{})
	go func() {
		cache.setDegraded(context.Background(), false)
		close(restoredDone)
	}()
	premature := false
	select {
	case <-restoredDone:
		premature = true
	case <-time.After(100 * time.Millisecond):
	}
	close(emitter.releaseFirst)
	<-degradedDone
	if !premature {
		<-restoredDone
	}
	require.False(t, premature, "recovery must wait for the degradation event")
	require.Equal(t, []securityevent.Event{
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAttestationCacheDegraded},
		{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeRestored, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonDependencyRecovered},
	}, emitter.snapshot())
}

type togglingSecurityEventReader struct{ err error }

func (r *togglingSecurityEventReader) ListActiveBinaries(context.Context) ([]ReleaseBinary, error) {
	return nil, r.err
}

func (r *togglingSecurityEventReader) ListActiveSPAs(context.Context) ([]ReleaseSPA, error) {
	return nil, r.err
}

func TestHandlerSecurityEventMappingsAreExact(t *testing.T) {
	t.Run("durable issue failure emits dependency degradation", func(t *testing.T) {
		brokenRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond, ReadTimeout: 10 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { require.NoError(t, brokenRedis.Close()) })
		handler := NewHandler(nil, nil, nil, nil, brokenRedis, logger.New("test"))
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		requestContext, response := issueTokenRequestContext(t)
		handler.issueToken(context.Background(), requestContext, "session-failed", testIssuedPayload(), time.Minute)
		require.Equal(t, 503, response.Code)
		require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)
	})

	t.Run("nil Redis emits dependency degradation", func(t *testing.T) {
		handler := NewHandler(nil, nil, nil, nil, nil, logger.New("test"))
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		requestContext, response := issueTokenRequestContext(t)
		handler.issueToken(context.Background(), requestContext, "session-failed", testIssuedPayload(), time.Minute)
		require.Equal(t, 503, response.Code)
		require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)
	})

	t.Run("durable issue success emits exactly one issued event", func(t *testing.T) {
		server := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
		t.Cleanup(func() { require.NoError(t, rdb.Close()) })
		handler := NewHandler(nil, nil, nil, nil, rdb, logger.New("test"))
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		requestContext, response := issueTokenRequestContext(t)
		handler.issueToken(context.Background(), requestContext, "session-issued", testIssuedPayload(), time.Minute)
		require.Equal(t, 200, response.Code)
		require.True(t, server.Exists("attestation:session-issued:web"), "the durable token record must exist before its mirror")
		require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeSuccess, Severity: securityevent.SeverityInformational, ReasonCode: securityevent.ReasonAttestationIssued}}, recorder.events)
	})

	t.Run("OIDC dependency absence emits degradation", func(t *testing.T) {
		handler := NewHandler(nil, nil, nil, nil, nil, logger.New("test"))
		recorder := &securityEventRecorder{}
		handler.SetSecurityEvents(recorder)
		response := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(response)
		requestContext.Request = httptest.NewRequest("POST", "/internal/attestation/publish/spa", nil)
		require.False(t, handler.requireOIDC(requestContext))
		require.Equal(t, []securityevent.Event{{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable}}, recorder.events)
	})
	t.Run("missing and malformed bearer emit rejection", func(t *testing.T) {
		for _, authorization := range []string{"", "Basic token"} {
			handler := NewHandler(nil, nil, nil, nil, nil, logger.New("test"))
			recorder := &securityEventRecorder{}
			handler.SetSecurityEvents(recorder)
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest("POST", "/internal/attestation/publish/spa", nil)
			requestContext.Request.Header.Set("Authorization", authorization)

			_, ok := handler.extractBearer(requestContext)
			require.False(t, ok)
			require.Equal(t, 401, response.Code)
			require.Equal(t, []securityevent.Event{{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityMedium, ReasonCode: securityevent.ReasonAttestationRejected}}, recorder.events)
		}
	})
}

func TestVerifyRedisRevocationFailureIsDependencyNotAttestationRejection(t *testing.T) {
	brokenRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond, ReadTimeout: 10 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, brokenRedis.Close()) })
	cache := NewCache(nil, nil, brokenRedis, logger.New("test"))
	cache.spas["20260901"] = &ReleaseSPA{SpaVersion: "20260901", HTMLHash: "hash"}
	handler := NewHandler(nil, cache, nil, nil, nil, logger.New("test"))
	recorder := &securityEventRecorder{}
	handler.SetSecurityEvents(recorder)
	response := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(response)
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/attestation/verify", strings.NewReader(`{"version":"1.0.0","platform":"web","spa_version":"20260901","spa_hash":"hash"}`))
	requestContext.Request.Header.Set("Content-Type", "application/json")
	requestContext.Request.Header.Set("X-Session-ID", "fixture-session")
	requestContext.Set("user_id", "fixture-user")

	handler.Verify(requestContext, time.Minute)

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, recorder.events, securityevent.Event{EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable})
	for _, event := range recorder.events {
		require.NotEqual(t, securityevent.Event{EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied, Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAttestationRejected}, event)
	}
}

func TestPublishFailureSecurityEventMappingsAreExact(t *testing.T) {
	require.Equal(t, securityevent.Event{
		EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
		Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonAttestationRejected,
	}, publishFailureEvent(ErrConflict))
	require.Equal(t, securityevent.Event{
		EventType: securityevent.EventDependency, Outcome: securityevent.OutcomeDegraded,
		Severity: securityevent.SeverityHigh, ReasonCode: securityevent.ReasonDependencyUnavailable,
	}, publishFailureEvent(errors.New("repository unavailable")))
}

func TestRejectedAttestationSeverityDependsOnRejection(t *testing.T) {
	handler := NewHandler(nil, nil, nil, nil, nil, logger.New("test"))
	for _, tc := range []struct {
		name     string
		code     ErrorCode
		severity securityevent.Severity
	}{
		{name: "revoked", code: ErrRevoked, severity: securityevent.SeverityHigh},
		{name: "invalid", code: ErrInvalid, severity: securityevent.SeverityMedium},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &securityEventRecorder{}
			handler.SetSecurityEvents(recorder)
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest("POST", "/api/v1/attestation/verify", nil)
			handler.reject(requestContext, tc.code, "1.0.0", PlatformWeb)
			require.Equal(t, []securityevent.Event{{
				EventType: securityevent.EventSecurityControl, Outcome: securityevent.OutcomeDenied,
				Severity: tc.severity, ReasonCode: securityevent.ReasonAttestationRejected,
			}}, recorder.events)
		})
	}
}

func issueTokenRequestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	response := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(response)
	requestContext.Request = httptest.NewRequest("POST", "/api/v1/attestation/verify", nil)
	requestContext.Set("user_id", "nightwatch-user")
	return requestContext, response
}

func testIssuedPayload() VerifyPayload {
	return VerifyPayload{Version: "1.0.0", SpaVersion: "20260901", Platform: PlatformWeb}
}

func TestHandlerSecurityEventSetterIsRaceSafe(t *testing.T) {
	cache := NewCache(securityEventReader{}, nil, nil, logger.New("test"))
	handler := NewHandler(nil, cache, nil, nil, nil, logger.New("test"))
	emitter := &concurrentSecurityEventEmitter{}
	handler.SetSecurityEvents(emitter)
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			handler.SetSecurityEvents(emitter)
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for index := range 500 {
			cache.setDegraded(context.Background(), index%2 == 0)
		}
	}()
	go func() {
		defer group.Done()
		<-start
		for range 500 {
			response := httptest.NewRecorder()
			requestContext, _ := gin.CreateTestContext(response)
			requestContext.Request = httptest.NewRequest("POST", "/internal/attestation/publish/spa", nil)
			handler.requireOIDC(requestContext)
		}
	}()
	close(start)
	group.Wait()
	require.Equal(t, uint64(1000), emitter.emitted.Load())
}
