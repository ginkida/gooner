package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version represents a semantic version.
type Version struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string // e.g., "alpha", "beta.1", "rc.2"
	Build      string // e.g., "20240101", build metadata
	Raw        string // Original version string
}

// semverRegex matches semantic versions with optional v prefix.
// Examples: v1.2.3, 1.2.3, 1.2.3-beta.1, 1.2.3-rc.1+build.123
var semverRegex = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([a-zA-Z0-9.]+))?(?:\+([a-zA-Z0-9.]+))?$`)

// ParseVersion parses a version string into a Version struct.
func ParseVersion(s string) (*Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrVersionParse
	}

	matches := semverRegex.FindStringSubmatch(s)
	if matches == nil {
		return nil, fmt.Errorf("%w: %q", ErrVersionParse, s)
	}

	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])
	patch, _ := strconv.Atoi(matches[3])

	return &Version{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		Prerelease: matches[4],
		Build:      matches[5],
		Raw:        s,
	}, nil
}

// String returns the version as a string (without v prefix).
func (v *Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// IsPrerelease returns true if this is a prerelease version.
func (v *Version) IsPrerelease() bool {
	return v.Prerelease != ""
}

// Compare compares two versions.
// Returns -1 if v < other, 0 if v == other, 1 if v > other.
func (v *Version) Compare(other *Version) int {
	if other == nil {
		return 1
	}

	// Compare major.minor.patch
	if v.Major != other.Major {
		return compareInts(v.Major, other.Major)
	}
	if v.Minor != other.Minor {
		return compareInts(v.Minor, other.Minor)
	}
	if v.Patch != other.Patch {
		return compareInts(v.Patch, other.Patch)
	}

	// If both have no prerelease, they're equal
	if v.Prerelease == "" && other.Prerelease == "" {
		return 0
	}

	// A version without prerelease is greater than one with prerelease
	// e.g., 1.0.0 > 1.0.0-beta
	if v.Prerelease == "" {
		return 1
	}
	if other.Prerelease == "" {
		return -1
	}

	// Compare prereleases
	return comparePrerelease(v.Prerelease, other.Prerelease)
}

// LessThan returns true if v < other.
func (v *Version) LessThan(other *Version) bool {
	return v.Compare(other) < 0
}

// GreaterThan returns true if v > other.
func (v *Version) GreaterThan(other *Version) bool {
	return v.Compare(other) > 0
}

// Equal returns true if v == other.
func (v *Version) Equal(other *Version) bool {
	return v.Compare(other) == 0
}

// compareInts compares two integers.
func compareInts(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// comparePrerelease compares two prerelease strings.
// Uses semver precedence rules.
func comparePrerelease(a, b string) int {
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	maxLen := len(partsA)
	if len(partsB) > maxLen {
		maxLen = len(partsB)
	}

	for i := 0; i < maxLen; i++ {
		var partA, partB string
		if i < len(partsA) {
			partA = partsA[i]
		}
		if i < len(partsB) {
			partB = partsB[i]
		}

		// Empty parts are less than non-empty
		if partA == "" && partB != "" {
			return -1
		}
		if partA != "" && partB == "" {
			return 1
		}
		if partA == "" && partB == "" {
			continue
		}

		// Try to compare as numbers
		numA, errA := strconv.Atoi(partA)
		numB, errB := strconv.Atoi(partB)

		if errA == nil && errB == nil {
			// Both are numbers
			if numA != numB {
				return compareInts(numA, numB)
			}
		} else if errA == nil {
			// Only A is a number - numbers come before strings
			return -1
		} else if errB == nil {
			// Only B is a number
			return 1
		} else {
			// Both are strings - compare lexicographically
			cmp := strings.Compare(partA, partB)
			if cmp != 0 {
				return cmp
			}
		}
	}

	return 0
}

// IsNewerVersion returns true if newVer is newer than currentVer.
func IsNewerVersion(newVer, currentVer string) bool {
	newV, err := ParseVersion(newVer)
	if err != nil {
		return false
	}

	currentV, err := ParseVersion(currentVer)
	if err != nil {
		return false
	}

	return newV.GreaterThan(currentV)
}

// CompareVersionStrings compares two version strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Returns 0 if either version is invalid.
func CompareVersionStrings(a, b string) int {
	vA, errA := ParseVersion(a)
	vB, errB := ParseVersion(b)

	if errA != nil || errB != nil {
		return 0
	}

	return vA.Compare(vB)
}

// NormalizeVersion strips the 'v' prefix and returns a clean version string.
func NormalizeVersion(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 0 && s[0] == 'v' {
		return s[1:]
	}
	return s
}
