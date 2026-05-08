package version

import (
	"strings"
	"testing"
)

func TestParseSpecExamples(t *testing.T) {
	cases := []struct {
		s        string
		want     Version
		wantBack string // String() round-trip
	}{
		{"1.26.2-3", Version{Epoch: 0, Upstream: "1.26.2", Revision: 3}, "1.26.2-3"},
		{"1.26.2-rc.1-1", Version{Epoch: 0, Upstream: "1.26.2-rc.1", Revision: 1}, "1.26.2-rc.1-1"},
		{"2:0.5.0-1", Version{Epoch: 2, Upstream: "0.5.0", Revision: 1}, "2:0.5.0-1"},
		{"0.22-1", Version{Epoch: 0, Upstream: "0.22", Revision: 1}, "0.22-1"},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			got, err := Parse(c.s)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("Parse(%q) = %+v, want %+v", c.s, got, c.want)
			}
			if got.String() != c.wantBack {
				t.Errorf("String() = %q, want %q", got.String(), c.wantBack)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",          // empty
		":",         // empty epoch and upstream
		"1.0",       // missing revision (no hyphen)
		"-1",        // empty upstream
		"1.0-",      // empty revision
		"1.0-0",     // revision 0 forbidden (§2.2.4)
		"1.0-01",    // leading-zero revision (§2.2.4)
		"01:1.0-1",  // leading-zero epoch (§2.2.3)
		"-1:1.0-1",  // negative epoch (not decimal digits)
		"abc:1.0-1", // non-numeric epoch
		".1.0-1",    // upstream starts with separator (§2.2.3)
		"1.0 -1",    // upstream contains space
		"1.0/-1",    // upstream contains forbidden char
	}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			if _, err := Parse(s); err == nil {
				t.Errorf("Parse(%q) succeeded; want error", s)
			}
		})
	}
}

// TestCompareSpecExamples covers every comparison the spec gives an
// example for, plus a few others that exercise the same rules.
func TestCompareSpecExamples(t *testing.T) {
	cases := []struct {
		a, b string
		want int // -1 if a < b, 0 if equal, 1 if a > b
	}{
		// §2.2.7.4 end-of-string examples.
		{"1.0-1", "1.0.1-1", -1},    // numeric extension: 1.0 < 1.0.1
		{"1.0-1", "1.0-rc.1-1", 1},  // pre-release suffix: 1.0 > 1.0-rc.1
		{"1.0-1", "1.0.alpha-1", 1}, // pre-release alphabetic via . separator
		{"1.0-1", "1.0a-1", 1},      // pre-release token via digit-letter transition

		// Epoch dominates everything else.
		{"2:0.5.0-1", "1:99.99-1", 1},
		{"0:1.0-1", "1.0-1", 0}, // explicit epoch 0 == implicit epoch 0

		// Revision is least-significant.
		{"1.0-1", "1.0-2", -1},
		{"1.0-1", "1.0-2", -1},
		{"1.0-10", "1.0-9", 1}, // numeric (not lexical) revision compare

		// Upstream segment-wise comparison.
		{"1.0-1", "1.0-1", 0},
		{"1.0-1", "1.1-1", -1},
		{"1.10-1", "1.9-1", 1}, // numeric (not lexical) segment compare

		// Pre-release ordering: dev < alpha < beta < pre < rc < release.
		{"1.0~dev1-1", "1.0~alpha1-1", -1},
		{"1.0~alpha1-1", "1.0~beta1-1", -1},
		{"1.0~beta1-1", "1.0~pre1-1", -1},
		{"1.0~pre1-1", "1.0~rc1-1", -1},
		{"1.0~rc1-1", "1.0-1", -1}, // any pre-release < release
		{"1.0~rc1-1", "1.0~rc2-1", -1},
		// alpha and a (and beta and b) are aliases per §2.2.7.2: tokens
		// at the same recognised rank sort equivalent. Lexical tiebreak
		// only applies at rank 5 (unrecognised tokens).
		{"1.0~a-1", "1.0~alpha-1", 0},
		{"1.0~b-1", "1.0~beta-1", 0},
		{"1.0~Alpha-1", "1.0~ALPHA-1", 0}, // case-insensitive too

		// `~` and implicit `-token` produce the same ordering.
		{"1.0~rc.1-1", "1.0-rc.1-1", 0},

		// `-foo` (unrecognized token) is NOT pre-release.
		{"1.0-1", "1.0-foo-1", -1}, // 1.0-foo > 1.0 (regular alphabetic extension)

		// Within-chunk digit-letter transitions form pre-release tokens.
		{"16beta1-1", "16-1", -1},      // 16beta1 < 16
		{"16beta1-1", "16beta2-1", -1}, // beta1 < beta2
		{"16beta1-1", "16rc1-1", -1},   // beta < rc

		// Case-insensitive pre-release recognition.
		{"1.0~Alpha-1", "1.0~beta-1", -1}, // Alpha rank == alpha rank == 1

		// Numeric segments don't suffer leading-zero ambiguity.
		{"1.01-1", "1.1-1", 0},
	}
	for _, c := range cases {
		t.Run(c.a+" vs "+c.b, func(t *testing.T) {
			va, err := Parse(c.a)
			if err != nil {
				t.Fatal(err)
			}
			vb, err := Parse(c.b)
			if err != nil {
				t.Fatal(err)
			}
			got := Compare(va, vb)
			if got != c.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
			}
			// Symmetry: Compare(b, a) == -Compare(a, b)
			rev := Compare(vb, va)
			if rev != -c.want {
				t.Errorf("Compare(%q, %q) = %d, want %d (asymmetric)",
					c.b, c.a, rev, -c.want)
			}
		})
	}
}

