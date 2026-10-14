package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dmIconFixture struct {
	ts                        *testSetup
	member, conversation, key string
	body                      []byte
}

func newDMIconFixture(t *testing.T) dmIconFixture {
	t.Helper()
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "iconowner")
	member := ts.createTestUser(t, "iconmember")
	conversation := ts.createTestGroupDM(t, owner, member)
	f := dmIconFixture{ts: ts, member: member, conversation: conversation, key: "dm-icons/" + conversation, body: makePNG(t, 64, 64)}
	var exists bool
	require.NoError(t, ts.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id=$1 AND user_id=$2)`, conversation, member).Scan(&exists))
	require.True(t, exists, "real membership is a fixture precondition")
	require.NoError(t, ts.store.PutObject(context.Background(), f.key, bytes.NewReader(f.body), int64(len(f.body)), mimeImagePNG))
	require.True(t, ts.store.hasObject(f.key), "the denied request must target a real stored icon")
	return f
}

func (f dmIconFixture) request(ctx context.Context, identity any, conversation string, writer http.ResponseWriter) {
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/media/dm-icons/"+conversation, nil).WithContext(ctx)
	if identity != nil {
		c.Set("user_id", identity)
	}
	c.Params = gin.Params{{Key: "conversationId", Value: conversation}}
	f.ts.handler.ProxyDMIcon(c)
}

func TestProxyDMIconMembershipAndFailures(t *testing.T) {
	f := newDMIconFixture(t)
	outsider := f.ts.createTestUser(t, "iconoutsider")
	removed := f.ts.createTestUser(t, "iconremoved")
	_, err := f.ts.db.Exec(`INSERT INTO dm_participants (conversation_id,user_id) VALUES ($1,$2)`, f.conversation, removed)
	require.NoError(t, err)
	result, err := f.ts.db.Exec(`DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, removed)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	unknown := uuid.NewString()

	t.Run("member_seeded_store_control", func(t *testing.T) {
		w := httptest.NewRecorder()
		f.request(context.Background(), f.member, f.conversation, w)
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, f.body, w.Body.Bytes())
		require.Equal(t, mimeImagePNG, w.Header().Get("Content-Type"))
		require.Equal(t, 1, f.ts.store.GetObjectCalls())
	})

	var refusalBody, refusalCache string
	cases := []struct {
		name                      string
		identity                  any
		conversation              string
		code, gets                int
		message                   string
		hidden, nilStore, missing bool
	}{
		{name: "member_private_cache", identity: f.member, conversation: f.conversation, code: 200, gets: 1},
		{name: "canonical_conversation_key", identity: f.member, conversation: strings.ToUpper(f.conversation), code: 200, gets: 1},
		{name: "unknown", identity: f.member, conversation: unknown, code: 404, message: "Not found"},
		{name: "nonmember", identity: outsider, conversation: f.conversation, code: 404, message: "Not found"},
		{name: "removed_member", identity: removed, conversation: f.conversation, code: 404, message: "Not found"},
		{name: "hidden_current_member", identity: f.member, conversation: f.conversation, code: 200, gets: 1, hidden: true},
		{name: "missing_identity", conversation: f.conversation, code: 401, message: "Authentication required"},
		{name: "empty_identity", identity: "", conversation: f.conversation, code: 401, message: "Authentication required"},
		{name: "invalid_identity", identity: "not-a-user-uuid", conversation: f.conversation, code: 401, message: "Authentication required"},
		{name: "wrong_identity_type", identity: 42, conversation: f.conversation, code: 401, message: "Authentication required"},
		{name: "malformed_conversation", identity: f.member, conversation: "not-uuid", code: 400, message: "Invalid conversation ID"},
		{name: "missing_object", identity: f.member, conversation: f.conversation, code: 404, gets: 1, message: "Not found", missing: true},
		{name: "authorized_nil_store", identity: f.member, conversation: f.conversation, code: 503, message: "Object storage unavailable", nilStore: true},
		{name: "nonmember_nil_store", identity: outsider, conversation: f.conversation, code: 404, message: "Not found", nilStore: true},
		{name: "unknown_nil_store", identity: f.member, conversation: unknown, code: 404, message: "Not found", nilStore: true},
		{name: "malformed_nil_store", identity: f.member, conversation: "not-uuid", code: 400, message: "Invalid conversation ID", nilStore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			// Seed BOTH the known and absent conversation keys, preventing storage absence
			// from masquerading as membership enforcement.
			for _, key := range []string{f.key, "dm-icons/" + unknown} {
				require.NoError(t, store.PutObject(context.Background(), key, bytes.NewReader(f.body), int64(len(f.body)), mimeImagePNG))
			}
			if tc.missing {
				require.NoError(t, store.DeleteObject(context.Background(), f.key))
			}
			f.ts.handler.store = store
			if tc.nilStore {
				f.ts.handler.store = nil
			}
			if tc.hidden {
				_, err := f.ts.db.Exec(`UPDATE dm_participants SET hidden_at=NOW() WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
				require.NoError(t, err)
			}
			logs := captureDMIconLogs(f)
			w := httptest.NewRecorder()
			f.request(context.Background(), tc.identity, tc.conversation, w)
			assert.Equal(t, tc.code, w.Code)
			assert.Equal(t, tc.gets, store.GetObjectCalls())
			assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
			if tc.code == 200 {
				assert.Equal(t, f.body, w.Body.Bytes())
				assert.Equal(t, mimeImagePNG, w.Header().Get("Content-Type"))
			} else {
				assert.JSONEq(t, `{"error":"`+tc.message+`"}`, w.Body.String())
				assert.NotContains(t, w.Body.String(), string(f.body))
			}
			if tc.name == "unknown" || tc.name == "nonmember" || tc.name == "removed_member" {
				assert.Empty(t, logs.String(), "indistinguishable refusals must not produce identifying diagnostics")
			}
			if tc.name == "unknown" {
				refusalBody = w.Body.String()
				refusalCache = w.Header().Get("Cache-Control")
			}
			if tc.name == "nonmember" || tc.name == "removed_member" {
				assert.Equal(t, refusalBody, w.Body.String(), "refusals must be byte-identical")
				assert.Equal(t, refusalCache, w.Header().Get("Cache-Control"))
			}
		})
	}
}

// The probe preserves the normal in-memory store's acquisition and partial-read
// faults. Its barriers expose where the real handler is blocked, without
// replacing membership or transaction behavior.
type dmIconProbeStore struct {
	*mockStore
	mu                      sync.Mutex
	ctx                     context.Context
	key                     string
	reader                  *dmIconProbeReader
	getStarted              chan struct{}
	getRelease              <-chan struct{}
	readStarted             chan struct{}
	readRelease             <-chan struct{}
	closeStarted            chan struct{}
	closeRelease            <-chan struct{}
	closeErr                error
	cancelRead, cancelClose context.CancelFunc
}

func (s *dmIconProbeStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	source, typ, err := s.mockStore.GetObject(ctx, key)
	s.mu.Lock()
	s.ctx, s.key = ctx, key
	if err == nil {
		s.reader = &dmIconProbeReader{source: source, ctx: ctx, store: s}
	}
	r := s.reader
	s.mu.Unlock()
	if s.getStarted != nil {
		close(s.getStarted)
	}
	if err != nil {
		return nil, typ, err
	}
	if s.getRelease != nil {
		select {
		case <-s.getRelease:
		case <-ctx.Done():
			closeErr := r.Close()
			if closeErr != nil {
				return nil, "", closeErr
			}
			return nil, "", ctx.Err()
		}
	}
	return r, typ, nil
}

func (s *dmIconProbeStore) closeCount() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader == nil {
		return 0
	}
	return s.reader.closes.Load()
}

func (s *dmIconProbeStore) storageContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

type dmIconProbeReader struct {
	source              io.ReadCloser
	ctx                 context.Context
	store               *dmIconProbeStore
	readOnce, closeOnce sync.Once
	closes              atomic.Int32
	bytesRead           atomic.Int64
}

func (r *dmIconProbeReader) Read(p []byte) (int, error) {
	r.readOnce.Do(func() {
		if r.store.readStarted != nil {
			close(r.store.readStarted)
		}
		if r.store.cancelRead != nil {
			r.store.cancelRead()
		}
	})
	if r.store.readRelease != nil {
		select {
		case <-r.store.readRelease:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n, err := r.source.Read(p)
	r.bytesRead.Add(int64(n))
	return n, err
}

func (r *dmIconProbeReader) Close() error {
	r.closes.Add(1)
	r.closeOnce.Do(func() {
		if r.store.closeStarted != nil {
			close(r.store.closeStarted)
		}
		if r.store.cancelClose != nil {
			r.store.cancelClose()
		}
	})
	if r.store.closeRelease != nil {
		select {
		case <-r.store.closeRelease:
		case <-r.ctx.Done():
		}
	}
	err := r.source.Close()
	if err != nil {
		return err
	}
	return r.store.closeErr
}

func installDMIconProbe(f dmIconFixture) *dmIconProbeStore {
	s := &dmIconProbeStore{mockStore: f.ts.store}
	f.ts.handler.store = s
	return s
}

func captureDMIconLogs(f dmIconFixture) *bytes.Buffer {
	b := &bytes.Buffer{}
	f.ts.handler.log = &logger.Logger{Logger: slog.New(slog.NewJSONHandler(b, nil))}
	return b
}

func assertDMIconPrivateFailure(t *testing.T, f dmIconFixture, w *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.LessOrEqual(t, w.Body.Len(), 512, "failure must have a small generic JSON body, never a streamed image")
	if w.Code == http.StatusInternalServerError && w.Body.Len() <= 512 {
		assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
	}
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.False(t, bytes.Contains(w.Body.Bytes(), f.body), "no fixture image bytes may be emitted")
}

func assertDMIconFailureLogs(t *testing.T, f dmIconFixture, b *bytes.Buffer, classes ...string) {
	t.Helper()
	data := b.String()
	assert.NotEmpty(t, data, "the operational failure needs a fixed failure class")
	for _, sensitiveValue := range []string{f.member, f.conversation, f.key, "sensitive-fixture-error", "connection reset mid-transfer"} {
		assert.NotContains(t, data, sensitiveValue)
	}
	decoder := json.NewDecoder(strings.NewReader(data))
	hasPrimary := false
	for decoder.More() {
		var record map[string]any
		require.NoError(t, decoder.Decode(&record))
		for key := range record {
			assert.Contains(t, []string{"time", "level", "msg", "failure_class"}, key, "only fixed diagnostic fields are allowed")
		}
		assert.Equal(t, "Failed to retrieve DM icon", record["msg"], "operational logs must use the fixed message")
		class, ok := record["failure_class"].(string)
		assert.True(t, ok, "the class must be a string")
		assert.Contains(t, append(classes, "cleanup"), class)
		if class != "cleanup" {
			for _, expectedClass := range classes {
				if class == expectedClass {
					hasPrimary = true
					break
				}
			}
		}
	}
	assert.True(t, hasPrimary, "at least one operational log must report an expected primary failure class")
}

func openDMIconDB(t *testing.T, app, searchPath string) *sql.DB {
	t.Helper()
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	require.NotEmpty(t, u.Host, "explicit throwaway DATABASE_URL is required")
	q := u.Query()
	q.Set("application_name", app)
	if searchPath != "" {
		q.Set("options", "-c search_path="+searchPath)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	return db
}

func rollbackDMIconTx(t *testing.T, tx *sql.Tx) {
	t.Helper()
	err := tx.Rollback()
	if !errors.Is(err, sql.ErrTxDone) {
		assert.NoError(t, err)
	}
}

// A real conflicting DELETE verifies cleanup, rather than inspecting a mock
// transaction or merely observing an idle connection count.
func assertDMIconLockReleased(t *testing.T, f dmIconFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := f.ts.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer rollbackDMIconTx(t, tx)
	result, err := tx.ExecContext(ctx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
	require.NoError(t, err, "a failed read must release its participant-row lock")
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)
	require.NoError(t, tx.Rollback())
}

func TestProxyDMIconReadFailures(t *testing.T) {
	cases := []struct {
		name, class string
		configure   func(*testing.T, dmIconFixture, *dmIconProbeStore)
		gets        int
		closes      int32
	}{
		{name: "database_begin", class: "database", configure: func(t *testing.T, f dmIconFixture, _ *dmIconProbeStore) {
			db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
			require.NoError(t, err)
			require.NoError(t, db.Close())
			f.ts.handler.db = db
		}},
		{name: "database_lookup", class: "database", configure: func(t *testing.T, f dmIconFixture, _ *dmIconProbeStore) {
			db := openDMIconDB(t, "dm_icon_lookup_fault", "pg_catalog")
			var exists bool
			require.NoError(t, db.QueryRow(`SELECT to_regclass('dm_participants') IS NOT NULL`).Scan(&exists))
			require.False(t, exists, "only the lookup is faulted; the real database is reachable")
			f.ts.handler.db = db
		}},
		{name: "initial_get_error", class: "storage", gets: 1, configure: func(_ *testing.T, f dmIconFixture, s *dmIconProbeStore) {
			s.getErr = errors.New("sensitive-fixture-error " + f.key)
		}},
		{name: "partial_reader_error", class: "read", gets: 1, closes: 1, configure: func(_ *testing.T, f dmIconFixture, s *dmIconProbeStore) { s.getPartial = f.body }},
		{name: "close_error", class: "close", gets: 1, closes: 1, configure: func(_ *testing.T, f dmIconFixture, s *dmIconProbeStore) {
			s.closeErr = errors.New("sensitive-fixture-error " + f.key)
		}},
		{name: "five_MiB_plus_one", class: "size", gets: 1, closes: 1, configure: func(t *testing.T, f dmIconFixture, s *dmIconProbeStore) {
			body := bytes.Repeat([]byte("x"), 5*1024*1024+1)
			require.NoError(t, s.PutObject(context.Background(), f.key, bytes.NewReader(body), int64(len(body)), mimeImagePNG))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDMIconFixture(t)
			logs := captureDMIconLogs(f)
			s := installDMIconProbe(f)
			tc.configure(t, f, s)
			w := httptest.NewRecorder()
			f.request(context.Background(), f.member, f.conversation, w)
			assertDMIconPrivateFailure(t, f, w)
			assert.Equal(t, tc.gets, s.GetObjectCalls())
			assert.Equal(t, tc.closes, s.closeCount())
			assertDMIconFailureLogs(t, f, logs, tc.class)
			assertDMIconLockReleased(t, f)
		})
	}
	t.Run("exactly_five_MiB_accepted", func(t *testing.T) {
		f := newDMIconFixture(t)
		body := bytes.Repeat([]byte("x"), 5*1024*1024)
		require.NoError(t, f.ts.store.PutObject(context.Background(), f.key, bytes.NewReader(body), int64(len(body)), "image/webp"))
		s := installDMIconProbe(f)
		w := httptest.NewRecorder()
		f.request(context.Background(), f.member, f.conversation, w)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, body, w.Body.Bytes())
		assert.Equal(t, "image/webp", w.Header().Get("Content-Type"))
		assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
		assert.Equal(t, int32(1), s.closeCount())
		assertDMIconLockReleased(t, f)
	})
	t.Run("large_object_read_is_bounded", func(t *testing.T) {
		f := newDMIconFixture(t)
		body := bytes.Repeat([]byte("x"), 6*1024*1024)
		require.NoError(t, f.ts.store.PutObject(context.Background(), f.key, bytes.NewReader(body), int64(len(body)), mimeImagePNG))
		s := installDMIconProbe(f)
		w := httptest.NewRecorder()
		f.request(context.Background(), f.member, f.conversation, w)
		assertDMIconPrivateFailure(t, f, w)
		require.NotNil(t, s.reader)
		assert.LessOrEqual(t, s.reader.bytesRead.Load(), int64(5*1024*1024+1))
		assert.Equal(t, int32(1), s.closeCount())
		assertDMIconLockReleased(t, f)
	})
}

func awaitDMIconSignal(ctx context.Context, t *testing.T, ch <-chan struct{}, label string) bool {
	t.Helper()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		assert.Fail(t, "barrier was not reached", label)
		return false
	}
}

func TestProxyDMIconDeadlineAndCleanup(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		t.Run(map[bool]string{false: "five_second_storage_deadline", true: "earlier_parent_deadline"}[earlier], func(t *testing.T) {
			f := newDMIconFixture(t)
			s := installDMIconProbe(f)
			ctx := context.Background()
			var cancel context.CancelFunc
			parentDeadline := time.Now().Add(2 * time.Second)
			if earlier {
				ctx, cancel = context.WithDeadline(ctx, parentDeadline)
				defer cancel()
			}
			w := httptest.NewRecorder()
			start := time.Now()
			f.request(ctx, f.member, f.conversation, w)
			assert.Equal(t, http.StatusOK, w.Code)
			require.NotNil(t, s.storageContext())
			deadline, ok := s.storageContext().Deadline()
			assert.True(t, ok, "storage must inherit a finite retrieval deadline")
			if ok {
				assert.LessOrEqual(t, deadline.Sub(start), 5*time.Second+10*time.Millisecond, "10ms covers only request/context construction")
				if earlier {
					assert.False(t, deadline.After(parentDeadline))
				}
			}
			assert.Equal(t, int32(1), s.closeCount())
			assertDMIconLockReleased(t, f)
		})
	}
	for _, mode := range []string{"canceled_parent", "expired_parent", "canceled_during_read", "canceled_during_close", "blocked_read_cancellation", "blocked_close_cancellation"} {
		t.Run(mode, func(t *testing.T) {
			f := newDMIconFixture(t)
			logs := captureDMIconLogs(f)
			s := installDMIconProbe(f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gets, closes := 1, int32(1)
			if mode == "canceled_parent" {
				cancel()
				gets, closes = 0, 0
			}
			if mode == "expired_parent" {
				var expireCancel context.CancelFunc
				ctx, expireCancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer expireCancel()
				gets, closes = 0, 0
			}
			if mode == "canceled_during_read" {
				s.cancelRead = cancel
			}
			if mode == "canceled_during_close" {
				s.cancelClose = cancel
			}
			if mode == "blocked_read_cancellation" {
				s.readStarted = make(chan struct{})
				s.readRelease = make(chan struct{})
			}
			if mode == "blocked_close_cancellation" {
				s.closeStarted = make(chan struct{})
				s.closeRelease = make(chan struct{})
			}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { f.request(ctx, f.member, f.conversation, w); close(done) }()
			waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer waitCancel()
			if s.readStarted != nil {
				awaitDMIconSignal(waitCtx, t, s.readStarted, "reader entered")
				cancel()
			}
			if s.closeStarted != nil {
				awaitDMIconSignal(waitCtx, t, s.closeStarted, "close entered")
				cancel()
			}
			require.True(t, awaitDMIconSignal(waitCtx, t, done, "canceled handler returned"))
			assertDMIconPrivateFailure(t, f, w)
			assert.Equal(t, gets, s.GetObjectCalls())
			assert.Equal(t, closes, s.closeCount())
			assertDMIconFailureLogs(t, f, logs, "canceled", "read")
			assertDMIconLockReleased(t, f)
		})
	}
}

// Observe a real waiter and its real blocker using PostgreSQL's lock graph.
// A finished operation before observation is an assertion failure; it never
// counts as an assumed wait or a setup success.
func awaitDMIconLock(t *testing.T, db *sql.DB, waiter, blocker string, finished <-chan struct{}) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (
   SELECT 1 FROM pg_stat_activity w WHERE w.application_name=$1 AND w.wait_event_type='Lock'
   AND EXISTS (SELECT 1 FROM pg_stat_activity b WHERE b.application_name=$2 AND b.pid=ANY(pg_blocking_pids(w.pid)))
  )`, waiter, blocker).Scan(&waiting)
		if err != nil {
			assert.NoError(t, err, "lock observation must succeed")
			return false
		}
		if waiting {
			t.Log("PostgreSQL observed the expected participant-row lock waiter and blocker")
			return true
		}
		select {
		case <-finished:
			assert.Fail(t, "operation completed without waiting on the participant-row lock")
			return false
		case <-ctx.Done():
			assert.Fail(t, "PostgreSQL never observed the expected participant-row lock wait")
			return false
		case <-ticker.C:
		}
	}
}

// Panic during acquisition, before a reader is returned to the loader.
type dmIconPanicStore struct {
	*mockStore
	started chan struct{}
	release <-chan struct{}
	key     string
	value   error
}

func (s *dmIconPanicStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	s.key = key
	close(s.started)
	select {
	case <-s.release:
		panic(s.value)
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
}

// Regression for #2625: a storage acquisition panic must release the loader's
// admitted participant lock even while its caller context remains live.
func TestLoadDMIconRollsBackOnStoragePanic(t *testing.T) {
	f := newDMIconFixture(t)
	getApp := "dm_icon_panic_get_" + f.conversation
	deleteApp := "dm_icon_panic_delete_" + f.conversation
	f.ts.handler.db = openDMIconDB(t, getApp, "")
	deleteDB := openDMIconDB(t, deleteApp, "")
	callerCtx, cancelCaller := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelCaller()
	deleteCtx, cancelDelete := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelDelete()

	viewer, conversation := uuid.MustParse(f.member), uuid.MustParse(f.conversation)
	image, err := f.ts.handler.loadDMIcon(callerCtx, viewer, conversation)
	require.NoError(t, err, "seeded member and stored icon must permit a normal loader read")
	require.Equal(t, f.body, image.body)
	require.Equal(t, mimeImagePNG, image.contentType)
	t.Log("seeded member/storage positive control returned the expected image")

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	panicValue := errors.New("storage acquisition panic")
	s := &dmIconPanicStore{mockStore: f.ts.store, started: make(chan struct{}), release: release, value: panicValue}
	f.ts.handler.store = s
	getDone := make(chan struct{})
	getResult := make(chan struct {
		panicValue any
		err        error
	}, 1)
	var deleteTx *sql.Tx
	var deleteDone chan struct{}
	// Cancellation belongs to teardown, after the live-context assertion. On RED
	// it frees the leaked read transaction and all waiters before fixture cleanup.
	defer func() {
		cancelCaller()
		unblock()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelCleanup()
		assert.True(t, awaitDMIconSignal(cleanupCtx, t, getDone, "panic loader teardown completed"))
		if deleteDone != nil {
			assert.True(t, awaitDMIconSignal(cleanupCtx, t, deleteDone, "participant DELETE teardown completed"))
		}
		if deleteTx != nil {
			rollbackDMIconTx(t, deleteTx)
		}
		cancelDelete()
		var exists bool
		assert.NoError(t, f.ts.db.QueryRowContext(cleanupCtx, `SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id=$1 AND user_id=$2)`, f.conversation, f.member).Scan(&exists))
		assert.True(t, exists, "DELETE rollback must restore the seeded participant")
		t.Log("teardown joined loader and DELETE goroutines and restored the participant")
	}()
	go func() {
		var loadErr error
		defer func() {
			getResult <- struct {
				panicValue any
				err        error
			}{recover(), loadErr}
			close(getDone)
		}()
		_, loadErr = f.ts.handler.loadDMIcon(callerCtx, viewer, conversation)
	}()
	require.True(t, awaitDMIconSignal(callerCtx, t, s.started, "storage acquisition reached after admission"))
	require.Equal(t, f.key, s.key)

	deleteTx, err = deleteDB.BeginTx(deleteCtx, nil)
	require.NoError(t, err)
	deleteDone = make(chan struct{})
	deleteResult := make(chan struct {
		rows int64
		err  error
	}, 1)
	go func() {
		result, deleteErr := deleteTx.ExecContext(deleteCtx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
		var rows int64
		if deleteErr == nil {
			rows, deleteErr = result.RowsAffected()
		}
		deleteResult <- struct {
			rows int64
			err  error
		}{rows, deleteErr}
		close(deleteDone)
	}()
	require.True(t, awaitDMIconLock(t, f.ts.db, deleteApp, getApp, deleteDone), "DELETE must really wait on the admitted loader before the panic")
	unblock()
	require.True(t, awaitDMIconSignal(callerCtx, t, getDone, "storage panic recovered by loader caller"))
	loadResult := <-getResult
	require.Same(t, panicValue, loadResult.panicValue, "the storage panic must reach the caller")
	require.NoError(t, loadResult.err)
	require.NoError(t, callerCtx.Err(), "caller context must remain live after the panic")
	t.Log("storage panic recovered while the caller context remains live")

	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRelease()
	select {
	case <-deleteDone:
		result := <-deleteResult
		require.NoError(t, callerCtx.Err(), "DELETE must resume before caller context cancellation")
		require.NoError(t, result.err)
		assert.EqualValues(t, 1, result.rows)
	case <-releaseCtx.Done():
		require.NoError(t, callerCtx.Err(), "the cleanup oracle must run before caller context cancellation")
		assert.Fail(t, "storage panic must release the admitted participant-row lock before caller context cancellation")
	}
}

func testDMIconDeleteFirst(t *testing.T, commit bool) {
	t.Helper()
	f := newDMIconFixture(t)
	logs := captureDMIconLogs(f)
	getApp := "dm_icon_get_" + f.conversation
	deleteApp := "dm_icon_delete_" + f.conversation
	f.ts.handler.db = openDMIconDB(t, getApp, "")
	deleteDB := openDMIconDB(t, deleteApp, "")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tx, err := deleteDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer rollbackDMIconTx(t, tx)
	result, err := tx.ExecContext(ctx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.request(ctx, f.member, f.conversation, w); close(done) }()
	assert.True(t, awaitDMIconLock(t, f.ts.db, getApp, deleteApp, done))
	assert.Zero(t, f.ts.store.GetObjectCalls(), "no storage access while admission is waiting")
	if commit {
		require.NoError(t, tx.Commit())
	} else {
		require.NoError(t, tx.Rollback())
	}
	require.True(t, awaitDMIconSignal(ctx, t, done, "GET returned after removal transaction completed"))
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	if commit {
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.JSONEq(t, `{"error":"Not found"}`, w.Body.String())
		assert.Zero(t, f.ts.store.GetObjectCalls())
		assert.Empty(t, logs.String(), "removal refusal must not emit a distinguishing log")
	} else {
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, f.body, w.Body.Bytes())
		assert.Equal(t, mimeImagePNG, w.Header().Get("Content-Type"))
		assert.Equal(t, 1, f.ts.store.GetObjectCalls())
		assertDMIconLockReleased(t, f)
	}
}

func TestProxyDMIconRemovalDeleteFirstCommit(t *testing.T)   { testDMIconDeleteFirst(t, true) }
func TestProxyDMIconRemovalDeleteFirstRollback(t *testing.T) { testDMIconDeleteFirst(t, false) }

func TestProxyDMIconRemovalGetFirst(t *testing.T) {
	for _, stage := range []string{"acquisition", "read", "close"} {
		t.Run(stage, func(t *testing.T) {
			f := newDMIconFixture(t)
			s := installDMIconProbe(f)
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			switch stage {
			case "acquisition":
				s.getStarted, s.getRelease = started, release
			case "read":
				s.readStarted, s.readRelease = started, release
			case "close":
				s.closeStarted, s.closeRelease = started, release
			}
			getApp := "dm_icon_get_" + f.conversation
			deleteApp := "dm_icon_delete_" + f.conversation
			f.ts.handler.db = openDMIconDB(t, getApp, "")
			deleteDB := openDMIconDB(t, deleteApp, "")
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			w := httptest.NewRecorder()
			getDone := make(chan struct{})
			go func() { f.request(ctx, f.member, f.conversation, w); close(getDone) }()
			require.True(t, awaitDMIconSignal(ctx, t, started, "GET storage stage reached"))
			tx, err := deleteDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer rollbackDMIconTx(t, tx)
			deleteDone := make(chan struct{})
			deleteErr := make(chan error, 1)
			go func() {
				result, err := tx.ExecContext(ctx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
				if err == nil {
					var n int64
					n, err = result.RowsAffected()
					if err == nil && n != 1 {
						err = errors.New("expected one participant deletion")
					}
				}
				if err == nil {
					err = tx.Commit()
				}
				deleteErr <- err
				close(deleteDone)
			}()
			assert.True(t, awaitDMIconLock(t, f.ts.db, deleteApp, getApp, deleteDone), "DELETE must wait throughout "+stage)
			unblock()
			require.True(t, awaitDMIconSignal(ctx, t, getDone, "admitted GET finished"))
			require.True(t, awaitDMIconSignal(ctx, t, deleteDone, "DELETE committed after retrieval"))
			require.NoError(t, <-deleteErr)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, f.body, w.Body.Bytes())
			assert.Equal(t, mimeImagePNG, w.Header().Get("Content-Type"))
			assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
			assert.Equal(t, int32(1), s.closeCount())
			var exists bool
			require.NoError(t, f.ts.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id=$1 AND user_id=$2)`, f.conversation, f.member).Scan(&exists))
			assert.False(t, exists, "removal must really commit")
		})
	}
}

type dmIconBlockedWriter struct {
	*httptest.ResponseRecorder
	started, release chan struct{}
	once             sync.Once
	ctx              context.Context
}

func (w *dmIconBlockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	select {
	case <-w.release:
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
	return w.ResponseRecorder.Write(p)
}

func TestProxyDMIconDoesNotLockDuringHTTPWrite(t *testing.T) {
	f := newDMIconFixture(t)
	s := installDMIconProbe(f)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	w := &dmIconBlockedWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{}), ctx: ctx}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(w.release) }) }
	defer unblock()
	done := make(chan struct{})
	go func() { f.request(ctx, f.member, f.conversation, w); close(done) }()
	require.True(t, awaitDMIconSignal(ctx, t, w.started, "HTTP Write blocked"))
	assert.Equal(t, int32(1), s.closeCount(), "retrieval including checked Close must finish before Write begins")
	deleteCtx, deleteCancel := context.WithTimeout(ctx, 2*time.Second)
	defer deleteCancel()
	tx, err := f.ts.db.BeginTx(deleteCtx, nil)
	require.NoError(t, err)
	defer rollbackDMIconTx(t, tx)
	result, err := tx.ExecContext(deleteCtx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
	require.NoError(t, err, "slow HTTP output must not hold the admission lock")
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	require.NoError(t, tx.Commit())
	// Prove commit while the writer is still blocked, then release it.
	select {
	case <-done:
		assert.Fail(t, "HTTP writer was released prematurely")
	default:
	}
	var exists bool
	require.NoError(t, f.ts.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id=$1 AND user_id=$2)`, f.conversation, f.member).Scan(&exists))
	assert.False(t, exists)
	unblock()
	require.True(t, awaitDMIconSignal(ctx, t, done, "HTTP write finished"))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, f.body, w.Body.Bytes())
	assert.Equal(t, mimeImagePNG, w.Header().Get("Content-Type"))
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, int32(1), s.closeCount())
}

// A positive observer control prevents the lock-order tests from reporting a
// false RED merely because pg_stat_activity observation cannot see a waiter.
func TestProxyDMIconLockObserverControl(t *testing.T) {
	f := newDMIconFixture(t)
	deleteApp := "dm_icon_observer_delete_" + f.conversation
	getApp := "dm_icon_observer_get_" + f.conversation
	deleteDB := openDMIconDB(t, deleteApp, "")
	getDB := openDMIconDB(t, getApp, "")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	deletion, err := deleteDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer rollbackDMIconTx(t, deletion)
	result, err := deletion.ExecContext(ctx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	admission, err := getDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer rollbackDMIconTx(t, admission)
	done := make(chan struct{})
	scanErr := make(chan error, 1)
	go func() {
		var one int
		err := admission.QueryRowContext(ctx, `SELECT 1 FROM dm_participants WHERE conversation_id=$1 AND user_id=$2 FOR SHARE`, f.conversation, f.member).Scan(&one)
		if err == nil && one != 1 {
			err = errors.New("locking control did not retrieve the participant")
		}
		scanErr <- err
		close(done)
	}()
	assert.True(t, awaitDMIconLock(t, f.ts.db, getApp, deleteApp, done), "the same observer must positively detect a real row lock")
	require.NoError(t, deletion.Rollback())
	require.True(t, awaitDMIconSignal(ctx, t, done, "locking control resumed after rollback"))
	require.NoError(t, <-scanErr)
	require.NoError(t, admission.Commit())
	assertDMIconLockReleased(t, f)
}

func TestProxyDMIconCommitFailure(t *testing.T) {
	for _, tc := range []struct {
		mode, class string
	}{
		{mode: "commit_error_before_completion", class: "commit"},
		{mode: "commit_error_after_completion", class: "commit"},
		{mode: "cancel_during_commit", class: "canceled"},
		{mode: "cancel_after_successful_commit", class: "canceled"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newDMIconFixture(t)
			logs := captureDMIconLogs(f)
			s := installDMIconProbe(f)
			getApp := "dm_icon_commit_get_" + f.conversation
			deleteApp := "dm_icon_commit_delete_" + f.conversation
			f.ts.handler.db = openDMIconDB(t, getApp, "")
			deleteDB := openDMIconDB(t, deleteApp, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer deleteCancel()
			deletion, err := deleteDB.BeginTx(deleteCtx, nil)
			require.NoError(t, err)
			defer rollbackDMIconTx(t, deletion)
			deleteDone := make(chan struct{})
			deleteErr := make(chan error, 1)
			invoked := 0
			var readTx *sql.Tx
			f.ts.handler.dmIconCommit = func(tx *sql.Tx) error {
				invoked++
				readTx = tx
				// Pin the intended completion branch: earlier read/Close/context
				// failures cannot stand in for a commit failure in this fixture.
				require.NoError(t, ctx.Err())
				require.NotNil(t, s.storageContext())
				require.NoError(t, s.storageContext().Err())
				require.Equal(t, 1, s.GetObjectCalls())
				require.Equal(t, int32(1), s.closeCount())
				require.NotNil(t, s.reader)
				require.Equal(t, int64(len(f.body)), s.reader.bytesRead.Load())
				// Observe a real conflicting DELETE before completing the read
				// transaction, then require that same DELETE to finish afterward.
				go func() {
					result, err := deletion.ExecContext(deleteCtx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
					if err == nil {
						var rows int64
						rows, err = result.RowsAffected()
						if err == nil && rows != 1 {
							err = errors.New("expected one participant deletion")
						}
					}
					deleteErr <- err
					close(deleteDone)
				}()
				require.True(t, awaitDMIconLock(t, f.ts.db, deleteApp, getApp, deleteDone), "the commit seam must retain the real participant-row lock")
				switch tc.mode {
				case "commit_error_before_completion":
					return errors.New("sensitive-fixture-error " + f.key)
				case "commit_error_after_completion":
					err := tx.Commit()
					require.NoError(t, err, "the real Commit must succeed before the injected error")
					return errors.New("sensitive-fixture-error " + f.key)
				case "cancel_during_commit":
					cancel()
					err := tx.Commit()
					require.True(t, errors.Is(err, context.Canceled) || errors.Is(err, sql.ErrTxDone), "real Commit must observe cancellation or the automatic rollback: %v", err)
					return err
				default:
					err := tx.Commit()
					require.NoError(t, err, "the real Commit must succeed before cancellation")
					cancel()
					return err
				}
			}
			w := httptest.NewRecorder()
			f.request(ctx, f.member, f.conversation, w)
			require.Equal(t, 1, invoked, "the complete seeded object must reach transaction completion exactly once")
			assertDMIconPrivateFailure(t, f, w)
			assert.Equal(t, 1, s.GetObjectCalls())
			assert.Equal(t, int32(1), s.closeCount())
			assertDMIconFailureLogs(t, f, logs, tc.class)
			if tc.class == "canceled" {
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
			} else {
				assert.NoError(t, ctx.Err())
			}
			require.True(t, awaitDMIconSignal(deleteCtx, t, deleteDone, "DELETE resumed after commit failure or cancellation"))
			require.NoError(t, <-deleteErr)
			require.NoError(t, deletion.Rollback(), "restore the fixture after proving release")
			assertDMIconLockReleased(t, f)
			require.NotNil(t, readTx)
			assert.ErrorIs(t, readTx.Rollback(), sql.ErrTxDone, "production must already have completed or rolled back the read transaction")
		})
	}
}

// Unlike the normal acquisition-error probe, this store transfers an opened
// reader to the loader together with the error; it never closes that reader.
type dmIconAcquisitionErrorStore struct {
	*mockStore
	started  chan struct{}
	release  <-chan struct{}
	getErr   error
	closeErr error
	reader   *dmIconAcquisitionErrorReader
	key      string
}

func (s *dmIconAcquisitionErrorStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	source, contentType, err := s.mockStore.GetObject(ctx, key)
	if err != nil {
		return nil, contentType, err
	}
	s.reader = &dmIconAcquisitionErrorReader{source: source, closeErr: s.closeErr}
	s.key = key
	close(s.started)
	select {
	case <-s.release:
		return s.reader, contentType, s.getErr
	case <-ctx.Done():
		return s.reader, contentType, ctx.Err()
	}
}

type dmIconAcquisitionErrorReader struct {
	source   io.ReadCloser
	closeErr error
	reads    atomic.Int32
	closes   atomic.Int32
}

func (r *dmIconAcquisitionErrorReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.source.Read(p)
}

func (r *dmIconAcquisitionErrorReader) Close() error {
	r.closes.Add(1)
	if err := r.source.Close(); err != nil {
		return err
	}
	return r.closeErr
}

// Removing loader-owned cleanup must fail the Close-count oracle. Treating an
// acquisition absence as final despite a Close error must fail the HTTP oracle.
func TestProxyDMIconAcquisitionErrorReaderCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, class            string
		notFound, closeFailure bool
	}{
		{name: "storage_error_close_ok", class: "storage"},
		{name: "storage_error_close_failure", class: "storage", closeFailure: true},
		{name: "not_found_close_ok", notFound: true},
		{name: "not_found_close_failure", class: "close", notFound: true, closeFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDMIconFixture(t)
			getApp := "dm_icon_acq_get_" + f.conversation
			deleteApp := "dm_icon_acq_delete_" + f.conversation
			f.ts.handler.db = openDMIconDB(t, getApp, "")
			deleteDB := openDMIconDB(t, deleteApp, "")
			callerCtx, cancelCaller := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancelCaller()
			deleteCtx, cancelDelete := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancelDelete()

			control := httptest.NewRecorder()
			f.request(callerCtx, f.member, f.conversation, control)
			require.Equal(t, http.StatusOK, control.Code, "real member and seeded storage must permit retrieval")
			require.Equal(t, f.body, control.Body.Bytes())
			require.Equal(t, mimeImagePNG, control.Header().Get("Content-Type"))
			require.Equal(t, "private, no-store", control.Header().Get("Cache-Control"))
			t.Log("seeded member/storage positive control returned the expected image")

			logs := captureDMIconLogs(f)
			absent := httptest.NewRecorder()
			f.request(callerCtx, f.member, uuid.NewString(), absent)
			require.Equal(t, http.StatusNotFound, absent.Code)
			require.JSONEq(t, `{"error":"Not found"}`, absent.Body.String())
			require.Empty(t, logs.String(), "the uniform absence control must be quiet")
			baselineGets := f.ts.store.GetObjectCalls()
			require.Equal(t, 1, baselineGets, "the absence control must not access storage")

			getErr := errors.New("sensitive-fixture-error " + f.key)
			if tc.notFound {
				// The ordinary empty store supplies the real storage.ErrObjectNotFound
				// sentinel; the faulting store still returns the seeded object's reader.
				_, _, getErr = newMockStore().GetObject(callerCtx, f.key)
				require.Error(t, getErr)
			}
			getRelease := make(chan struct{})
			var getReleaseOnce, writeReleaseOnce sync.Once
			unblockGet := func() { getReleaseOnce.Do(func() { close(getRelease) }) }
			s := &dmIconAcquisitionErrorStore{mockStore: f.ts.store, started: make(chan struct{}), release: getRelease, getErr: getErr}
			if tc.closeFailure {
				s.closeErr = errors.New("sensitive-fixture-error " + f.key)
			}
			f.ts.handler.store = s
			w := &dmIconBlockedWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{}), ctx: callerCtx}
			unblockWrite := func() { writeReleaseOnce.Do(func() { close(w.release) }) }
			getDone := make(chan struct{})
			var deletion *sql.Tx
			var deleteDone chan struct{}
			defer func() {
				cancelCaller()
				unblockGet()
				unblockWrite()
				cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancelCleanup()
				assert.True(t, awaitDMIconSignal(cleanupCtx, t, getDone, "acquisition-error handler teardown completed"))
				if deleteDone != nil {
					assert.True(t, awaitDMIconSignal(cleanupCtx, t, deleteDone, "participant DELETE teardown completed"))
				}
				if deletion != nil {
					rollbackDMIconTx(t, deletion)
				}
				cancelDelete()
				var exists bool
				assert.NoError(t, f.ts.db.QueryRowContext(cleanupCtx, `SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id=$1 AND user_id=$2)`, f.conversation, f.member).Scan(&exists))
				assert.True(t, exists, "teardown must restore the seeded participant even after an assertion failure")
				t.Log("teardown joined handler and DELETE goroutines and restored the participant")
			}()
			go func() { f.request(callerCtx, f.member, f.conversation, w); close(getDone) }()
			require.True(t, awaitDMIconSignal(callerCtx, t, s.started, "opened reader acquired after real admission"))
			require.Equal(t, f.key, s.key)
			require.NotNil(t, s.reader)
			require.Equal(t, baselineGets+1, s.GetObjectCalls())
			require.Zero(t, s.reader.reads.Load())
			require.Zero(t, s.reader.closes.Load(), "the store must leave reader ownership to the loader")

			var err error
			deletion, err = deleteDB.BeginTx(deleteCtx, nil)
			require.NoError(t, err)
			deleteDone = make(chan struct{})
			deleteResult := make(chan struct {
				rows int64
				err  error
			}, 1)
			go func() {
				result, deleteErr := deletion.ExecContext(deleteCtx, `DELETE FROM dm_participants WHERE conversation_id=$1 AND user_id=$2`, f.conversation, f.member)
				var rows int64
				if deleteErr == nil {
					rows, deleteErr = result.RowsAffected()
				}
				deleteResult <- struct {
					rows int64
					err  error
				}{rows, deleteErr}
				close(deleteDone)
			}()
			require.True(t, awaitDMIconLock(t, f.ts.db, deleteApp, getApp, deleteDone), "DELETE must really wait before the acquisition error is returned")
			unblockGet()
			require.True(t, awaitDMIconSignal(callerCtx, t, w.started, "HTTP output reached after acquisition-error cleanup"))
			assert.Equal(t, int32(1), s.reader.closes.Load(), "loader must close an acquisition-error reader exactly once before HTTP output")
			assert.Zero(t, s.reader.reads.Load(), "an acquisition error must never consume image bytes")
			require.Empty(t, w.Body.Bytes(), "HTTP output must remain blocked while release is verified")
			releaseCtx, cancelRelease := context.WithTimeout(callerCtx, 2*time.Second)
			defer cancelRelease()
			require.True(t, awaitDMIconSignal(releaseCtx, t, deleteDone, "DELETE resumed before HTTP output"))
			result := <-deleteResult
			require.NoError(t, callerCtx.Err(), "lock release must be observed while the caller context is live")
			require.NoError(t, result.err)
			require.EqualValues(t, 1, result.rows)
			require.NoError(t, deletion.Rollback(), "restore membership before releasing HTTP output")
			var exists bool
			require.NoError(t, f.ts.db.QueryRowContext(releaseCtx, `SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id=$1 AND user_id=$2)`, f.conversation, f.member).Scan(&exists))
			require.True(t, exists)
			t.Log("the observed DELETE resumed and rolled back while HTTP output remained blocked")

			unblockWrite()
			require.True(t, awaitDMIconSignal(callerCtx, t, getDone, "acquisition-error handler completed"))
			assert.Equal(t, int32(1), s.reader.closes.Load())
			assert.Zero(t, s.reader.reads.Load())
			assert.Equal(t, baselineGets+1, s.GetObjectCalls())
			if tc.notFound && !tc.closeFailure {
				assert.Equal(t, http.StatusNotFound, w.Code)
				assert.Equal(t, absent.Body.Bytes(), w.Body.Bytes(), "object absence must be byte-identical to the ordinary membership absence")
				assert.Equal(t, absent.Header().Get("Cache-Control"), w.Header().Get("Cache-Control"))
				assert.Equal(t, absent.Header().Get("Content-Type"), w.Header().Get("Content-Type"))
				assert.False(t, bytes.Contains(w.Body.Bytes(), f.body))
				assert.Empty(t, logs.String(), "successful absence cleanup must not produce a distinguishing diagnostic")
			} else {
				assertDMIconPrivateFailure(t, f, w.ResponseRecorder)
				assertDMIconFailureLogs(t, f, logs, tc.class)
			}
		})
	}
}
