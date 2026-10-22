package store

import (
	"context"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/authoritydb"
)

// The binary migrates from its embedded copy (B2-D1), through the shared
// runner and so through its dirty refusal (D-33).
func TestMigrateEmbeddedBuildsTheSchemaAndRefusesADirtyOne(t *testing.T) {
	owner, super := authoritydb.DirtySetup(t)
	if err := MigrateEmbedded(owner); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := CheckSchema(context.Background(), owner); err != nil {
		t.Fatalf("schema after the embedded run: %v", err)
	}
	if err := MigrateEmbedded(owner); err != nil {
		t.Fatalf("second run is no change: %v", err)
	}
	mustExec(t, super, `UPDATE authority.schema_migrations SET dirty = true`)
	// The runner's own text (migrationrunner.RunSource), not ErrSchemaDirty's
	// "schema is dirty", which CheckSchema returns and "is dirty" also matched.
	if err := MigrateEmbedded(owner); err == nil || !strings.Contains(err.Error(), "migration version 1 is dirty") {
		t.Fatalf("a dirty schema: %v, want the runner's dirty refusal", err)
	}
}
