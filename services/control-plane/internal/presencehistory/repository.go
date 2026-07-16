package presencehistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 100
)

// ErrInvalidListOptions identifies a client-correctable pagination request.
var ErrInvalidListOptions = errors.New("invalid activity history list options")

// ItemStatus describes whether the server understands an item's typed payload.
type ItemStatus string

const (
	// ItemStatusSupported means Payload contains a validated typed value.
	ItemStatusSupported ItemStatus = "supported"
	// ItemStatusUnsupported preserves metadata while withholding raw payload JSON.
	ItemStatusUnsupported ItemStatus = "unsupported"
)

// HistoryItem is the concrete response shape for an Activity History interval.
type HistoryItem struct {
	Status         ItemStatus       `json:"status"`
	ID             uuid.UUID        `json:"id"`
	Category       Category         `json:"category"`
	PayloadVersion int16            `json:"payload_version"`
	Payload        *CustomTextState `json:"payload"`
	StartedAt      time.Time        `json:"started_at"`
	EndedAt        *time.Time       `json:"ended_at"`
	RecordedAt     time.Time        `json:"recorded_at"`
	ExpiresAt      time.Time        `json:"expires_at"`
}

// HistoryPage is one self-scoped keyset page.
type HistoryPage struct {
	Items      []HistoryItem `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

// ListOptions contains a validated page size and optional decoded boundary.
type ListOptions struct {
	Limit  int
	Before *PageCursor
}

// SettingsRow is the settings subset required while holding the canonical row
// locks for a presence write.
type SettingsRow struct {
	HistoryEnabled bool
	RetentionDays  int16
	ConsentVersion sql.NullInt16
	ConsentHash    sql.NullString
}

// Repository owns immutable disclosure state and the concrete PostgreSQL
// connection used for self-scoped reads.
type Repository struct {
	db         *sql.DB
	disclosure DisclosureState
	log        *logger.Logger
}

// NewRepository constructs an Activity History repository for one process's
// immutable startup disclosure.
func NewRepository(db *sql.DB, disclosure DisclosureState) *Repository {
	return newRepositoryWithLogger(db, disclosure, logger.New("production"))
}

func newRepositoryWithLogger(
	db *sql.DB,
	disclosure DisclosureState,
	log *logger.Logger,
) *Repository {
	if log == nil {
		log = logger.New("production")
	}
	return &Repository{db: db, disclosure: cloneDisclosure(disclosure), log: log}
}

func cloneDisclosure(disclosure DisclosureState) DisclosureState {
	if disclosure.RequiredConsent == nil {
		return DisclosureState{Available: disclosure.Available}
	}
	consent := *disclosure.RequiredConsent
	consent.Details = append([]string(nil), consent.Details...)
	return DisclosureState{Available: disclosure.Available, RequiredConsent: &consent}
}

// List returns only the requested sender's unexpired intervals.
func (r *Repository) List(
	ctx context.Context,
	senderID uuid.UUID,
	options ListOptions,
) (HistoryPage, error) {
	limit, err := normalizeListOptions(options)
	if err != nil {
		return HistoryPage{}, err
	}
	rows, err := r.queryHistoryRows(ctx, senderID, options.Before, limit+1)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("list activity history: %w", err)
	}

	items, unsupportedCount, err := readHistoryItems(rows, limit+1)
	if err != nil {
		return HistoryPage{}, err
	}
	page, err := buildHistoryPage(items, limit)
	if err != nil {
		return HistoryPage{}, closeListRowsWithError(rows, err)
	}
	if err := rows.Close(); err != nil {
		return HistoryPage{}, fmt.Errorf("close activity history rows: %w", err)
	}
	if unsupportedCount > 0 {
		r.log.Warn(
			"stored activity payload is unsupported",
			"operation", "activity_history_read",
			"error_class", "unsupported_stored_payload",
			"unsupported_count", unsupportedCount,
		)
	}
	return page, nil
}

func normalizeListOptions(options ListOptions) (int, error) {
	limit := options.Limit
	if limit == 0 {
		limit = defaultPageLimit
	}
	if limit < 1 || limit > maxPageLimit {
		return 0, ErrInvalidListOptions
	}
	if options.Before != nil &&
		(options.Before.Version != 1 || options.Before.RecordedAt.IsZero() || options.Before.ID == uuid.Nil) {
		return 0, ErrInvalidListOptions
	}
	return limit, nil
}

func (r *Repository) queryHistoryRows(
	ctx context.Context,
	senderID uuid.UUID,
	before *PageCursor,
	limit int,
) (*sql.Rows, error) {
	if before == nil {
		return r.db.QueryContext(ctx, `
			SELECT id, category, payload_version, payload,
			       started_at, ended_at, recorded_at, expires_at
			FROM presence_history
			WHERE sender_id = $1
			  AND expires_at > clock_timestamp()
			ORDER BY recorded_at DESC, id DESC
			LIMIT $2
		`, senderID, limit)
	}
	return r.db.QueryContext(ctx, `
		SELECT id, category, payload_version, payload,
		       started_at, ended_at, recorded_at, expires_at
		FROM presence_history
		WHERE sender_id = $1
		  AND (recorded_at, id) < ($2, $3)
		  AND expires_at > clock_timestamp()
		ORDER BY recorded_at DESC, id DESC
		LIMIT $4
	`, senderID, before.RecordedAt, before.ID, limit)
}

func readHistoryItems(rows *sql.Rows, capacity int) ([]HistoryItem, int, error) {
	items := make([]HistoryItem, 0, capacity)
	unsupportedCount := 0
	for rows.Next() {
		item, err := scanHistoryItem(rows)
		if err != nil {
			return nil, 0, closeListRowsWithError(rows, err)
		}
		if item.Status == ItemStatusUnsupported {
			unsupportedCount++
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, closeListRowsWithError(
			rows,
			fmt.Errorf("list activity history rows: %w", err),
		)
	}
	return items, unsupportedCount, nil
}

func buildHistoryPage(items []HistoryItem, limit int) (HistoryPage, error) {
	if len(items) <= limit {
		return HistoryPage{Items: items}, nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	encoded, err := EncodeCursor(PageCursor{Version: 1, RecordedAt: last.RecordedAt, ID: last.ID})
	if err != nil {
		return HistoryPage{}, fmt.Errorf("encode activity history page boundary: %w", err)
	}
	return HistoryPage{Items: items, NextCursor: &encoded}, nil
}

func closeListRowsWithError(rows *sql.Rows, cause error) error {
	if closeErr := rows.Close(); closeErr != nil {
		return errors.Join(cause, fmt.Errorf("close activity history rows: %w", closeErr))
	}
	return cause
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanHistoryItem(row rowScanner) (HistoryItem, error) {
	var (
		item    HistoryItem
		payload json.RawMessage
		ended   sql.NullTime
	)
	if err := row.Scan(
		&item.ID,
		&item.Category,
		&item.PayloadVersion,
		&payload,
		&item.StartedAt,
		&ended,
		&item.RecordedAt,
		&item.ExpiresAt,
	); err != nil {
		return HistoryItem{}, fmt.Errorf("scan activity history metadata: %w", err)
	}
	if ended.Valid {
		value := ended.Time
		item.EndedAt = &value
	}
	if decoded, ok := decodeStoredPayload(item.Category, item.PayloadVersion, payload); ok {
		item.Status = ItemStatusSupported
		item.Payload = decoded
	} else {
		item.Status = ItemStatusUnsupported
	}
	return item, nil
}

func decodeStoredPayload(category Category, version int16, raw json.RawMessage) (*CustomTextState, bool) {
	reader, ok := payloadReaders[payloadKey{Category: category, Version: version}]
	if !ok {
		return nil, false
	}
	decoded, err := reader(raw)
	if err != nil {
		return nil, false
	}
	state, ok := decoded.(CustomTextState)
	if !ok {
		return nil, false
	}
	return &state, true
}

func lockUserAndSettings(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
) (SettingsRow, error) {
	var lockedUser uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE
	`, userID).Scan(&lockedUser); err != nil {
		return SettingsRow{}, fmt.Errorf("lock activity history owner: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_presence_settings (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
	`, userID); err != nil {
		return SettingsRow{}, fmt.Errorf("ensure activity history settings: %w", err)
	}

	var settings SettingsRow
	if err := tx.QueryRowContext(ctx, `
		SELECT activity_history_enabled,
		       activity_history_retention_days,
		       activity_history_consent_version,
		       activity_history_consent_copy_hash
		FROM user_presence_settings
		WHERE user_id = $1
		FOR UPDATE
	`, userID).Scan(
		&settings.HistoryEnabled,
		&settings.RetentionDays,
		&settings.ConsentVersion,
		&settings.ConsentHash,
	); err != nil {
		return SettingsRow{}, fmt.Errorf("lock activity history settings: %w", err)
	}
	return settings, nil
}
