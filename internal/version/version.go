// Package version implements PSD-009 §2.2 version parsing and comparison.
//
// A version string has the form `[<epoch>:]<upstream>-<peios_revision>`:
//
//   - Epoch is an optional non-negative integer; absent means 0.
//   - Upstream is the upstream project's version, drawn from a permissive
//     character set to accommodate diverse upstream conventions.
//   - Peios revision is a positive integer identifying the build of this
//     upstream version produced by the Peios project.
//
// Comparison is hierarchical: epoch first (integer), then upstream
// (tokenised and segment-compared per §2.2.7), then revision (integer).
// The upstream comparator is the only non-trivial piece — it tokenises
// the string, identifies pre-release segments, and applies the asymmetric
// end-of-sequence rules from §2.2.7.4.
//
// This package has no I/O, no external state, and no external dependencies.
// The version comparator is a pure function of its inputs.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed PSD-009 version. Compare with Compare, format with
// String. Construct only via Parse — the zero value is not a valid version.
type Version struct {
	Epoch    int
	Upstream string
	Revision int
}

// String renders v back to its canonical wire form. Round-trips Parse.
func (v Version) String() string {
	if v.Epoch == 0 {
		return fmt.Sprintf("%s-%d", v.Upstream, v.Revision)
	}
	return fmt.Sprintf("%d:%s-%d", v.Epoch, v.Upstream, v.Revision)
}

// Parse parses a version string per §2.2.5.
func Parse(s string) (Version, error) {
	var v Version

	// Epoch (optional, separated by ':').
	rest := s
	if i := strings.IndexByte(s, ':'); i >= 0 {
		epochStr := s[:i]
		if epochStr == "" {
			return v, fmt.Errorf("version %q: empty epoch", s)
		}
		if !isASCIIDigits(epochStr) {
			return v, fmt.Errorf("version %q: epoch %q is not decimal digits", s, epochStr)
		}
		if len(epochStr) > 1 && epochStr[0] == '0' {
			return v, fmt.Errorf("version %q: epoch %q has leading zero", s, epochStr)
		}
		e, err := strconv.Atoi(epochStr)
		if err != nil {
			return v, fmt.Errorf("version %q: parse epoch: %w", s, err)
		}
		v.Epoch = e
		rest = s[i+1:]
	}

	// Revision (after the LAST hyphen).
	i := strings.LastIndexByte(rest, '-')
	if i < 0 {
		return v, fmt.Errorf("version %q: missing peios revision (expected '<upstream>-<revision>')", s)
	}
	upstream, revStr := rest[:i], rest[i+1:]
	if upstream == "" {
		return v, fmt.Errorf("version %q: empty upstream", s)
	}
	if revStr == "" {
		return v, fmt.Errorf("version %q: empty revision", s)
	}
	if !isASCIIDigits(revStr) {
		return v, fmt.Errorf("version %q: revision %q is not decimal digits", s, revStr)
	}
	if len(revStr) > 1 && revStr[0] == '0' {
		return v, fmt.Errorf("version %q: revision %q has leading zero", s, revStr)
	}
	rev, err := strconv.Atoi(revStr)
	if err != nil {
		return v, fmt.Errorf("version %q: parse revision: %w", s, err)
	}
	if rev < 1 {
		return v, fmt.Errorf("version %q: revision %d (must be >= 1; revision 0 is reserved)", s, rev)
	}
	v.Revision = rev

	if err := validateUpstream(upstream); err != nil {
		return v, fmt.Errorf("version %q: %w", s, err)
	}
	v.Upstream = upstream

	return v, nil
}

