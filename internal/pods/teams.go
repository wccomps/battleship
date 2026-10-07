// Package pods holds the competition rules: which VMs make up a team's pod,
// what they are named, how they are wired, and how to converge them.
package pods

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// FormatTeam zero-pads a team number: 7 -> "07". n must be 0-99.
func FormatTeam(n int) string { return fmt.Sprintf("%02d", n) }

// ParseTeams parses "7", "1-32", "1,3,5" or "1-3,7" into sorted, unique
// two-digit teams. A single number means that team only; 00 is the test
// team.
func ParseTeams(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("no teams given (e.g. 7, 1-32 or 1,3,5)")
	}

	seen := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("extra comma in %q", spec)
		}
		lo, hi := part, part
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, hi = a, b
		}

		start, err := parseTeamNumber(part, strings.TrimSpace(lo))
		if err != nil {
			return nil, err
		}
		end, err := parseTeamNumber(part, strings.TrimSpace(hi))
		if err != nil {
			return nil, err
		}

		if start > end {
			return nil, fmt.Errorf("range %q is backwards; write it low-high, e.g. 1-5", part)
		}

		for n := start; n <= end; n++ {
			seen[n] = true
		}
	}

	return teamList(seen), nil
}

// teamList is the teams in seen, sorted, two digits each.
func teamList(seen map[int]bool) []string {
	teams := make([]string, 0, len(seen))
	for _, n := range slices.Sorted(maps.Keys(seen)) {
		teams = append(teams, FormatTeam(n))
	}
	return teams
}

// TeamRanges groups teams into consecutive runs: 01 02 03 07 -> [1 3] [7 7].
// Non-numeric teams are left out.
func TeamRanges(teams []string) [][2]int {
	var nums []int
	for _, t := range teams {
		if n, err := strconv.Atoi(t); err == nil {
			nums = append(nums, n)
		}
	}
	slices.Sort(nums)
	nums = slices.Compact(nums)
	var out [][2]int
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		out = append(out, [2]int{nums[i], nums[j]})
		i = j + 1
	}
	return out
}

// FormatTeams writes teams the way people type them, with runs as ranges:
// 01 02 03 07 -> "1-3,7". ParseTeams reads it back.
func FormatTeams(teams []string) string {
	var parts []string
	for _, r := range TeamRanges(teams) {
		if r[0] == r[1] {
			parts = append(parts, strconv.Itoa(r[0]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", r[0], r[1]))
		}
	}
	return strings.Join(parts, ",")
}

// parseTeamNumber parses s; part is the spec piece that errors quote.
func parseTeamNumber(part, s string) (int, error) {
	if !isAllDigits(s) {
		if s == part {
			return 0, fmt.Errorf("invalid team %q (use numbers like 7 or 1-32)", s)
		}
		return 0, fmt.Errorf("invalid team %q in %q (use numbers like 7 or 1-32)", s, part)
	}

	n, err := strconv.Atoi(s) // all digits, so only overflow fails
	if err != nil || n > 99 {
		return 0, fmt.Errorf("team %s is out of range (0-99)", s)
	}

	return n, nil
}

func isAllDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
