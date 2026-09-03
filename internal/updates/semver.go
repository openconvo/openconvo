package updates

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var gitDescribeVersion = regexp.MustCompile(`(?i)^v?\d+\.\d+\.\d+-\d+-g[0-9a-f]+(?:-dirty)?$`)

func isDevelopmentBuild(input string) bool {
	value := strings.ToLower(strings.TrimSpace(input))
	return strings.Contains(value, "dirty") || strings.Contains(value, "-dev") || gitDescribeVersion.MatchString(value)
}

type semVersion struct {
	major, minor, patch int
	prerelease          []string
}

func parseVersion(input string) (semVersion, error) {
	value := strings.TrimPrefix(strings.TrimSpace(input), "v")
	value = strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(value, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return semVersion{}, fmt.Errorf("version %q is not semantic", input)
	}
	parsed := make([]int, 3)
	for i, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return semVersion{}, fmt.Errorf("version %q is not semantic", input)
		}
		value, err := strconv.Atoi(number)
		if err != nil || value < 0 {
			return semVersion{}, fmt.Errorf("version %q is not semantic", input)
		}
		parsed[i] = value
	}
	result := semVersion{major: parsed[0], minor: parsed[1], patch: parsed[2]}
	if len(parts) == 2 {
		if parts[1] == "" {
			return semVersion{}, fmt.Errorf("version %q is not semantic", input)
		}
		result.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range result.prerelease {
			if identifier == "" {
				return semVersion{}, fmt.Errorf("version %q is not semantic", input)
			}
			for _, character := range identifier {
				if character != '-' && !(character >= '0' && character <= '9') &&
					!(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') {
					return semVersion{}, fmt.Errorf("version %q is not semantic", input)
				}
			}
		}
	}
	return result, nil
}

func (v semVersion) compare(other semVersion) int {
	for _, pair := range [][2]int{{v.major, other.major}, {v.minor, other.minor}, {v.patch, other.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(v.prerelease) == 0 && len(other.prerelease) == 0 {
		return 0
	}
	if len(v.prerelease) == 0 {
		return 1
	}
	if len(other.prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(v.prerelease) && i < len(other.prerelease); i++ {
		left, right := v.prerelease[i], other.prerelease[i]
		if left == right {
			continue
		}
		leftNumber, leftErr := strconv.Atoi(left)
		rightNumber, rightErr := strconv.Atoi(right)
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		case left < right:
			return -1
		default:
			return 1
		}
	}
	if len(v.prerelease) < len(other.prerelease) {
		return -1
	}
	if len(v.prerelease) > len(other.prerelease) {
		return 1
	}
	return 0
}
