// Package update implements signed, staged, reversible updates for the SHIFT
// agent and desktop application. An update is only ever applied when the
// release was signed by a trusted key, the artifact matches its recorded
// digest, the machine is inside the rollout cohort, the running version is
// compatible with the new one, and the machine is not in the middle of work
// that an upgrade would interrupt. A staged binary that cannot report its own
// version is never committed, and a committed swap keeps the previous binary so
// a bad release can be rolled back.
package update

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version. Updates need an ordering that is not string
// comparison: 0.10.0 must sort above 0.9.0, and a pre-release must sort below
// the release it leads to, so a nightly build is never treated as an upgrade
// over the stable version with the same numbers.
type Version struct {
	Major      int    `json:"major"`
	Minor      int    `json:"minor"`
	Patch      int    `json:"patch"`
	PreRelease string `json:"pre_release,omitempty"`
	Build      string `json:"build,omitempty"`
}

// ParseVersion reads a semantic version, with or without a leading "v".
func ParseVersion(value string) (Version, error) {
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(text, "v")
	if text == "" {
		return Version{}, errors.New("version is empty")
	}
	var version Version
	if index := strings.IndexByte(text, '+'); index >= 0 {
		version.Build = text[index+1:]
		text = text[:index]
		if version.Build == "" {
			return Version{}, errors.New("version has an empty build identifier")
		}
	}
	if index := strings.IndexByte(text, '-'); index >= 0 {
		version.PreRelease = text[index+1:]
		text = text[:index]
		if version.PreRelease == "" {
			return Version{}, errors.New("version has an empty pre-release identifier")
		}
	}
	numbers := strings.Split(text, ".")
	if len(numbers) != 3 {
		return Version{}, fmt.Errorf("version %q is not major.minor.patch", value)
	}
	targets := []*int{&version.Major, &version.Minor, &version.Patch}
	for index, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return Version{}, fmt.Errorf("version %q has a malformed number %q", value, number)
		}
		parsed, err := strconv.Atoi(number)
		if err != nil || parsed < 0 {
			return Version{}, fmt.Errorf("version %q has a non-numeric component %q", value, number)
		}
		*targets[index] = parsed
	}
	if err := validIdentifiers(version.PreRelease); err != nil {
		return Version{}, fmt.Errorf("version %q pre-release: %w", value, err)
	}
	if err := validIdentifiers(version.Build); err != nil {
		return Version{}, fmt.Errorf("version %q build: %w", value, err)
	}
	return version, nil
}

func validIdentifiers(value string) error {
	if value == "" {
		return nil
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return errors.New("empty identifier")
		}
		for _, character := range identifier {
			switch {
			case character >= '0' && character <= '9':
			case character >= 'a' && character <= 'z':
			case character >= 'A' && character <= 'Z':
			case character == '-':
			default:
				return fmt.Errorf("identifier %q contains %q", identifier, character)
			}
		}
	}
	return nil
}

// String renders the version in the form it was parsed from, without the "v".
func (version Version) String() string {
	text := strconv.Itoa(version.Major) + "." + strconv.Itoa(version.Minor) + "." + strconv.Itoa(version.Patch)
	if version.PreRelease != "" {
		text += "-" + version.PreRelease
	}
	if version.Build != "" {
		text += "+" + version.Build
	}
	return text
}

// Stable reports whether this is a released version rather than a pre-release.
func (version Version) Stable() bool {
	return version.PreRelease == ""
}

// Compare orders two versions by semantic-version precedence, returning a
// negative number when the receiver is older. Build metadata is ignored, as the
// specification requires: two builds of the same version are the same release.
func (version Version) Compare(other Version) int {
	for _, pair := range [][2]int{
		{version.Major, other.Major},
		{version.Minor, other.Minor},
		{version.Patch, other.Patch},
	} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return comparePreRelease(version.PreRelease, other.PreRelease)
}

// Precedes reports whether the receiver is strictly older than other.
func (version Version) Precedes(other Version) bool {
	return version.Compare(other) < 0
}

// SameRelease reports whether two versions are the same release, ignoring the
// build metadata that distinguishes rebuilds of identical source.
func (version Version) SameRelease(other Version) bool {
	return version.Compare(other) == 0
}

func comparePreRelease(left, right string) int {
	if left == right {
		return 0
	}
	// A version without a pre-release outranks one with it.
	if left == "" {
		return 1
	}
	if right == "" {
		return -1
	}
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		result := comparePreReleaseIdentifier(leftParts[index], rightParts[index])
		if result != 0 {
			return result
		}
	}
	switch {
	case len(leftParts) < len(rightParts):
		return -1
	case len(leftParts) > len(rightParts):
		return 1
	default:
		return 0
	}
}

func comparePreReleaseIdentifier(left, right string) int {
	leftNumber, leftNumeric := strconv.Atoi(left)
	rightNumber, rightNumeric := strconv.Atoi(right)
	switch {
	case leftNumeric == nil && rightNumeric == nil:
		switch {
		case leftNumber < rightNumber:
			return -1
		case leftNumber > rightNumber:
			return 1
		default:
			return 0
		}
	case leftNumeric == nil:
		// Numeric identifiers always have lower precedence than alphanumeric ones.
		return -1
	case rightNumeric == nil:
		return 1
	default:
		return strings.Compare(left, right)
	}
}
