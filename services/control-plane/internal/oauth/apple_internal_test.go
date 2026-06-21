package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// parseAppleUserData is an unexported helper; these tests are package-internal.

func TestParseAppleUserData_HappyPath(t *testing.T) {
	got := parseAppleUserData(`{"name":{"firstName":"Jane","lastName":"Doe"},"email":"jane@example.com"}`)
	assert.Equal(t, "Jane Doe", got)
}

func TestParseAppleUserData_FirstNameOnly(t *testing.T) {
	got := parseAppleUserData(`{"name":{"firstName":"Jane"}}`)
	assert.Equal(t, "Jane", got)
}

func TestParseAppleUserData_LastNameOnly(t *testing.T) {
	got := parseAppleUserData(`{"name":{"lastName":"Doe"}}`)
	assert.Equal(t, "Doe", got)
}

func TestParseAppleUserData_Empty(t *testing.T) {
	assert.Equal(t, "", parseAppleUserData(""))
}

func TestParseAppleUserData_MalformedJSON(t *testing.T) {
	assert.Equal(t, "", parseAppleUserData("not json at all"))
}

func TestParseAppleUserData_NoNameField(t *testing.T) {
	assert.Equal(t, "", parseAppleUserData(`{"email":"jane@example.com"}`))
}

func TestParseAppleUserData_TrimsWhitespace(t *testing.T) {
	// Leading/trailing whitespace inside the name fields is tolerated; the
	// helper trims the joined "first last" so a missing first or last name
	// doesn't leave a stray space.
	got := parseAppleUserData(`{"name":{"firstName":"  Jane  ","lastName":"  Doe  "}}`)
	// The current contract trims only the joined string (spec §4.1 last line:
	// strings.TrimSpace on the concatenation). Per-field internal whitespace
	// is preserved verbatim — Apple does not send padded names in practice,
	// and trimming per-field would mask malformed inputs.
	assert.Equal(t, "Jane     Doe", got, "join is firstName + ' ' + lastName, only outer trim applied")
}

func TestParseAppleUserData_OnlyWhitespaceNames(t *testing.T) {
	// All-whitespace names → "" after trim.
	got := parseAppleUserData(`{"name":{"firstName":"   ","lastName":"   "}}`)
	assert.Equal(t, "", got)
}

// TestParseAppleUserData_InternationalNames covers RTL scripts (Arabic,
// Hebrew), CJK ideographs (Japanese, Chinese, Korean), Cyrillic, accented
// Latin. Apple Sign in is global; users have
// names in every Unicode-supported script. JSON unmarshaling is UTF-8 aware,
// so these should round-trip cleanly through the helper.
func TestParseAppleUserData_InternationalNames(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"Japanese", `{"name":{"firstName":"太郎","lastName":"田中"}}`, "太郎 田中"},
		{"Korean", `{"name":{"firstName":"수","lastName":"김"}}`, "수 김"},
		{"Chinese", `{"name":{"firstName":"伟","lastName":"王"}}`, "伟 王"},
		{"Arabic", `{"name":{"firstName":"محمد","lastName":"الأحمد"}}`, "محمد الأحمد"},
		{"Hebrew", `{"name":{"firstName":"יוסף","lastName":"כהן"}}`, "יוסף כהן"},
		{"Cyrillic", `{"name":{"firstName":"Иван","lastName":"Петров"}}`, "Иван Петров"},
		{"Accented Latin", `{"name":{"firstName":"François","lastName":"Müller"}}`, "François Müller"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAppleUserData(tc.json)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseAppleUserData_RejectsRawControlCharacters covers the negative
// case where a JSON name field contains an unescaped control character
// (U+0000 NUL is the canonical RFC 8259 violation). json.Unmarshal returns
// an error and parseAppleUserData maps that to "". This is the lower bound
// on the helper's resilience to malformed Apple input.
func TestParseAppleUserData_RejectsRawControlCharacters(t *testing.T) {
	// Literal "\x00" (NUL byte) inside the firstName value. JSON parsing
	// rejects unescaped U+0000 per RFC 8259 §7. The helper must catch
	// the resulting error and return "".
	got := parseAppleUserData("{\"name\":{\"firstName\":\"Jane\x00\",\"lastName\":\"Doe\"}}")
	assert.Equal(t, "", got, "raw NUL byte in name must surface as empty result")
}
