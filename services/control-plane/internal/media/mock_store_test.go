package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

// mockStore is an in-memory ObjectStore implementation for testing.
type mockStore struct {
	mu      sync.Mutex
	objects map[string]*mockObject
	putErr  error // if non-nil, PutObject returns this error
	getErr  error // if non-nil, GetObject returns this error (see friend_avatar_outage_test.go)
	// getPartial, when non-nil, makes GetObject SUCCEED and hand back a reader
	// that yields these bytes and then fails — a connection dropped mid-transfer.
	// The pre-read and mid-read failures are the same defect at two moments.
	getPartial []byte
}

// partialReader yields its bytes once, then fails.
type partialReader struct {
	data []byte
	done bool
}

func (r *partialReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("connection reset mid-transfer")
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

func (r *partialReader) Close() error { return nil }

type mockObject struct {
	data        []byte
	contentType string
}

func newMockStore() *mockStore {
	return &mockStore{objects: make(map[string]*mockObject)}
}

func (m *mockStore) PutObject(_ context.Context, key string, reader io.Reader, _ int64, contentType string) error {
	if m.putErr != nil {
		return m.putErr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &mockObject{data: data, contentType: contentType}
	return nil
}

func (m *mockStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getPartial != nil {
		return &partialReader{data: m.getPartial}, "image/png", nil
	}
	if m.getErr != nil {
		// Deliberately NOT wrapped in storage.ErrObjectNotFound: this models a
		// live backend fault (an R2/S3 incident), which is the branch that made
		// the friend-avatar route a validity classifier.
		return nil, "", m.getErr
	}
	obj, ok := m.objects[key]
	if !ok {
		// Wrapped to mirror the real client, proving errors.Is sees through wrapping.
		return nil, "", fmt.Errorf("mock get %q: %w", key, storage.ErrObjectNotFound)
	}
	return io.NopCloser(bytes.NewReader(obj.data)), obj.contentType, nil
}

func (m *mockStore) PresignedGetURL(_ context.Context, key string, _ time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; !ok {
		return "", fmt.Errorf("mock presign %q: %w", key, storage.ErrObjectNotFound)
	}
	return "http://minio:9000/test-bucket/" + key + "?presigned=true", nil
}

func (m *mockStore) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// hasObject returns true if the given key exists in the mock store.
func (m *mockStore) hasObject(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}
