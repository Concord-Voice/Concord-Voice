package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type probeFakeStore struct {
	mu                 sync.Mutex
	objects            map[string][]byte
	parts              map[string]map[int][]byte
	partSizes          map[string]map[int]int64
	deleteLeavesObject bool
	rejectUnequalParts bool
	deleteCalls        []string
	objectExistsCalls  []string
	cleanupContextErrs []error
	failOperation      string
	failPartNumber     int
	cancelParent       context.CancelFunc
	putCalls           int
	getCalls           int
	abortCalls         int
	completeCalls      int
}

func newProbeFakeStore() *probeFakeStore {
	return &probeFakeStore{
		objects:   make(map[string][]byte),
		parts:     make(map[string]map[int][]byte),
		partSizes: make(map[string]map[int]int64),
	}
}

func (f *probeFakeStore) PutObject(_ context.Context, key string, r io.Reader, size int64, _ string) error {
	if f.failOperation == "put" {
		return errors.New("forced put failure")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("size=%d, bytes=%d", size, len(b))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	f.objects[key] = append([]byte(nil), b...)
	return nil
}

func TestProbeTimeoutReservesCleanupSlice(t *testing.T) {
	assert.Equal(t, 60*time.Second, probeTimeout)
	assert.Equal(t, 15*time.Second, cleanupTimeout)
	assert.Equal(t, 45*time.Second, probeTimeout-cleanupTimeout,
		"the work context must leave the final cleanup slice inside the hard bound")
}

func TestProbeWorkContext_ReservesCleanupDeadline(t *testing.T) {
	wantParentDeadline := time.Now().Add(time.Hour)
	parentCtx, parentCancel := context.WithDeadline(context.Background(), wantParentDeadline)
	defer parentCancel()

	workCtx, workCancel := probeWorkContext(parentCtx)
	defer workCancel()
	workDeadline, ok := workCtx.Deadline()

	require.True(t, ok)
	assert.Equal(t, wantParentDeadline.Add(-cleanupTimeout), workDeadline)
}

func TestProbeCleanupContext_HonorsEarlierParentDeadline(t *testing.T) {
	wantParentDeadline := time.Now().Add(time.Second)
	parentCtx, parentCancel := context.WithDeadline(context.Background(), wantParentDeadline)
	defer parentCancel()

	cleanupCtx, cleanupCancel := probeCleanupContext(parentCtx)
	defer cleanupCancel()
	cleanupDeadline, ok := cleanupCtx.Deadline()

	require.True(t, ok)
	assert.Equal(t, wantParentDeadline, cleanupDeadline)
}

func TestRun_CommandOutcomes(t *testing.T) {
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ATTACHMENT_WRITE_BACKEND", "legacy")
	t.Setenv("CONTROL_PLANE_REPLICA_COUNT", "1")
	t.Setenv("ACTIVITY_HISTORY_CLUSTER_ENABLED", "false")
	t.Setenv("GOOGLE_SSO_ENABLED", "false")
	t.Setenv("APPLE_SSO_ENABLED", "false")
	t.Setenv("CLOUDFLARE_KV_BRIDGE_ENABLED", "false")
	t.Setenv("CLOUDFLARE_R2_USEAST_ID", "")
	t.Setenv("CLOUDFLARE_R2_USEAST_ID_KEY", "")

	t.Run("invalid arguments", func(t *testing.T) {
		stdout, stderr, code := captureRunOutput(t, []string{"--backend", "unknown"})
		assert.Equal(t, 64, code)
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	})

	t.Run("invalid flag", func(t *testing.T) {
		stdout, stderr, code := captureRunOutput(t, []string{"--unknown"})
		assert.Equal(t, 64, code)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "flag provided but not defined")
	})

	t.Run("unexpected positional argument", func(t *testing.T) {
		stdout, stderr, code := captureRunOutput(t, []string{"unexpected"})
		assert.Equal(t, 64, code)
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	})

	t.Run("legacy default skips", func(t *testing.T) {
		stdout, stderr, code := captureRunOutput(t, nil)
		assert.Equal(t, 0, code)
		assert.Equal(t, "skipped: the write default is legacy\n", stdout)
		assert.Empty(t, stderr)
	})

	t.Run("legacy default skips with JSON result", func(t *testing.T) {
		stdout, stderr, code := captureRunOutput(t, []string{"--json"})
		assert.Equal(t, 0, code)
		assert.Empty(t, stderr)
		assertJSONResult(t, stdout, "legacy", "skipped", "")
	})

	t.Run("explicit unregistered R2 fails closed", func(t *testing.T) {
		stdout, stderr, code := captureRunOutput(t, []string{"--backend", "r2-useast", "--json"})
		assert.Equal(t, 1, code)
		assert.Empty(t, stderr)
		assertJSONResult(t, stdout, "r2-useast", "failed", "unknown storage backend")
	})

	t.Run("explicit unregistered R2 reports text failure", func(t *testing.T) {
		stdout, stderr, code := captureRunOutput(t, []string{"--backend", "r2-useast"})
		assert.Equal(t, 1, code)
		assert.Contains(t, stdout, "ATTACHMENT WRITE BACKEND UNUSABLE")
		assert.Contains(t, stderr, "storage-probe: storage: unknown storage backend")
	})
}

