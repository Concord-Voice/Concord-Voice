package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// coldStore is a mockStore that reports a storage class and records promotions.
type coldStore struct {
	*mockStore
	class      string
	promoted   []string
	promoteErr error
	done       chan string // optional: signals a promotion from a goroutine
}

func (s *coldStore) GetObjectWithClass(ctx context.Context, key string) (io.ReadCloser, string, string, error) {
	r, ct, err := s.GetObject(ctx, key)
	return r, ct, s.class, err
}

func (s *coldStore) StorageClass(context.Context, string) (string, error) { return s.class, nil }

func (s *coldStore) PromoteToStandard(_ context.Context, key string) error {
	if s.promoteErr != nil {
		return s.promoteErr
	}
	s.promoted = append(s.promoted, key)
	if s.done != nil {
		s.done <- key
	}
	return nil
}

func coldTierHandler(t *testing.T) (*Handler, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &Handler{sessionRedis: rdb, log: logger.New("test")}, mr
}

func TestNoteColdRead_SecondReadInWindowPromotes(t *testing.T) {
	h, mr := coldTierHandler(t)
	store := &coldStore{mockStore: newMockStore()}
	counter := coldReadKey("f1")

	h.noteColdRead(store, "f1", "attachments/f1")
	require.Empty(t, store.promoted, "one read must not promote")
	require.Equal(t, coldReadWindow, mr.TTL(counter), "window starts at the first read")

	mr.FastForward(coldReadWindow / 2)
	h.noteColdRead(store, "f1", "attachments/f1")
	require.Equal(t, []string{"attachments/f1"}, store.promoted)
	require.False(t, mr.Exists(counter), "promotion clears the counter")
}

func TestNoteColdRead_WindowExpiryForgetsFirstRead(t *testing.T) {
	h, mr := coldTierHandler(t)
	store := &coldStore{mockStore: newMockStore()}

	h.noteColdRead(store, "f2", "attachments/f2")
	mr.FastForward(coldReadWindow)
	h.noteColdRead(store, "f2", "attachments/f2")
	require.Empty(t, store.promoted, "reads 30+ days apart are not a hot file")
}

func TestNoteColdRead_FailedPromotionRetriesNextRead(t *testing.T) {
	h, mr := coldTierHandler(t)
	store := &coldStore{mockStore: newMockStore(), promoteErr: errors.New("r2 down")}

	h.noteColdRead(store, "f3", "attachments/f3")
	h.noteColdRead(store, "f3", "attachments/f3")
	require.True(t, mr.Exists(coldReadKey("f3")), "a failed copy keeps the count")

	store.promoteErr = nil
	h.noteColdRead(store, "f3", "attachments/f3")
	require.Equal(t, []string{"attachments/f3"}, store.promoted)
}

func TestNoteColdRead_NoRedisIsNoop(t *testing.T) {
	h := &Handler{log: logger.New("test")}
	store := &coldStore{mockStore: newMockStore()}
	h.noteColdRead(store, "f4", "attachments/f4")
	h.noteColdRead(store, "f4", "attachments/f4")
	require.Empty(t, store.promoted)
}

func TestGetAttachmentObject_ReportsClassOnlyWhenStoreCan(t *testing.T) {
	ctx := context.Background()
	plain := newMockStore()
	require.NoError(t, plain.PutObject(ctx, "k", bytes.NewReader([]byte("x")), 1, mimeOctetStream))
	obj, _, class, err := getAttachmentObject(ctx, plain, "k")
	require.NoError(t, err)
	_ = obj.Close()
	require.Empty(t, class)

	cold := &coldStore{mockStore: plain, class: "STANDARD_IA"}
	obj, _, class, err = getAttachmentObject(ctx, cold, "k")
	require.NoError(t, err)
	_ = obj.Close()
	require.True(t, isColdClass(class))
	require.False(t, isColdClass("STANDARD"))
	require.False(t, isColdClass(""))
}

func TestRecordAttachmentRead_LooksUpClassThenCounts(t *testing.T) {
	h, _ := coldTierHandler(t)
	store := &coldStore{mockStore: newMockStore(), class: "STANDARD_IA", done: make(chan string, 1)}
	h.noteColdRead(store, "f5", "attachments/f5") // first read, e.g. via the proxy

	h.recordAttachmentRead(store, "f5", "attachments/f5")
	select {
	case key := <-store.done:
		require.Equal(t, "attachments/f5", key)
	case <-time.After(5 * time.Second):
		t.Fatal("second read did not promote")
	}
}

func TestRecordAttachmentRead_StandardClassNotCounted(t *testing.T) {
	h, mr := coldTierHandler(t)
	store := &coldStore{mockStore: newMockStore(), class: "STANDARD"}
	h.recordAttachmentRead(store, "f6", "attachments/f6")
	require.Never(t, func() bool { return mr.Exists(coldReadKey("f6")) }, 200*time.Millisecond, 20*time.Millisecond)
}

func TestNoteColdRead_PromotionInFlightSkipsCopy(t *testing.T) {
	h, mr := coldTierHandler(t)
	store := &coldStore{mockStore: newMockStore()}
	require.NoError(t, mr.Set(coldPromoteKey("f7"), "1"))

	h.noteColdRead(store, "f7", "attachments/f7")
	h.noteColdRead(store, "f7", "attachments/f7")
	require.Empty(t, store.promoted, "a held promotion lock means another copy is running")
	require.True(t, mr.Exists(coldReadKey("f7")), "the count survives for a later retry")

	mr.Del(coldPromoteKey("f7"))
	h.noteColdRead(store, "f7", "attachments/f7")
	require.Equal(t, []string{"attachments/f7"}, store.promoted)
	require.False(t, mr.Exists(coldPromoteKey("f7")), "the lock is released after the copy")
}

func TestGoColdRead_NoFreeSlotSkipsCount(t *testing.T) {
	h, mr := coldTierHandler(t)
	store := &coldStore{mockStore: newMockStore()}
	for i := 0; i < cap(coldReadSlots); i++ {
		coldReadSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(coldReadSlots); i++ {
			<-coldReadSlots
		}
	})

	h.goColdRead(store, "f8", "attachments/f8")
	h.recordAttachmentRead(&coldStore{mockStore: newMockStore(), class: "STANDARD_IA"}, "f8", "attachments/f8")
	require.Never(t, func() bool { return mr.Exists(coldReadKey("f8")) }, 200*time.Millisecond, 20*time.Millisecond)
}
