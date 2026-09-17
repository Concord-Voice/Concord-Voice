package expiration

import (
	"context"
	"database/sql"
	"time"
)

// RowQuerier is the single-row read this package needs to resolve a broadcast's
// actor name. Declared at the consumer so *sql.DB, *sql.Tx and a test double all
// satisfy it without either caller package exporting a narrower interface of its
// own.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// ActorName resolves the acting user's display name for a live expiration-event
// broadcast. The DURABLE row does not need it — every read path JOINs users and
// returns username/display_name alongside the row — but the broadcast carries no
// such join, so without this a participant receiving the event live would render
// "Someone" until their next history fetch corrected it.
//
// One extra query on a policy change is the cheap side of the trade: resolving the
// name client-side would need three different lookups (channel members, group-DM
// participants, the 1:1 peer) each with its own fallback, and would still miss
// cases this query cannot. A lookup failure degrades to empty strings and the
// renderer's "Someone" fallback, which is exactly the pre-existing behaviour — it
// must never fail the broadcast, because the durable row has already committed.
//
// It lives here rather than in internal/channels and internal/dm because both
// broadcast paths need byte-identical behaviour: a divergence between them would
// show up as one surface naming the actor and the other not, which is precisely
// the kind of asymmetry nothing in the test suite would flag.
func ActorName(ctx context.Context, db RowQuerier, actorUserID string) (username string, displayName string) {
	var display sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT username, display_name FROM users WHERE id = $1`,
		actorUserID,
	).Scan(&username, &display); err != nil {
		return "", ""
	}
	if display.Valid {
		displayName = display.String
	}
	return username, displayName
}

// UpdatedAtRFC3339 renders a policy's UpdatedAt for the wire, or nil when the
// policy carries none. It returns interface{} rather than *string so the value
// drops straight into a broadcast's map: a typed nil *string would marshal to
// JSON null either way, but returning the untyped nil keeps the call site from
// having to reason about that at all.
func UpdatedAtRFC3339(policy Policy) interface{} {
	if policy.UpdatedAt == nil {
		return nil
	}
	return policy.UpdatedAt.UTC().Format(time.RFC3339)
}
