package authoritycontract

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rowbindNorm is the one naming rule a row and its struct field share: the row
// name lowercased with its underscores removed equals the Go field name
// lowercased (realm_id and RealmID, ek_public_key and EKPublicKey).
func rowbindNorm(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

// TestEveryRowBindsItsOwnStructField closes a hole no byte-level test can see.
// A spec() table whose pointers are swapped consistently (disk_ready writing
// into ArchiveOK and archive_ok into DiskReady) encodes, decodes and
// re-encodes to the same bytes, so a round trip passes, and Node's vectors
// pass too because Go reads and writes them through the same swapped pointers.
//
// It walks every registered layout, so a layout added later is covered without
// touching this test: for each row it finds which struct field the row's
// pointer addresses and requires that field to carry the row's name, and it
// requires every struct field to be carried by exactly one row (a field no row
// carries is silently never on the wire).
func TestEveryRowBindsItsOwnStructField(t *testing.T) {
	tags := Tags()
	require.NotEmpty(t, tags)
	for _, tag := range tags {
		m := registry[tag]()
		gotTag, fs := m.spec()
		require.Equal(t, tag, gotTag, "registry key and layout tag")

		v := reflect.ValueOf(m)
		require.Equal(t, reflect.Pointer, v.Kind(), "%s: registry must hold a pointer to the layout struct", tag)
		v = v.Elem()
		require.Equal(t, reflect.Struct, v.Kind(), "%s: registry must hold a layout struct", tag)

		bound := make([]int, v.NumField())
		for _, f := range fs {
			got := -1
			for i := 0; i < v.NumField(); i++ {
				if v.Field(i).Addr().Interface() == f.ptr {
					got = i
				}
			}
			require.GreaterOrEqual(t, got, 0, "%s.%s points at no field of its struct", tag, f.name)
			require.Equal(t, rowbindNorm(f.name), rowbindNorm(v.Type().Field(got).Name),
				"%s.%s is bound to the wrong struct field %s", tag, f.name, v.Type().Field(got).Name)
			bound[got]++
		}
		for i, n := range bound {
			require.Equal(t, 1, n, "%s struct field %s must be carried by exactly one row", tag, v.Type().Field(i).Name)
		}
	}
}
