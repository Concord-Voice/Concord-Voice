// Package expiration drains expired messages through the restricted purge
// terminal. Discovery is deliberately bounded; the database remains the
// durable backlog between passes.
package expiration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/purge"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// DefaultExpirySweepInterval is the cadence for live expiry passes.
const (
	DefaultExpirySweepInterval = 5 * time.Minute
	MaxExpiryContextsPerTable  = 10
)

// SweeperDeps supplies the bounded deletion terminal and notification sinks.
// The function fields keep the coordinator independently testable without
// widening the purge engine's restricted expiry contract.
type SweeperDeps struct {
	DB               *sql.DB
	Clock            func(context.Context) (time.Time, error)
	RunExpiryBatch   func(context.Context, purge.ExpiryPlan) (purge.Result, error)
	EmitDMPurged     func(context.Context, string, time.Time)
	EmitServerPurged func(context.Context, string, time.Time)
	Log              *logger.Logger
}

// Sweeper discovers a small oldest-first set from each message table per pass.
type Sweeper struct {
	db               *sql.DB
	clock            func(context.Context) (time.Time, error)
	runExpiryBatch   func(context.Context, purge.ExpiryPlan) (purge.Result, error)
	emitDMPurged     func(context.Context, string, time.Time)
	emitServerPurged func(context.Context, string, time.Time)
	log              *logger.Logger
}

type expiryGroup struct {
	contextType purge.ContextType
	contextID   string
	serverID    string
	candidates  []string
}

// NewSweeper validates the dependencies required to discover and delete expiry candidates.
func NewSweeper(deps SweeperDeps) (*Sweeper, error) {
	if deps.DB == nil {
		return nil, errors.New("expiration sweeper requires database")
	}
	if deps.RunExpiryBatch == nil {
		return nil, errors.New("expiration sweeper requires expiry purge terminal")
	}
	if deps.EmitDMPurged == nil {
		return nil, errors.New("expiration sweeper requires DM purge notifier")
	}
	if deps.EmitServerPurged == nil {
		return nil, errors.New("expiration sweeper requires server purge notifier")
	}
	if deps.Clock == nil {
		deps.Clock = func(ctx context.Context) (time.Time, error) {
			var now time.Time
			if err := deps.DB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return time.Time{}, fmt.Errorf("expiration sweep clock: %w", err)
			}
			return now, nil
		}
	}
	return &Sweeper{
		db: deps.DB, clock: deps.Clock, runExpiryBatch: deps.RunExpiryBatch,
		emitDMPurged: deps.EmitDMPurged, emitServerPurged: deps.EmitServerPurged, log: deps.Log,
	}, nil
}

// RunPass obtains one database-authoritative cutoff and drains at most ten
// channel contexts and ten DM/group contexts from their oldest 5,000 candidates.
func (s *Sweeper) RunPass(ctx context.Context) (int, error) {
	cutoff, err := s.now(ctx)
	if err != nil {
		return 0, err
	}
	return s.runPass(ctx, cutoff)
}

// RunPreflight drains bounded passes until discovery is empty, then proves a
// fresh cutoff has no eligible row in either table before startup may continue.
func (s *Sweeper) RunPreflight(ctx context.Context) error {
	for {
		discovered, err := s.RunPass(ctx)
		if err != nil {
			return err
		}
		if discovered != 0 {
			continue
		}
		cutoff, err := s.now(ctx)
		if err != nil {
			return err
		}
		eligible, err := s.anyEligible(ctx, cutoff)
		if err != nil {
			return err
		}
		if !eligible {
			return nil
		}
	}
}

// RunWorker waits for its first tick so successful preflight is the only
// startup sweep, then retries failures on the fixed cadence.
func (s *Sweeper) RunWorker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultExpirySweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.RunPass(ctx); err != nil && ctx.Err() == nil && s.log != nil {
				s.log.Warn("expiration sweep pass failed", "error", err)
			}
		}
	}
}

