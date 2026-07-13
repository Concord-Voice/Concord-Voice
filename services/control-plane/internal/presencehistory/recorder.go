package presencehistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ErrInvalidCustomTextState identifies a typed state that cannot be persisted.
var ErrInvalidCustomTextState = errors.New("invalid custom text history state")

// RecordCustomTextTransition applies one semantic Custom Status transition in
// the caller's transaction. The caller retains commit/rollback ownership.
func (r *Repository) RecordCustomTextTransition(
	ctx context.Context,
	tx *sql.Tx,
	userID uuid.UUID,
	before CustomTextState,
	after CustomTextState,
) error {
	before = normalizeCustomTextState(before)
	after = normalizeCustomTextState(after)
	if err := validateCustomTextState(after); err != nil {
		return err
	}

	settings, err := lockUserAndSettings(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !settings.HistoryEnabled {
		return nil
	}
	if !r.disclosureMatches(settings) {
		return pauseStaleConsent(ctx, tx, userID)
	}
	if before == after {
		return nil
	}

	var now sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read activity history transition clock: %w", err)
	}
	if !now.Valid {
		return fmt.Errorf("read activity history transition clock: missing value")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE presence_history
		SET ended_at = $2
		WHERE sender_id = $1
		  AND category = 'custom_text'
		  AND ended_at IS NULL
	`, userID, now.Time); err != nil {
		return fmt.Errorf("close custom text history interval: %w", err)
	}
	if after.Text == "" {
		return nil
	}

	payload, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("encode custom text history payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO presence_history (
			id,
			sender_id,
			category,
			payload_version,
			payload,
			started_at,
			ended_at,
			recorded_at,
			expires_at
		) VALUES (
			$1,
			$2,
			'custom_text',
			1,
			$3::JSONB,
			$4,
			NULL,
			$4,
			$4::TIMESTAMPTZ + ($5::INTEGER * INTERVAL '1 day')
		)
	`, uuid.New(), userID, payload, now.Time, settings.RetentionDays); err != nil {
		return fmt.Errorf("open custom text history interval: %w", err)
	}
	return nil
}

func normalizeCustomTextState(state CustomTextState) CustomTextState {
	if state.Text == "" {
		return CustomTextState{}
	}
	return state
}

func validateCustomTextState(state CustomTextState) error {
	if state.Text == "" {
		return nil
	}
	if !utf8.ValidString(state.Text) || !utf8.ValidString(state.Emoji) ||
		utf8.RuneCountInString(state.Text) > 140 || utf8.RuneCountInString(state.Emoji) > 32 {
		return ErrInvalidCustomTextState
	}
	return nil
}

func (r *Repository) disclosureMatches(settings SettingsRow) bool {
	if !r.disclosure.Available || r.disclosure.RequiredConsent == nil {
		return false
	}
	consent := r.disclosure.RequiredConsent
	return settings.ConsentVersion.Valid &&
		settings.ConsentVersion.Int16 == consent.Version &&
		settings.ConsentHash.Valid &&
		settings.ConsentHash.String == consent.CopyHash
}

func pauseStaleConsent(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `
		WITH pause_clock AS (
			SELECT clock_timestamp() AS paused_at
		), paused AS (
			UPDATE user_presence_settings AS settings
			SET activity_history_enabled = FALSE,
			    activity_history_consent_version = NULL,
			    activity_history_consent_copy_hash = NULL,
			    activity_history_consented_at = NULL,
			    activity_history_reconsent_required = TRUE,
			    updated_at = pause_clock.paused_at
			FROM pause_clock
			WHERE settings.user_id = $1
			RETURNING settings.user_id, settings.updated_at AS paused_at
		)
		UPDATE presence_history AS history
		SET ended_at = paused.paused_at
		FROM paused
		WHERE history.sender_id = paused.user_id
		  AND history.ended_at IS NULL
	`, userID); err != nil {
		return fmt.Errorf("pause stale activity history consent: %w", err)
	}
	return nil
}
