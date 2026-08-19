package dm_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Helpers for reading decoded JSON response bodies.
//
// A single-return type assertion (`body["x"].(map[string]interface{})`) PANICS
// when the response is not the expected shape. A panic aborts the whole dm_test
// binary, so every other test in the package is reported as failed and the one
// assertion that actually tripped is buried. These helpers assert with the
// two-return comma-ok form and fail ONE test with a readable message instead.
//
// ponytail: three generic helpers rather than comma-ok expanded at ~70 call
// sites — same guarantee, no boilerplate.

// jsonAs asserts that v holds a T.
func jsonAs[T any](t *testing.T, v interface{}, what string) T {
	t.Helper()
	tv, ok := v.(T)
	require.True(t, ok, "%s: got %T (%v), want %T", what, v, v, tv)
	return tv
}

// jsonField asserts that m[key] holds a T.
func jsonField[T any](t *testing.T, m map[string]interface{}, key string) T {
	t.Helper()
	return jsonAs[T](t, m[key], fmt.Sprintf("field %q", key))
}

// jsonElem asserts that s[i] exists and holds a T.
func jsonElem[T any](t *testing.T, s []interface{}, i int) T {
	t.Helper()
	// Both bounds. require.Greater alone reads len(s) > i, which is true for a
	// negative i on any slice -- s[i] would then panic, the exact failure mode
	// this file exists to remove.
	if i < 0 || i >= len(s) {
		t.Fatalf("index %d out of range (len %d)", i, len(s))
	}
	return jsonAs[T](t, s[i], fmt.Sprintf("index %d", i))
}