func captureRunOutput(t *testing.T, args []string) (string, string, int) {
	t.Helper()
	stdoutFile, err := os.CreateTemp(t.TempDir(), "stdout")
	require.NoError(t, err)
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)

	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutFile, stderrFile
	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
	}()

	code := Run(args)
	os.Stdout, os.Stderr = oldStdout, oldStderr
	stdout := readCapturedOutput(t, stdoutFile)
	stderr := readCapturedOutput(t, stderrFile)
	return stdout, stderr, code
}

func readCapturedOutput(t *testing.T, file *os.File) string {
	t.Helper()
	_, err := file.Seek(0, io.SeekStart)
	require.NoError(t, err)
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	return string(data)
}

func assertJSONResult(t *testing.T, raw, backend, status, errContains string) {
	t.Helper()
	var result struct {
		Backend string `json:"backend"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &result))
	assert.Equal(t, backend, result.Backend)
	assert.Equal(t, status, result.Status)
	if errContains == "" {
		assert.Empty(t, result.Error)
		return
	}
	assert.Contains(t, result.Error, errContains)
}

func (f *probeFakeStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	if f.failOperation == "get" {
		return nil, "", errors.New("forced get failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	b, ok := f.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), "application/octet-stream", nil
}

func (f *probeFakeStore) DeleteObject(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupContextErrs = append(f.cleanupContextErrs, ctx.Err())
	f.deleteCalls = append(f.deleteCalls, key)
	if f.failOperation == "delete" {
		return errors.New("forced delete failure")
	}
	if !f.deleteLeavesObject {
		delete(f.objects, key)
	}
	return nil
}

func (f *probeFakeStore) ObjectExists(ctx context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupContextErrs = append(f.cleanupContextErrs, ctx.Err())
	f.objectExistsCalls = append(f.objectExistsCalls, key)
	if f.failOperation == "exists" {
		return false, errors.New("forced exists failure")
	}
	_, ok := f.objects[key]
	return ok, nil
}

func (f *probeFakeStore) NewMultipartUpload(_ context.Context, key, _ string) (string, error) {
	if f.failOperation == "multipart" {
		return "", errors.New("forced multipart failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parts[key] = make(map[int][]byte)
	f.partSizes[key] = make(map[int]int64)
	return key + "-upload", nil
}

func (f *probeFakeStore) PutObjectPart(_ context.Context, key, _ string, number int, r io.Reader, size int64) (storage.ObjectPartInfo, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return storage.ObjectPartInfo{}, err
	}
	if int64(len(b)) != size {
		return storage.ObjectPartInfo{}, fmt.Errorf("part %d size=%d, bytes=%d", number, size, len(b))
	}
	if number == f.failPartNumber {
		if f.cancelParent != nil {
			f.cancelParent()
		}
		return storage.ObjectPartInfo{}, errors.New("forced part failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectUnequalParts && number > 1 && size != f.partSizes[key][1] {
		return storage.ObjectPartInfo{}, errors.New("non-trailing parts have unequal sizes")
	}
	f.parts[key][number] = append([]byte(nil), b...)
	f.partSizes[key][number] = size
	return storage.ObjectPartInfo{PartNumber: number, Size: size, ETag: fmt.Sprintf("etag-%d", number)}, nil
}

func (f *probeFakeStore) CompleteMultipartUpload(_ context.Context, key, _ string, parts []storage.ObjectPartInfo) error {
	if f.failOperation == "complete" {
		return errors.New("forced complete failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls++
	var out []byte
	for _, part := range parts {
		out = append(out, f.parts[key][part.PartNumber]...)
	}
	f.objects[key] = out
	return nil
}

func (f *probeFakeStore) AbortMultipartUpload(ctx context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupContextErrs = append(f.cleanupContextErrs, ctx.Err())
	f.abortCalls++
	delete(f.parts, key)
	delete(f.partSizes, key)
	return nil
}

func TestRunProbe_V3UniformPartsPassV2GeometryFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version media.EnvelopeVersion
		wantErr bool
	}{
		{name: "uniform v3", version: media.EnvelopeVersionV3},
		{name: "nonuniform v2", version: media.EnvelopeVersionV2, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newProbeFakeStore()
			store.rejectUnequalParts = true
			err := runProbeWithStore(context.Background(), store, tc.version)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "unequal")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 2, len(store.objectExistsCalls))
		})
	}
}

func TestRunProbe_DeleteSuccessButObjectRemainsFails(t *testing.T) {
	store := newProbeFakeStore()
	store.deleteLeavesObject = true

	err := runProbeWithStore(context.Background(), store, media.EnvelopeVersionV3)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "still exists")
	assert.Len(t, store.objectExistsCalls, 3,
		"cleanup must re-check the first failed absence check and the second object")
	assert.Len(t, store.deleteCalls, 4,
		"cleanup must retry deletion after absence verification fails")
}

func TestRunProbe_OperationFailures(t *testing.T) {
	for _, tc := range []struct {
		operation string
		wantError string
	}{
		{operation: "put", wantError: "put probe object: forced put failure"},
		{operation: "get", wantError: "read probe object: forced get failure"},
		{operation: "multipart", wantError: "start multipart probe: forced multipart failure"},
		{operation: "complete", wantError: "complete multipart probe: forced complete failure"},
		{operation: "delete", wantError: "delete probe object: forced delete failure"},
		{operation: "exists", wantError: "check deleted probe object: forced exists failure"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			store := newProbeFakeStore()
			store.failOperation = tc.operation

			err := runProbeWithStore(context.Background(), store, media.EnvelopeVersionV3)

			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

func TestRunProbe_RejectsUnavailableStoreAndEnvelope(t *testing.T) {
	require.ErrorContains(t,
		runProbeWithStore(context.Background(), nil, media.EnvelopeVersionV3),
		"store unavailable")
	require.ErrorContains(t,
		runProbeWithStore(context.Background(), newProbeFakeStore(), media.EnvelopeVersion(99)),
		"unsupported envelope version")
}

func TestCompareObject_RejectsDifferentContents(t *testing.T) {
	store := newProbeFakeStore()
	store.objects["probe"] = []byte("actual")

	require.ErrorContains(t,
		compareObject(context.Background(), store, "probe", []byte("expected")),
		"object contents differ")
}

func TestRunProbe_FailureUsesFreshCleanupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := newProbeFakeStore()
	store.failPartNumber = 2
	store.cancelParent = cancel

	err := runProbeWithStore(ctx, store, media.EnvelopeVersionV3)

	require.ErrorContains(t, err, "forced part failure")
	require.NotEmpty(t, store.cleanupContextErrs)
	for _, contextErr := range store.cleanupContextErrs {
		assert.NoError(t, contextErr, "cleanup must not inherit the canceled work context")
	}
	assert.Equal(t, 1, store.abortCalls)
}

func TestParseArgs_BackendExplicitness(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		wantBackend  string
		wantExplicit bool
	}{
		{name: "omitted backend", args: nil, wantBackend: ""},
		{name: "explicit legacy", args: []string{"--backend", string(storage.LegacyBackendID)}, wantBackend: string(storage.LegacyBackendID), wantExplicit: true},
		{name: "explicit r2", args: []string{"--backend=r2-useast"}, wantBackend: "r2-useast", wantExplicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBackend, got.backend)
			assert.Equal(t, tc.wantExplicit, got.backendExplicit)
		})
	}
}
