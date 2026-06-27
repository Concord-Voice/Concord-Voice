package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/storage"
)

// mockStore is an in-memory ObjectStore implementation for testing.
type mockStore struct {
	mu      sync.Mutex
	objects map[string]*mockObject
	putErr  error // if non-nil, PutObject returns this error
}

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
