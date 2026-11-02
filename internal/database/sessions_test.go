package database

import "testing"

// Refresh rotation is a three-way decision table: the live-hash lookup wins,
// then the stale-hash lookup, then plain rejection. Reuse is what turns a
// stolen rotated-out token into a session-wide revocation.
func TestClassifyRefreshLookup(t *testing.T) {
	for _, test := range []struct {
		name                     string
		currentFound, reuseFound bool
		want                     refreshClassification
	}{
		{name: "live hash rotates", currentFound: true, reuseFound: false, want: refreshRotate},
		{name: "live hash wins even when a stale hash also matches", currentFound: true, reuseFound: true, want: refreshRotate},
		{name: "stale hash is reuse", currentFound: false, reuseFound: true, want: refreshReuse},
		{name: "unknown digest is invalid", currentFound: false, reuseFound: false, want: refreshInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRefreshLookup(test.currentFound, test.reuseFound); got != test.want {
				t.Fatalf("classifyRefreshLookup(%v, %v) = %v, want %v", test.currentFound, test.reuseFound, got, test.want)
			}
		})
	}
}
