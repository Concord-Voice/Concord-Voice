// Package probe runs a bounded read/write check against one attachment store.
package probe

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/media"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/config"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

const (
	probeTimeout   = 60 * time.Second
	cleanupTimeout = 15 * time.Second
)

type objectStore interface {
	PutObject(context.Context, string, io.Reader, int64, string) error
	GetObject(context.Context, string) (io.ReadCloser, string, error)
	DeleteObject(context.Context, string) error
	ObjectExists(context.Context, string) (bool, error)
	NewMultipartUpload(context.Context, string, string) (string, error)
	PutObjectPart(context.Context, string, string, int, io.Reader, int64) (storage.ObjectPartInfo, error)
	CompleteMultipartUpload(context.Context, string, string, []storage.ObjectPartInfo) error
	AbortMultipartUpload(context.Context, string, string) error
}

type probeOptions struct {
	backend         string
	backendExplicit bool
	json            bool
}

// Run executes the attachment storage probe. It returns 64 for invalid CLI
// arguments, 1 for configuration, resolution, or storage failures, and 0 on
// success or an intentionally skipped legacy default.
func Run(args []string) int {
	opts, err := parseArgs(args)
	if err != nil {
		return 64
	}
	totalCtx, totalCancel := context.WithTimeout(context.Background(), probeTimeout)
	defer totalCancel()

	cfg, err := config.Load()
	if err != nil {
		writeResult(opts, "failed", err)
		return 1
	}
	if opts.backend == "" {
		opts.backend = cfg.AttachmentWriteBackend
	}
	if !opts.backendExplicit && opts.backend == string(storage.LegacyBackendID) {
		if opts.json {
			writeResult(opts, "skipped", nil)
		} else {
			writeSkipped()
		}
		return 0
	}
	probeLog := logger.New(cfg.Environment)
	if opts.json {
		probeLog = logger.NewWithWriter(io.Discard)
	}
	var legacy *storage.Client
	if opts.backendExplicit && opts.backend == string(storage.LegacyBackendID) {
		if err := totalCtx.Err(); err != nil {
			writeResult(opts, "failed", fmt.Errorf("storage probe timed out before construction: %w", err))
			return 1
		}
		legacy, err = storage.New(cfg, probeLog)
		if err != nil {
			writeResult(opts, "failed", err)
			return 1
		}
		if err := totalCtx.Err(); err != nil {
			writeResult(opts, "failed", fmt.Errorf("storage probe timed out during construction: %w", err))
			return 1
		}
	}
	registry := storage.NewRegistry(cfg, legacy, probeLog)
	store, err := registry.Resolve(storage.BackendID(opts.backend))
	if err != nil {
		writeResult(opts, "failed", err)
		return 1
	}

	if err := runProbeWithStore(totalCtx, store, media.EnvelopeVersionV3); err != nil {
		writeResult(opts, "failed", err)
		return 1
	}
	writeResult(opts, "passed", nil)
	return 0
}

func writeSkipped() {
	if _, err := fmt.Fprintln(os.Stdout, "skipped: the write default is legacy"); err != nil {
		fmt.Fprintf(os.Stderr, "storage-probe: write skipped result: %v\n", err)
	}
}

func parseArgs(args []string) (probeOptions, error) {
	fs := flag.NewFlagSet("storage-probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	backend := fs.String("backend", "", "attachment backend (legacy or r2-useast)")
	jsonOutput := fs.Bool("json", false, "write machine-readable output")
	if err := fs.Parse(args); err != nil {
		return probeOptions{}, err
	}
	if fs.NArg() != 0 {
		return probeOptions{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *backend != "" && *backend != string(storage.LegacyBackendID) && *backend != config.AttachmentBackendR2USEast {
		return probeOptions{}, fmt.Errorf("unknown backend %q", *backend)
	}
	backendExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "backend" {
			backendExplicit = true
		}
	})
	return probeOptions{backend: *backend, backendExplicit: backendExplicit, json: *jsonOutput}, nil
}

func writeResult(opts probeOptions, status string, err error) {
	if opts.json {
		result := struct {
			Backend string `json:"backend"`
			Status  string `json:"status"`
			Error   string `json:"error,omitempty"`
		}{Backend: opts.backend, Status: status}
		if err != nil {
			result.Error = err.Error()
		}
		if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
			fmt.Fprintf(os.Stderr, "storage-probe: write result: %v\n", encodeErr)
		}
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage-probe: %v\n", err)
	}
}

