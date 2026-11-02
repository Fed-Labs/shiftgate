package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScoreboardResponseNeverCarriesExactRevenue is the regression guard for
// the scoreboard revenue leak: the wire shape must carry banded money only,
// never an exact-cents figure any authenticated principal could scrape.
func TestScoreboardResponseNeverCarriesExactRevenue(t *testing.T) {
	response := scoreboardResponse{
		From: "2026-10-14", To: "2026-10-21",
		NewOrgs:             3,
		ActiveSubscriptions: map[string]int{"pro": 7, "business": 2},
		MRRBandUSD:          revenueBandUSD(453),
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(encoded)
	for _, leak := range []string{"mrr_cents", "453"} {
		if strings.Contains(payload, leak) {
			t.Fatalf("scoreboard payload leaks %q: %s", leak, payload)
		}
	}
	if !strings.Contains(payload, `"mrr_band_usd":"$100–500"`) {
		t.Fatalf("banded revenue missing or finer than the public band: %s", payload)
	}
}

// TestRevenueBandMatchesPublicBands keeps the scoreboard's money banding no
// finer than the public numbers page's, so no surface publishes a figure the
// other refuses to.
func TestRevenueBandMatchesPublicBands(t *testing.T) {
	cases := map[int64]string{
		0:     "$0–100",
		29:    "$0–100",
		453:   "$100–500",
		600:   "$500–1k",
		12345: "$10k–50k",
		60000: "$50k+",
	}
	for dollars, want := range cases {
		if got := revenueBandUSD(dollars); got != want {
			t.Fatalf("revenueBandUSD(%d) = %q, want %q", dollars, got, want)
		}
	}
}
