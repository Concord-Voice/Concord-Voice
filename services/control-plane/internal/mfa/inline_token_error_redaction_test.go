package mfa

// The WebAuthn inline-token consume error path must never carry the token. A
// faulted spend returns an error its callers log, and a driver or client hook
// may annotate that error with the statement's own arguments (as one
// legitimately can). Before #3509 the token lived in a Redis key that embedded
// it, so such an annotation leaked a live, spendable token. Since #3509 the
// spend is a SQL DELETE on step_up_tokens whose only token-derived argument is
// SHA-256(token): the property under test is that the token is hashed BEFORE it
// reaches the store, so even an argument-echoing fault cannot reveal it, while
// the consume still fails closed with a non-nil error.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// iteArgsEchoConnector fails every statement on step_up_tokens with an error
// that carries the statement's arguments verbatim.
type iteArgsEchoConnector struct{ base driver.Connector }

func (c iteArgsEchoConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return iteArgsEchoConn{Conn: conn}, nil
}

func (c iteArgsEchoConnector) Driver() driver.Driver { return c.base.Driver() }

type iteArgsEchoConn struct{ driver.Conn }

func (c iteArgsEchoConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "step_up_tokens") {
		values := make([]string, 0, len(args))
		for _, a := range args {
			values = append(values, fmt.Sprintf("%s|%x", a.Value, a.Value))
		}
		return nil, fmt.Errorf("db: %s", strings.Join(values, ","))
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// TestInlineToken_ConsumeErrorDoesNotCarryTheToken: a store fault on consuming
// a WebAuthn inline token must fail closed without leaking the live token
// through the returned (and subsequently logged) error.
//
// Mutant killed: passing the token itself to the spend statement instead of
// its hash (the echoed argument then carries the canary).
func TestInlineToken_ConsumeErrorDoesNotCarryTheToken(t *testing.T) {
	dbtest.SetupTestDB(t)
	base, err := pq.NewConnector(dbtest.DatabaseURL())
	require.NoError(t, err)
	db := sql.OpenDB(iteArgsEchoConnector{base: base})
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{db: db}

	const canary = "this-is-a-leak-canary-not-a-token" // > 20 chars: required to reach the consume path

	verified, err := h.VerifyCode(context.Background(), "00000000-0000-0000-0000-000000000001", stepup.PurposeBackupEmailSet, canary)

	assert.False(t, verified, "a store fault on token consumption must never verify")
	require.Error(t, err, "a store fault on token consumption must fail closed, not silently pass")
	require.Contains(t, err.Error(), "db: ", "precondition: the argument-echoing fault fired")
	assert.NotContains(t, err.Error(), canary,
		"the error returned from a faulted inline-token consume must not carry the submitted token")
	assert.NotContains(t, err.Error(), fmt.Sprintf("%x", canary),
		"nor its bytes in hex")
}
