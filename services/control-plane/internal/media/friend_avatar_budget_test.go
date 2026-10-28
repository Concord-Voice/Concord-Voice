package media

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store gives each request a full-size avatar, then holds its final EOF.
// At that point io.ReadAll retains the whole response buffer until release.
type heldFriendAvatarStore struct {
	*mockStore
	avatar    []byte
	exhausted chan struct{}
	release   chan struct{}
}

func (s *heldFriendAvatarStore) GetObject(ctx context.Context, _ string) (io.ReadCloser, string, error) {
	return &heldFriendAvatarReader{
		ctx:       ctx,
		body:      bytes.NewReader(s.avatar),
		exhausted: s.exhausted,
		release:   s.release,
	}, mimeImagePNG, nil
}

type heldFriendAvatarReader struct {
	ctx       context.Context
	body      *bytes.Reader
	exhausted chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (r *heldFriendAvatarReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if err != io.EOF {
		return n, err
	}
	r.once.Do(func() { r.exhausted <- struct{}{} })
	select {
	case <-r.release:
		return 0, io.EOF
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
}

func (r *heldFriendAvatarReader) Close() error { return nil }

func TestProxyFriendCodeAvatarBoundsConcurrentWholeObjectReads(t *testing.T) {
	ts := setupMediaTest(t)
	owner := ts.createTestUser(t, "fcavatarbudget")
	ts.setUserAvatarURL(t, owner, "/api/v1/media/avatars/"+owner)
	ts.createTestFriendCode(t, owner, "FAVBUDG2", false, nil, 0, 0)
	ts.createTestTier1Media(t, owner, "avatars/"+owner)

	// The public invalid-code arm is the exact response a saturated valid-code
	// request must receive, including the headers and silhouette bytes.
	fallback := ts.getFriendCodeAvatar(friendCodeAvatarPath("ZZZZZZZZ"), "ZZZZZZZZ")
	require.Equal(t, http.StatusOK, fallback.Code)

	const concurrentReads = 8
	store := &heldFriendAvatarStore{
		mockStore: newMockStore(),
		avatar:    bytes.Repeat([]byte{0x5a}, int(friendAvatarMaxBytes)),
		exhausted: make(chan struct{}, concurrentReads+1),
		release:   make(chan struct{}),
	}
	ts.handler.store = store
	results := make(chan *httptest.ResponseRecorder, concurrentReads+1)
	startRequest := func() { results <- ts.getFriendCodeAvatar(friendCodeAvatarPath("FAVBUDG2"), "FAVBUDG2") }

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	defer release()
	// A timeout only diagnoses a broken fixture; the saturation verdict below
	// is ordered by reader and response channels, never by elapsed time.
	watchdog := time.NewTimer(15 * time.Second)
	defer watchdog.Stop()
	for range concurrentReads {
		go startRequest()
		select {
		case <-store.exhausted:
		case <-watchdog.C:
			t.Fatal("avatar request did not reach its blocked full-size read")
		}
	}

	go startRequest()
	var ninth *httptest.ResponseRecorder
	ninthReadStarted := false
	select {
	case <-store.exhausted:
		ninthReadStarted = true
	case ninth = <-results:
	case <-watchdog.C:
		t.Fatal("saturated avatar request neither fell back nor started reading")
	}
	release()
	if ninth == nil {
		select {
		case ninth = <-results:
		case <-watchdog.C:
			t.Fatal("avatar request did not finish after releasing the readers")
		}
	}
	// The ninth request must be refused before it can hold another 8 MiB
	// buffer. A valid code still receives the shared, fail-uniform fallback.
	assert.False(t, ninthReadStarted, "a ninth full-size avatar read exceeded the process budget")
	if !ninthReadStarted && assert.NotNil(t, ninth) {
		assert.Equal(t, fallback.Code, ninth.Code)
		assert.Equal(t, fallback.Header().Get(hdrContentType), ninth.Header().Get(hdrContentType))
		assert.Equal(t, fallback.Header().Get(hdrCacheControl), ninth.Header().Get(hdrCacheControl))
		assert.True(t, bytes.Equal(fallback.Body.Bytes(), ninth.Body.Bytes()),
			"saturation must serve the same silhouette bytes as an invalid code")
	}

	for range concurrentReads {
		select {
		case w := <-results:
			require.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, mimeImagePNG, w.Header().Get(hdrContentType))
			assert.Equal(t, cacheControlNoStore, w.Header().Get(hdrCacheControl))
			assert.Equal(t, len(store.avatar), w.Body.Len())
			assert.True(t, bytes.Equal(store.avatar, w.Body.Bytes()),
				"admitted readers must still serve the avatar")
		case <-watchdog.C:
			t.Fatal("admitted avatar request did not finish after release")
		}
	}
}
