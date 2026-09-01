// Package clientversion parses and compares stable client versions.
package clientversion

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const maxVersionBytes = 32

// Version is a stable client version with numeric major, minor, and patch components.
type Version struct {
	Major uint64
	Minor uint64
	Patch uint64
}

// Parse parses a stable client version in X.Y.Z form.
func Parse(raw string) (Version, error) {
	if len(raw) == 0 || len(raw) > maxVersionBytes {
		return Version{}, errors.New("invalid client version")
	}

	components := strings.Split(raw, ".")
	if len(components) != 3 {
		return Version{}, errors.New("invalid client version")
	}

	values := [3]uint64{}
	for i, component := range components {
		if component == "" || (len(component) > 1 && component[0] == '0') {
			return Version{}, errors.New("invalid client version")
		}
		for j := 0; j < len(component); j++ {
			if component[j] < '0' || component[j] > '9' {
				return Version{}, errors.New("invalid client version")
			}
		}

		value, err := strconv.ParseUint(component, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("invalid client version component %d: %w", i+1, err)
		}
		values[i] = value
	}

	return Version{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

// Compare orders versions numerically, returning -1, 0, or 1.
func Compare(left, right Version) int {
	if left.Major != right.Major {
		if left.Major < right.Major {
			return -1
		}
		return 1
	}
	if left.Minor != right.Minor {
		if left.Minor < right.Minor {
			return -1
		}
		return 1
	}
	if left.Patch < right.Patch {
		return -1
	}
	if left.Patch > right.Patch {
		return 1
	}
	return 0
}
