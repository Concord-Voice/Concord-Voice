package klipy_test

import (
	"strings"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/klipy"
	"github.com/stretchr/testify/assert"
)

func strPtr(s string) *string { return &s }

func TestValidateSlugNilOrEmpty(t *testing.T) {
	assert.True(t, klipy.ValidateSlug(nil), "nil slug should be valid (no GIF attached)")
	assert.True(t, klipy.ValidateSlug(strPtr("")), "empty slug should be valid (no GIF attached)")
}

func TestValidateSlugHappyPath(t *testing.T) {
	for _, slug := range []string{
		"abc",
		"abc-def",
		"happy-cat-dance",
		"ABC123",
		"a-b-c-d-e-f-g-h-i-j",
		"1234567890",
		strings.Repeat("a", klipy.MaxSlugLength), // exactly at limit
	} {
		assert.True(t, klipy.ValidateSlug(strPtr(slug)), "slug should be valid: %q", slug)
	}
}

func TestValidateSlugInvalidChars(t *testing.T) {
	for _, slug := range []string{
		"has space",
		"has/slash",
		"has.dot",
		"has_underscore",
		"has!bang",
		"has@at",
		"has#hash",
		"has?question",
		"has=equal",
		"has\nnewline",
	} {
		assert.False(t, klipy.ValidateSlug(strPtr(slug)), "slug should be rejected: %q", slug)
		assert.Contains(t, klipy.SlugValidationError(strPtr(slug)), "invalid characters")
	}
}

func TestValidateSlugTooLong(t *testing.T) {
	tooLong := strings.Repeat("a", klipy.MaxSlugLength+1)
	assert.False(t, klipy.ValidateSlug(strPtr(tooLong)))
	assert.Contains(t, klipy.SlugValidationError(strPtr(tooLong)), "maximum length")
}

func TestSlugValidationErrorNilReturnsEmpty(t *testing.T) {
	assert.Empty(t, klipy.SlugValidationError(nil))
}

func TestNormalizeSlugNilStaysNil(t *testing.T) {
	assert.Nil(t, klipy.NormalizeSlug(nil))
}

func TestNormalizeSlugEmptyBecomesNil(t *testing.T) {
	assert.Nil(t, klipy.NormalizeSlug(strPtr("")))
}

func TestNormalizeSlugWhitespaceBecomesNil(t *testing.T) {
	for _, s := range []string{" ", "  ", "\t", "\n", " \t\n "} {
		assert.Nil(t, klipy.NormalizeSlug(strPtr(s)), "should normalize %q to nil", s)
	}
}

func TestNormalizeSlugTrimsWhitespace(t *testing.T) {
	got := klipy.NormalizeSlug(strPtr("  happy-cat-dance  "))
	assert.NotNil(t, got)
	assert.Equal(t, "happy-cat-dance", *got)
}

func TestNormalizeSlugLeavesValidSlugAlone(t *testing.T) {
	got := klipy.NormalizeSlug(strPtr("abc-123"))
	assert.NotNil(t, got)
	assert.Equal(t, "abc-123", *got)
}
