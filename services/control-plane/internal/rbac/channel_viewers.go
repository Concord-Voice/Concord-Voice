package rbac

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// FilterVisibleUserIDsForChannelFresh returns the candidate users who can
// currently view one exact channel. It bypasses the permission cache and
// resolves membership, owner/administrator bypasses, role permissions, and
// role/user SBAC overrides in one fresh database query.
func (r *Resolver) FilterVisibleUserIDsForChannelFresh(
	ctx context.Context,
	serverID string,
	channelID string,
	candidateUserIDs []string,
) (viewerIDs []string, returnErr error) {
	return filterVisibleUserIDsForChannel(ctx, r.db, serverID, channelID, candidateUserIDs)
}

// FilterVisibleUserIDsForChannelTx is the transaction-bound variant for a
// channel being created or updated in the caller's transaction.
func (r *Resolver) FilterVisibleUserIDsForChannelTx(
	ctx context.Context,
	tx *sql.Tx,
	serverID string,
	channelID string,
	candidateUserIDs []string,
) (viewerIDs []string, returnErr error) {
	return filterVisibleUserIDsForChannel(ctx, tx, serverID, channelID, candidateUserIDs)
}

// CanDistributeChannelKeyTx reports whether one current channel viewer can
// serve as the sole writer for an incomplete initial key distribution.
func (r *Resolver) CanDistributeChannelKeyTx(ctx context.Context, tx *sql.Tx, serverID, channelID, userID string) (bool, error) {
	viewers, err := r.FilterVisibleUserIDsForChannelTx(ctx, tx, serverID, channelID, []string{userID})
	return len(viewers) == 1, err
}

type channelViewerQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func filterVisibleUserIDsForChannel(
	ctx context.Context,
	queryer channelViewerQueryer,
	serverID string,
	channelID string,
	candidateUserIDs []string,
) (viewerIDs []string, returnErr error) {
	if len(candidateUserIDs) == 0 {
		return []string{}, nil
	}

	const query = `
		WITH candidates AS (
			SELECT DISTINCT candidate_id AS user_id
			FROM unnest($3::uuid[]) AS supplied(candidate_id)
		),
		members AS (
			SELECT candidates.user_id
			FROM candidates
			JOIN server_members sm
			  ON sm.server_id = $1 AND sm.user_id = candidates.user_id
		),
		base_permissions AS (
			SELECT members.user_id,
			       COALESCE(BIT_OR(roles.permissions), 0) AS permissions
			FROM members
			LEFT JOIN member_roles mr
			  ON mr.server_id = $1 AND mr.user_id = members.user_id
			LEFT JOIN roles
			  ON roles.id = mr.role_id AND roles.server_id = $1
			GROUP BY members.user_id
		),
		role_overrides AS (
			SELECT members.user_id,
			       COALESCE(BIT_OR(cpo.allow), 0) AS role_allow,
			       COALESCE(BIT_OR(cpo.deny), 0) AS role_deny
			FROM members
			LEFT JOIN member_roles mr
			  ON mr.server_id = $1 AND mr.user_id = members.user_id
			LEFT JOIN roles override_roles
			  ON override_roles.id = mr.role_id AND override_roles.server_id = $1
			LEFT JOIN channel_permission_overrides cpo
			  ON cpo.channel_id = $2
			 AND cpo.target_type = 'role'
			 AND cpo.target_id = override_roles.id
			GROUP BY members.user_id
		),
		user_overrides AS (
			SELECT members.user_id,
			       COALESCE(BIT_OR(cpo.allow), 0) AS user_allow,
			       COALESCE(BIT_OR(cpo.deny), 0) AS user_deny
			FROM members
			LEFT JOIN channel_permission_overrides cpo
			  ON cpo.channel_id = $2
			 AND cpo.target_type = 'user'
			 AND cpo.target_id = members.user_id
			GROUP BY members.user_id
		)
		SELECT members.user_id
		FROM members
		JOIN channels c ON c.id = $2 AND c.server_id = $1
		JOIN servers s ON s.id = $1
		JOIN base_permissions bp ON bp.user_id = members.user_id
		JOIN role_overrides ro ON ro.user_id = members.user_id
		JOIN user_overrides uo ON uo.user_id = members.user_id
		WHERE
			s.owner_id = members.user_id
			OR (bp.permissions & $4::bigint) != 0
			OR (
				(
					(
						(bp.permissions | ro.role_allow) & ~ro.role_deny
						| uo.user_allow
					) & ~uo.user_deny
				) &
				CASE WHEN c.type = 'voice' THEN $6::bigint ELSE $5::bigint END
				!= 0
			)
		ORDER BY members.user_id
	`

	rows, err := queryer.QueryContext(
		ctx,
		query,
		serverID,
		channelID,
		pq.Array(candidateUserIDs),
		int64(PermAdministrator),
		int64(PermViewTextChannels),
		int64(PermViewVoiceChannels),
	)
	if err != nil {
		return nil, fmt.Errorf("query channel viewers: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			viewerIDs = nil
			returnErr = errors.Join(returnErr, fmt.Errorf("close channel viewer rows: %w", closeErr))
		}
	}()

	for rows.Next() {
		var viewerID string
		if err := rows.Scan(&viewerID); err != nil {
			return nil, fmt.Errorf("scan channel viewer row: %w", err)
		}
		viewerIDs = append(viewerIDs, viewerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel viewer rows: %w", err)
	}
	if viewerIDs == nil {
		viewerIDs = []string{}
	}
	return viewerIDs, nil
}