func (s *Sweeper) runPass(ctx context.Context, cutoff time.Time) (discovered int, err error) {
	seenServers := make(map[string]struct{}, MaxExpiryContextsPerTable)
	deleted := 0
	defer func() {
		for serverID := range seenServers {
			s.emitServerPurged(ctx, serverID, cutoff)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(err, ctxErr)
		}
		if s.log == nil {
			return
		}
		if err != nil {
			s.log.Warn("expiration sweep pass incomplete", "discovered", discovered, "deleted", deleted, "error", err)
			return
		}
		if discovered == 0 {
			s.log.Info("expiration sweep pass idle", "discovered", 0, "deleted", 0)
			return
		}
		s.log.Info("expiration sweep pass completed", "discovered", discovered, "deleted", deleted)
	}()

	channelGroups, channelDiscovered, err := s.channelGroups(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	discovered += channelDiscovered
	for _, group := range channelGroups {
		result, batchErr := s.runExpiryBatch(ctx, purge.ExpiryPlan{
			ContextType: group.contextType, ContextID: group.contextID, ServerID: &group.serverID,
			CandidateIDs: group.candidates, ExpiresBefore: cutoff,
		})
		if result.DeletedCount > 0 {
			deleted += result.DeletedCount
			seenServers[group.serverID] = struct{}{}
		}
		if batchErr != nil {
			return discovered, fmt.Errorf("expire channel context: %w", batchErr)
		}
	}

	dmGroups, dmDiscovered, err := s.dmGroups(ctx, cutoff)
	if err != nil {
		return discovered, err
	}
	discovered += dmDiscovered
	for _, group := range dmGroups {
		result, batchErr := s.runExpiryBatch(ctx, purge.ExpiryPlan{
			ContextType: group.contextType, ContextID: group.contextID,
			CandidateIDs: group.candidates, ExpiresBefore: cutoff,
		})
		if result.DeletedCount > 0 {
			deleted += result.DeletedCount
			s.emitDMPurged(ctx, group.contextID, cutoff)
		}
		if batchErr != nil {
			return discovered, fmt.Errorf("expire DM context: %w", batchErr)
		}
	}
	return discovered, nil
}

func (s *Sweeper) now(ctx context.Context) (time.Time, error) {
	now, err := s.clock(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("get expiration sweep cutoff: %w", err)
	}
	if now.IsZero() {
		return time.Time{}, errors.New("expiration sweep clock returned zero cutoff")
	}
	return now.UTC().Truncate(time.Microsecond), nil
}

func (s *Sweeper) anyEligible(ctx context.Context, cutoff time.Time) (bool, error) {
	var eligible bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE expires_at < $1)
		OR EXISTS (SELECT 1 FROM dm_messages WHERE expires_at < $1)`, cutoff).Scan(&eligible)
	if err != nil {
		return false, fmt.Errorf("verify expired messages: %w", err)
	}
	return eligible, nil
}

func (s *Sweeper) channelGroups(ctx context.Context, cutoff time.Time) (groups []expiryGroup, discovered int, returnErr error) {
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS MATERIALIZED (
		SELECT id, channel_id, expires_at FROM messages
		WHERE expires_at < $1 ORDER BY expires_at LIMIT $2
	)
	SELECT candidates.id, candidates.channel_id, channels.server_id
	FROM candidates JOIN channels ON channels.id = candidates.channel_id
	ORDER BY candidates.expires_at`, cutoff, purge.MaxExpiryCandidateIDs)
	if err != nil {
		return nil, 0, fmt.Errorf("discover expired channel messages: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("close expired channel message discovery: %w", closeErr)
		}
	}()
	return collectChannelGroups(rows)
}

func (s *Sweeper) dmGroups(ctx context.Context, cutoff time.Time) (groups []expiryGroup, discovered int, returnErr error) {
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS MATERIALIZED (
		SELECT id, conversation_id, expires_at FROM dm_messages
		WHERE expires_at < $1 ORDER BY expires_at LIMIT $2
	)
	SELECT candidates.id, candidates.conversation_id, conversations.is_group
	FROM candidates JOIN dm_conversations conversations ON conversations.id = candidates.conversation_id
	ORDER BY candidates.expires_at`, cutoff, purge.MaxExpiryCandidateIDs)
	if err != nil {
		return nil, 0, fmt.Errorf("discover expired DM messages: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("close expired DM message discovery: %w", closeErr)
		}
	}()
	return collectDMGroups(rows)
}

func collectChannelGroups(rows *sql.Rows) ([]expiryGroup, int, error) {
	groups := make([]expiryGroup, 0, MaxExpiryContextsPerTable)
	indices := make(map[string]int, MaxExpiryContextsPerTable)
	discovered := 0
	for rows.Next() {
		var messageID, channelID, serverID string
		if err := rows.Scan(&messageID, &channelID, &serverID); err != nil {
			return nil, 0, fmt.Errorf("scan expired channel message: %w", err)
		}
		discovered++
		index, found := indices[channelID]
		if !found {
			if len(groups) == MaxExpiryContextsPerTable {
				continue
			}
			index = len(groups)
			indices[channelID] = index
			groups = append(groups, expiryGroup{contextType: purge.ContextChannel, contextID: channelID, serverID: serverID})
		}
		groups[index].candidates = append(groups[index].candidates, messageID)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate expired channel messages: %w", err)
	}
	return groups, discovered, nil
}

func collectDMGroups(rows *sql.Rows) ([]expiryGroup, int, error) {
	groups := make([]expiryGroup, 0, MaxExpiryContextsPerTable)
	indices := make(map[string]int, MaxExpiryContextsPerTable)
	discovered := 0
	for rows.Next() {
		var messageID, conversationID string
		var isGroup bool
		if err := rows.Scan(&messageID, &conversationID, &isGroup); err != nil {
			return nil, 0, fmt.Errorf("scan expired DM message: %w", err)
		}
		discovered++
		index, found := indices[conversationID]
		if !found {
			if len(groups) == MaxExpiryContextsPerTable {
				continue
			}
			contextType := purge.ContextDM
			if isGroup {
				contextType = purge.ContextGroup
			}
			index = len(groups)
			indices[conversationID] = index
			groups = append(groups, expiryGroup{contextType: contextType, contextID: conversationID})
		}
		groups[index].candidates = append(groups[index].candidates, messageID)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate expired DM messages: %w", err)
	}
	return groups, discovered, nil
}