// TestTokeniseExamples spot-checks the tokenisation table from the spec.
// It uses the public Compare path indirectly (through behaviours that
// would diverge if tokenisation were wrong) but verifies the segment
// list directly for clarity.
func TestTokeniseExamples(t *testing.T) {
	cases := []struct {
		upstream string
		want     []seg // text, numeric, pre
	}{
		{"1.26.2", []seg{{"1", true, false}, {"26", true, false}, {"2", true, false}}},
		{"1.0.0-rc.1", []seg{
			{"1", true, false},
			{"0", true, false},
			{"0", true, false},
			{"rc", false, true},
			{"1", true, true},
		}},
		{"1.0~rc1", []seg{
			{"1", true, false},
			{"0", true, false},
			{"rc", false, true},
			{"1", true, true},
		}},
		{"16beta1", []seg{
			{"16", true, false},
			{"beta", false, true},
			{"1", true, true},
		}},
		{"1.0-foo", []seg{
			{"1", true, false},
			{"0", true, false},
			{"foo", false, false}, // foo is rank-5, not pre-release
		}},
	}
	for _, c := range cases {
		t.Run(c.upstream, func(t *testing.T) {
			got := tokenise(c.upstream)
			if len(got) != len(c.want) {
				t.Fatalf("tokenise(%q) length = %d, want %d (%v)",
					c.upstream, len(got), len(c.want), summarise(got))
			}
			for i, w := range c.want {
				g := got[i]
				if g.text != w.text || g.numeric != w.numeric || g.pre != w.pre {
					t.Errorf("tokenise(%q)[%d] = {text:%q, numeric:%v, pre:%v}, want {%q, %v, %v}",
						c.upstream, i, g.text, g.numeric, g.pre, w.text, w.numeric, w.pre)
				}
			}
		})
	}
}

// TestStability ensures version comparison is total and reflexive.
// Property test on a small handful of versions.
func TestStabilityProperties(t *testing.T) {
	versions := []string{
		"1.0-1", "1.0-2", "1.1-1", "2.0-1",
		"1.0~alpha-1", "1.0~beta-1", "1.0~rc1-1",
		"1.0-rc.1-1", "1.0a-1",
		"1:1.0-1", "2:1.0-1",
		"1.0-foo-1", "1.0+build.42-1",
	}
	parsed := make([]Version, len(versions))
	for i, s := range versions {
		v, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		parsed[i] = v
	}

	for i, va := range parsed {
		// Reflexivity
		if Compare(va, va) != 0 {
			t.Errorf("Compare(%q, %q) != 0", versions[i], versions[i])
		}
		for j, vb := range parsed {
			if i == j {
				continue
			}
			cab := Compare(va, vb)
			cba := Compare(vb, va)
			// Antisymmetry
			if cab != -cba {
				t.Errorf("Compare(%q,%q)=%d but Compare(%q,%q)=%d (asymmetric)",
					versions[i], versions[j], cab, versions[j], versions[i], cba)
			}
		}
	}
}

type seg struct {
	text    string
	numeric bool
	pre     bool
}

func summarise(segs []segment) string {
	parts := make([]string, len(segs))
	for i, s := range segs {
		marker := ""
		if s.pre {
			marker = "(pre)"
		}
		parts[i] = s.text + marker
	}
	return strings.Join(parts, " ")
}
