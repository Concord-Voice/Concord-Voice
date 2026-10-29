package clientversion

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Version
		wantErr bool
	}{
		{name: "zero version", raw: "0.0.0", want: Version{}},
		{name: "stable version", raw: "0.2.44", want: Version{Major: 0, Minor: 2, Patch: 44}},
		{name: "maximum uint64 component", raw: "18446744073709551615.0.0", want: Version{Major: ^uint64(0)}},
		{name: "exactly 32 bytes", raw: "1234567890.1234567890.1234567890", want: Version{Major: 1234567890, Minor: 1234567890, Patch: 1234567890}},
		{name: "empty input", raw: "", wantErr: true},
		{name: "empty major", raw: ".1.2", wantErr: true},
		{name: "empty minor", raw: "1..2", wantErr: true},
		{name: "empty patch", raw: "1.2.", wantErr: true},
		{name: "missing component", raw: "1.2", wantErr: true},
		{name: "extra component", raw: "1.2.3.4", wantErr: true},
		{name: "leading zero major", raw: "01.2.3", wantErr: true},
		{name: "leading zero minor", raw: "1.02.3", wantErr: true},
		{name: "leading zero patch", raw: "1.2.03", wantErr: true},
		{name: "v prefix", raw: "v1.2.3", wantErr: true},
		{name: "prerelease suffix", raw: "1.2.3-alpha", wantErr: true},
		{name: "build metadata suffix", raw: "1.2.3+build", wantErr: true},
		{name: "leading whitespace", raw: " 1.2.3", wantErr: true},
		{name: "trailing whitespace", raw: "1.2.3 ", wantErr: true},
		{name: "tab whitespace", raw: "1.2.3\t", wantErr: true},
		{name: "non ASCII digits", raw: "１.2.3", wantErr: true},
		{name: "control byte", raw: "1.2.\x00", wantErr: true},
		{name: "newline", raw: "1.2.3\n", wantErr: true},
		{name: "component overflow", raw: "18446744073709551616.0.0", wantErr: true},
		{name: "over 32 bytes", raw: "12345678901.1234567890.1234567890", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name        string
		left, right Version
		want        int
	}{
		{name: "equal", left: Version{1, 2, 3}, right: Version{1, 2, 3}, want: 0},
		{name: "major ordering", left: Version{2, 0, 0}, right: Version{1, 99, 99}, want: 1},
		{name: "minor is numeric not lexical", left: Version{0, 10, 0}, right: Version{0, 9, 99}, want: 1},
		{name: "patch ordering", left: Version{1, 2, 3}, right: Version{1, 2, 4}, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compare(tt.left, tt.right); got != tt.want {
				t.Fatalf("Compare(%#v, %#v) = %d, want %d", tt.left, tt.right, got, tt.want)
			}
		})
	}
}

func TestParseClient(t *testing.T) {
	valid := map[string]Version{
		"0.0.0": {}, "1.2.3": {1, 2, 3}, "1.0.0-pilot.0": {1, 0, 0}, "1.1.0-pilot.2": {1, 1, 0},
		"1.0.0-pilot.18446744073709551615": {1, 0, 0},
		"18446744073709551615.0.0-pilot.0": {^uint64(0), 0, 0},
	}
	for raw, want := range valid {
		t.Run(raw, func(t *testing.T) {
			got, err := ParseClient(raw)
			if err != nil || got != want {
				t.Fatalf("ParseClient(%q) = %#v, %v; want %#v", raw, got, err, want)
			}
		})
	}
	for _, raw := range []string{
		"", "1.1.1-pilot.2", "1.1.0-pilot.", "1.1.0-pilot.01", "1.1.0-pilot.-1",
		"1.1.0-pilot.18446744073709551616", "1.1.0-pilot.1+build", "1.1.0-Pilot.1",
		"1.1.0-pilot.1\n", " 1.1.0-pilot.1", "1.1.0-pilot.1 ", "1.1.0-pilot.1\t", "1.1.0-pilot.1\x00",
		"01.1.0-pilot.1", "1.01.0-pilot.1", "1.1.00-pilot.1", "1.1.0-pilot.１", "1.1.0-beta.1",
		"18446744073709551616.0.0-pilot.0", "1.18446744073709551616.0-pilot.0",
		"12.0.0-pilot.18446744073709551615", "1.1.0-pilot.1-pilot.2",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseClient(raw); err == nil {
				t.Fatalf("ParseClient(%q) accepted malformed input", raw)
			}
		})
	}
}

func TestParseRejectsPilot(t *testing.T) {
	for _, raw := range []string{"1.0.0-pilot.0", "1.1.0-pilot.2"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("stable Parse(%q) accepted Pilot", raw)
		}
	}
}
