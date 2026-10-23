package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// B3-D3: the owner verbs run in the owner CLI against a store that holds no
// serve state, so a nil pool is refused at construction rather than at the
// first verb.
func TestOpenOwnerRefusesNilDB(t *testing.T) {
	o, err := OpenOwner(OwnerConfig{})
	if err == nil {
		t.Fatal("OpenOwner accepted a config with no database")
	}
	if o != nil {
		t.Fatalf("OpenOwner returned a non-nil *Owner alongside the error: %+v", o)
	}
}

// B3-D3: Operator is the four HTTP routes serve registers (§3.4). The other
// seven verbs are owner-CLI verbs and are methods of *Owner alone, so a
// serve process cannot be handed one.
func TestOperatorInterfaceIsTheFourRoutes(t *testing.T) {
	typ := reflect.TypeOf((*Operator)(nil)).Elem()
	if typ.Kind() != reflect.Interface {
		t.Fatalf("Operator is a %s, want an interface", typ.Kind())
	}
	got := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	slices.Sort(got)
	want := []string{"InsertLegacyPopulation", "ReconcileHold", "ReconcileImport", "ReconcileRequired"}
	if typ.NumMethod() != 4 || !slices.Equal(got, want) {
		t.Fatalf("Operator has %d methods %v, want exactly %v", typ.NumMethod(), got, want)
	}
}

// B3-D6 maps each sentinel to a distinct exit reason and message, so a
// sentinel that wrapped or aliased another would send two different refusals
// down one path.
func TestOwnerSentinelsAreDistinct(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrSealed", ErrSealed},
		{"ErrReconciled", ErrReconciled},
		{"ErrBadAuthz", ErrBadAuthz},
		{"ErrNotLeader", ErrNotLeader},
		{"ErrNotReplicated", ErrNotReplicated},
		{"ErrRefused", ErrRefused},
	}
	for _, s := range sentinels {
		if s.err == nil {
			t.Fatalf("%s is nil", s.name)
		}
		if !errors.Is(s.err, s.err) {
			t.Errorf("%s is not itself under errors.Is", s.name)
		}
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i != j && errors.Is(a.err, b.err) {
				t.Errorf("errors.Is(%s, %s) is true: the sentinels are not distinct", a.name, b.name)
			}
		}
	}
}
