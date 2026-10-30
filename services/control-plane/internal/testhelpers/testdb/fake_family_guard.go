package testdb

import (
	"context"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
)

// FakeFamilyExpiry is the access expiry of every fake family identity.
var FakeFamilyExpiry = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

var fakeFamilyID = uuid.MustParse("00000000-0000-4000-8000-00000000f4a1")

// WithFakeFamily attaches a synthetic signed identity for userID with the
// given credential epoch, for a handler test whose database is a fake driver
// wired to a FakeFamilyGuard.
func WithFakeFamily(ctx context.Context, userID, epoch string) context.Context {
	return loginfamily.WithAccessIdentity(ctx, loginfamily.AccessIdentity{
		UserID: uuid.MustParse(userID), SID: uuid.New(), CredentialEpoch: epoch, AccessExpiresAt: FakeFamilyExpiry,
	})
}

// FakeFamilyGuard answers credepoch.GuardFamilyTx's statements for a fake
// database driver as one live family whose credential epoch is Epoch (a
// 32-hex value or ""). A driver consults it first and records nothing it
// answers, so the driver's own statement traces are unchanged.
type FakeFamilyGuard struct {
	Epoch    string
	mu       sync.Mutex
	settings map[string]string
}

// Query answers a guard statement; ok is false for any other statement.
func (g *FakeFamilyGuard) Query(query string, args []driver.NamedValue) (rows driver.Rows, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.settings == nil {
		g.settings = map[string]string{}
	}
	arg := func(i int) string {
		if i < len(args) {
			if s, isString := args[i].Value.(string); isString {
				return s
			}
		}
		return ""
	}
	switch {
	case strings.Contains(query, "SELECT CASE WHEN COALESCE(current_setting"):
		if g.settings[arg(0)] != "" {
			return fakeRow(""), true
		}
		g.settings[arg(0)] = arg(1)
		return fakeRow(arg(1)), true
	case strings.Contains(query, "current_setting"):
		return fakeRow(g.settings[arg(0)]), true
	case strings.Contains(query, "SHOW transaction_isolation"):
		return fakeRow("read committed"), true
	case strings.Contains(query, "FROM login_family_rollout"):
		return fakeRow(true), true
	case strings.Contains(query, "FROM login_family_sids"):
		return fakeRow(fakeFamilyID.String(), FakeFamilyExpiry), true
	case strings.Contains(query, "FROM login_families"):
		return fakeRow(arg(0), fakeFamilyID.String(), g.Epoch, nil, nil, nil), true
	case strings.Contains(query, "SELECT clock_timestamp()"):
		return fakeRow(time.Now()), true
	case strings.Contains(query, "SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE"),
		strings.Contains(query, "SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1"):
		return fakeRow(g.Epoch), true
	}
	return nil, false
}

// Exec answers the guard's marker write; it is false for any other statement.
func (g *FakeFamilyGuard) Exec(query string, args []driver.NamedValue) bool {
	if !strings.Contains(query, "SELECT set_config(") {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.settings == nil {
		g.settings = map[string]string{}
	}
	if len(args) == 2 {
		key, _ := args[0].Value.(string)
		value, _ := args[1].Value.(string)
		g.settings[key] = value
	}
	return true
}

type fakeRows struct {
	values []driver.Value
	sent   bool
}

func fakeRow(values ...driver.Value) *fakeRows { return &fakeRows{values: values} }

func (r *fakeRows) Columns() []string { return make([]string, len(r.values)) }
func (*fakeRows) Close() error        { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	copy(dest, r.values)
	r.sent = true
	return nil
}
