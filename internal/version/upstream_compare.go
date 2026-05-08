// Helpers built on top of the spec-derived comparator in version.go.
// Kept in a separate file so version.go stays bit-identical to
// peipkg-repo's copy (both implement PSD-009 §2.2 verbatim).

package version

import "fmt"

// UpstreamGTE reports whether captured >= minVersion in the §2.2.7
// upstream-segment ordering. Both inputs are upstream-only strings
// (no peios revision); we synthesize a fake "<v>-1" suffix to satisfy
// Parse, which expects full peipkg version syntax. Both sides get the
// same revision, so the revision drops out and the comparison
// reduces to upstream segments alone.
//
// Returns an error when either input is unparseable. Callers that
// need a tolerant default (e.g., "tolerate bad config and emit") can
// treat the error as the "passes filter" case.
func UpstreamGTE(captured, minVersion string) (bool, error) {
	a, err := Parse(captured + "-1")
	if err != nil {
		return false, fmt.Errorf("captured version %q: %w", captured, err)
	}
	b, err := Parse(minVersion + "-1")
	if err != nil {
		return false, fmt.Errorf("min_version %q: %w", minVersion, err)
	}
	return Compare(a, b) >= 0, nil
}
