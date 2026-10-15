package config

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"pgregory.net/rapid"
)

func TestWildcardRegexpIsAnchoredAndCaseInsensitive(t *testing.T) {
	tests := map[string]struct {
		pattern string
		name    string
		want    bool
	}{
		"case insensitive": {
			pattern: "*.kilo.alpha",
			name:    "Violet.Kilo.Alpha",
			want:    true,
		},
		"anchored start": {
			pattern: "*.kilo.alpha",
			name:    "violet.kilo.alpha.old",
			want:    false,
		},
		"anchored end": {
			pattern: "*.kilo.alpha",
			name:    "violet.kilo.alpha",
			want:    true,
		},
		"no wildcards matches exactly": {
			pattern: "teak",
			name:    "Teak",
			want:    true,
		},
		"wildcard star alone": {
			pattern: "*",
			name:    "anything",
			want:    true,
		},
	}
	for testName, tt := range tests {
		t.Run(testName, func(t *testing.T) {
			re, err := WildcardRegexp(tt.pattern)
			if err != nil {
				t.Fatal(err)
			}
			if got := re.MatchString(tt.name); got != tt.want {
				t.Errorf("WildcardRegexp(%q).MatchString(%q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
			}
		})
	}
}

// patternPiece draws text for template patterns: any runes, raw bytes (so
// invalid UTF-8), and regexp metacharacters.
var patternPiece = rapid.OneOf(
	rapid.String(),
	rapid.Map(rapid.SliceOf(rapid.Byte()), func(b []byte) string { return string(b) }),
	rapid.SampledFrom([]string{"*", "\xff", "\xc3", "(", `\`, "[a-", ".", "$", "^", "K", "k", "K", "ß", "dc", "kilo"}),
)

// globMatch is a case-insensitive glob where only * is special.
func globMatch(pattern, name []rune) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == '*' {
		for i := 0; i <= len(name); i++ {
			if globMatch(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	return len(name) > 0 && strings.EqualFold(string(pattern[0]), string(name[0])) && globMatch(pattern[1:], name[1:])
}

// WildcardRegexp never panics: it refuses exactly invalid UTF-8, and
// otherwise matches as a case-insensitive glob.
func TestPropWildcardRegexpIsAGlob(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pattern := strings.Join(rapid.SliceOfN(patternPiece, 0, 6).Draw(t, "pattern"), "")
		re, err := WildcardRegexp(pattern)
		if !utf8.ValidString(pattern) {
			if err == nil {
				t.Fatalf("accepted invalid UTF-8 %q", pattern)
			}
			return
		}
		if err != nil {
			t.Fatalf("WildcardRegexp(%q): %v", pattern, err)
		}
		// A name made from the pattern, stars filled in and case flipped,
		// or any name at all.
		var b strings.Builder
		for _, r := range pattern {
			switch {
			case r == '*':
				b.WriteString(rapid.String().Draw(t, "fill"))
			case rapid.Bool().Draw(t, "flip"):
				b.WriteRune(unicode.SimpleFold(r))
			default:
				b.WriteRune(r)
			}
		}
		name := rapid.OneOf(rapid.Just(b.String()), rapid.String()).Draw(t, "name")
		if got, want := re.MatchString(name), globMatch([]rune(pattern), []rune(name)); got != want {
			t.Fatalf("WildcardRegexp(%q).MatchString(%q) = %v, want %v", pattern, name, got, want)
		}
	})
}