func validateUpstream(s string) error {
	if s == "" {
		return fmt.Errorf("upstream is empty")
	}
	first := s[0]
	if !isAlpha(first) && !isDigit(first) {
		return fmt.Errorf("upstream %q: first character must be a letter or digit", s)
	}
	for _, c := range []byte(s) {
		if !isAlpha(c) && !isDigit(c) && c != '.' && c != '+' && c != '-' && c != '~' {
			return fmt.Errorf("upstream %q: forbidden character %q", s, c)
		}
	}
	return nil
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if !isDigit(c) {
			return false
		}
	}
	return true
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Compare returns -1 if a < b, 0 if equal, +1 if a > b. Equality is in
// the §2.2.6 sense: epoch, upstream, and revision all match.
func Compare(a, b Version) int {
	if a.Epoch != b.Epoch {
		return cmpInt(a.Epoch, b.Epoch)
	}
	if c := compareUpstream(a.Upstream, b.Upstream); c != 0 {
		return c
	}
	return cmpInt(a.Revision, b.Revision)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// segment is one tokenisation unit of an upstream version. The pre flag
// captures the "is pre-release" property derived from §2.2.7.3 (recognised
// pre-release tokens, ~ separator, and -<token> implicit detection).
type segment struct {
	text     string
	numeric  bool
	numValue int64
	rank     int // §2.2.7.2 pre-release rank for alphabetic; ignored for numeric
	pre      bool
}

// preReleaseRanks lists the §2.2.7.2 explicitly-ranked pre-release tokens.
// Lookup is case-insensitive (the spec says "Comparison is case-insensitive
// for pre-release rank lookup").
//
// Tokens at the same rank are aliases per §2.2.7.2: `alpha` ≡ `a`,
// `beta` ≡ `b`. Two segments at the same recognised rank (0–4) sort
// equal regardless of which alias they use. Lexical tiebreak applies
// only at rank 5 (unrecognised tokens).
var preReleaseRanks = map[string]int{
	"dev":   0,
	"alpha": 1,
	"a":     1,
	"beta":  2,
	"b":     2,
	"pre":   3,
	"rc":    4,
}

// nonPreReleaseRank is the rank assigned to alphabetic tokens not in the
// pre-release table. Per §2.2.7.2 they sort by ASCII byte order amongst
// themselves and above all pre-release ranks.
const nonPreReleaseRank = 5

// compareUpstream returns -1, 0, or 1 per §2.2.7.
func compareUpstream(a, b string) int {
	sa := tokenise(a)
	sb := tokenise(b)

	common := len(sa)
	if len(sb) < common {
		common = len(sb)
	}
	for i := range common {
		if c := compareSegment(sa[i], sb[i]); c != 0 {
			return c
		}
	}

	// Equal up to the shorter length: apply §2.2.7.4 end-of-string rules.
	if len(sa) == len(sb) {
		return 0
	}
	var (
		next    segment
		shorter int // -1 if a is shorter, +1 if b is shorter
	)
	if len(sa) < len(sb) {
		next = sb[common]
		shorter = -1
	} else {
		next = sa[common]
		shorter = 1
	}

	// next is from the longer sequence. Translate "shorter < longer" into
	// the (a vs b) result via the shorter sign.
	switch {
	case next.numeric:
		// shorter < longer (numeric extension makes the longer one greater)
		return shorter
	case next.pre:
		// shorter > longer (pre-release suffix makes the longer one lesser)
		return -shorter
	default:
		// shorter < longer (regular alphabetic extension)
		return shorter
	}
}

// compareSegment implements §2.2.7.2's three-way comparison.
func compareSegment(a, b segment) int {
	switch {
	case a.numeric && b.numeric:
		return cmpInt64(a.numValue, b.numValue)
	case !a.numeric && !b.numeric:
		if c := cmpInt(a.rank, b.rank); c != 0 {
			return c
		}
		// Equal rank: aliases at recognised ranks (0–4) are equivalent
		// (§2.2.7.2 alias rule). Only rank 5 (unrecognised tokens)
		// tiebreaks lexically.
		if a.rank == nonPreReleaseRank {
			return strings.Compare(a.text, b.text)
		}
		return 0
	default:
		// Mixed: §2.2.7.2 rule 3.
		if a.numeric {
			// b is alphabetic
			if b.pre {
				return 1 // pre-release alpha < numeric, so b < a
			}
			return -1 // non-pre alpha > numeric, so a < b
		}
		// a is alphabetic, b is numeric
		if a.pre {
			return -1
		}
		return 1
	}
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// tokenise splits an upstream version string into segments per §2.2.7.1
// and §2.2.7.3.
//
// The algorithm is two-pass:
//
//  1. Split on the separator characters `.`, `+`, `-`, `~`, producing a
//     list of (chunk, separator-before) pairs. Within each chunk, split
//     again at every digit↔letter transition to produce segments.
//
//  2. Walk the segments left-to-right tracking whether the current
//     hyphen-group is "infected" by pre-release semantics. A `-` separator
//     resets the infection (start of a new hyphen-group). A `~` separator
//     sets it (everything after a tilde is pre-release). An alphabetic
//     segment whose lowercase text is in the pre-release rank table also
//     sets it (and is itself pre-release). Numeric segments inherit the
//     current infection state.
func tokenise(upstream string) []segment {
	type chunk struct {
		text string
		sep  byte // separator BEFORE this chunk; 0 for the first chunk
	}

	var chunks []chunk
	start := 0
	var prevSep byte
	for i := 0; i <= len(upstream); i++ {
		atEnd := i == len(upstream)
		var c byte
		if !atEnd {
			c = upstream[i]
		}
		if atEnd || c == '.' || c == '+' || c == '-' || c == '~' {
			if i > start {
				chunks = append(chunks, chunk{text: upstream[start:i], sep: prevSep})
			}
			if !atEnd {
				prevSep = c
				start = i + 1
			}
		}
	}

	var out []segment
	hyphenGroupPre := false

	for _, ch := range chunks {
		// Update hyphen-group pre-release state from the separator that
		// introduced this chunk. `-` resets; `~` sets.
		switch ch.sep {
		case '-':
			hyphenGroupPre = false
		case '~':
			hyphenGroupPre = true
		}

		for _, seg := range splitAtDigitLetterTransitions(ch.text) {
			if seg.numeric {
				seg.pre = hyphenGroupPre
				out = append(out, seg)
				continue
			}
			// Alphabetic. Determine rank and (possibly) update infection.
			if rank, ok := preReleaseRanks[strings.ToLower(seg.text)]; ok {
				seg.rank = rank
				seg.pre = true
				hyphenGroupPre = true
			} else {
				seg.rank = nonPreReleaseRank
				seg.pre = hyphenGroupPre
			}
			out = append(out, seg)
		}
	}

	return out
}

// splitAtDigitLetterTransitions returns the segments produced by walking
// chunk and breaking at every transition between a digit run and a letter
// run. Numeric segments carry their parsed integer value; alphabetic
// segments carry the raw text (rank assigned by the caller).
func splitAtDigitLetterTransitions(chunk string) []segment {
	if chunk == "" {
		return nil
	}
	var out []segment
	start := 0
	startIsDigit := isDigit(chunk[0])
	for i := 1; i < len(chunk); i++ {
		c := isDigit(chunk[i])
		if c != startIsDigit {
			out = append(out, makeSegment(chunk[start:i]))
			start = i
			startIsDigit = c
		}
	}
	out = append(out, makeSegment(chunk[start:]))
	return out
}

func makeSegment(text string) segment {
	if isDigit(text[0]) {
		// strconv.ParseInt won't error here because we know the string is
		// all digits — splitAtDigitLetterTransitions guarantees it. But
		// arbitrarily long digit runs could overflow int64; clamp by
		// truncating to math.MaxInt64 in the unlikely event. Versions in
		// practice are well within int32, let alone int64.
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			// Overflow: a 19+ digit version segment. Treat as MaxInt64
			// so it compares as the largest possible numeric segment.
			n = 1<<63 - 1
		}
		return segment{text: text, numeric: true, numValue: n}
	}
	return segment{text: text, numeric: false}
}
