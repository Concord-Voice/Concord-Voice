package voice

import (
	"context"
	"database/sql"
	"time"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/websocket"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	natsclient "github.com/markdrogersjr/Concord/services/control-plane/pkg/nats"
)

// DefaultTempGrantSweepInterval is the cadence for the orphan-sweep backstop (#487
// D3). The grant lifetime is presence-bound: the primary cleanup runs on
// voice.left and the heartbeat stale-removal path. The sweep is a daily safety net
// that catches temp grants whose user is no longer in the channel but whose
// override survived a missed trigger (e.g., a server restart between leave and the
// revoke completing).
const DefaultTempGrantSweepInterval = 24 * time.Hour

// orphanedTempGrant is one (channel, user, server) tuple whose temporary SBAC
// override has no matching live voice_participants row.
type orphanedTempGrant struct {
	channelID string
	userID    string
	serverID  string
}

// TempGrantSweeper runs the nightly orphan-sweep backstop. It owns a
// tempGrantManager so every swept orphan converges on the exact same
// revokeTemporaryChannelAccess path the live triggers use (#487 P1).
type TempGrantSweeper struct {
	db  *sql.DB
	log *logger.Logger
	mgr *tempGrantManager
}

// NewTempGrantSweeper constructs a sweeper bound to the same dependencies as the
// live cleanup triggers so the convergence path is identical.
func NewTempGrantSweeper(db *sql.DB, log *logger.Logger, hub *websocket.Hub, resolver *rbac.Resolver, nats *natsclient.Client) *TempGrantSweeper {
	return &TempGrantSweeper{
		db:  db,
		log: log,
		mgr: newTempGrantManager(db, log, hub, resolver, nats),
	}
}

// sweepOrphanedTempGrants finds every is_temporary='user' override whose target has
// no live voice_participants row in that channel and revokes each through the single
// cleanup convergence point. Returns the count of orphans revoked. actorID is ""
// (system-initiated → revoked_by NULL).
func (s *TempGrantSweeper) sweepOrphanedTempGrants(ctx context.Context) (int, error) {
	orphans, err := s.selectOrphanedTempGrants(ctx)
	if err != nil {
		return 0, err
	}
	revoked := 0
	for _, o := range orphans {
		if err := s.mgr.revokeTemporaryChannelAccess(ctx, o.serverID, o.channelID, o.userID, ""); err != nil {
			// Log and continue — one bad row must not strand the rest of the sweep.
			s.log.Error("temp-grant sweep: revoke orphan",
				"error", err, "channel_id", o.channelID, "user_id", o.userID, "server_id", o.serverID)
			continue
		}
		revoked++
	}
	return revoked, nil
}

// selectOrphanedTempGrants returns temp grants whose holder is no longer present in
// the channel. The LEFT JOIN + vp.user_id IS NULL is the anti-join that isolates
// orphans (a grant whose user IS still present has a non-NULL vp.user_id and is
// excluded).
//
// Grant→join grace (finding #3): a grant inserted moments before the user's
// voice.joined event lands has NO voice_participants row yet, so it would look
// like an orphan to the anti-join above. The granted_at < NOW() - 60s predicate
// excludes brand-new grants from the sweep, leaving the grant→join window to the
// presence-bound triggers. granted_at IS NULL keeps pre-#487 rows (none carry the
// column historically) sweepable for backward safety.
func (s *TempGrantSweeper) selectOrphanedTempGrants(ctx context.Context) ([]orphanedTempGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cpo.channel_id, cpo.target_id, c.server_id
		FROM channel_permission_overrides cpo
		JOIN channels c ON c.id = cpo.channel_id
		LEFT JOIN voice_participants vp
		  ON vp.channel_id = cpo.channel_id AND vp.user_id = cpo.target_id
		WHERE cpo.is_temporary AND cpo.target_type = 'user' AND vp.user_id IS NULL
		  AND (cpo.granted_at IS NULL OR cpo.granted_at < NOW() - INTERVAL '60 seconds')`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var orphans []orphanedTempGrant
	for rows.Next() {
		var o orphanedTempGrant
		if scanErr := rows.Scan(&o.channelID, &o.userID, &o.serverID); scanErr != nil {
			return nil, scanErr
		}
		orphans = append(orphans, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return orphans, nil
}

// StartTempGrantSweepWorker launches a goroutine that runs the orphan sweep on a
// fixed interval (default 24h), plus once at startup. It stops cleanly when ctx is
// cancelled. Mirrors auth.StartPendingCleanupWorker (the established background-job
// pattern). Safe no-op constructed at call sites; failures are logged, never fatal.
func StartTempGrantSweepWorker(
	ctx context.Context,
	db *sql.DB,
	log *logger.Logger,
	hub *websocket.Hub,
	resolver *rbac.Resolver,
	nats *natsclient.Client,
	interval time.Duration,
) {
	sweeper := NewTempGrantSweeper(db, log, hub, resolver, nats)
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		if n, err := sweeper.sweepOrphanedTempGrants(ctx); err != nil {
			log.Warn("temp-grant sweep: startup sweep failed", "error", err)
		} else if n > 0 {
			log.Info("temp-grant sweep: startup sweep", "revoked", n)
		}
		for {
			select {
			case <-ctx.Done():
				log.Info("temp-grant sweep worker stopped")
				return
			case <-ticker.C:
				if n, err := sweeper.sweepOrphanedTempGrants(ctx); err != nil {
					log.Warn("temp-grant sweep: sweep failed", "error", err)
				} else if n > 0 {
					log.Info("temp-grant sweep: revoked orphaned grants", "revoked", n)
				}
			}
		}
	}()
}
