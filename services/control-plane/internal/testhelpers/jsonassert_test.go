package testhelpers

import (
	"strings"
	"testing"
)

func TestJSONAsE_WrongType(t *testing.T) {
	_, err := jsonAsE[map[string]interface{}](42.0, `field "conversation"`)
	if err == nil {
		t.Fatal("want error for float64 -> map, got nil")
	}
	want := `field "conversation": got float64 (42), want map[string]interface {}`
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

func TestJSONAsE_HappyPath(t *testing.T) {
	got, err := jsonAsE[string]("abc", "x")
	if err != nil || got != "abc" {
		t.Fatalf("got (%q, %v), want (\"abc\", nil)", got, err)
	}
}

func TestJSONFieldE_MissingKey(t *testing.T) {
	if _, err := jsonFieldE[string](map[string]interface{}{}, "nope"); err == nil {
		t.Fatal("want error for missing key, got nil")
	}
}

func TestJSONElemE_NegativeIndex(t *testing.T) {
	_, err := jsonElemE[string]([]interface{}{"a", "b"}, -1)
	if err == nil || err.Error() != "index -1 out of range (len 2)" {
		t.Fatalf("got %v, want index -1 out of range (len 2)", err)
	}
}

func TestJSONElemE_OverBound(t *testing.T) {
	_, err := jsonElemE[string]([]interface{}{"a"}, 5)
	if err == nil || err.Error() != "index 5 out of range (len 1)" {
		t.Fatalf("got %v, want index 5 out of range (len 1)", err)
	}
}

// --- regression locks from PR #2813 review ---------------------------------

type stringy interface{ String() string }

// %T on a zero value of an INTERFACE T renders "<nil>", so the message stopped
// naming what was wanted. reflect.TypeFor[T]() is correct for interfaces too.
func TestJSONAsE_InterfaceTypeIsNamed(t *testing.T) {
	_, err := jsonAsE[stringy](42, "x")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if want := "want testhelpers.stringy"; !strings.Contains(err.Error(), want) {
		t.Fatalf("got %q, want it to contain %q", err.Error(), want)
	}
}

// An absent key and an explicit JSON null are different server bugs.
func TestJSONFieldE_AbsentVsNull(t *testing.T) {
	_, absentErr := jsonFieldE[string](map[string]interface{}{}, "k")
	_, nullErr := jsonFieldE[string](map[string]interface{}{"k": nil}, "k")
	if absentErr == nil || nullErr == nil {
		t.Fatal("both cases must error")
	}
	if !strings.Contains(absentErr.Error(), "absent from object") {
		t.Fatalf("absent: got %q", absentErr.Error())
	}
	if absentErr.Error() == nullErr.Error() {
		t.Fatalf("absent and null must be distinguishable, both said %q", absentErr.Error())
	}
}

// The exported wrappers are the public surface; the failure paths are
// untestable (FailNow -> Goexit) but the success paths lock the return-value
// and t.Helper() wiring.
func TestJSONWrappers_HappyPath(t *testing.T) {
	body := map[string]interface{}{
		"conv":  map[string]interface{}{"id": "abc"},
		"items": []interface{}{"first", "second"},
	}
	conv := JSONField[map[string]interface{}](t, body, "conv")
	if got := JSONField[string](t, conv, "id"); got != "abc" {
		t.Fatalf("JSONField: got %q, want abc", got)
	}
	items := JSONField[[]interface{}](t, body, "items")
	if got := JSONElem[string](t, items, 1); got != "second" {
		t.Fatalf("JSONElem: got %q, want second", got)
	}
	if got := JSONAs[string](t, items[0], "items[0]"); got != "first" {
		t.Fatalf("JSONAs: got %q, want first", got)
	}
}
