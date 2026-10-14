package media

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/storage"
)

const (
	dmIconReadTimeout = 5 * time.Second
	dmIconMaxBytes    = iconMaxUpload
)

var (
	errDMIconNotFound           = errors.New("DM icon not found")
	errDMIconStorageUnavailable = errors.New("DM icon storage unavailable")
)

type dmIconImage struct {
	body        []byte
	contentType string
}

func (h *Handler) loadDMIcon(ctx context.Context, viewerID, conversationID uuid.UUID) (image dmIconImage, err error) {
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: false})
	if err != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "database", err)
	}
	defer func() {
		cleanupErr := tx.Rollback()
		err = h.finishDMIconRead(ctx, cleanupErr, err)
	}()

	// This row lock conflicts with participant removal without acquiring any
	// parent/user/voice locks. READ COMMITTED rechecks a row after a DELETE wait.
	var admitted int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM dm_participants
		WHERE conversation_id = $1 AND user_id = $2
		FOR SHARE
	`, conversationID, viewerID).Scan(&admitted)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "canceled", ctxErr)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return dmIconImage{}, errDMIconNotFound
	}
	if err != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "database", err)
	}
	if h.store == nil {
		return dmIconImage{}, errDMIconStorageUnavailable
	}

	image, err = h.readDMIconObject(ctx, "dm-icons/"+conversationID.String())
	if err != nil {
		return dmIconImage{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "canceled", ctxErr)
	}
	if err = h.commitDMIconReadTx(tx); err != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "commit", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "canceled", ctxErr)
	}
	return image, nil
}

func (h *Handler) finishDMIconRead(ctx context.Context, cleanupErr, err error) error {
	if cleanupErr != nil && !errors.Is(cleanupErr, sql.ErrTxDone) {
		h.logDMIconFailure("cleanup")
		err = fmt.Errorf("rollback DM icon read: %w", cleanupErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil && (errors.Is(err, errDMIconNotFound) || errors.Is(err, errDMIconStorageUnavailable)) {
		err = h.dmIconReadFailure(ctx, "canceled", ctxErr)
	}
	return err
}

func (h *Handler) readDMIconObject(ctx context.Context, key string) (image dmIconImage, err error) {
	reader, contentType, getErr := h.store.GetObject(ctx, key)
	closed := false
	// A store may supply an opened reader alongside an acquisition error.
	// Keep ownership until it has been closed, including cancellation paths.
	defer func() {
		err = h.closeUnfinishedDMIconRead(ctx, reader, closed, err)
	}()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "canceled", ctxErr)
	}
	if errors.Is(getErr, storage.ErrObjectNotFound) {
		return dmIconImage{}, errDMIconNotFound
	}
	if getErr != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "storage", getErr)
	}
	if reader == nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "storage", errors.New("DM icon reader unavailable"))
	}

	body, readErr := io.ReadAll(io.LimitReader(reader, dmIconMaxBytes+1))
	readCtxErr := ctx.Err()
	closeErr := reader.Close()
	closed = true
	if readCtxErr != nil {
		readErr = readCtxErr
	}
	if readErr != nil {
		err = h.dmIconReadFailure(ctx, "read", readErr)
		if closeErr != nil {
			h.logDMIconFailure("cleanup")
		}
		return dmIconImage{}, err
	}
	if closeErr != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "close", closeErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "canceled", ctxErr)
	}
	if len(body) > dmIconMaxBytes {
		return dmIconImage{}, h.dmIconReadFailure(ctx, "size", errors.New("DM icon exceeds size limit"))
	}
	return dmIconImage{body: body, contentType: contentType}, nil
}

func (h *Handler) closeUnfinishedDMIconRead(ctx context.Context, reader io.ReadCloser, closed bool, err error) error {
	if reader == nil || closed {
		return err
	}
	if cleanupErr := reader.Close(); cleanupErr != nil {
		if errors.Is(err, errDMIconNotFound) {
			return h.dmIconReadFailure(ctx, "close", cleanupErr)
		}
		h.logDMIconFailure("cleanup")
		err = fmt.Errorf("close DM icon after acquisition failure: %w", cleanupErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, errDMIconNotFound) {
		err = h.dmIconReadFailure(ctx, "canceled", ctxErr)
	}
	return err
}

func (h *Handler) commitDMIconReadTx(tx *sql.Tx) error {
	if h.dmIconCommit != nil {
		return h.dmIconCommit(tx)
	}
	return tx.Commit()
}

func (h *Handler) dmIconReadFailure(ctx context.Context, class string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		class, err = "canceled", ctxErr
	}
	h.logDMIconFailure(class)
	return fmt.Errorf("load DM icon %s: %w", class, err)
}

func (h *Handler) logDMIconFailure(class string) {
	h.log.Error("Failed to retrieve DM icon", "failure_class", class)
}