func runProbeWithStore(ctx context.Context, store objectStore, version media.EnvelopeVersion) (err error) {
	if store == nil {
		return errors.New("storage probe: store unavailable")
	}
	partSizes, err := multipartPartSizes(version)
	if err != nil {
		return err
	}

	smallKey := "attachments/" + uuid.NewString()
	multipartKey := "attachments/" + uuid.NewString()
	var uploadID string
	cleanupVerified := false
	workCtx, workCancel := probeWorkContext(ctx)
	defer workCancel()
	defer func() {
		if err == nil || cleanupVerified {
			return
		}
		if cleanupErr := cleanupProbeObjects(ctx, store, smallKey, multipartKey, uploadID); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	smallData, err := randomBytes(64)
	if err != nil {
		return fmt.Errorf("generate probe object: %w", err)
	}
	if err := store.PutObject(workCtx, smallKey, bytes.NewReader(smallData), int64(len(smallData)), "application/octet-stream"); err != nil {
		return fmt.Errorf("put probe object: %w", err)
	}
	if err := compareObject(workCtx, store, smallKey, smallData); err != nil {
		return fmt.Errorf("read probe object: %w", err)
	}

	uploadID, err = writeMultipartProbe(workCtx, store, multipartKey, partSizes)
	if err != nil {
		return err
	}
	if err := deleteProbeObjects(workCtx, store, smallKey, multipartKey); err != nil {
		return err
	}
	cleanupVerified = true
	return nil
}

func writeMultipartProbe(ctx context.Context, store objectStore, key string, partSizes []int64) (string, error) {
	uploadID, err := store.NewMultipartUpload(ctx, key, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("start multipart probe: %w", err)
	}
	parts := make([]storage.ObjectPartInfo, 0, len(partSizes))
	want := make([]byte, 0, partSizes[0]+partSizes[1]+partSizes[2])
	for i, size := range partSizes {
		part, generateErr := randomBytes(size)
		if generateErr != nil {
			return uploadID, fmt.Errorf("generate multipart part %d: %w", i+1, generateErr)
		}
		info, putErr := store.PutObjectPart(ctx, key, uploadID, i+1, bytes.NewReader(part), size)
		if putErr != nil {
			return uploadID, fmt.Errorf("put multipart part %d: %w", i+1, putErr)
		}
		parts = append(parts, info)
		want = append(want, part...)
	}
	if err := store.CompleteMultipartUpload(ctx, key, uploadID, parts); err != nil {
		return uploadID, fmt.Errorf("complete multipart probe: %w", err)
	}
	if err := compareObject(ctx, store, key, want); err != nil {
		return "", fmt.Errorf("read completed multipart probe: %w", err)
	}
	return "", nil
}

func deleteProbeObjects(ctx context.Context, store objectStore, keys ...string) error {
	for _, key := range keys {
		if err := store.DeleteObject(ctx, key); err != nil {
			return fmt.Errorf("delete probe object: %w", err)
		}
	}
	for _, key := range keys {
		exists, err := store.ObjectExists(ctx, key)
		if err != nil {
			return fmt.Errorf("check deleted probe object: %w", err)
		}
		if exists {
			return fmt.Errorf("probe object %q still exists after delete", key)
		}
	}
	return nil
}

func probeWorkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, deadline.Add(-cleanupTimeout))
	}
	return context.WithCancel(ctx)
}

func cleanupProbeObjects(parentCtx context.Context, store objectStore, smallKey, multipartKey, uploadID string) error {
	cleanupCtx, cleanupCancel := probeCleanupContext(parentCtx)
	defer cleanupCancel()

	var cleanupErrs []error
	if uploadID != "" {
		if err := store.AbortMultipartUpload(cleanupCtx, multipartKey, uploadID); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("abort multipart upload: %w", err))
		}
	}
	for _, key := range []string{smallKey, multipartKey} {
		if err := store.DeleteObject(cleanupCtx, key); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete %q: %w", key, err))
		}
	}
	for _, key := range []string{smallKey, multipartKey} {
		exists, err := store.ObjectExists(cleanupCtx, key)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("check deleted probe object %q: %w", key, err))
		} else if exists {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("probe object %q still exists after cleanup", key))
		}
	}
	return errors.Join(cleanupErrs...)
}

func probeCleanupContext(parentCtx context.Context) (context.Context, context.CancelFunc) {
	baseCtx := context.WithoutCancel(parentCtx)
	if deadline, ok := parentCtx.Deadline(); ok && time.Until(deadline) < cleanupTimeout {
		return context.WithDeadline(baseCtx, deadline)
	}
	return context.WithTimeout(baseCtx, cleanupTimeout)
}

func multipartPartSizes(version media.EnvelopeVersion) ([]int64, error) {
	if !version.Valid() {
		return nil, fmt.Errorf("unsupported envelope version %d", version)
	}
	firstPlaintext := media.AttachmentChunkPlaintextBytes
	if version == media.EnvelopeVersionV3 {
		firstPlaintext -= media.AttachmentEnvelopeHeaderBytes
	}
	first := media.AttachmentEnvelopeHeaderBytes + media.AttachmentChunkOverheadBytes + firstPlaintext
	other := media.AttachmentChunkPlaintextBytes + media.AttachmentChunkOverheadBytes
	return []int64{first, other, other}, nil
}

func randomBytes(size int64) ([]byte, error) {
	data := make([]byte, size)
	if _, err := cryptorand.Read(data); err != nil {
		return nil, err
	}
	return data, nil
}

func compareObject(ctx context.Context, store objectStore, key string, want []byte) error {
	reader, _, err := store.GetObject(ctx, key)
	if err != nil {
		return err
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return fmt.Errorf("close object: %w", closeErr)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("object contents differ")
	}
	return nil
}
