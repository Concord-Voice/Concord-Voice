package testhelpers

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// Assertion helpers for decoded JSON response bodies.
//
// A single-return type assertion (`body["x"].(map[string]interface{})`) PANICS
// when the response is not the expected shape, and a panic aborts the whole
// package test binary -- every other test reports as failed and the assertion
// that actually tripped is buried. These assert with the two-return comma-ok
// form and fail ONE test with a readable message instead.
//
// Each helper is a pure (T, error) core plus a thin *testing.T wrapper. The
// split exists so the failure paths are unit-testable: require.NoError calls
// FailNow -> runtime.Goexit(), so a test asserting on the wrapper's failure
// would kill its own goroutine.
//
// The wrappers take *testing.T and NOT require.TestingT on purpose: TestingT is
// {Logf, Errorf, FailNow} and has no Helper(), and without t.Helper() every
// failure would report this file's line instead of the caller's.
//
// Enforced by scripts/check-no-single-return-json-assertion.sh (#2811).

func jsonAsE[T any](v interface{}, what string) (T, error) {
	tv, ok := v.(T)
	if !ok {
		// reflect.TypeFor[T](), not %T on the zero value: boxing a zero T into
		// interface{} loses the type whenever T is ITSELF an interface, so %T
		// renders "<nil>" and the message stops naming what was wanted. No call
		// site instantiates T as an interface today, but this is shared helper
		// handed to 19 packages. (code-reviewer, PR #2813.)
		return tv, fmt.Errorf("%s: got %T (%v), want %v", what, v, v, reflect.TypeFor[T]())
	}
	return tv, nil
}

func jsonFieldE[T any](m map[string]interface{}, key string) (T, error) {
	// A missing key and an explicit JSON null both read as nil here, but they
	// are different server bugs -- "the field was omitted" vs "the field was
	// sent as null" -- so say which. (code-reviewer, PR #2813.)
	raw, present := m[key]
	if !present {
		var zero T
		return zero, fmt.Errorf("field %q: absent from object", key)
	}
	return jsonAsE[T](raw, fmt.Sprintf("field %q", key))
}

func jsonElemE[T any](s []interface{}, i int) (T, error) {
	if i < 0 || i >= len(s) {
		var zero T
		return zero, fmt.Errorf("index %d out of range (len %d)", i, len(s))
	}
	return jsonAsE[T](s[i], fmt.Sprintf("index %d", i))
}

// JSONAs asserts that v holds a T, failing this test instead of panicking.
func JSONAs[T any](t *testing.T, v interface{}, what string) T {
	t.Helper()
	tv, err := jsonAsE[T](v, what)
	require.NoError(t, err)
	return tv
}

// JSONField asserts that m[key] holds a T, failing this test instead of panicking.
func JSONField[T any](t *testing.T, m map[string]interface{}, key string) T {
	t.Helper()
	tv, err := jsonFieldE[T](m, key)
	require.NoError(t, err)
	return tv
}

// JSONElem asserts that s[i] exists and holds a T, failing this test instead of
// panicking. Checks BOTH bounds: a negative index would otherwise reach s[i].
func JSONElem[T any](t *testing.T, s []interface{}, i int) T {
	t.Helper()
	tv, err := jsonElemE[T](s, i)
	require.NoError(t, err)
	return tv
}
